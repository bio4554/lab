package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/creds"
	"github.com/bio4554/lab/internal/labd/gitrepo"
	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// ClientServer is the client (TUI) API. Localhost only, no auth in v1
// — which is also why credential creation may carry the secret in the
// request body.
type ClientServer struct {
	Store   *store.Store
	Pool    *pgxpool.Pool // health checks only
	Git     *gitrepo.Manager
	Driver  *claude.Driver  // Retire
	Manager *claude.Manager // start/stop drivers
	Hub     *Hub
	Vault   *creds.Vault // encrypts credential secrets at rest
	Gate    *budget.Gate // live verdicts for the usage status endpoint
	Version string
	Log     *slog.Logger
}

// Handler returns the client API routes.
func (s *ClientServer) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.status)

	mux.HandleFunc("POST /v1/projects", s.projectCreate)
	mux.HandleFunc("GET /v1/projects", s.projectList)
	mux.HandleFunc("GET /v1/projects/{project}", s.projectGet)
	mux.HandleFunc("DELETE /v1/projects/{project}", s.projectDelete)

	mux.HandleFunc("POST /v1/projects/{project}/agents", s.agentCreate)
	mux.HandleFunc("GET /v1/projects/{project}/agents", s.agentList)
	mux.HandleFunc("POST /v1/projects/{project}/agents/{agent}/start", s.agentStart)
	mux.HandleFunc("POST /v1/projects/{project}/agents/{agent}/stop", s.agentStop)
	mux.HandleFunc("POST /v1/projects/{project}/agents/{agent}/retire", s.agentRetire)
	mux.HandleFunc("POST /v1/projects/{project}/agents/{agent}/turns", s.turnSubmit)
	mux.HandleFunc("GET /v1/projects/{project}/agents/{agent}/sessions", s.sessionList)
	mux.HandleFunc("GET /v1/projects/{project}/usage", s.projectUsage)

	mux.HandleFunc("POST /v1/credentials", s.credentialCreate)
	mux.HandleFunc("GET /v1/credentials", s.credentialList)
	mux.HandleFunc("DELETE /v1/credentials/{credential}", s.credentialDelete)
	mux.HandleFunc("PUT /v1/credentials/{credential}/expiry", s.credentialSetExpiry)
	mux.HandleFunc("POST /v1/credentials/{credential}/resume", s.credentialResume)
	mux.HandleFunc("GET /v1/credentials/{credential}/budget", s.credentialBudgetGet)
	mux.HandleFunc("PUT /v1/credentials/{credential}/budget", s.credentialBudgetSet)
	mux.HandleFunc("GET /v1/projects/{project}/agents/{agent}/budget", s.agentBudgetGet)
	mux.HandleFunc("PUT /v1/projects/{project}/agents/{agent}/budget", s.agentBudgetSet)
	mux.HandleFunc("PUT /v1/projects/{project}/agents/{agent}/credential", s.agentCredentialSet)
	mux.HandleFunc("GET /v1/usage", s.usageStatus)

	mux.HandleFunc("GET /v1/turns/{id}", s.turnGet)
	mux.HandleFunc("GET /v1/sessions/{session}/events", s.sessionEvents)
	mux.HandleFunc("GET /v1/events", s.globalEvents)
	mux.HandleFunc("GET /v1/events/stream", s.eventStream)
	return mux
}

// findProject resolves the {project} path value.
func (s *ClientServer) findProject(ctx context.Context, r *http.Request) (store.Project, error) {
	return s.Store.GetProjectByName(ctx, r.PathValue("project"))
}

// findAgent resolves {project}/{agent} path values.
func (s *ClientServer) findAgent(ctx context.Context, r *http.Request) (store.Project, store.Agent, error) {
	proj, err := s.findProject(ctx, r)
	if err != nil {
		return store.Project{}, store.Agent{}, err
	}
	agents, err := s.Store.ListAgents(ctx, proj.ID)
	if err != nil {
		return store.Project{}, store.Agent{}, err
	}
	name := r.PathValue("agent")
	for _, a := range agents {
		if a.Name == name {
			return proj, a, nil
		}
	}
	return store.Project{}, store.Agent{}, fmt.Errorf("agent %q: %w", name, store.ErrNotFound)
}

// ── daemon status ────────────────────────────────────────────────────

func (s *ClientServer) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	dbHealthy := s.Pool != nil && s.Pool.Ping(ctx) == nil
	running := []uuid.UUID{}
	if s.Manager != nil {
		running = s.Manager.Running()
	}
	writeJSON(s.Log, w, http.StatusOK, wire.DaemonStatus{
		Version:   s.Version,
		DBHealthy: dbHealthy,
		Stacks:    runtime.Stacks(),
		Running:   running,
	})
}

// ── projects ─────────────────────────────────────────────────────────

