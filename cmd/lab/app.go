package main

import (
	"context"
	"sort"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labclient"
	"github.com/bio4554/lab/internal/wire"
)

type mainTab int

const (
	tabTranscript mainTab = iota
	tabSessions
	tabUsage
)

type focusArea int

const (
	focusTree focusArea = iota
	focusMain
	focusComposer
)

// transcriptState accumulates one session's events, merged from
// backfill and the live stream, deduplicated by seq.
type transcriptState struct {
	events     []wire.Event
	seen       map[int64]struct{}
	backfilled bool
}

func newTranscriptState() *transcriptState {
	return &transcriptState{seen: map[int64]struct{}{}}
}

// add inserts events not seen yet; returns true when anything landed.
func (t *transcriptState) add(events ...wire.Event) bool {
	changed := false
	for _, e := range events {
		if _, dup := t.seen[e.Seq]; dup {
			continue
		}
		t.seen[e.Seq] = struct{}{}
		t.events = append(t.events, e)
		changed = true
	}
	if changed {
		sort.Slice(t.events, func(i, j int) bool { return t.events[i].Seq < t.events[j].Seq })
	}
	return changed
}

type model struct {
	client *labclient.Client
	addr   string

	width, height int

	// Daemon connectivity.
	status   wire.DaemonStatus
	connOK   bool
	connErr  error
	streamSt labclient.StreamStatus
	stream   *labclient.Stream

	// Data.
	projects  []wire.Project
	agents    map[uuid.UUID][]wire.Agent
	collapsed map[uuid.UUID]bool
	loaded    bool

	// Tree.
	tree    []TreeRow
	treeSel int

	// Opened agent (nil: projects table in the main pane).
	curAgent   *wire.Agent
	curProject wire.Project

	// Main pane.
	tab         mainTab
	focus       focusArea
	transcripts map[uuid.UUID]*transcriptState
	viewSession uuid.UUID // session shown in the transcript tab
	histSession bool      // read-only historical view (from sessions tab)
	vp          viewport.Model
	follow      bool // pinned to the bottom

	sessions []wire.Session
	sessSel  int
	usage    []wire.AgentUsage

	composer textarea.Model
	inFlight map[uuid.UUID]wire.Turn // per agent: last turn submitted here

	form    *form
	lastErr string // transient action error (footer)
}

func newModel(client *labclient.Client, addr string) model {
	ta := textarea.New()
	ta.Placeholder = "message — enter to send · ctrl+j newline"
	ta.SetHeight(1)
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = sAccent.Render("> ")
	return model{
		client:      client,
		addr:        addr,
		agents:      map[uuid.UUID][]wire.Agent{},
		collapsed:   map[uuid.UUID]bool{},
		transcripts: map[uuid.UUID]*transcriptState{},
		inFlight:    map[uuid.UUID]wire.Turn{},
		composer:    ta,
		follow:      true,
		vp:          viewport.New(0, 0),
	}
}

func (m model) Init() tea.Cmd {
	// The stream starts live-only (-1): scrollback comes from
	// per-session backfill, and the subscriber's Last-Event-ID resume
	// keeps the live feed gapless across daemon restarts.
	stream := m.client.StreamEvents(context.Background(), -1)
	return tea.Batch(
		func() tea.Msg { return streamInitMsg{stream} },
		refreshCmd(m.client),
		tickCmd(),
	)
}

type streamInitMsg struct{ stream *labclient.Stream }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case streamInitMsg:
		m.stream = msg.stream
		return m, tea.Batch(waitEvCmd(m.stream), waitStCmd(m.stream))

	case tickMsg:
		return m, tea.Batch(refreshCmd(m.client), tickCmd())

	case refreshMsg:
		return m.onRefresh(msg)

	case streamEvMsg:
		if !msg.ok {
			return m, nil // program is shutting down
		}
		m.onEvent(msg.ev)
		return m, waitEvCmd(m.stream)

	case streamStMsg:
		if !msg.ok {
			return m, nil
		}
		m.streamSt = msg.st
		return m, waitStCmd(m.stream)

	case backfillMsg:
		if msg.err != nil {
			m.lastErr = msg.err.Error()
			return m, nil
		}
		ts, ok := m.transcripts[msg.session]
		if !ok {
			ts = newTranscriptState()
			m.transcripts[msg.session] = ts
		}
		ts.add(msg.events...)
		ts.backfilled = true
		m.refreshTranscript()
		return m, nil

	case sessionsMsg:
		if msg.err != nil {
			m.lastErr = msg.err.Error()
			return m, nil
		}
		if m.curAgent != nil && m.curAgent.ID == msg.agentID {
			m.sessions = msg.sessions
			if m.sessSel >= len(m.sessions) {
				m.sessSel = max(0, len(m.sessions)-1)
			}
		}
		return m, nil

	case usageMsg:
		if msg.err != nil {
			m.lastErr = msg.err.Error()
			return m, nil
		}
		if m.curAgent != nil && m.curProject.ID == msg.projectID {
			m.usage = msg.usage
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.lastErr = msg.action + ": " + msg.err.Error()
			return m, nil
		}
		m.lastErr = ""
		return m, refreshCmd(m.client)

	case turnQueuedMsg:
		if msg.err != nil {
			m.lastErr = "send: " + msg.err.Error()
			return m, nil
		}
		m.inFlight[msg.turn.AgentID] = msg.turn
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

