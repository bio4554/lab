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
	ctx    context.Context // bounds the SSE stream; cancelled on exit
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

	// Prompts behind turn_ids seen in the stream, for transcript
	// display (the CLI does not echo prompts as events).
	turnCache map[uuid.UUID]wire.Turn
	turnFetch map[uuid.UUID]bool // fetches in flight

	form    *form
	lastErr string // transient action error (footer)
}

func newModel(ctx context.Context, client *labclient.Client, addr string) model {
	ta := textarea.New()
	ta.Placeholder = "message — enter to send · ctrl+j newline"
	ta.SetHeight(1)
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = sAccent.Render("> ")
	return model{
		ctx:         ctx,
		client:      client,
		addr:        addr,
		agents:      map[uuid.UUID][]wire.Agent{},
		collapsed:   map[uuid.UUID]bool{},
		transcripts: map[uuid.UUID]*transcriptState{},
		inFlight:    map[uuid.UUID]wire.Turn{},
		turnCache:   map[uuid.UUID]wire.Turn{},
		turnFetch:   map[uuid.UUID]bool{},
		composer:    ta,
		follow:      true,
		vp:          viewport.New(0, 0),
	}
}

func (m model) Init() tea.Cmd {
	// The stream starts live-only (-1): scrollback comes from
	// per-session backfill, and the subscriber's Last-Event-ID resume
	// keeps the live feed gapless across daemon restarts. It rides the
	// program's context, so exiting the TUI ends the stream goroutine.
	stream := m.client.StreamEvents(m.ctx, -1)
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
		return m, tea.Batch(append(m.missingTurnCmds(), waitEvCmd(m.stream))...)

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
		return m, tea.Batch(m.missingTurnCmds()...)

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
		m.turnCache[msg.turn.ID] = msg.turn
		return m, nil

	case turnFetchedMsg:
		delete(m.turnFetch, msg.id)
		if msg.err != nil {
			// Quietly retried: the id stays uncached, so the next
			// stream/backfill activity re-requests it.
			return m, nil
		}
		m.turnCache[msg.id] = msg.turn
		m.refreshTranscript()
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

// missingTurnCmds requests any turn ids present in the viewed
// session's events but not yet cached or being fetched.
func (m *model) missingTurnCmds() []tea.Cmd {
	ts := m.transcripts[m.viewSession]
	if ts == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, e := range ts.events {
		if e.TurnID == nil {
			continue
		}
		id := *e.TurnID
		if _, cached := m.turnCache[id]; cached || m.turnFetch[id] {
			continue
		}
		m.turnFetch[id] = true
		cmds = append(cmds, getTurnCmd(m.client, id))
	}
	return cmds
}

// turnInfos labels cached turns for transcript injection: "you" for
// user turns, the source agent's name (any project) for agent turns.
func (m *model) turnInfos() map[uuid.UUID]TurnInfo {
	infos := make(map[uuid.UUID]TurnInfo, len(m.turnCache))
	for id, turn := range m.turnCache {
		who := "you"
		if turn.SourceKind == "agent" {
			who = "agent"
			if turn.SourceID != nil {
				for _, list := range m.agents {
					if a := findAgent(list, *turn.SourceID); a != nil {
						who = a.Name
						break
					}
				}
			}
		}
		infos[id] = TurnInfo{Who: who, Content: turn.Content}
	}
	return infos
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
