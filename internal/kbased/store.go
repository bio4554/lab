// Package kbased is the knowledge-base daemon: typed Postgres access
// to the kbase schema plus the bearer-token HTTP API (entries,
// versions, recall) and the labd-facing admin API (principals,
// tokens).
//
// Everything is versioned, append-only, with provenance: writes only
// ever insert entry/version rows, "current" is the highest version_no,
// and every version records the authoring principal resolved from the
// request token — never a client-supplied author.
package kbased

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/kbclient"
)

// Sentinel errors, mapped to HTTP statuses by the server layer.
var (
	// ErrNotFound: no entry (or version) visible in the caller's scope.
	ErrNotFound = errors.New("kbased: not found")
	// ErrSlugTaken: an entry with that slug already exists in the
	// target scope.
	ErrSlugTaken = errors.New("kbased: slug already exists in this scope")
	// ErrAmbiguousSlug: a scope-less caller named a slug that exists in
	// several project scopes; disambiguate with project_id.
	ErrAmbiguousSlug = errors.New("kbased: slug exists in multiple scopes; specify project_id")
)

// Store wraps a pgx pool with typed queries for the kbase schema. All
// queries are plain pgx, schema-qualified, no ORM.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by the given pool. The Store does
// not own the pool; the caller closes it.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Scope is a caller's visibility: a project-scoped token sees its
// project's entries plus global ones; a scope-less token (ProjectID
// nil) sees everything.
type Scope struct {
	ProjectID *uuid.UUID
}

// EntryTypes is the closed set of entry types (component/ticket are
// stored from Phase 9 on so the enum doesn't churn; their behavior
// arrives in Phase 10).
var EntryTypes = []string{"decision", "note", "architecture", "component", "ticket"}

// ValidType reports whether t is a known entry type.
func ValidType(t string) bool {
	for _, v := range EntryTypes {
		if t == v {
			return true
		}
	}
	return false
}

// ── Principals ───────────────────────────────────────────────────────

const principalCols = "id, kind, external_id, display_name, created_at"

