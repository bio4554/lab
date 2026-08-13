package kbased

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/kbclient"
)

// component creates a component entry through the API.
func component(t *testing.T, c *kbclient.Client, title string, global bool) kbclient.Entry {
	t.Helper()
	e, err := c.CreateEntry(context.Background(), kbclient.CreateEntryRequest{
		Type: "component", Title: title, Content: title, Global: global,
	})
	if err != nil {
		t.Fatalf("create component %q: %v", title, err)
	}
	return e
}

// TestEdgeValidationAndScope covers the server-side edge rules: both
// endpoints must be components readable in the edge's scope, and
// duplicate live edges conflict.
func TestEdgeValidationAndScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	agent := f.principal(t, "agent", "graph-agent")
	human := f.principal(t, "human", "graph-human")
	projA, projB := uuid.New(), uuid.New()
	cA := f.client(f.token(t, agent.ID, &projA))
	cB := f.client(f.token(t, agent.ID, &projB))
	cAll := f.client(f.token(t, human.ID, nil))

	api := component(t, cA, "api server", false)
	db := component(t, cA, "db", false)
	component(t, cAll, "cache", true) // global
	note, err := cA.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "not a component", Content: "n",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Non-component endpoint → 400.
	_, err = cA.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: note.Slug, Label: "uses"})
	wantAPIError(t, err, http.StatusBadRequest)

	// Own project → own project.
	edge, err := cA.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: db.Slug, Label: "uses"})
	if err != nil {
		t.Fatalf("link api->db: %v", err)
	}
	if edge.From != api.Slug || edge.To != db.Slug || edge.Label != "uses" {
		t.Fatalf("edge = %+v", edge)
	}
	if edge.ProjectID == nil || *edge.ProjectID != projA {
		t.Fatalf("edge scope = %v, want project A", edge.ProjectID)
	}

	// Duplicate live edge → 409; a different label is a new edge.
	_, err = cA.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: db.Slug, Label: "uses"})
	wantAPIError(t, err, http.StatusConflict)
	if _, err := cA.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: db.Slug, Label: "migrates"}); err != nil {
		t.Fatalf("different label: %v", err)
	}

	// Own project → global is allowed.
	if _, err := cA.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: "cache", Label: "reads"}); err != nil {
		t.Fatalf("link to global: %v", err)
	}

	// Another project's components are invisible to project B → 404.
	_, err = cB.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: db.Slug, Label: "uses"})
	wantAPIError(t, err, http.StatusNotFound)

	// A scope-less token writing a global edge needs global endpoints.
	_, err = cAll.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: api.Slug, To: "cache", Label: "reads"})
	wantAPIError(t, err, http.StatusNotFound)

	// Project B sees only global nodes and edges — none of project A's.
	g, err := cB.Graph(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if n.ProjectID != nil && *n.ProjectID == projA {
			t.Fatalf("project B sees project A node %q", n.Slug)
		}
	}
	for _, e := range g.Edges {
		if e.ProjectID != nil && *e.ProjectID == projA {
			t.Fatalf("project B sees project A edge %s", e.ID)
		}
	}
}

// TestEdgeTombstoneAndReconstruction covers the tombstone lifecycle:
// one tombstone only, recreation afterwards, and ?at= returning the
// past topology.
func TestEdgeTombstoneAndReconstruction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	agent := f.principal(t, "agent", "tomb-agent")
	proj := uuid.New()
	c := f.client(f.token(t, agent.ID, &proj))

	a := component(t, c, "svc a", false)
	b := component(t, c, "svc b", false)
	x := component(t, c, "svc x", false)

	first, err := c.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: a.Slug, To: b.Slug, Label: "calls"})
	if err != nil {
		t.Fatal(err)
	}

	// Tombstone records the acting principal; a second attempt is 409.
	dead, err := c.TombstoneEdge(ctx, first.ID)
	if err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if dead.TombstonedBy == nil || dead.TombstonedBy.ID != agent.ID || dead.TombstonedAt == nil {
		t.Fatalf("tombstone provenance missing: %+v", dead)
	}
	_, err = c.TombstoneEdge(ctx, first.ID)
	wantAPIError(t, err, http.StatusConflict)

	// The same edge may be recreated as a new row after tombstoning.
	if _, err := c.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: a.Slug, To: b.Slug, Label: "calls"}); err != nil {
		t.Fatalf("recreate after tombstone: %v", err)
	}

	// New topology after the tombstone.
	second, err := c.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: a.Slug, To: x.Slug, Label: "calls"})
	if err != nil {
		t.Fatal(err)
	}

	// At a time just before the tombstone, only the original edge is
	// live (timestamps come from the server, so compare against its
	// clock, not ours).
	past, err := c.Graph(ctx, dead.TombstonedAt.Add(-time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(past.Edges) != 1 || past.Edges[0].ID != first.ID {
		t.Fatalf("graph at T: edges = %+v, want just the original", past.Edges)
	}

	// Default (now): recreated a->b plus a->x, not the tombstoned row.
	now, err := c.Graph(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(now.Edges) != 2 {
		t.Fatalf("graph now: %d edges, want 2 (%+v)", len(now.Edges), now.Edges)
	}
	for _, e := range now.Edges {
		if e.ID == first.ID {
			t.Fatal("tombstoned edge still live")
		}
	}
	if now.Edges[0].To != b.Slug || now.Edges[1].To != x.Slug {
		t.Fatalf("graph now: unexpected topology %+v", now.Edges)
	}
	if len(now.Nodes) != 3 {
		t.Fatalf("graph now: %d nodes, want 3", len(now.Nodes))
	}

	// Tombstoning an edge in another project is forbidden for a
	// project token.
	other := f.client(f.token(t, agent.ID, &uuid.UUID{}))
	_, err = other.TombstoneEdge(ctx, second.ID)
	wantAPIError(t, err, http.StatusForbidden)
}

// TestEdgeImmutabilitySQL asserts the trigger blocks direct SQL
// tampering: any non-tombstone UPDATE and any DELETE fail.
func TestEdgeImmutabilitySQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	agent := f.principal(t, "agent", "sql-agent")
	proj := uuid.New()
	c := f.client(f.token(t, agent.ID, &proj))

	a := component(t, c, "imm a", false)
	b := component(t, c, "imm b", false)
	edge, err := c.CreateEdge(ctx, kbclient.CreateEdgeRequest{From: a.Slug, To: b.Slug, Label: "l"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE kbase.edges SET label = 'tampered' WHERE id = $1`, edge.ID); err == nil {
		t.Fatal("direct label UPDATE succeeded, want trigger rejection")
	}
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM kbase.edges WHERE id = $1`, edge.ID); err == nil {
		t.Fatal("direct DELETE succeeded, want trigger rejection")
	}

	// The one permitted mutation still works, exactly once.
	if _, err := c.TombstoneEdge(ctx, edge.ID); err != nil {
		t.Fatalf("legitimate tombstone: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE kbase.edges SET tombstoned_by = $2, tombstoned_at = now() WHERE id = $1`,
		edge.ID, agent.ID); err == nil {
		t.Fatal("second tombstone via SQL succeeded, want trigger rejection")
	}
}
