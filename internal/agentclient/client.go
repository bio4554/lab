// Package agentclient is the typed HTTP client for labd's agent API,
// used by the lab-agent CLI and tests. It mirrors kbclient's shape:
// typed methods over the wire types, non-2xx responses as *APIError.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bio4554/lab/internal/wire"
)

// Client talks to one labd agent API. Token is the agent bearer token
// injected into containers as LAB_AGENT_TOKEN.
type Client struct {
	// BaseURL is the API root, e.g. "http://host.docker.internal:7711".
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

// APIError is a non-2xx response, carrying the labd error message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("lab: %s (HTTP %d)", e.Message, e.Status)
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
			return fmt.Errorf("agentclient: encoding request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return fmt.Errorf("agentclient: %w", err)
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
		var apiErr wire.Error
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
		return fmt.Errorf("agentclient: decoding response: %w", err)
	}
	return nil
}

// Whoami identifies the calling agent.
func (c *Client) Whoami(ctx context.Context) (wire.Whoami, error) {
	var who wire.Whoami
	err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, &who)
	return who, err
}

// Agents lists the agents of the caller's project (the caller
// included), with Running flags and context occupancy.
func (c *Client) Agents(ctx context.Context) ([]wire.Agent, error) {
	var out []wire.Agent
	err := c.do(ctx, http.MethodGet, "/v1/agents", nil, &out)
	return out, err
}

// SendTurn enqueues a prompt for a sibling agent (by name), attributed
// to the caller (source_kind=agent, source_id=caller).
func (c *Client) SendTurn(ctx context.Context, agent, content string) (wire.Turn, error) {
	var turn wire.Turn
	err := c.do(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(agent)+"/turns",
		wire.SubmitTurnRequest{Content: content}, &turn)
	return turn, err
}

// ReportStatus stores the caller's status text; empty clears it.
func (c *Client) ReportStatus(ctx context.Context, status string) error {
	return c.do(ctx, http.MethodPost, "/v1/status", wire.ReportStatusRequest{Status: status}, nil)
}

// Spawn creates and starts a worker agent in the caller's project.
// Callable only when the caller's can_spawn is true (403 otherwise).
func (c *Client) Spawn(ctx context.Context, req wire.SpawnAgentRequest) (wire.Agent, error) {
	var agent wire.Agent
	err := c.do(ctx, http.MethodPost, "/v1/agents", req, &agent)
	return agent, err
}
