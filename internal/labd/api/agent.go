package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// AgentServer is the agent-facing API, reachable from containers via
// host.docker.internal. Every route requires a bearer token minted at
// container create; the token maps to the calling agent, and agents
// see and touch only their own project.
type AgentServer struct {
	Store   *store.Store
	Manager *claude.Manager // Running flag in listings; may be nil
	Log     *slog.Logger
}

// callerKey carries the authenticated agent through the request
// context.
type callerKey struct{}

func callerFrom(ctx context.Context) store.Agent {
	return ctx.Value(callerKey{}).(store.Agent)
}

// Handler returns the agent API routes wrapped in token auth.
func (s *AgentServer) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", s.whoami)
	mux.HandleFunc("GET /v1/agents", s.siblings)
	mux.HandleFunc("POST /v1/agents/{agent}/turns", s.sendTurn)
	mux.HandleFunc("POST /v1/status", s.reportStatus)
	return s.authenticate(mux)
}

// authenticate resolves the bearer token to an agent and stores it in
// the request context. Missing, malformed, unknown and revoked tokens
// are all a plain 401.
func (s *AgentServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			writeError(s.Log, w, http.StatusUnauthorized, errors.New("missing bearer token"))
			return
		}
		agentID, err := s.Store.VerifyAgentToken(r.Context(), secret)
		if err != nil {
			if !errors.Is(err, store.ErrTokenInvalid) {
				s.Log.Error("token verification failed", "error", err)
			}
			writeError(s.Log, w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		agent, err := s.Store.GetAgent(r.Context(), agentID)
		if err != nil {
			writeError(s.Log, w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, agent)))
	})
}

func (s *AgentServer) whoami(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	proj, err := s.Store.GetProject(ctx, caller.ProjectID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	sess, err := s.Store.CurrentSession(ctx, caller.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	resp := wire.Whoami{
		AgentID:    caller.ID,
		Name:       caller.Name,
		Project:    proj.Name,
		ProjectID:  proj.ID,
		State:      caller.State,
		StatusText: caller.StatusText,
	}
	if sess != nil {
		resp.SessionID = &sess.ID
	}
	writeJSON(s.Log, w, http.StatusOK, resp)
}

// siblings lists the agents of the caller's project (the caller
// included).
func (s *AgentServer) siblings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	agents, err := s.Store.ListAgents(ctx, caller.ProjectID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Agent, 0, len(agents))
	for _, a := range agents {
		sess, err := s.Store.CurrentSession(ctx, a.ID)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		running := s.Manager != nil && s.Manager.IsRunning(a.ID)
		out = append(out, toWireAgent(a, sess, running))
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

// sendTurn enqueues a turn for an agent in the caller's project, with
// the caller recorded as the source. This is the orchestration
// primitive lab-agent (Phase 11) calls. Agents in other projects are
// indistinguishable from nonexistent ones (404).
func (s *AgentServer) sendTurn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req wire.SubmitTurnRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Content == "" {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("content is required"))
		return
	}
	agents, err := s.Store.ListAgents(ctx, caller.ProjectID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	name := r.PathValue("agent")
	var target *store.Agent
	for i := range agents {
		if agents[i].Name == name {
			target = &agents[i]
			break
		}
	}
	if target == nil {
		writeError(s.Log, w, http.StatusNotFound, fmt.Errorf("agent %q: %w", name, store.ErrNotFound))
		return
	}
	turn, err := s.Store.EnqueueTurn(ctx, store.NewTurn{
		AgentID:    target.ID,
		SourceKind: store.SourceKindAgent,
		SourceID:   &caller.ID,
		Content:    req.Content,
	})
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusCreated, toWireTurn(turn))
}

// reportStatus stores the caller's self-reported status string; an
// empty status clears it. Surfaced in both APIs' agent listings.
func (s *AgentServer) reportStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := callerFrom(ctx)
	var req wire.ReportStatusRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	var status *string
	if req.Status != "" {
		status = &req.Status
	}
	if err := s.Store.SetAgentStatusText(ctx, caller.ID, status); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
