// Package api is labd's HTTP layer: the client API (localhost, no
// auth in v1; the TUI's interface) and the agent API (bearer-token
// auth; lab-agent's interface). Both speak the JSON types in
// internal/wire.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// writeJSON writes v with the given status. Encoding failures are
// logged, not surfaced; the status line is already gone.
func writeJSON(log *slog.Logger, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warn("writing response", "error", err)
	}
}

// writeError maps err to a status (store.ErrNotFound → 404 unless a
// more specific status was chosen by the caller) and writes a
// wire.Error body.
func writeError(log *slog.Logger, w http.ResponseWriter, status int, err error) {
	if errors.Is(err, store.ErrNotFound) && status == http.StatusInternalServerError {
		status = http.StatusNotFound
	}
	if errors.Is(err, store.ErrDuplicateName) && status == http.StatusInternalServerError {
		status = http.StatusConflict
	}
	if status == http.StatusInternalServerError {
		// Don't leak internals to clients on unexpected errors, but do
		// log them.
		log.Error("request failed", "error", err)
	}
	writeJSON(log, w, status, wire.Error{Error: err.Error()})
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

// ── store → wire conversions ─────────────────────────────────────────

func toWireProject(p store.Project) wire.Project {
	return wire.Project{
		ID:         p.ID,
		Name:       p.Name,
		OriginKind: p.OriginKind,
		Origin:     p.Origin,
		Stack:      p.Stack,
		CreatedAt:  p.CreatedAt,
	}
}

func toWireAgent(a store.Agent, sess *store.Session, running bool) wire.Agent {
	wa := wire.Agent{
		ID:                  a.ID,
		ProjectID:           a.ProjectID,
		Name:                a.Name,
		Model:               a.Model,
		State:               a.State,
		StatusText:          a.StatusText,
		Branch:              a.Branch,
		CredentialID:        a.CredentialID,
		Running:             running,
		CanSpawn:            a.CanSpawn,
		RetireContextTokens: a.RetireContextTokens,
		CreatedAt:           a.CreatedAt,
	}
	if sess != nil {
		wa.SessionID = &sess.ID
	}
	return wa
}

// agentContextTokens computes the agent's current context occupancy
// (0 when it has no open session).
func agentContextTokens(ctx context.Context, st *store.Store, sess *store.Session) (int64, error) {
	if sess == nil {
		return 0, nil
	}
	return st.SessionContextTokens(ctx, sess.ID)
}

func toWireTurn(t store.Turn) wire.Turn {
	return wire.Turn{
		ID:         t.ID,
		AgentID:    t.AgentID,
		SessionID:  t.SessionID,
		SourceKind: t.SourceKind,
		SourceID:   t.SourceID,
		Content:    t.Content,
		Status:     t.Status,
		Error:      t.Error,
		CreatedAt:  t.CreatedAt,
		FinishedAt: t.FinishedAt,
	}
}

func toWireSession(s store.SessionStats) wire.Session {
	return wire.Session{
		ID:              s.ID,
		AgentID:         s.AgentID,
		ClaudeSessionID: s.ClaudeSessionID,
		StartedAt:       s.StartedAt,
		EndedAt:         s.EndedAt,
		EndReason:       s.EndReason,
		PrevSessionID:   s.PrevSessionID,
		EventCount:      s.EventCount,
	}
}

func toWireAgentUsage(u store.AgentUsage) wire.AgentUsage {
	return wire.AgentUsage{
		AgentID:   u.AgentID,
		AgentName: u.AgentName,
		LastHour:  wire.Usage{TokensIn: u.HourIn, TokensOut: u.HourOut, CostUSD: u.HourCost, Turns: u.HourTurns},
		Today:     wire.Usage{TokensIn: u.DayIn, TokensOut: u.DayOut, CostUSD: u.DayCost, Turns: u.DayTurns},
		Total:     wire.Usage{TokensIn: u.TotalIn, TokensOut: u.TotalOut, CostUSD: u.TotalCost, Turns: u.TotalTurn},
	}
}

func toWireEvent(e store.Event) wire.Event {
	return wire.Event{
		ID:        e.ID,
		AgentID:   e.AgentID,
		SessionID: e.SessionID,
		TurnID:    e.TurnID,
		Seq:       e.Seq,
		Kind:      e.Kind,
		Payload:   e.Payload,
		TS:        e.TS,
	}
}