// originKind classifies an origin as a git URL or a local path (made
// absolute so git operations don't depend on the daemon's cwd).
func originKind(origin string) (kind, resolved string) {
	for _, p := range []string{"http://", "https://", "ssh://", "git://", "git@"} {
		if strings.HasPrefix(origin, p) {
			return store.OriginKindGitURL, origin
		}
	}
	if abs, err := filepath.Abs(origin); err == nil {
		return store.OriginKindLocalPath, abs
	}
	return store.OriginKindLocalPath, origin
}

func (s *ClientServer) projectCreate(w http.ResponseWriter, r *http.Request) {
	var req wire.CreateProjectRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Origin == "" {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("name and origin are required"))
		return
	}
	if !runtime.ValidStack(req.Stack) {
		writeError(s.Log, w, http.StatusBadRequest,
			fmt.Errorf("unknown stack %q (valid: %s)", req.Stack, strings.Join(runtime.Stacks(), ", ")))
		return
	}
	ctx := r.Context()
	kind, origin := originKind(req.Origin)
	proj, err := s.Store.CreateProject(ctx, store.NewProject{
		Name: req.Name, OriginKind: kind, Origin: origin, Stack: req.Stack,
	})
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Git.CreateProject(ctx, proj.ID.String(), kind, origin); err != nil {
		// Don't leave a row without a repo behind it.
		if delErr := s.Store.DeleteProject(ctx, proj.ID); delErr != nil {
			s.Log.Error("rolling back project row", "project", proj.Name, "error", delErr)
		}
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusCreated, toWireProject(proj))
}

