package kbased

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/kbclient"
)

// These tests run the full stack — kbclient → httptest kbased →
// live Postgres — so they double as the kbclient integration tests.

// wantAPIError asserts err is an APIError with the given status.
func wantAPIError(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *kbclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError %d, got %v", status, err)
	}
	if apiErr.Status != status {
		t.Fatalf("want HTTP %d, got %d (%s)", status, apiErr.Status, apiErr.Message)
	}
}

func TestImmutability(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	agent := f.principal(t, "agent", "impl-1")
	project := uuid.New()
	secret := f.token(t, agent.ID, &project)
	c := f.client(secret)

	created, err := c.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "decision", Title: "Use Postgres", Content: "v1 body: single instance.",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Slug != "use-postgres" || created.VersionNo != 1 {
		t.Fatalf("created = %q v%d, want use-postgres v1", created.Slug, created.VersionNo)
	}

	updated, err := c.AppendVersion(ctx, "use-postgres", kbclient.AppendVersionRequest{
		Content: "v2 body: single instance, two schemas.",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.VersionNo != 2 {
		t.Fatalf("update -> v%d, want v2", updated.VersionNo)
	}
	if updated.Title != "Use Postgres" {
		t.Fatalf("empty title should carry forward, got %q", updated.Title)
	}

	// The old version is still readable verbatim.
	v1, err := c.GetEntry(ctx, "use-postgres", kbclient.GetEntryOptions{Version: 1})
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.Content != "v1 body: single instance." {
		t.Fatalf("v1 content changed: %q", v1.Content)
	}

	// No HTTP path mutates a version: only GET and the append route
	// exist, so mutation-shaped requests are 4xx even with a valid
	// token.
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method, f.srv.URL+"/v1/entries/use-postgres", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode > 499 {
			t.Fatalf("%s on an entry: got %d, want 4xx", method, resp.StatusCode)
		}
	}

	// Even direct SQL cannot mutate a version: the append-only trigger
	// rejects UPDATE and DELETE.
	_, err = f.pool.Exec(ctx,
		"UPDATE kbase.entry_versions SET content = 'tampered' WHERE entry_id = $1", created.ID)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("direct UPDATE: want append-only error, got %v", err)
	}
	_, err = f.pool.Exec(ctx,
		"DELETE FROM kbase.entry_versions WHERE entry_id = $1", created.ID)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("direct DELETE: want append-only error, got %v", err)
	}
}

func TestProvenance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	project := uuid.New()

	agent := f.principal(t, "agent", "impl-1")
	human := f.principal(t, "human", "charles")
	agentClient := f.client(f.token(t, agent.ID, &project))
	humanClient := f.client(f.token(t, human.ID, nil))

	created, err := agentClient.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "Retry budget", Content: "agents wrote this",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The author comes from the token, never from the client.
	if created.Author.ID != agent.ID || created.Author.Kind != "agent" {
		t.Fatalf("v1 author = %+v, want agent %s", created.Author, agent.ID)
	}

	if _, err := humanClient.AppendVersion(ctx, "retry-budget", kbclient.AppendVersionRequest{
		Content: "a human amended this",
	}); err != nil {
		t.Fatalf("human update: %v", err)
	}

	entry, err := humanClient.GetEntry(ctx, "retry-budget", kbclient.GetEntryOptions{History: true})
	if err != nil {
		t.Fatalf("get with history: %v", err)
	}
	if len(entry.History) != 2 {
		t.Fatalf("history has %d versions, want 2", len(entry.History))
	}
	// Newest first: v2 by the human, v1 by the agent.
	if entry.History[0].Author.ID != human.ID {
		t.Errorf("v2 author = %+v, want human %s", entry.History[0].Author, human.ID)
	}
	if entry.History[1].Author.ID != agent.ID {
		t.Errorf("v1 author = %+v, want agent %s", entry.History[1].Author, agent.ID)
	}
}

