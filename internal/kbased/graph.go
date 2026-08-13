package kbased

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bio4554/lab/internal/kbclient"
)

// Architecture graph: directed labeled edges between component
// entries. Edges are append-only with tombstones (a schema property,
// see migration 00003), so the graph at any past time is
// reconstructible.

// Graph sentinels, mapped to HTTP statuses by writeStoreError.
var (
	// ErrEdgeExists: a live edge with the same from/to/label already
	// exists in the target scope.
	ErrEdgeExists = errors.New("kbased: an identical live edge already exists")
	// ErrEdgeTombstoned: the edge is already tombstoned.
	ErrEdgeTombstoned = errors.New("kbased: edge is already tombstoned")
	// ErrNotComponent: an edge endpoint is not a component entry.
	ErrNotComponent = errors.New("kbased: edge endpoints must be component entries")
)

// ── Store ────────────────────────────────────────────────────────────

// NewEdge is the input to CreateEdge; endpoints are already resolved
// entry ids. CreatedBy is the principal resolved from the request
// token.
type NewEdge struct {
	ProjectID *uuid.UUID
	From      uuid.UUID
	To        uuid.UUID
	Label     string
	CreatedBy uuid.UUID
}

// edgeSelect joins an edge with its endpoint slugs and principals.
const edgeSelect = `
	SELECT e.id, e.project_id, f.slug, t.slug, e.label,
	       cp.id, cp.kind, cp.external_id, cp.display_name,
	       e.created_at,
	       tp.id, tp.kind, tp.external_id, tp.display_name,
	       e.tombstoned_at
	FROM kbase.edges e
	JOIN kbase.entries f ON f.id = e.from_entry
	JOIN kbase.entries t ON t.id = e.to_entry
	JOIN kbase.principals cp ON cp.id = e.created_by
	LEFT JOIN kbase.principals tp ON tp.id = e.tombstoned_by`

// scanEdge scans one edgeSelect row.
func scanEdge(row pgx.Row) (kbclient.Edge, error) {
	var (
		e          kbclient.Edge
		tID        *uuid.UUID
		tKind      *string
		tExternal  *string
		tDisplay   *string
		tombstoned *time.Time
	)
	err := row.Scan(&e.ID, &e.ProjectID, &e.From, &e.To, &e.Label,
		&e.CreatedBy.ID, &e.CreatedBy.Kind, &e.CreatedBy.ExternalID, &e.CreatedBy.DisplayName,
		&e.CreatedAt,
		&tID, &tKind, &tExternal, &tDisplay,
		&tombstoned)
	if err != nil {
		return kbclient.Edge{}, err
	}
	if tID != nil {
		e.TombstonedBy = &kbclient.Author{
			ID: *tID, Kind: *tKind, ExternalID: *tExternal, DisplayName: *tDisplay,
		}
		e.TombstonedAt = tombstoned
	}
	return e, nil
}

// CreateEdge inserts a live edge. A live duplicate (same scope, from,
// to, label) is ErrEdgeExists; a tombstoned duplicate does not
// conflict.
func (s *Store) CreateEdge(ctx context.Context, e NewEdge) (kbclient.Edge, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO kbase.edges (project_id, from_entry, to_entry, label, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		e.ProjectID, e.From, e.To, e.Label, e.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		return kbclient.Edge{}, ErrEdgeExists
	}
	if err != nil {
		return kbclient.Edge{}, fmt.Errorf("create edge: %w", err)
	}
	return s.GetEdge(ctx, id)
}

