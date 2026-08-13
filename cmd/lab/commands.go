package main

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labclient"
	"github.com/bio4554/lab/internal/wire"
)

// Messages: every API call runs in a tea.Cmd and reports back as one
// of these.

type refreshMsg struct {
	status   wire.DaemonStatus
	projects []wire.Project
	agents   map[uuid.UUID][]wire.Agent
	err      error
}

type tickMsg time.Time

type streamEvMsg struct {
	ev wire.Event
	ok bool // false: channel closed
}

type streamStMsg struct {
	st labclient.StreamStatus
	ok bool
}

type backfillMsg struct {
	session uuid.UUID
	events  []wire.Event
	err     error
}

type sessionsMsg struct {
	agentID  uuid.UUID
	sessions []wire.Session
	err      error
}

type usageMsg struct {
	projectID uuid.UUID
	usage     []wire.AgentUsage
	err       error
}

// actionMsg is the outcome of a mutating call (start/stop/retire/
// create/delete). A nil err triggers an immediate refresh.
type actionMsg struct {
	action string
	err    error
}

type turnQueuedMsg struct {
	turn wire.Turn
	err  error
}

const requestTimeout = 5 * time.Second

func apiCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

// refreshCmd fetches daemon status, projects, and every project's
// agents in one round.
func refreshCmd(c *labclient.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := apiCtx()
		defer cancel()
		status, err := c.Status(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		projects, err := c.ListProjects(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		agents := make(map[uuid.UUID][]wire.Agent, len(projects))
		for _, p := range projects {
			list, err := c.ListAgents(ctx, p.Name)
			if err != nil {
				return refreshMsg{err: err}
			}
			agents[p.ID] = list
		}
		return refreshMsg{status: status, projects: projects, agents: agents}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// waitEvCmd/waitStCmd bridge the SSE stream's channels into the tea
// loop; each received message re-issues the wait.
func waitEvCmd(s *labclient.Stream) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.Events
		return streamEvMsg{ev: ev, ok: ok}
	}
}

func waitStCmd(s *labclient.Stream) tea.Cmd {
	return func() tea.Msg {
		st, ok := <-s.Status
		return streamStMsg{st: st, ok: ok}
	}
}

func backfillCmd(c *labclient.Client, session uuid.UUID) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		events, err := c.AllSessionEvents(ctx, session, 0)
		return backfillMsg{session: session, events: events, err: err}
	}
}

func sessionsCmd(c *labclient.Client, project string, agentID uuid.UUID, agent string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := apiCtx()
		defer cancel()
		sessions, err := c.ListSessions(ctx, project, agent)
		return sessionsMsg{agentID: agentID, sessions: sessions, err: err}
	}
}

func usageCmd(c *labclient.Client, projectID uuid.UUID, project string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := apiCtx()
		defer cancel()
		usage, err := c.ProjectUsage(ctx, project)
		return usageMsg{projectID: projectID, usage: usage, err: err}
	}
}

func actionCmd(action string, f func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := apiCtx()
		defer cancel()
		return actionMsg{action: action, err: f(ctx)}
	}
}

func submitTurnCmd(c *labclient.Client, project, agent, content string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := apiCtx()
		defer cancel()
		turn, err := c.SubmitTurn(ctx, project, agent, content)
		return turnQueuedMsg{turn: turn, err: err}
	}
}
