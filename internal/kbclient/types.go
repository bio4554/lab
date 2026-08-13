package kbclient

import (
	"time"

	"github.com/google/uuid"
)

// The JSON types of the kbased HTTP API. They live in this package
// (rather than a third shared package) so the daemon, the CLI and labd
// all speak the same shapes; internal/kbased imports them.

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// Principal is an identity kbase attributes writes to: a lab agent
// (external_id = lab agent uuid, opaque to kbase) or a human handle.
type Principal struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"` // "agent" | "human"
	ExternalID  string    `json:"external_id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// Author is the provenance stamp on a version: the authoring
// principal, resolved server-side from the request token.
type Author struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	ExternalID  string    `json:"external_id"`
	DisplayName string    `json:"display_name"`
}

// Entry is one knowledge-base entry at a specific version (the current
// one unless ?version=n asked otherwise). Titles live on versions.
type Entry struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID *uuid.UUID `json:"project_id,omitempty"` // nil = global scope
	Type      string     `json:"type"`
	Slug      string     `json:"slug"`
	VersionNo int        `json:"version_no"`
	Title     string     `json:"title"`
	Content   string     `json:"content,omitempty"` // omitted in listings
	Author    Author     `json:"author"`
	CreatedAt time.Time  `json:"created_at"` // entry creation
	UpdatedAt time.Time  `json:"updated_at"` // this version's creation
	// History is the full version list (newest first), present only
	// when requested with ?history=1.
	History []VersionMeta `json:"history,omitempty"`
}

// VersionMeta is one row of an entry's history: everything but the
// content.
type VersionMeta struct {
	VersionNo int       `json:"version_no"`
	Title     string    `json:"title"`
	Author    Author    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// RecallHit is one full-text-search result over current versions.
type RecallHit struct {
	Slug      string     `json:"slug"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
	Rank      float32    `json:"rank"`
	Excerpt   string     `json:"excerpt"`
}

// CreateEntryRequest is the body of POST /v1/entries. Slug is
// generated from the title when empty. Global requires a scope-less
// (all-projects) token; project tokens always write to their own
// project.
type CreateEntryRequest struct {
	Type    string `json:"type"`
	Slug    string `json:"slug,omitempty"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Global  bool   `json:"global,omitempty"`
	// ProjectID targets a specific project's scope. Only honored for
	// scope-less tokens; project tokens are pinned to their project.
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// AppendVersionRequest is the body of POST /v1/entries/{slug}/versions.
// An empty title carries the previous version's title forward.
type AppendVersionRequest struct {
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
	// ProjectID disambiguates the slug for scope-less tokens when the
	// same slug exists in several scopes.
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// ── Graph API ────────────────────────────────────────────────────────

// Edge is one directed edge between two component entries, identified
// to clients by the endpoint slugs. Removal is a tombstone, never a
// delete, so past topologies stay reconstructible.
type Edge struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID *uuid.UUID `json:"project_id,omitempty"` // nil = global scope
	From      string     `json:"from"`                 // from-entry slug
	To        string     `json:"to"`                   // to-entry slug
	Label     string     `json:"label"`
	CreatedBy Author     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	// Tombstone provenance, present once the edge has been removed.
	TombstonedBy *Author    `json:"tombstoned_by,omitempty"`
	TombstonedAt *time.Time `json:"tombstoned_at,omitempty"`
}

// CreateEdgeRequest is the body of POST /v1/edges. From/To are
// component slugs resolved in the edge's scope (the writer's project,
// or global for scope-less tokens unless ProjectID targets a project).
type CreateEdgeRequest struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	// ProjectID targets a specific project's scope. Only honored for
	// scope-less tokens; project tokens are pinned to their project.
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// GraphNode is one component entry in a graph response.
type GraphNode struct {
	Slug      string     `json:"slug"`
	Title     string     `json:"title"` // current version's title
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// Graph is the body of GET /v1/graph: the component entries visible to
// the caller plus the edges live at the requested time.
type Graph struct {
	At    time.Time   `json:"at"`
	Nodes []GraphNode `json:"nodes"`
	Edges []Edge      `json:"edges"`
}

// ── Ticket API ───────────────────────────────────────────────────────

// Ticket is the coordination state of one ticket: the only in-place
// mutable rows in kbase, guarded by compare-and-swap on CASVersion.
// The narrative (body, comments, status history) lives in the backing
// entry's version chain.
type Ticket struct {
	ID         uuid.UUID  `json:"id"`
	ProjectID  *uuid.UUID `json:"project_id,omitempty"` // nil = global scope
	EntryID    uuid.UUID  `json:"entry_id"`
	Slug       string     `json:"slug"`
	Title      string     `json:"title"` // current version's title
	Status     string     `json:"status"`
	ClaimedBy  *Author    `json:"claimed_by,omitempty"`
	CASVersion int        `json:"cas_version"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	// Entry is the backing entry at its current version, present only
	// on single-ticket responses.
	Entry *Entry `json:"entry,omitempty"`
}

// CreateTicketRequest is the body of POST /v1/tickets: it creates the
// backing entry (type ticket, version 1 = Body) and the ticket row in
// one transaction. Slug is generated from the title when empty.
type CreateTicketRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Slug  string `json:"slug,omitempty"`
	// ProjectID targets a specific project's scope. Only honored for
	// scope-less tokens; project tokens are pinned to their project.
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// TicketTransitionRequest is the body of the claim/start/done/abandon
// endpoints: the caller's expected cas_version. A stale version (or a
// transition by a non-claimant) is a 409 carrying the current state.
type TicketTransitionRequest struct {
	CASVersion int `json:"cas_version"`
}

// CommentTicketRequest is the body of POST /v1/tickets/{id}/comment.
type CommentTicketRequest struct {
	Text string `json:"text"`
}

// ── Admin API (labd-facing) ──────────────────────────────────────────

// RegisterPrincipalRequest is the body of POST /admin/v1/principals.
// It upserts on (kind, external_id); a non-empty display name updates
// the stored one.
type RegisterPrincipalRequest struct {
	Kind        string `json:"kind"`
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// MintTokenRequest is the body of POST /admin/v1/tokens. A nil
// ProjectID mints a scope-less (all-projects) token. RevokeExisting
// first revokes the principal's active tokens with the same scope —
// one live token per (principal, scope).
type MintTokenRequest struct {
	PrincipalID    uuid.UUID  `json:"principal_id"`
	ProjectID      *uuid.UUID `json:"project_id,omitempty"`
	RevokeExisting bool       `json:"revoke_existing,omitempty"`
}

// MintTokenResponse carries the plaintext secret — the only time it is
// ever available. kbased stores only its hash.
type MintTokenResponse struct {
	TokenID     uuid.UUID  `json:"token_id"`
	PrincipalID uuid.UUID  `json:"principal_id"`
	ProjectID   *uuid.UUID `json:"project_id,omitempty"`
	Token       string     `json:"token"`
	CreatedAt   time.Time  `json:"created_at"`
}

// RevokeTokensRequest is the body of POST /admin/v1/tokens/revoke: it
// revokes the principal's active tokens with exactly the given scope
// (nil ProjectID = the scope-less tokens).
type RevokeTokensRequest struct {
	PrincipalID uuid.UUID  `json:"principal_id"`
	ProjectID   *uuid.UUID `json:"project_id,omitempty"`
}

// RevokeTokensResponse reports how many tokens were revoked.
type RevokeTokensResponse struct {
	Revoked int64 `json:"revoked"`
}