// GetEdge fetches one edge by id.
func (s *Store) GetEdge(ctx context.Context, id uuid.UUID) (kbclient.Edge, error) {
	edge, err := scanEdge(s.pool.QueryRow(ctx, edgeSelect+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Edge{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Edge{}, fmt.Errorf("get edge: %w", err)
	}
	return edge, nil
}

// TombstoneEdge performs the one permitted edge mutation: setting the
// tombstone, exactly once. A second attempt is ErrEdgeTombstoned.
func (s *Store) TombstoneEdge(ctx context.Context, id, principal uuid.UUID) (kbclient.Edge, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kbase.edges SET tombstoned_by = $2, tombstoned_at = now()
		WHERE id = $1 AND tombstoned_at IS NULL`,
		id, principal)
	if err != nil {
		return kbclient.Edge{}, fmt.Errorf("tombstone edge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Zero rows: either gone (404) or already tombstoned (409).
		if _, err := s.GetEdge(ctx, id); err != nil {
			return kbclient.Edge{}, err
		}
		return kbclient.Edge{}, ErrEdgeTombstoned
	}
	return s.GetEdge(ctx, id)
}

// GraphAt returns the component nodes visible in scope that existed at
// the given time, plus the edges live at that time.
func (s *Store) GraphAt(ctx context.Context, scope Scope, at time.Time) (kbclient.Graph, error) {
	g := kbclient.Graph{At: at, Nodes: []kbclient.GraphNode{}, Edges: []kbclient.Edge{}}

	rows, _ := s.pool.Query(ctx, `
		SELECT e.slug, v.title, e.project_id
		FROM kbase.entries e
		JOIN kbase.entry_versions v ON v.entry_id = e.id
		WHERE e.type = 'component'
		  AND v.version_no = (SELECT max(version_no) FROM kbase.entry_versions WHERE entry_id = e.id)
		  AND e.created_at <= $2
		  AND ($1::uuid IS NULL OR e.project_id = $1 OR e.project_id IS NULL)
		ORDER BY e.slug`,
		scope.ProjectID, at)
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[kbclient.GraphNode])
	if err != nil {
		return kbclient.Graph{}, fmt.Errorf("graph nodes: %w", err)
	}
	g.Nodes = append(g.Nodes, nodes...)

	rows, _ = s.pool.Query(ctx, edgeSelect+`
		WHERE e.created_at <= $2
		  AND (e.tombstoned_at IS NULL OR e.tombstoned_at > $2)
		  AND ($1::uuid IS NULL OR e.project_id = $1 OR e.project_id IS NULL)
		ORDER BY f.slug, t.slug, e.label`,
		scope.ProjectID, at)
	defer rows.Close()
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return kbclient.Graph{}, fmt.Errorf("graph edges: %w", err)
		}
		g.Edges = append(g.Edges, edge)
	}
	if err := rows.Err(); err != nil {
		return kbclient.Graph{}, fmt.Errorf("graph edges: %w", err)
	}
	return g, nil
}

// ── HTTP handlers ────────────────────────────────────────────────────

// createEdge handles POST /v1/edges. The edge lives in the writer's
// scope; both endpoints must be component entries readable in that
// scope (own project or global).
func (s *Server) createEdge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req kbclient.CreateEdgeRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.From == "" || req.To == "" || req.Label == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("from, to and label are required"))
		return
	}
	projectID, err := writeScope(caller, false, req.ProjectID)
	if err != nil {
		s.writeError(w, http.StatusForbidden, err)
		return
	}
	from, err := s.resolveComponent(ctx, projectID, req.From)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	to, err := s.resolveComponent(ctx, projectID, req.To)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	edge, err := s.Store.CreateEdge(ctx, NewEdge{
		ProjectID: projectID,
		From:      from.ID,
		To:        to.ID,
		Label:     req.Label,
		CreatedBy: caller.Principal.ID,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, edge)
}

// resolveComponent resolves a slug within an edge's scope (the
// project, falling back to global; global-only when projectID is nil)
// and requires the entry to be a component.
func (s *Server) resolveComponent(ctx context.Context, projectID *uuid.UUID, slug string) (EntryRef, error) {
	var (
		ref EntryRef
		err error
	)
	if projectID != nil {
		ref, err = s.Store.ResolveSlug(ctx, Scope{ProjectID: projectID}, slug, nil, false)
	} else {
		ref, err = s.Store.ResolveSlug(ctx, Scope{}, slug, nil, true)
	}
	if errors.Is(err, ErrNotFound) {
		return EntryRef{}, fmt.Errorf("%w: no component %q in scope", ErrNotFound, slug)
	}
	if err != nil {
		return EntryRef{}, err
	}
	if ref.Type != "component" {
		return EntryRef{}, fmt.Errorf("%w (%q is a %s)", ErrNotComponent, slug, ref.Type)
	}
	return ref, nil
}

// tombstoneEdge handles DELETE /v1/edges/{id}: the append-only removal.
// Project tokens may only tombstone edges in their own project.
func (s *Server) tombstoneEdge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid edge id %q", r.PathValue("id")))
		return
	}
	edge, err := s.Store.GetEdge(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if caller.ProjectID != nil &&
		(edge.ProjectID == nil || *edge.ProjectID != *caller.ProjectID) {
		s.writeError(w, http.StatusForbidden,
			errors.New("project-scoped token can only tombstone its own project's edges"))
		return
	}
	edge, err = s.Store.TombstoneEdge(ctx, id, caller.Principal.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, edge)
}

// graph handles GET /v1/graph?at=<RFC3339>: the caller-visible
// component nodes plus the edges live at the given time (default now).
func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	at := time.Now()
	if v := r.URL.Query().Get("at"); v != "" {
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid at %q (want RFC3339)", v))
			return
		}
		at = parsed
	}
	g, err := s.Store.GraphAt(ctx, caller.Scope(), at)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}
