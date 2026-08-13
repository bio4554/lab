package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Enum values enforced by CHECK constraints in the lab schema.
const (
	OriginKindGitURL    = "git_url"
	OriginKindLocalPath = "local_path"

	CredentialKindAPIKey     = "api_key"
	CredentialKindOAuthToken = "oauth_token"

	CredentialStatusActive  = "active"
	CredentialStatusExpired = "expired"

	AgentStateStopped = "stopped"
	AgentStateIdle    = "idle"
	AgentStateWorking = "working"
	AgentStatePaused  = "paused"
	AgentStateRetired = "retired"

	SourceKindUser  = "user"
	SourceKindAgent = "agent"

	TurnStatusQueued  = "queued"
	TurnStatusRunning = "running"
	TurnStatusDone    = "done"
	TurnStatusError   = "error"
)

type Project struct {
	ID                  uuid.UUID  `db:"id"`
	Name                string     `db:"name"`
	OriginKind          string     `db:"origin_kind"`
	Origin              string     `db:"origin"`
	Stack               string     `db:"stack"`
	DefaultCredentialID *uuid.UUID `db:"default_credential_id"`
	CreatedAt           time.Time  `db:"created_at"`
}

// NewProject holds the caller-supplied fields for CreateProject.
type NewProject struct {
	Name                string
	OriginKind          string
	Origin              string
	Stack               string
	DefaultCredentialID *uuid.UUID
}

type Credential struct {
	ID           uuid.UUID       `db:"id"`
	Kind         string          `db:"kind"`
	SecretEnc    []byte          `db:"secret_enc"`
	Label        string          `db:"label"`
	Status       string          `db:"status"`
	ExpiresAt    *time.Time      `db:"expires_at"`
	Budget       json.RawMessage `db:"budget"`
	LimitedUntil *time.Time      `db:"limited_until"`
	CreatedAt    time.Time       `db:"created_at"`
}

// NewCredential holds the caller-supplied fields for CreateCredential.
// SecretEnc is opaque to the store; encryption is the caller's problem.
// A nil Budget defaults to '{}'.
type NewCredential struct {
	Kind      string
	SecretEnc []byte
	Label     string
	ExpiresAt *time.Time
	Budget    json.RawMessage
}

type Agent struct {
	ID           uuid.UUID       `db:"id"`
	ProjectID    uuid.UUID       `db:"project_id"`
	Name         string          `db:"name"`
	RolePrompt   string          `db:"role_prompt"`
	Model        *string         `db:"model"`
	CredentialID *uuid.UUID      `db:"credential_id"`
	Budget       json.RawMessage `db:"budget"`
	State        string          `db:"state"`
	ContainerID  *string         `db:"container_id"`
	Branch       string          `db:"branch"`
	StatusText   *string         `db:"status_text"`
	// CanSpawn allows the agent to create workers via the agent API.
	CanSpawn bool `db:"can_spawn"`
	// RetireContextTokens, when set, auto-retires the agent's session
	// between turns once context occupancy reaches it. NULL = never.
	RetireContextTokens *int64    `db:"retire_context_tokens"`
	CreatedAt           time.Time `db:"created_at"`
}

// NewAgent holds the caller-supplied fields for CreateAgent. A nil
// Budget defaults to '{}'.
type NewAgent struct {
	ProjectID           uuid.UUID
	Name                string
	RolePrompt          string
	Model               *string
	CredentialID        *uuid.UUID
	Budget              json.RawMessage
	Branch              string
	CanSpawn            bool
	RetireContextTokens *int64
}

type Session struct {
	ID              uuid.UUID  `db:"id"`
	AgentID         uuid.UUID  `db:"agent_id"`
	ClaudeSessionID *string    `db:"claude_session_id"`
	StartedAt       time.Time  `db:"started_at"`
	EndedAt         *time.Time `db:"ended_at"`
	EndReason       *string    `db:"end_reason"`
	PrevSessionID   *uuid.UUID `db:"prev_session_id"`
}

type Turn struct {
	ID         uuid.UUID  `db:"id"`
	AgentID    uuid.UUID  `db:"agent_id"`
	SessionID  *uuid.UUID `db:"session_id"`
	SourceKind string     `db:"source_kind"`
	SourceID   *uuid.UUID `db:"source_id"`
	Content    string     `db:"content"`
	Status     string     `db:"status"`
	Error      *string    `db:"error"`
	CreatedAt  time.Time  `db:"created_at"`
	FinishedAt *time.Time `db:"finished_at"`
}

// NewTurn holds the caller-supplied fields for EnqueueTurn.
type NewTurn struct {
	AgentID    uuid.UUID
	SessionID  *uuid.UUID
	SourceKind string
	SourceID   *uuid.UUID
	Content    string
}

type Event struct {
	ID        int64           `db:"id"`
	AgentID   uuid.UUID       `db:"agent_id"`
	SessionID uuid.UUID       `db:"session_id"`
	TurnID    *uuid.UUID      `db:"turn_id"`
	Seq       int64           `db:"seq"`
	Kind      string          `db:"kind"`
	Payload   json.RawMessage `db:"payload"`
	TS        time.Time       `db:"ts"`
}

// AgentToken is one bearer token for the agent API. The secret is
// stored only as a SHA-256 hash; the plaintext is returned exactly
// once, by MintAgentToken.
type AgentToken struct {
	ID         uuid.UUID  `db:"id"`
	AgentID    uuid.UUID  `db:"agent_id"`
	SecretHash []byte     `db:"secret_hash"`
	CreatedAt  time.Time  `db:"created_at"`
	RevokedAt  *time.Time `db:"revoked_at"`
}

// UsageDelta is one increment applied to a usage rollup window.
type UsageDelta struct {
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
	Turns     int32
}

// Usage is an aggregate over rollup windows.
type Usage struct {
	TokensIn  int64   `db:"tokens_in"`
	TokensOut int64   `db:"tokens_out"`
	CostUSD   float64 `db:"cost_usd"`
	Turns     int64   `db:"turns"`
}