func (s *ClientServer) projectList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Project, len(projects))
	for i, p := range projects {
		out[i] = toWireProject(p)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

func (s *ClientServer) projectGet(w http.ResponseWriter, r *http.Request) {
	proj, err := s.findProject(r.Context(), r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, toWireProject(proj))
}

func (s *ClientServer) projectDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proj, err := s.findProject(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	agents, err := s.Store.ListAgents(ctx, proj.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if len(agents) > 0 {
		writeError(s.Log, w, http.StatusConflict,
			fmt.Errorf("project %q still has %d agent(s)", proj.Name, len(agents)))
		return
	}
	if err := s.Store.DeleteProject(ctx, proj.ID); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Git.RemoveProject(ctx, proj.ID.String()); err != nil {
		// The row is gone; the leftover directory is only disk.
		s.Log.Warn("removing project repo dir", "project", proj.Name, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── agents ───────────────────────────────────────────────────────────

func (s *ClientServer) agentCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proj, err := s.findProject(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	var req wire.CreateAgentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	na := store.NewAgent{
		ProjectID:           proj.ID,
		Name:                req.Name,
		RolePrompt:          req.RolePrompt,
		Branch:              gitrepo.BranchName(req.Name),
		CanSpawn:            req.CanSpawn,
		RetireContextTokens: req.RetireContextTokens,
	}
	if req.Model != "" {
		na.Model = &req.Model
	}
	if _, err := budget.ParseLimits(req.Budget); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	na.Budget = req.Budget
	switch {
	case req.CredentialID != nil:
		if _, err := s.Store.GetCredential(ctx, *req.CredentialID); err != nil {
			writeError(s.Log, w, http.StatusBadRequest, fmt.Errorf("credential %s: %w", req.CredentialID, err))
			return
		}
		na.CredentialID = req.CredentialID
	case req.CredentialKind != "":
		// Phase 6 compat shim: the old API took a bare kind (env
		// passthrough). Now it binds the only stored credential of
		// that kind, erroring when none or several exist.
		cred, err := s.credentialByKind(ctx, req.CredentialKind)
		if err != nil {
			writeError(s.Log, w, http.StatusBadRequest, err)
			return
		}
		na.CredentialID = &cred.ID
	}
	agent, err := s.Store.CreateAgent(ctx, na)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusCreated, toWireAgent(agent, nil, false))
}

func (s *ClientServer) agentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proj, err := s.findProject(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	agents, err := s.Store.ListAgents(ctx, proj.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	credCache := map[uuid.UUID]store.Credential{}
	out := make([]wire.Agent, 0, len(agents))
	for _, a := range agents {
		sess, err := s.Store.CurrentSession(ctx, a.ID)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		running := s.Manager != nil && s.Manager.IsRunning(a.ID)
		wa := toWireAgent(a, sess, running)
		if wa.ContextTokens, err = agentContextTokens(ctx, s.Store, sess); err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		// A paused agent surfaces its credential's recorded reset time.
		if a.State == store.AgentStatePaused && a.CredentialID != nil {
			cred, ok := credCache[*a.CredentialID]
			if !ok {
				cred, err = s.Store.GetCredential(ctx, *a.CredentialID)
				if err != nil {
					writeError(s.Log, w, http.StatusInternalServerError, err)
					return
				}
				credCache[*a.CredentialID] = cred
			}
			wa.PausedUntil = cred.LimitedUntil
		}
		out = append(out, wa)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

// credentialByKind resolves the Phase 6 compat shim: exactly one
// stored credential of the kind.
func (s *ClientServer) credentialByKind(ctx context.Context, kind string) (store.Credential, error) {
	if kind != store.CredentialKindAPIKey && kind != store.CredentialKindOAuthToken {
		return store.Credential{}, errors.New("credential_kind must be api_key or oauth_token")
	}
	all, err := s.Store.ListCredentials(ctx)
	if err != nil {
		return store.Credential{}, err
	}
	var matches []store.Credential
	for _, c := range all {
		if c.Kind == kind {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return store.Credential{}, fmt.Errorf("no stored credential of kind %s; create one (labctl cred add) or pass credential_id", kind)
	case 1:
		return matches[0], nil
	default:
		return store.Credential{}, fmt.Errorf("%d credentials of kind %s; pass credential_id to disambiguate", len(matches), kind)
	}
}

func (s *ClientServer) agentStart(w http.ResponseWriter, r *http.Request) {
	_, agent, err := s.findAgent(r.Context(), r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if agent.State == store.AgentStateRetired {
		writeError(s.Log, w, http.StatusConflict, errors.New("agent is retired"))
		return
	}
	if err := s.Manager.Start(agent.ID); err != nil {
		if errors.Is(err, claude.ErrAlreadyRunning) {
			writeError(s.Log, w, http.StatusConflict, err)
			return
		}
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ClientServer) agentStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Manager.Stop(ctx, agent.ID); err != nil {
		if errors.Is(err, claude.ErrNotRunning) {
			writeError(s.Log, w, http.StatusConflict, err)
			return
		}
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ClientServer) agentRetire(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	var req wire.RetireAgentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Reason == "" {
		req.Reason = "retired via client API"
	}
	if req.Seed == "" {
		// Default seed: memory recovery through kbase (see RetireSeed).
		// An explicit seed is used verbatim.
		req.Seed = claude.RetireSeed(req.Reason, agent.Name)
	}
	sess, err := s.Driver.Retire(ctx, agent.ID, req.Reason, req.Seed)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, wire.RetireAgentResponse{
		SessionID:     sess.ID,
		PrevSessionID: sess.PrevSessionID,
	})
}

// ── turns ────────────────────────────────────────────────────────────

func (s *ClientServer) turnSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	var req wire.SubmitTurnRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Content == "" {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("content is required"))
		return
	}
	turn, err := s.Store.EnqueueTurn(ctx, store.NewTurn{
		AgentID:    agent.ID,
		SourceKind: store.SourceKindUser,
		Content:    req.Content,
	})
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusCreated, toWireTurn(turn))
}

func (s *ClientServer) turnGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, fmt.Errorf("invalid turn id: %w", err))
		return
	}
	turn, err := s.Store.GetTurn(r.Context(), id)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, toWireTurn(turn))
}

// ── sessions & usage ─────────────────────────────────────────────────

func (s *ClientServer) sessionList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	sessions, err := s.Store.ListSessions(ctx, agent.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Session, len(sessions))
	for i, sess := range sessions {
		out[i] = toWireSession(sess)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

func (s *ClientServer) projectUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proj, err := s.findProject(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows, err := s.Store.ProjectUsage(ctx, proj.ID, now.Add(-time.Hour), dayStart)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.AgentUsage, len(rows))
	for i, u := range rows {
		out[i] = toWireAgentUsage(u)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

// ── events ───────────────────────────────────────────────────────────

// queryInt parses an optional integer query parameter.
func queryInt(r *http.Request, name string, def int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q", name, v)
	}
	return n, nil
}

// clampLimit bounds a client-supplied limit.
func clampLimit(limit int64) int {
	if limit <= 0 || limit > 1000 {
		return 1000
	}
	return int(limit)
}

func (s *ClientServer) sessionEvents(w http.ResponseWriter, r *http.Request) {
	sessID, err := uuid.Parse(r.PathValue("session"))
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, fmt.Errorf("invalid session id: %w", err))
		return
	}
	afterSeq, err := queryInt(r, "after_seq", 0)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", 200)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	events, err := s.Store.EventsSince(r.Context(), sessID, afterSeq, clampLimit(limit))
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Event, len(events))
	for i, e := range events {
		out[i] = toWireEvent(e)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

func (s *ClientServer) globalEvents(w http.ResponseWriter, r *http.Request) {
	afterID, err := queryInt(r, "after_id", 0)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", 200)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	events, err := s.Store.EventsSinceID(r.Context(), afterID, clampLimit(limit))
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Event, len(events))
	for i, e := range events {
		out[i] = toWireEvent(e)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}
