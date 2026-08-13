package kbased

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bio4554/lab/internal/kbclient"
)

// CAS ticketing. The tickets table is the single sanctioned in-place
// mutation in kbase: status/claimed_by/cas_version change under
// compare-and-swap, while everything narrative (body, comments, status
// history) is appended to the backing entry's version chain with full
// provenance.

// TicketStatuses is the closed set of ticket states.
var TicketStatuses = []string{"open", "claimed", "in_progress", "done", "abandoned"}

// validStatus reports whether st is a known ticket status.
func validStatus(st string) bool {
	for _, v := range TicketStatuses {
		if st == v {
			return true
		}
	}
	return false
}

// TicketConflictError is a failed CAS transition: stale cas_version,
// wrong state, lost claim race, or a non-claimant caller. It carries
// the ticket's current state so the caller can re-inspect and retry
// without a second GET.
type TicketConflictError struct {
	Ticket kbclient.Ticket
}

func (e *TicketConflictError) Error() string {
	return fmt.Sprintf("kbased: ticket transition conflict (status %s, cas_version %d)",
		e.Ticket.Status, e.Ticket.CASVersion)
}

// ── Store ────────────────────────────────────────────────────────────

// NewTicket is the input to CreateTicket. Author is the principal
// resolved from the request token.
type NewTicket struct {
	ProjectID *uuid.UUID
	Slug      string
	Title     string
	Body      string
	Author    uuid.UUID
}

// ticketSelect joins a ticket with its entry's slug, current title and
// the claimant principal.
const ticketSelect = `
	SELECT t.id, t.project_id, t.entry_id, e.slug, v.title, t.status,
	       cp.id, cp.kind, cp.external_id, cp.display_name,
	       t.cas_version, t.created_at, t.updated_at
	FROM kbase.tickets t
	JOIN kbase.entries e ON e.id = t.entry_id
	JOIN kbase.entry_versions v ON v.entry_id = e.id
	 AND v.version_no = (SELECT max(version_no) FROM kbase.entry_versions WHERE entry_id = e.id)
	LEFT JOIN kbase.principals cp ON cp.id = t.claimed_by`

// scanTicket scans one ticketSelect row.
func scanTicket(row pgx.Row) (kbclient.Ticket, error) {
	var (
		t         kbclient.Ticket
		cID       *uuid.UUID
		cKind     *string
		cExternal *string
		cDisplay  *string
	)
	err := row.Scan(&t.ID, &t.ProjectID, &t.EntryID, &t.Slug, &t.Title, &t.Status,
		&cID, &cKind, &cExternal, &cDisplay,
		&t.CASVersion, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return kbclient.Ticket{}, err
	}
	if cID != nil {
		t.ClaimedBy = &kbclient.Author{
			ID: *cID, Kind: *cKind, ExternalID: *cExternal, DisplayName: *cDisplay,
		}
	}
	return t, nil
}

// CreateTicket creates the backing entry (type ticket, version 1 =
// body) and the ticket row in one transaction.
func (s *Store) CreateTicket(ctx context.Context, nt NewTicket) (kbclient.Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("create ticket: %w", err)
	}
	defer tx.Rollback(ctx)

	var entryID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO kbase.entries (project_id, type, slug, created_by)
		VALUES ($1, 'ticket', $2, $3)
		RETURNING id`,
		nt.ProjectID, nt.Slug, nt.Author).Scan(&entryID)
	if isUniqueViolation(err) {
		return kbclient.Ticket{}, ErrSlugTaken
	}
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("create ticket entry: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO kbase.entry_versions (entry_id, version_no, title, content, author)
		VALUES ($1, 1, $2, $3, $4)`,
		entryID, nt.Title, nt.Body, nt.Author)
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("create ticket version: %w", err)
	}
	var ticketID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO kbase.tickets (project_id, entry_id)
		VALUES ($1, $2)
		RETURNING id`,
		nt.ProjectID, entryID).Scan(&ticketID)
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("create ticket row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kbclient.Ticket{}, fmt.Errorf("create ticket: %w", err)
	}
	return s.GetTicket(ctx, ticketID)
}

// GetTicket fetches one ticket by ticket id.
func (s *Store) GetTicket(ctx context.Context, id uuid.UUID) (kbclient.Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, ticketSelect+` WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Ticket{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("get ticket: %w", err)
	}
	return t, nil
}

// GetTicketByEntry fetches one ticket by its backing entry id.
func (s *Store) GetTicketByEntry(ctx context.Context, entryID uuid.UUID) (kbclient.Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, ticketSelect+` WHERE t.entry_id = $1`, entryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Ticket{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("get ticket: %w", err)
	}
	return t, nil
}

