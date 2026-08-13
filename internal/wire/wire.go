// Package wire holds the shared JSON types of labd's two HTTP APIs:
// the client API (TUI, Phase 7) and the agent API (lab-agent, Phase
// 11). Pure data, no behavior; both servers and clients import it.
package wire

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// ── Projects ──────────────────────────────────────────────────────────

type Project struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	OriginKind string    `json:"origin_kind"`
	Origin     string    `json:"origin"`
	Stack      string    `json:"stack"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateProjectRequest creates a project. Origin is a git URL or a
// local path; the server classifies it. Stack must be a valid stack
// name (see the daemon status endpoint for the list).
type CreateProjectRequest struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
	Stack  string `json:"stack"`
}

// ── Agents ────────────────────────────────────────────────────────────

// Agent is an agent as reported by both APIs. SessionID is the current
// (open) session, when one exists. StatusText is the agent's last
// self-reported status.
type Agent struct {
	ID         uuid.UUID  `json:"id"`
	ProjectID  uuid.UUID  `json:"project_id"`
	Name       string     `json:"name"`
	Model      *string    `json:"model,omitempty"`
	State      string     `json:"state"`
	StatusText *string    `json:"status_text,omitempty"`
	Branch     string     `json:"branch"`
	SessionID  *uuid.UUID `json:"session_id,omitempty"`
	Running    bool       `json:"running"` // a driver is hosted for it right now
	CreatedAt  time.Time  `json:"created_at"`
}

// CreateAgentRequest creates an agent in a project. CredentialKind,
// when non-empty, must be api_key or oauth_token and creates an
// env-passthrough credential row for the agent (the Phase 5 stand-in;
// real credential storage is Phase 8).
type CreateAgentRequest struct {
	Name           string `json:"name"`
	RolePrompt     string `json:"role_prompt,omitempty"`
	Model          string `json:"model,omitempty"`
	CredentialKind string `json:"credential_kind,omitempty"`
}

// RetireAgentRequest retires the agent's current session and seeds the
// next one.
type RetireAgentRequest struct {
	Reason string `json:"reason,omitempty"`
	Seed   string `json:"seed,omitempty"`
}

// RetireAgentResponse reports the new session.
type RetireAgentResponse struct {
	SessionID     uuid.UUID  `json:"session_id"`
	PrevSessionID *uuid.UUID `json:"prev_session_id,omitempty"`
}

// ── Turns ─────────────────────────────────────────────────────────────

// SubmitTurnRequest enqueues a prompt for an agent. On the client API
// the source is the user; on the agent API the source is the
// authenticated calling agent.
type SubmitTurnRequest struct {
	Content string `json:"content"`
}

type Turn struct {
	ID         uuid.UUID  `json:"id"`
	AgentID    uuid.UUID  `json:"agent_id"`
	SessionID  *uuid.UUID `json:"session_id,omitempty"`
	SourceKind string     `json:"source_kind"`
	SourceID   *uuid.UUID `json:"source_id,omitempty"`
	Content    string     `json:"content"`
	Status     string     `json:"status"`
	Error      *string    `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ── Sessions ──────────────────────────────────────────────────────────

// Session is one session in an agent's history listing. EventCount is
// the number of events stored for it.
type Session struct {
	ID              uuid.UUID  `json:"id"`
	AgentID         uuid.UUID  `json:"agent_id"`
	ClaudeSessionID *string    `json:"claude_session_id,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	EndReason       *string    `json:"end_reason,omitempty"`
	PrevSessionID   *uuid.UUID `json:"prev_session_id,omitempty"`
	EventCount      int64      `json:"event_count"`
}

// ── Usage ─────────────────────────────────────────────────────────────

// Usage is one aggregate over usage rollup windows.
type Usage struct {
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Turns     int64   `json:"turns"`
}

// AgentUsage is one agent's usage aggregates: the last hour, today
// (since local midnight, daemon clock), and all time. Buckets are by
// rollup window start.
type AgentUsage struct {
	AgentID   uuid.UUID `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	LastHour  Usage     `json:"last_hour"`
	Today     Usage     `json:"today"`
	Total     Usage     `json:"total"`
}

// ── Events ────────────────────────────────────────────────────────────

// Event is one stored stream-json event. Payload is the raw event as
// the Claude Code CLI emitted it. This is also the `data:` body of
// each SSE frame on the events stream, whose `id:` is ID.
type Event struct {
	ID        int64           `json:"id"`
	AgentID   uuid.UUID       `json:"agent_id"`
	SessionID uuid.UUID       `json:"session_id"`
	TurnID    *uuid.UUID      `json:"turn_id,omitempty"`
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	TS        time.Time       `json:"ts"`
}

// ── Daemon status ─────────────────────────────────────────────────────

// DaemonStatus is the client API's status report.
type DaemonStatus struct {
	Version   string      `json:"version"`
	DBHealthy bool        `json:"db_healthy"`
	Stacks    []string    `json:"stacks"`
	Running   []uuid.UUID `json:"running_agents"`
}

// ── Agent API ─────────────────────────────────────────────────────────

// Whoami identifies the authenticated agent.
type Whoami struct {
	AgentID    uuid.UUID  `json:"agent_id"`
	Name       string     `json:"name"`
	Project    string     `json:"project"`
	ProjectID  uuid.UUID  `json:"project_id"`
	State      string     `json:"state"`
	StatusText *string    `json:"status_text,omitempty"`
	SessionID  *uuid.UUID `json:"session_id,omitempty"`
}

// ReportStatusRequest is an agent's status self-report. Empty clears
// the stored status.
type ReportStatusRequest struct {
	Status string `json:"status"`
}