func TestScoping(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	projectA, projectB := uuid.New(), uuid.New()

	agentA := f.principal(t, "agent", "a-1")
	agentB := f.principal(t, "agent", "b-1")
	human := f.principal(t, "human", "admin")
	clientA := f.client(f.token(t, agentA.ID, &projectA))
	clientB := f.client(f.token(t, agentB.ID, &projectB))
	humanClient := f.client(f.token(t, human.ID, nil))

	// Seed: one entry per project, one global (scope-less token only).
	if _, err := clientA.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "A secret plan", Content: "belongs to project A",
	}); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if _, err := clientB.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "B roadmap", Content: "belongs to project B",
	}); err != nil {
		t.Fatalf("create in B: %v", err)
	}
	if _, err := humanClient.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "architecture", Title: "Org conventions", Content: "applies everywhere", Global: true,
	}); err != nil {
		t.Fatalf("create global: %v", err)
	}

	// A project token cannot read another project's entries...
	_, err := clientA.GetEntry(ctx, "b-roadmap", kbclient.GetEntryOptions{})
	wantAPIError(t, err, http.StatusNotFound)
	// ...they are absent from its listings...
	entries, err := clientA.ListEntries(ctx, "", 0)
	if err != nil {
		t.Fatalf("list as A: %v", err)
	}
	for _, e := range entries {
		if e.Slug == "b-roadmap" {
			t.Fatal("project A listing leaked project B's entry")
		}
	}
	// ...but global entries are readable.
	if _, err := clientA.GetEntry(ctx, "org-conventions", kbclient.GetEntryOptions{}); err != nil {
		t.Fatalf("A reading global: %v", err)
	}

	// A project token cannot write global...
	_, err = clientA.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "Sneaky global", Content: "nope", Global: true,
	})
	wantAPIError(t, err, http.StatusForbidden)
	// ...not even by appending to an existing global entry...
	_, err = clientA.AppendVersion(ctx, "org-conventions", kbclient.AppendVersionRequest{Content: "nope"})
	wantAPIError(t, err, http.StatusForbidden)
	// ...nor name another project.
	_, err = clientA.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "Cross write", Content: "nope", ProjectID: &projectB,
	})
	wantAPIError(t, err, http.StatusForbidden)

	// The scope-less token sees and writes everything.
	if _, err := humanClient.GetEntry(ctx, "b-roadmap", kbclient.GetEntryOptions{}); err != nil {
		t.Fatalf("scope-less reading B: %v", err)
	}
	if _, err := humanClient.AppendVersion(ctx, "org-conventions", kbclient.AppendVersionRequest{
		Content: "updated conventions",
	}); err != nil {
		t.Fatalf("scope-less updating global: %v", err)
	}
	if _, err := humanClient.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Title: "Planted in A", Content: "human writing into A", ProjectID: &projectA,
	}); err != nil {
		t.Fatalf("scope-less writing into A: %v", err)
	}

	// Same slug in two scopes: the project token prefers its own.
	if _, err := clientA.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "note", Slug: "org-conventions", Title: "A's conventions", Content: "project A override",
	}); err != nil {
		t.Fatalf("create shadowing slug: %v", err)
	}
	got, err := clientA.GetEntry(ctx, "org-conventions", kbclient.GetEntryOptions{})
	if err != nil {
		t.Fatalf("get shadowed slug: %v", err)
	}
	if got.ProjectID == nil || *got.ProjectID != projectA {
		t.Fatalf("project token resolved %q to scope %v, want its own project", got.Slug, got.ProjectID)
	}
	// The scope-less token must disambiguate...
	_, err = humanClient.GetEntry(ctx, "org-conventions", kbclient.GetEntryOptions{})
	wantAPIError(t, err, http.StatusConflict)
	// ...and can, with an explicit project.
	got, err = humanClient.GetEntry(ctx, "org-conventions", kbclient.GetEntryOptions{ProjectID: &projectA})
	if err != nil {
		t.Fatalf("get with explicit project: %v", err)
	}
	if got.Title != "A's conventions" {
		t.Fatalf("explicit project resolved to %q", got.Title)
	}
}

