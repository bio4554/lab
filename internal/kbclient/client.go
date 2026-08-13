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
type APIError struct {
	Status  int
	Message string
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
		var apiErr Error
		msg := resp.Status
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
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
