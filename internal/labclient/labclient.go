// Package labclient is the typed HTTP client for labd's client API
// (the localhost TUI API, internal/labd/api.ClientServer). It speaks
// the JSON types in internal/wire and adds an SSE subscriber with
// resume and reconnect (see stream.go).
package labclient

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

	"github.com/bio4554/lab/internal/wire"
)

// Client talks to one labd client API.
type Client struct {
	// BaseURL is the API root, e.g. "http://127.0.0.1:7710".
	BaseURL string
	// HTTPClient defaults to http.DefaultClient. Streaming requests
	// ignore its Timeout (they are bounded by context instead).
	HTTPClient *http.Client
}

// New returns a client for addr, which may be a host:port or a full
// http:// URL.
func New(addr string) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Client{BaseURL: strings.TrimRight(addr, "/")}
}

// APIError is a non-2xx response, carrying the wire.Error message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("labd: %s (HTTP %d)", e.Message, e.Status)
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
			return fmt.Errorf("labclient: encoding request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return fmt.Errorf("labclient: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var werr wire.Error
		msg := resp.Status
		if raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); err == nil {
			if json.Unmarshal(raw, &werr) == nil && werr.Error != "" {
				msg = werr.Error
			}
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("labclient: decoding response: %w", err)
	}
	return nil
}

// pathEscape keeps user-supplied names safe in URL paths.
func pathEscape(s string) string { return url.PathEscape(s) }

// ── daemon ───────────────────────────────────────────────────────────

func (c *Client) Status(ctx context.Context) (wire.DaemonStatus, error) {
	var out wire.DaemonStatus
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

// ── projects ─────────────────────────────────────────────────────────

func (c *Client) ListProjects(ctx context.Context) ([]wire.Project, error) {
	var out []wire.Project
	err := c.do(ctx, http.MethodGet, "/v1/projects", nil, &out)
	return out, err
}

func (c *Client) GetProject(ctx context.Context, name string) (wire.Project, error) {
	var out wire.Project
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+pathEscape(name), nil, &out)
	return out, err
}

func (c *Client) CreateProject(ctx context.Context, req wire.CreateProjectRequest) (wire.Project, error) {
	var out wire.Project
	err := c.do(ctx, http.MethodPost, "/v1/projects", req, &out)
	return out, err
}

func (c *Client) DeleteProject(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+pathEscape(name), nil, nil)
}

// ── agents ───────────────────────────────────────────────────────────

func agentPath(project, agent string) string {
	return "/v1/projects/" + pathEscape(project) + "/agents/" + pathEscape(agent)
}

func (c *Client) ListAgents(ctx context.Context, project string) ([]wire.Agent, error) {
	var out []wire.Agent
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+pathEscape(project)+"/agents", nil, &out)
	return out, err
}

func (c *Client) CreateAgent(ctx context.Context, project string, req wire.CreateAgentRequest) (wire.Agent, error) {
	var out wire.Agent
	err := c.do(ctx, http.MethodPost, "/v1/projects/"+pathEscape(project)+"/agents", req, &out)
	return out, err
}

func (c *Client) StartAgent(ctx context.Context, project, agent string) error {
	return c.do(ctx, http.MethodPost, agentPath(project, agent)+"/start", nil, nil)
}

func (c *Client) StopAgent(ctx context.Context, project, agent string) error {
	return c.do(ctx, http.MethodPost, agentPath(project, agent)+"/stop", nil, nil)
}

func (c *Client) RetireAgent(ctx context.Context, project, agent string, req wire.RetireAgentRequest) (wire.RetireAgentResponse, error) {
	var out wire.RetireAgentResponse
	err := c.do(ctx, http.MethodPost, agentPath(project, agent)+"/retire", req, &out)
	return out, err
}

// ── turns ────────────────────────────────────────────────────────────

func (c *Client) SubmitTurn(ctx context.Context, project, agent, content string) (wire.Turn, error) {
	var out wire.Turn
	err := c.do(ctx, http.MethodPost, agentPath(project, agent)+"/turns",
		wire.SubmitTurnRequest{Content: content}, &out)
	return out, err
}

func (c *Client) GetTurn(ctx context.Context, id uuid.UUID) (wire.Turn, error) {
	var out wire.Turn
	err := c.do(ctx, http.MethodGet, "/v1/turns/"+id.String(), nil, &out)
	return out, err
}

// ── sessions & usage ─────────────────────────────────────────────────

func (c *Client) ListSessions(ctx context.Context, project, agent string) ([]wire.Session, error) {
	var out []wire.Session
	err := c.do(ctx, http.MethodGet, agentPath(project, agent)+"/sessions", nil, &out)
	return out, err
}

func (c *Client) ProjectUsage(ctx context.Context, project string) ([]wire.AgentUsage, error) {
	var out []wire.AgentUsage
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+pathEscape(project)+"/usage", nil, &out)
	return out, err
}

// ── event backfill ───────────────────────────────────────────────────

// SessionEvents returns up to limit events of a session with seq >
// afterSeq (limit <= 0 means the server default).
func (c *Client) SessionEvents(ctx context.Context, session uuid.UUID, afterSeq int64, limit int) ([]wire.Event, error) {
	q := url.Values{}
	if afterSeq > 0 {
		q.Set("after_seq", strconv.FormatInt(afterSeq, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sessions/" + session.String() + "/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []wire.Event
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// AllSessionEvents pages through SessionEvents until the session's
// whole log after afterSeq is fetched.
func (c *Client) AllSessionEvents(ctx context.Context, session uuid.UUID, afterSeq int64) ([]wire.Event, error) {
	var all []wire.Event
	for {
		page, err := c.SessionEvents(ctx, session, afterSeq, 1000)
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if len(page) < 1000 {
			return all, nil
		}
		afterSeq = page[len(page)-1].Seq
	}
}

// Events returns up to limit global events with id > afterID.
func (c *Client) Events(ctx context.Context, afterID int64, limit int) ([]wire.Event, error) {
	q := url.Values{}
	if afterID > 0 {
		q.Set("after_id", strconv.FormatInt(afterID, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []wire.Event
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