func TestRecall(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	project := uuid.New()
	agent := f.principal(t, "agent", "impl-1")
	c := f.client(f.token(t, agent.ID, &project))

	seed := []struct{ typ, title, content string }{
		{"decision", "Use Postgres for persistence", "Single Postgres instance, two schemas: lab and kbase. No ORM, plain pgx."},
		{"decision", "Bearer tokens for agent auth", "Per-agent bearer tokens minted by labd at container create, hashed at rest."},
		{"architecture", "Worktree per agent", "Each agent gets its own git worktree on its own branch, bind-mounted at /work."},
		{"architecture", "Stream-json pipe", "labd speaks stream-json to the claude process over docker attach."},
		{"note", "Connection pooling", "pgxpool with default settings has been fine; revisit under load."},
		{"note", "Rate limits", "Anthropic rate limits pause the pump; resume is driven by retry-after."},
		{"note", "Full-text search tuning", "Postgres websearch_to_tsquery with ts_rank over title and content."},
		{"component", "kbased", "The knowledge base daemon. Owns the kbase schema in Postgres."},
		{"component", "labd", "The lab daemon: drivers, agent API, client API."},
		{"ticket", "Flaky merge test", "gitrepo merge test occasionally fails on macOS runners."},
		{"note", "Docker on macOS", "host.docker.internal reaches services bound to localhost on Docker Desktop."},
		{"decision", "Goose for migrations", "Two independent goose streams with schema-qualified version tables."},
	}
	for _, s := range seed {
		var err error
		if s.typ == "ticket" {
			// Ticket entries only exist via the ticket API (Phase 10);
			// recall still sees them like any other entry.
			_, err = c.CreateTicket(ctx, kbclient.CreateTicketRequest{
				Title: s.title, Body: s.content,
			})
		} else {
			_, err = c.CreateEntry(ctx, kbclient.CreateEntryRequest{
				Type: s.typ, Title: s.title, Content: s.content,
			})
		}
		if err != nil {
			t.Fatalf("seed %q: %v", s.title, err)
		}
	}

	// Keyword query: the Postgres decision should surface, ranked
	// above entries that merely mention Postgres in passing.
	hits, err := c.Recall(ctx, "postgres persistence", "", 8)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("recall(postgres persistence): no hits")
	}
	if hits[0].Slug != "use-postgres-for-persistence" {
		t.Errorf("top hit = %q, want use-postgres-for-persistence (all: %v)", hits[0].Slug, slugs(hits))
	}
	if hits[0].Excerpt == "" {
		t.Error("top hit has no excerpt")
	}

	// Phrase query.
	hits, err = c.Recall(ctx, `"stream-json"`, "", 8)
	if err != nil {
		t.Fatalf("recall phrase: %v", err)
	}
	if !containsSlug(hits, "stream-json-pipe") {
		t.Errorf("recall(\"stream-json\") = %v, want stream-json-pipe", slugs(hits))
	}

	// Slug words match even when the body says something else.
	hits, err = c.Recall(ctx, "kbased", "", 8)
	if err != nil {
		t.Fatalf("recall slug word: %v", err)
	}
	if !containsSlug(hits, "kbased") {
		t.Errorf("recall(kbased) = %v, want the kbased component", slugs(hits))
	}

	// Type filter.
	hits, err = c.Recall(ctx, "postgres", "decision", 8)
	if err != nil {
		t.Fatalf("recall typed: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("recall(postgres, decision): no hits")
	}
	for _, h := range hits {
		if h.Type != "decision" {
			t.Errorf("type filter leaked %q (%s)", h.Slug, h.Type)
		}
	}

	// n caps the result count.
	hits, err = c.Recall(ctx, "postgres", "", 2)
	if err != nil {
		t.Fatalf("recall capped: %v", err)
	}
	if len(hits) > 2 {
		t.Errorf("n=2 returned %d hits", len(hits))
	}

	// Recall searches current versions only: after an update the old
	// content stops matching and the new content matches.
	if _, err := c.AppendVersion(ctx, "connection-pooling", kbclient.AppendVersionRequest{
		Content: "Switched to explicit pool sizing: max 10 conns per daemon.",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	hits, err = c.Recall(ctx, "revisit under load", "", 8)
	if err != nil {
		t.Fatalf("recall old content: %v", err)
	}
	if containsSlug(hits, "connection-pooling") {
		t.Error("recall matched a superseded version's content")
	}
	hits, err = c.Recall(ctx, "explicit pool sizing", "", 8)
	if err != nil {
		t.Fatalf("recall new content: %v", err)
	}
	if !containsSlug(hits, "connection-pooling") {
		t.Errorf("recall(new content) = %v, want connection-pooling", slugs(hits))
	}
}

func slugs(hits []kbclient.RecallHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = fmt.Sprintf("%s(%.3f)", h.Slug, h.Rank)
	}
	return out
}

func containsSlug(hits []kbclient.RecallHit, slug string) bool {
	for _, h := range hits {
		if h.Slug == slug {
			return true
		}
	}
	return false
}

func TestTokenAuth(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	project := uuid.New()
	agent := f.principal(t, "agent", "impl-1")

	// Missing and unknown tokens are a plain 401.
	_, err := f.client("").ListEntries(ctx, "", 0)
	wantAPIError(t, err, http.StatusUnauthorized)
	_, err = f.client("not-a-token").ListEntries(ctx, "", 0)
	wantAPIError(t, err, http.StatusUnauthorized)

	// A live token works; after revocation it is 401 again,
	// indistinguishable from unknown.
	secret := f.token(t, agent.ID, &project)
	if _, err := f.client(secret).ListEntries(ctx, "", 0); err != nil {
		t.Fatalf("live token: %v", err)
	}
	if _, err := f.admin.RevokeTokens(ctx, kbclient.RevokeTokensRequest{
		PrincipalID: agent.ID, ProjectID: &project,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = f.client(secret).ListEntries(ctx, "", 0)
	wantAPIError(t, err, http.StatusUnauthorized)

	// Minting with revoke_existing rotates: old dies, new works.
	first := f.token(t, agent.ID, &project)
	resp, err := f.admin.MintToken(ctx, kbclient.MintTokenRequest{
		PrincipalID: agent.ID, ProjectID: &project, RevokeExisting: true,
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_, err = f.client(first).ListEntries(ctx, "", 0)
	wantAPIError(t, err, http.StatusUnauthorized)
	if _, err := f.client(resp.Token).ListEntries(ctx, "", 0); err != nil {
		t.Fatalf("rotated token: %v", err)
	}

	// Agent tokens cannot reach the admin API.
	agentAdmin := f.client(resp.Token)
	_, err = agentAdmin.RegisterPrincipal(ctx, kbclient.RegisterPrincipalRequest{
		Kind: "agent", ExternalID: "sneaky",
	})
	wantAPIError(t, err, http.StatusUnauthorized)

	// No plaintext secret at rest: only 32-byte SHA-256 hashes, never
	// the hex secret itself.
	var n int
	err = f.pool.QueryRow(ctx, `
		SELECT count(*) FROM kbase.tokens
		WHERE principal_id = $1
		  AND (octet_length(secret_hash) != 32 OR encode(secret_hash, 'hex') = $2)`,
		agent.ID, resp.Token).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("found a token row that is not a 32-byte hash")
	}
}

func TestAdminDisabled(t *testing.T) {
	f := newFixture(t)
	// A server with no admin token refuses admin calls outright.
	disabled := &Server{Store: f.store}
	srv := httptest.NewServer(disabled.Handler())
	t.Cleanup(srv.Close)
	admin := kbclient.New(srv.URL, "anything")
	_, err := admin.RegisterPrincipal(context.Background(), kbclient.RegisterPrincipalRequest{
		Kind: "human", ExternalID: "someone",
	})
	wantAPIError(t, err, http.StatusForbidden)
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Use Postgres":              "use-postgres",
		"  Weird -- punctuation!! ": "weird-punctuation",
		"CamelCase Title 2":         "camelcase-title-2",
		"---":                       "",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
