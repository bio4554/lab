package kbased

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/kbclient"
)

// Server is kbased's HTTP layer: the bearer-token entry/recall API
// plus the admin API (see admin.go). It speaks the JSON types in
// internal/kbclient.
type Server struct {
	Store *Store
	// AdminToken guards the /admin/v1 routes. Empty disables them
	// (labd then runs without kbase wiring). It comes from config —
	// never from the database — and is compared constant-time.
	AdminToken string
	Log        *slog.Logger
}

// callerKey carries the authenticated caller through the request
// context.
type callerKey struct{}

func callerFrom(ctx context.Context) Caller {
	return ctx.Value(callerKey{}).(Caller)
}

// Handler returns all routes: /v1/* behind token auth, /admin/v1/*
// behind the admin token.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	api := http.NewServeMux()
	api.HandleFunc("POST /v1/entries", s.createEntry)
	api.HandleFunc("GET /v1/entries", s.listEntries)
	api.HandleFunc("GET /v1/entries/{slug}", s.getEntry)
	api.HandleFunc("POST /v1/entries/{slug}/versions", s.appendVersion)
	api.HandleFunc("GET /v1/recall", s.recall)

	mux := http.NewServeMux()
	mux.Handle("/v1/", s.authenticate(api))
	mux.Handle("/admin/v1/", s.adminOnly(s.adminMux()))
	return mux
}

// authenticate resolves the bearer token to a caller and stores it in
// the request context. Missing, malformed, unknown and revoked tokens
// are all a plain 401.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			s.writeError(w, http.StatusUnauthorized, errors.New("missing bearer token"))
			return
		}
		caller, err := s.Store.VerifyToken(r.Context(), secret)
		if err != nil {
			if !errors.Is(err, ErrTokenInvalid) {
				s.Log.Error("token verification failed", "error", err)
			}
			s.writeError(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}

// createEntry handles POST /v1/entries: new entry + version 1. The
// author is the caller's principal; the scope is the caller's project
// unless a scope-less token asked for global (the scope-less default)
// or named a project.
func (s *Server) createEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req kbclient.CreateEntryRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if !ValidType(req.Type) {
		s.writeError(w, http.StatusBadRequest,
			fmt.Errorf("unknown type %q (valid: %s)", req.Type, strings.Join(EntryTypes, ", ")))
		return
	}
	if req.Title == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("title is required"))
		return
	}
	if req.Content == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("content is required"))
		return
	}

	projectID, err := writeScope(caller, req.Global, req.ProjectID)
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

	entry, err := s.Store.CreateEntry(ctx, NewEntry{
		ProjectID: projectID,
		Type:      req.Type,
		Slug:      slug,
		Title:     req.Title,
		Content:   req.Content,
		Author:    caller.Principal.ID,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, entry)
}

// appendVersion handles POST /v1/entries/{slug}/versions: the only way
// content ever changes — an appended version. Project tokens may
// append only to their own project's entries (reading global is fine,
// writing it is not).
func (s *Server) appendVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req kbclient.AppendVersionRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Content == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("content is required"))
		return
	}
	ref, err := s.resolveSlug(ctx, caller, r.PathValue("slug"), req.ProjectID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if caller.ProjectID != nil &&
		(ref.ProjectID == nil || *ref.ProjectID != *caller.ProjectID) {
		s.writeError(w, http.StatusForbidden,
			errors.New("project-scoped token can only write its own project's entries"))
		return
	}
	entry, err := s.Store.AppendVersion(ctx, ref.ID, req.Title, req.Content, caller.Principal.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, entry)
}

// getEntry handles GET /v1/entries/{slug}?version=n&history=1&project=.
func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	q := r.URL.Query()

	var explicitProject *uuid.UUID
	if p := q.Get("project"); p != "" {
		id, err := uuid.Parse(p)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid project %q", p))
			return
		}
		explicitProject = &id
	}
	ref, err := s.resolveSlug(ctx, caller, r.PathValue("slug"), explicitProject)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	var version *int
	if v := q.Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid version %q", v))
			return
		}
		version = &n
	}
	entry, err := s.Store.GetEntry(ctx, ref.ID, version)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if q.Get("history") == "1" {
		history, err := s.Store.History(ctx, ref.ID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		entry.History = history
	}
	s.writeJSON(w, http.StatusOK, entry)
}