// EnsurePrincipal upserts a principal on (kind, external_id). A
// non-empty displayName updates the stored one; empty leaves it alone.
func (s *Store) EnsurePrincipal(ctx context.Context, kind, externalID, displayName string) (kbclient.Principal, error) {
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO kbase.principals (kind, external_id, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (kind, external_id) DO UPDATE SET
			display_name = CASE WHEN EXCLUDED.display_name = ''
				THEN kbase.principals.display_name
				ELSE EXCLUDED.display_name END
		RETURNING `+principalCols,
		kind, externalID, displayName)
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[kbclient.Principal])
	if err != nil {
		return kbclient.Principal{}, fmt.Errorf("ensure principal: %w", err)
	}
	return p, nil
}

// GetPrincipal fetches one principal by id.
func (s *Store) GetPrincipal(ctx context.Context, id uuid.UUID) (kbclient.Principal, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+principalCols+` FROM kbase.principals WHERE id = $1`, id)
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[kbclient.Principal])
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Principal{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Principal{}, fmt.Errorf("get principal: %w", err)
	}
	return p, nil
}

// ── Entries & versions ───────────────────────────────────────────────

// NewEntry is the input to CreateEntry. ProjectID nil creates a global
// entry. Author is the principal resolved from the request token.
type NewEntry struct {
	ProjectID *uuid.UUID
	Type      string
	Slug      string
	Title     string
	Content   string
	Author    uuid.UUID
}

// EntryRef identifies one entry after slug resolution.
type EntryRef struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID
	Type      string
	Slug      string
	CreatedAt time.Time
}

// CreateEntry inserts the entry and its version 1 in one transaction.
func (s *Store) CreateEntry(ctx context.Context, e NewEntry) (kbclient.Entry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return kbclient.Entry{}, fmt.Errorf("create entry: %w", err)
	}
	defer tx.Rollback(ctx)

	var entryID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO kbase.entries (project_id, type, slug, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		e.ProjectID, e.Type, e.Slug, e.Author).Scan(&entryID)
	if isUniqueViolation(err) {
		return kbclient.Entry{}, ErrSlugTaken
	}
	if err != nil {
		return kbclient.Entry{}, fmt.Errorf("create entry: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO kbase.entry_versions (entry_id, version_no, title, content, author)
		VALUES ($1, 1, $2, $3, $4)`,
		entryID, e.Title, e.Content, e.Author)
	if err != nil {
		return kbclient.Entry{}, fmt.Errorf("create entry version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kbclient.Entry{}, fmt.Errorf("create entry: %w", err)
	}
	return s.GetEntry(ctx, entryID, nil)
}

// AppendVersion appends the next version to an entry. An empty title
// carries the previous version's title forward. Concurrent appends
// race on the (entry_id, version_no) unique index; the loser retries.
func (s *Store) AppendVersion(ctx context.Context, entryID uuid.UUID, title, content string, author uuid.UUID) (kbclient.Entry, error) {
	for attempt := 0; ; attempt++ {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO kbase.entry_versions (entry_id, version_no, title, content, author)
			SELECT $1,
			       coalesce(max(version_no), 0) + 1,
			       CASE WHEN $2 = '' THEN coalesce(
			           (SELECT title FROM kbase.entry_versions
			            WHERE entry_id = $1 ORDER BY version_no DESC LIMIT 1), '')
			           ELSE $2 END,
			       $3, $4
			FROM kbase.entry_versions WHERE entry_id = $1`,
			entryID, title, content, author)
		if isUniqueViolation(err) && attempt < 3 {
			continue
		}
		if err != nil {
			return kbclient.Entry{}, fmt.Errorf("append version: %w", err)
		}
		return s.GetEntry(ctx, entryID, nil)
	}
}

// ResolveSlug finds the entry a slug names within the caller's scope.
// A project-scoped caller prefers its project's entry over a global
// one with the same slug. A scope-less caller may pass projectID to
// disambiguate (uuid.Nil-as-pointer is not used; nil projectID means
// "any scope", and multiple matches are ErrAmbiguousSlug). explicit
// reports whether projectID was client-supplied (then it also selects
// the global scope when nil).
func (s *Store) ResolveSlug(ctx context.Context, scope Scope, slug string, projectID *uuid.UUID, explicit bool) (EntryRef, error) {
	var rows pgx.Rows
	switch {
	case scope.ProjectID != nil:
		// Project token: own project first, then global.
		rows, _ = s.pool.Query(ctx, `
			SELECT id, project_id, type, slug, created_at FROM kbase.entries
			WHERE slug = $1 AND (project_id = $2 OR project_id IS NULL)
			ORDER BY project_id NULLS LAST
			LIMIT 1`,
			slug, scope.ProjectID)
	case explicit:
		rows, _ = s.pool.Query(ctx, `
			SELECT id, project_id, type, slug, created_at FROM kbase.entries
			WHERE slug = $1 AND project_id IS NOT DISTINCT FROM $2`,
			slug, projectID)
	default:
		rows, _ = s.pool.Query(ctx, `
			SELECT id, project_id, type, slug, created_at FROM kbase.entries
			WHERE slug = $1`,
			slug)
	}
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[EntryRef])
	if err != nil {
		return EntryRef{}, fmt.Errorf("resolve slug: %w", err)
	}
	switch len(refs) {
	case 0:
		return EntryRef{}, ErrNotFound
	case 1:
		return refs[0], nil
	default:
		return EntryRef{}, ErrAmbiguousSlug
	}
}

// entrySelect joins an entry with one version and its author.
const entrySelect = `
	SELECT e.id, e.project_id, e.type, e.slug, e.created_at,
	       v.version_no, v.title, v.content, v.created_at AS updated_at,
	       p.id AS author_id, p.kind, p.external_id, p.display_name
	FROM kbase.entries e
	JOIN kbase.entry_versions v ON v.entry_id = e.id
	JOIN kbase.principals p ON p.id = v.author`

// entryRow is the scan target for entrySelect.
type entryRow struct {
	ID          uuid.UUID
	ProjectID   *uuid.UUID
	Type        string
	Slug        string
	CreatedAt   time.Time
	VersionNo   int
	Title       string
	Content     string
	UpdatedAt   time.Time
	AuthorID    uuid.UUID
	Kind        string
	ExternalID  string
	DisplayName string
}

func (r entryRow) toEntry() kbclient.Entry {
	return kbclient.Entry{
		ID:        r.ID,
		ProjectID: r.ProjectID,
		Type:      r.Type,
		Slug:      r.Slug,
		VersionNo: r.VersionNo,
		Title:     r.Title,
		Content:   r.Content,
		Author: kbclient.Author{
			ID:          r.AuthorID,
			Kind:        r.Kind,
			ExternalID:  r.ExternalID,
			DisplayName: r.DisplayName,
		},
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// GetEntry fetches one entry at a specific version, or at the current
// (highest) version when version is nil.
func (s *Store) GetEntry(ctx context.Context, entryID uuid.UUID, version *int) (kbclient.Entry, error) {
	rows, _ := s.pool.Query(ctx, entrySelect+`
		WHERE e.id = $1 AND ($2::int IS NULL OR v.version_no = $2)
		ORDER BY v.version_no DESC
		LIMIT 1`,
		entryID, version)
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[entryRow])
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Entry{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Entry{}, fmt.Errorf("get entry: %w", err)
	}
	return row.toEntry(), nil
}

// History returns an entry's full version list, newest first, without
// content.
func (s *Store) History(ctx context.Context, entryID uuid.UUID) ([]kbclient.VersionMeta, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT v.version_no, v.title,
		       p.id, p.kind, p.external_id, p.display_name,
		       v.created_at
		FROM kbase.entry_versions v
		JOIN kbase.principals p ON p.id = v.author
		WHERE v.entry_id = $1
		ORDER BY v.version_no DESC`,
		entryID)
	defer rows.Close()
	var out []kbclient.VersionMeta
	for rows.Next() {
		var m kbclient.VersionMeta
		err := rows.Scan(&m.VersionNo, &m.Title,
			&m.Author.ID, &m.Author.Kind, &m.Author.ExternalID, &m.Author.DisplayName,
			&m.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return out, nil
}

// ListEntries returns current versions in scope, newest-updated first.
// Content is cleared: listings carry metadata only. typ empty means
// all types; limit <= 0 means 100.
func (s *Store) ListEntries(ctx context.Context, scope Scope, typ string, limit int) ([]kbclient.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, _ := s.pool.Query(ctx, entrySelect+`
		WHERE v.version_no = (SELECT max(version_no) FROM kbase.entry_versions WHERE entry_id = e.id)
		  AND ($1::uuid IS NULL OR e.project_id = $1 OR e.project_id IS NULL)
		  AND ($2 = '' OR e.type = $2)
		ORDER BY v.created_at DESC
		LIMIT $3`,
		scope.ProjectID, typ, limit)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entryRow])
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	out := make([]kbclient.Entry, 0, len(list))
	for _, r := range list {
		e := r.toEntry()
		e.Content = ""
		out = append(out, e)
	}
	return out, nil
}

// Recall runs ranked full-text search over the current versions in
// scope: websearch_to_tsquery against title + content + slug (the
// version vector concatenated with the entry's slug vector), ts_rank
// ordered, with a ts_headline excerpt from the content.
func (s *Store) Recall(ctx context.Context, scope Scope, query, typ string, n int) ([]kbclient.RecallHit, error) {
	if n <= 0 {
		n = 8
	}
	rows, _ := s.pool.Query(ctx, `
		SELECT e.slug, e.type, v.title, e.project_id,
		       ts_rank(v.search || e.search, q) AS rank,
		       ts_headline('english', v.content, q,
		           'StartSel=**, StopSel=**, MaxWords=30, MinWords=10') AS excerpt
		FROM kbase.entries e
		JOIN kbase.entry_versions v ON v.entry_id = e.id,
		     websearch_to_tsquery('english', $1) q
		WHERE v.version_no = (SELECT max(version_no) FROM kbase.entry_versions WHERE entry_id = e.id)
		  AND (v.search || e.search) @@ q
		  AND ($2::uuid IS NULL OR e.project_id = $2 OR e.project_id IS NULL)
		  AND ($3 = '' OR e.type = $3)
		ORDER BY rank DESC, v.created_at DESC
		LIMIT $4`,
		query, scope.ProjectID, typ, n)
	hits, err := pgx.CollectRows(rows, pgx.RowToStructByPos[kbclient.RecallHit])
	if err != nil {
		return nil, fmt.Errorf("recall: %w", err)
	}
	return hits, nil
}

// isUniqueViolation reports whether err is a Postgres unique-index
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