// ListTickets returns tickets in scope, newest first. status empty
// means all; limit <= 0 means 100.
func (s *Store) ListTickets(ctx context.Context, scope Scope, status string, limit int) ([]kbclient.Ticket, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, _ := s.pool.Query(ctx, ticketSelect+`
		WHERE ($1::uuid IS NULL OR t.project_id = $1 OR t.project_id IS NULL)
		  AND ($2 = '' OR t.status = $2)
		ORDER BY t.created_at DESC
		LIMIT $3`,
		scope.ProjectID, status, limit)
	defer rows.Close()
	out := []kbclient.Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("list tickets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	return out, nil
}

// ticketActor is the acting principal of a transition or comment: the
// id for claimed_by/authorship, the label for activity lines.
type ticketActor struct {
	ID    uuid.UUID
	Label string // e.g. "agent:impl1"
}

// activityLine is the stable trailer appended to the ticket body on
// every transition: agents read these, keep the shape boring.
func activityLine(verb, actor string, at time.Time) string {
	return fmt.Sprintf("\n\n---\n_%s by %s — %s_", verb, actor, at.UTC().Format(time.RFC3339))
}

// appendTicketVersion appends the next version of the ticket's entry
// inside tx: previous title, previous content + trailer, authored by
// the actor.
func appendTicketVersion(ctx context.Context, tx pgx.Tx, entryID uuid.UUID, trailer string, author uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO kbase.entry_versions (entry_id, version_no, title, content, author)
		SELECT $1, max(version_no) + 1,
		       (SELECT title FROM kbase.entry_versions
		        WHERE entry_id = $1 ORDER BY version_no DESC LIMIT 1),
		       (SELECT content FROM kbase.entry_versions
		        WHERE entry_id = $1 ORDER BY version_no DESC LIMIT 1) || $2,
		       $3
		FROM kbase.entry_versions WHERE entry_id = $1`,
		entryID, trailer, author)
	if err != nil {
		return fmt.Errorf("append ticket version: %w", err)
	}
	return nil
}

// TransitionTicket runs one CAS transition (claim, start, done,
// abandon) and appends the matching activity version to the ticket's
// entry, atomically. A zero-row CAS update — stale cas_version, wrong
// state, lost race, or not the claimant — rolls back and returns
// *TicketConflictError with the current state.
func (s *Store) TransitionTicket(ctx context.Context, id uuid.UUID, action string, expected int, actor ticketActor) (kbclient.Ticket, error) {
	// Each statement is the atomic claim shape from DESIGN.md: the
	// WHERE clause carries the expected cas_version plus the states
	// (and claimant, where required) the transition is valid from.
	var set, where, verb string
	switch action {
	case "claim":
		set, verb = `claimed_by = $3, status = 'claimed'`, "claimed"
		where = `claimed_by IS NULL` // open or abandoned
	case "start":
		set, verb = `status = 'in_progress'`, "started"
		where = `status = 'claimed' AND claimed_by = $3`
	case "done":
		set, verb = `status = 'done'`, "completed"
		where = `status IN ('claimed', 'in_progress') AND claimed_by = $3`
	case "abandon":
		set, verb = `status = 'abandoned', claimed_by = NULL`, "abandoned"
		where = `status IN ('claimed', 'in_progress') AND claimed_by = $3`
	default:
		return kbclient.Ticket{}, fmt.Errorf("unknown ticket action %q", action)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("transition ticket: %w", err)
	}
	defer tx.Rollback(ctx)

	var entryID uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE kbase.tickets
		SET `+set+`, cas_version = cas_version + 1, updated_at = now()
		WHERE id = $1 AND cas_version = $2 AND `+where+`
		RETURNING entry_id`,
		id, expected, actor.ID).Scan(&entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost the CAS. Report the current state (or 404 if the id is
		// simply unknown).
		current, gerr := s.GetTicket(ctx, id)
		if gerr != nil {
			return kbclient.Ticket{}, gerr
		}
		return kbclient.Ticket{}, &TicketConflictError{Ticket: current}
	}
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("transition ticket: %w", err)
	}
	if err := appendTicketVersion(ctx, tx, entryID,
		activityLine(verb, actor.Label, time.Now()), actor.ID); err != nil {
		return kbclient.Ticket{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return kbclient.Ticket{}, fmt.Errorf("transition ticket: %w", err)
	}
	return s.GetTicket(ctx, id)
}

// CommentTicket appends a comment version to the ticket's entry and
// bumps updated_at. Comments don't touch status and need no CAS.
func (s *Store) CommentTicket(ctx context.Context, id uuid.UUID, text string, actor ticketActor) (kbclient.Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("comment ticket: %w", err)
	}
	defer tx.Rollback(ctx)

	var entryID uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE kbase.tickets SET updated_at = now()
		WHERE id = $1
		RETURNING entry_id`,
		id).Scan(&entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return kbclient.Ticket{}, ErrNotFound
	}
	if err != nil {
		return kbclient.Ticket{}, fmt.Errorf("comment ticket: %w", err)
	}
	trailer := activityLine("comment", actor.Label, time.Now()) + "\n" + text
	if err := appendTicketVersion(ctx, tx, entryID, trailer, actor.ID); err != nil {
		return kbclient.Ticket{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return kbclient.Ticket{}, fmt.Errorf("comment ticket: %w", err)
	}
	return s.GetTicket(ctx, id)
}

// ── HTTP handlers ────────────────────────────────────────────────────

// createTicket handles POST /v1/tickets.
func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req kbclient.CreateTicketRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Title == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("title is required"))
		return
	}
	if req.Body == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("body is required"))
		return
	}
	projectID, err := writeScope(caller, false, req.ProjectID)
	if err != nil {
		s.writeError(w, http.StatusForbidden, err)
		return
	}
	slug := req.Slug
	if slug == "" {
		slug = Slugify(req.Title)
	}
	if !validSlug(slug) {
		s.writeError(w, http.StatusBadRequest,
			fmt.Errorf("invalid slug %q (want lowercase words separated by dashes)", slug))
		return
	}
	ticket, err := s.Store.CreateTicket(ctx, NewTicket{
		ProjectID: projectID,
		Slug:      slug,
		Title:     req.Title,
		Body:      req.Body,
		Author:    caller.Principal.ID,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, ticket)
}

// listTickets handles GET /v1/tickets?status=&limit=.
func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	q := r.URL.Query()

	status := q.Get("status")
	if status != "" && !validStatus(status) {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("unknown status %q", status))
		return
	}
	limit := 0
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid limit %q", l))
			return
		}
		limit = n
	}
	tickets, err := s.Store.ListTickets(ctx, caller.Scope(), status, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, tickets)
}

// getTicket handles GET /v1/tickets/{ref}: the ticket row plus the
// backing entry at its current version.
func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	ticket, err := s.resolveTicket(ctx, caller, r.PathValue("ref"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	entry, err := s.Store.GetEntry(ctx, ticket.EntryID, nil)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	ticket.Entry = &entry
	s.writeJSON(w, http.StatusOK, ticket)
}

// resolveTicket finds a ticket by ticket id or entry slug, enforcing
// the caller's read scope. Slugs go through the shared resolveSlug
// rules, so a project token prefers its own project's ticket.
func (s *Server) resolveTicket(ctx context.Context, caller Caller, ref string) (kbclient.Ticket, error) {
	if id, err := uuid.Parse(ref); err == nil {
		ticket, err := s.Store.GetTicket(ctx, id)
		if err != nil {
			return kbclient.Ticket{}, err
		}
		if caller.ProjectID != nil && ticket.ProjectID != nil &&
			*ticket.ProjectID != *caller.ProjectID {
			return kbclient.Ticket{}, ErrNotFound // invisible outside its project
		}
		return ticket, nil
	}
	entryRef, err := s.resolveSlug(ctx, caller, ref, nil)
	if err != nil {
		return kbclient.Ticket{}, err
	}
	return s.Store.GetTicketByEntry(ctx, entryRef.ID)
}

// requireTicketWrite enforces the write half of the scope rules for
// ticket mutations: project tokens mutate only their own project's
// tickets (global tickets need a scope-less token).
func requireTicketWrite(caller Caller, ticket kbclient.Ticket) error {
	if caller.ProjectID != nil &&
		(ticket.ProjectID == nil || *ticket.ProjectID != *caller.ProjectID) {
		return errForbiddenProject
	}
	return nil
}

// actorFor builds the acting principal's activity-line identity.
func actorFor(caller Caller) ticketActor {
	name := caller.Principal.DisplayName
	if name == "" {
		name = caller.Principal.ExternalID
	}
	return ticketActor{
		ID:    caller.Principal.ID,
		Label: caller.Principal.Kind + ":" + name,
	}
}

// transitionTicket handles POST /v1/tickets/{ref}/{claim,start,done,
// abandon}: one CAS attempt; conflicts are 409 with the current state.
func (s *Server) transitionTicket(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caller := callerFrom(ctx)
		var req kbclient.TicketTransitionRequest
		if err := decodeBody(r, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, err)
			return
		}
		ticket, err := s.resolveTicket(ctx, caller, r.PathValue("ref"))
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		if err := requireTicketWrite(caller, ticket); err != nil {
			s.writeError(w, http.StatusForbidden, err)
			return
		}
		updated, err := s.Store.TransitionTicket(ctx, ticket.ID, action, req.CASVersion, actorFor(caller))
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, updated)
	}
}

// commentTicket handles POST /v1/tickets/{ref}/comment.
func (s *Server) commentTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req kbclient.CommentTicketRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Text == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}
	ticket, err := s.resolveTicket(ctx, caller, r.PathValue("ref"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := requireTicketWrite(caller, ticket); err != nil {
		s.writeError(w, http.StatusForbidden, err)
		return
	}
	updated, err := s.Store.CommentTicket(ctx, ticket.ID, req.Text, actorFor(caller))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}