// listEntries handles GET /v1/entries?type=&limit=.
func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	q := r.URL.Query()

	typ := q.Get("type")
	if typ != "" && !ValidType(typ) {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("unknown type %q", typ))
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
	entries, err := s.Store.ListEntries(ctx, caller.Scope(), typ, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, entries)
}

// recall handles GET /v1/recall?q=&type=&n=.
func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	q := r.URL.Query()

	query := q.Get("q")
	if query == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("q is required"))
		return
	}
	typ := q.Get("type")
	if typ != "" && !ValidType(typ) {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("unknown type %q", typ))
		return
	}
	n := 0
	if v := q.Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid n %q", v))
			return
		}
		n = parsed
	}
	hits, err := s.Store.Recall(ctx, caller.Scope(), query, typ, n)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if hits == nil {
		hits = []kbclient.RecallHit{}
	}
	s.writeJSON(w, http.StatusOK, hits)
}

// resolveSlug applies the caller-dependent slug rules: project tokens
// prefer their project's entry over a global one and may not name
// another project; scope-less tokens may disambiguate with an explicit
// project id (which then also pins nil = global — but a nil pointer
// from them means "any single match").
func (s *Server) resolveSlug(ctx context.Context, caller Caller, slug string, explicitProject *uuid.UUID) (EntryRef, error) {
	if caller.ProjectID != nil {
		if explicitProject != nil && *explicitProject != *caller.ProjectID {
			return EntryRef{}, errForbiddenProject
		}
		return s.Store.ResolveSlug(ctx, caller.Scope(), slug, nil, false)
	}
	return s.Store.ResolveSlug(ctx, caller.Scope(), slug, explicitProject, explicitProject != nil)
}

// errForbiddenProject: a project-scoped token named a different
// project.
var errForbiddenProject = errors.New("kbased: token is scoped to a different project")

// writeScope decides the project scope of a new entry. Project tokens
// always write to their own project; global (or a foreign project id)
// needs a scope-less token. Scope-less tokens default to global.
func writeScope(caller Caller, global bool, requested *uuid.UUID) (*uuid.UUID, error) {
	if caller.ProjectID != nil {
		if global {
			return nil, errors.New("global entries require a token scoped to all projects")
		}
		if requested != nil && *requested != *caller.ProjectID {
			return nil, errForbiddenProject
		}
		return caller.ProjectID, nil
	}
	if global || requested == nil {
		return nil, nil
	}
	return requested, nil
}

// ── Slugs ────────────────────────────────────────────────────────────

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validSlug reports whether s is a well-formed slug: lowercase
// alphanumeric words separated by single dashes, at most 128 bytes.
func validSlug(s string) bool {
	return len(s) <= 128 && slugRe.MatchString(s)
}

// Slugify derives a slug from a title: lowercased, alphanumeric runs
// kept, everything else collapsed to single dashes.
func Slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// ── Response helpers ─────────────────────────────────────────────────

// writeJSON writes v with the given status. Encoding failures are
// logged, not surfaced; the status line is already gone.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Log.Warn("writing response", "error", err)
	}
}

// writeError writes a kbclient.Error body. Unexpected 500s are logged
// server-side and not leaked to clients.
func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	if status == http.StatusInternalServerError {
		s.Log.Error("request failed", "error", err)
		err = errors.New("internal error")
	}
	s.writeJSON(w, status, kbclient.Error{Error: err.Error()})
}

// writeStoreError maps store sentinels to statuses.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		s.writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ErrSlugTaken), errors.Is(err, ErrAmbiguousSlug):
		s.writeError(w, http.StatusConflict, err)
	case errors.Is(err, errForbiddenProject):
		s.writeError(w, http.StatusForbidden, err)
	default:
		s.writeError(w, http.StatusInternalServerError, err)
	}
}

// decodeBody strictly decodes a JSON request body into v.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
