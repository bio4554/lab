// Package kbclient is the typed HTTP client for the kbased API, used
// by the kbase CLI, labd (admin: principal registration + token mint)
// and later phases. It also defines the API's JSON types (types.go).
package kbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Client talks to one kbased instance. Token is a bearer token: an
// agent/human token for the /v1 API, or the admin token for the
// /admin/v1 API (use two Clients when you need both).
type Client struct {
	// BaseURL is the API root, e.g. "http://127.0.0.1:7720".
	BaseURL string
	// Token is sent as the Authorization bearer token.
	Token string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// New returns a client for addr (host:port or a full http:// URL)
// authenticating with token.
func New(addr, token string) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Client{BaseURL: strings.TrimRight(addr, "/"), Token: token}
}

// APIError is a non-2xx response, carrying the kbased error message.
// Ticket CAS/permission conflicts (409) also carry the ticket's
// current state so callers can re-inspect and retry without a second
// GET.
type APIError struct {
	Status  int
	Message string
	Ticket  *Ticket
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kbased: %s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// do performs one JSON request. body and out may be nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("kbclient: encoding request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return fmt.Errorf("kbclient: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Error  string  `json:"error"`
			Ticket *Ticket `json:"ticket"`
		}
		msg := resp.Status
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return &APIError{Status: resp.StatusCode, Message: msg, Ticket: apiErr.Ticket}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("kbclient: decoding response: %w", err)
	}
	return nil
}

// ── Entry API ────────────────────────────────────────────────────────

// CreateEntry creates a new entry (version 1).
func (c *Client) CreateEntry(ctx context.Context, req CreateEntryRequest) (Entry, error) {
	var e Entry
	err := c.do(ctx, http.MethodPost, "/v1/entries", req, &e)
	return e, err
}

// AppendVersion appends a version to the entry named by slug.
func (c *Client) AppendVersion(ctx context.Context, slug string, req AppendVersionRequest) (Entry, error) {
	var e Entry
	err := c.do(ctx, http.MethodPost, "/v1/entries/"+url.PathEscape(slug)+"/versions", req, &e)
	return e, err
}

// GetEntryOptions modifies GetEntry: a specific version (0 = current),
// the version history, or an explicit project scope (scope-less
// tokens, for disambiguating slugs).
type GetEntryOptions struct {
	Version   int
	History   bool
	ProjectID *uuid.UUID
}

// GetEntry fetches one entry by slug.
func (c *Client) GetEntry(ctx context.Context, slug string, opts GetEntryOptions) (Entry, error) {
	q := url.Values{}
	if opts.Version > 0 {
		q.Set("version", strconv.Itoa(opts.Version))
	}
	if opts.History {
		q.Set("history", "1")
	}
	if opts.ProjectID != nil {
		q.Set("project", opts.ProjectID.String())
	}
	path := "/v1/entries/" + url.PathEscape(slug)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var e Entry
	err := c.do(ctx, http.MethodGet, path, nil, &e)
	return e, err
}