// onRefresh merges a poll round and keeps the selection stable.
func (m model) onRefresh(msg refreshMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.connOK, m.connErr = false, msg.err
		return m, nil
	}
	m.connOK, m.connErr, m.loaded = true, nil, true
	m.status = msg.status
	m.projects = msg.projects
	m.agents = msg.agents
	m.rebuildTree()

	var cmds []tea.Cmd
	if m.curAgent != nil {
		// Track the opened agent across refreshes; follow its current
		// session unless a historical one is pinned.
		if fresh := findAgent(m.agents[m.curProject.ID], m.curAgent.ID); fresh != nil {
			m.curAgent = fresh
			if !m.histSession && fresh.SessionID != nil && *fresh.SessionID != m.viewSession {
				cmds = append(cmds, m.openSession(*fresh.SessionID, false)...)
			}
		}
	}
	return m, tea.Batch(cmds...)
}

func findAgent(list []wire.Agent, id uuid.UUID) *wire.Agent {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// onEvent routes one live event into any tracked transcript and the
// in-flight turn indicator.
func (m *model) onEvent(ev wire.Event) {
	if turn, ok := m.inFlight[ev.AgentID]; ok && ev.TurnID != nil && *ev.TurnID == turn.ID {
		switch {
		case ev.Kind == "result":
			turn.Status = "done"
		case turn.Status == "queued":
			turn.Status = "running"
		}
		m.inFlight[ev.AgentID] = turn
	}

	// A new session for the opened agent (e.g. after retire): follow it.
	if m.curAgent != nil && ev.AgentID == m.curAgent.ID && !m.histSession && ev.SessionID != m.viewSession {
		if _, tracked := m.transcripts[ev.SessionID]; !tracked {
			m.viewSession = ev.SessionID
			m.transcripts[ev.SessionID] = newTranscriptState()
			// Backfill of the new session is triggered by the next
			// refresh cycle; live events accumulate meanwhile.
		}
	}

	if ts, ok := m.transcripts[ev.SessionID]; ok {
		if ts.add(ev) && ev.SessionID == m.viewSession {
			m.refreshTranscript()
		}
	}
}

// openSession points the transcript tab at a session and starts its
// backfill when needed. hist marks a read-only historical view.
func (m *model) openSession(session uuid.UUID, hist bool) []tea.Cmd {
	m.viewSession = session
	m.histSession = hist
	m.follow = !hist
	var cmds []tea.Cmd
	ts, ok := m.transcripts[session]
	if !ok {
		ts = newTranscriptState()
		m.transcripts[session] = ts
	}
	if !ts.backfilled {
		cmds = append(cmds, backfillCmd(m.client, session))
	}
	m.refreshTranscript()
	return cmds
}

// openAgent switches the main pane to an agent's transcript tab.
func (m *model) openAgent(p wire.Project, a wire.Agent) []tea.Cmd {
	agent := a
	m.curAgent = &agent
	m.curProject = p
	m.tab = tabTranscript
	m.focus = focusComposer
	m.composer.Focus()
	m.sessions, m.usage = nil, nil
	m.sessSel = 0
	if a.SessionID != nil {
		return m.openSession(*a.SessionID, false)
	}
	m.viewSession = uuid.Nil
	m.histSession = false
	m.refreshTranscript()
	return nil
}

func (m *model) rebuildTree() {
	m.tree = buildTree(m.projects, m.agents, m.collapsed)
	if m.treeSel >= len(m.tree) {
		m.treeSel = max(0, len(m.tree)-1)
	}
}

// refreshTranscript re-renders the viewport content from the view
// session's accumulated events.
func (m *model) refreshTranscript() {
	m.vp.SetContent(m.transcriptContent())
	if m.follow {
		m.vp.GotoBottom()
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