// ListEntries lists current versions in the token's scope, newest
// first. typ empty = all types; limit 0 = server default.
func (c *Client) ListEntries(ctx context.Context, typ string, limit int) ([]Entry, error) {
	q := url.Values{}
	if typ != "" {
		q.Set("type", typ)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/entries"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []Entry
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Recall runs ranked full-text search over current versions. typ empty
// = all types; n 0 = server default (8).
func (c *Client) Recall(ctx context.Context, query, typ string, n int) ([]RecallHit, error) {
	q := url.Values{"q": {query}}
	if typ != "" {
		q.Set("type", typ)
	}
	if n > 0 {
		q.Set("n", strconv.Itoa(n))
	}
	var out []RecallHit
	err := c.do(ctx, http.MethodGet, "/v1/recall?"+q.Encode(), nil, &out)
	return out, err
}

// ── Graph API ────────────────────────────────────────────────────────

// CreateEdge links two components with a labeled directed edge.
func (c *Client) CreateEdge(ctx context.Context, req CreateEdgeRequest) (Edge, error) {
	var e Edge
	err := c.do(ctx, http.MethodPost, "/v1/edges", req, &e)
	return e, err
}

// TombstoneEdge removes an edge by tombstoning it (the row survives so
// past topologies stay reconstructible). A second tombstone is a 409.
func (c *Client) TombstoneEdge(ctx context.Context, id uuid.UUID) (Edge, error) {
	var e Edge
	err := c.do(ctx, http.MethodDelete, "/v1/edges/"+id.String(), nil, &e)
	return e, err
}

// Graph fetches the component graph: nodes visible to the token plus
// the edges live at the given time (zero at = now).
func (c *Client) Graph(ctx context.Context, at time.Time) (Graph, error) {
	path := "/v1/graph"
	if !at.IsZero() {
		path += "?at=" + url.QueryEscape(at.Format(time.RFC3339Nano))
	}
	var g Graph
	err := c.do(ctx, http.MethodGet, path, nil, &g)
	return g, err
}

// ── Ticket API ───────────────────────────────────────────────────────

// CreateTicket creates the backing entry (type ticket) and the ticket
// row in one transaction.
func (c *Client) CreateTicket(ctx context.Context, req CreateTicketRequest) (Ticket, error) {
	var t Ticket
	err := c.do(ctx, http.MethodPost, "/v1/tickets", req, &t)
	return t, err
}

// ListTickets lists tickets in the token's scope, newest first. status
// empty = all statuses; limit 0 = server default.
func (c *Client) ListTickets(ctx context.Context, status string, limit int) ([]Ticket, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/tickets"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []Ticket
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetTicket fetches one ticket (with its backing entry's current
// version) by ticket id or entry slug.
func (c *Client) GetTicket(ctx context.Context, ref string) (Ticket, error) {
	var t Ticket
	err := c.do(ctx, http.MethodGet, "/v1/tickets/"+url.PathEscape(ref), nil, &t)
	return t, err
}

// transitionTicket posts one CAS transition. Conflicts come back as an
// *APIError with Status 409 and the ticket's current state attached.
func (c *Client) transitionTicket(ctx context.Context, ref, action string, casVersion int) (Ticket, error) {
	var t Ticket
	err := c.do(ctx, http.MethodPost,
		"/v1/tickets/"+url.PathEscape(ref)+"/"+action,
		TicketTransitionRequest{CASVersion: casVersion}, &t)
	return t, err
}

// ClaimTicket atomically claims an open or abandoned ticket. Exactly
// one concurrent claimer wins; losers get a 409 with current state.
func (c *Client) ClaimTicket(ctx context.Context, ref string, casVersion int) (Ticket, error) {
	return c.transitionTicket(ctx, ref, "claim", casVersion)
}

// StartTicket moves a claimed ticket to in_progress (claimant only).
func (c *Client) StartTicket(ctx context.Context, ref string, casVersion int) (Ticket, error) {
	return c.transitionTicket(ctx, ref, "start", casVersion)
}

// DoneTicket moves a claimed/in_progress ticket to done (claimant
// only).
func (c *Client) DoneTicket(ctx context.Context, ref string, casVersion int) (Ticket, error) {
	return c.transitionTicket(ctx, ref, "done", casVersion)
}

// AbandonTicket moves a claimed/in_progress ticket to abandoned and
// clears the claimant, making it claimable again (claimant only).
func (c *Client) AbandonTicket(ctx context.Context, ref string, casVersion int) (Ticket, error) {
	return c.transitionTicket(ctx, ref, "abandon", casVersion)
}

// CommentTicket appends a comment version to the ticket's entry. It
// does not touch status and needs no CAS version.
func (c *Client) CommentTicket(ctx context.Context, ref, text string) (Ticket, error) {
	var t Ticket
	err := c.do(ctx, http.MethodPost,
		"/v1/tickets/"+url.PathEscape(ref)+"/comment",
		CommentTicketRequest{Text: text}, &t)
	return t, err
}

// ── Admin API (Token must be the configured admin token) ────────────

// RegisterPrincipal upserts a principal on (kind, external_id).
func (c *Client) RegisterPrincipal(ctx context.Context, req RegisterPrincipalRequest) (Principal, error) {
	var p Principal
	err := c.do(ctx, http.MethodPost, "/admin/v1/principals", req, &p)
	return p, err
}

// MintToken mints a bearer token; the response carries the plaintext
// secret, the only time it is ever available.
func (c *Client) MintToken(ctx context.Context, req MintTokenRequest) (MintTokenResponse, error) {
	var resp MintTokenResponse
	err := c.do(ctx, http.MethodPost, "/admin/v1/tokens", req, &resp)
	return resp, err
}

// RevokeTokens revokes the principal's active tokens with exactly the
// given scope.
func (c *Client) RevokeTokens(ctx context.Context, req RevokeTokensRequest) (int64, error) {
	var resp RevokeTokensResponse
	err := c.do(ctx, http.MethodPost, "/admin/v1/tokens/revoke", req, &resp)
	return resp.Revoked, err
}
