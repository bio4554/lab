package main

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bio4554/lab/internal/wire"
)

// onKey is the single keyboard dispatcher. Precedence: modal form →
// composer → global/pane keys.
func (m model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form != nil {
		return m.onFormKey(msg)
	}

	// ctrl+c always quits; q quits unless typing in the composer.
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}

	if m.focus == focusComposer && m.curAgent != nil && !m.histSession {
		return m.onComposerKey(msg)
	}

	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "r":
		if !m.connOK {
			return m, refreshCmd(m.client) // retry now
		}
	case "tab":
		m.cycleFocus()
		return m, nil
	}

	if m.focus == focusTree {
		return m.onTreeKey(msg)
	}
	return m.onMainKey(msg)
}

func (m *model) cycleFocus() {
	switch m.focus {
	case focusTree:
		m.focus = focusMain
	case focusMain:
		if m.curAgent != nil && m.tab == tabTranscript && !m.histSession {
			m.focus = focusComposer
			m.composer.Focus()
		} else {
			m.focus = focusTree
		}
	case focusComposer:
		m.composer.Blur()
		m.focus = focusTree
	}
}

// ── tree ─────────────────────────────────────────────────────────────

func (m model) onTreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sel := func() *TreeRow {
		if m.treeSel < len(m.tree) {
			return &m.tree[m.treeSel]
		}
		return nil
	}
	switch msg.String() {
	case "up", "k":
		if m.treeSel > 0 {
			m.treeSel--
		}
	case "down", "j":
		if m.treeSel < len(m.tree)-1 {
			m.treeSel++
		}
	case "enter":
		row := sel()
		if row == nil {
			break
		}
		if row.Kind == rowProject {
			m.collapsed[row.Project.ID] = !m.collapsed[row.Project.ID]
			m.rebuildTree()
			break
		}
		return m, tea.Batch(m.openAgent(row.Project, row.Agent)...)
	case "p":
		if m.connOK {
			m.form = newProjectForm(m.status.Stacks)
		}
	case "n":
		if row := sel(); row != nil && m.connOK {
			m.form = newAgentForm(row.Project.Name, m.status.Stacks)
		}
	case "s", "x":
		if row := sel(); row != nil && row.Kind == rowAgent {
			project, agent := row.Project.Name, row.Agent.Name
			if msg.String() == "s" {
				return m, actionCmd("start "+agent, func(ctx context.Context) error {
					return m.client.StartAgent(ctx, project, agent)
				})
			}
			return m, actionCmd("stop "+agent, func(ctx context.Context) error {
				return m.client.StopAgent(ctx, project, agent)
			})
		}
	case "r":
		if row := sel(); row != nil && row.Kind == rowAgent {
			m.form = newRetireForm(row.Project.Name, row.Agent.Name)
		}
	}
	return m, nil
}

// ── main pane ────────────────────────────────────────────────────────

func (m model) onMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.curAgent == nil {
		// Projects table: navigation happens in the tree; p creates.
		if msg.String() == "p" && m.connOK {
			m.form = newProjectForm(m.status.Stacks)
		}
		return m, nil
	}

	switch msg.String() {
	case "1", "t":
		m.tab = tabTranscript
		if m.histSession && m.curAgent.SessionID != nil {
			return m, tea.Batch(m.openSession(*m.curAgent.SessionID, false)...)
		}
		return m, nil
	case "2":
		m.tab = tabSessions
		return m, sessionsCmd(m.client, m.curProject.Name, m.curAgent.ID, m.curAgent.Name)
	case "3", "u":
		m.tab = tabUsage
		return m, usageCmd(m.client, m.curProject.ID, m.curProject.Name)
	case "esc":
		if m.histSession {
			// Back to the live transcript.
			m.tab = tabTranscript
			if m.curAgent.SessionID != nil {
				return m, tea.Batch(m.openSession(*m.curAgent.SessionID, false)...)
			}
			m.histSession = false
			return m, nil
		}
		m.curAgent = nil
		m.focus = focusTree
		return m, nil
	}

	switch m.tab {
	case tabTranscript:
		switch msg.String() {
		case "up", "k":
			m.follow = false
			m.vp.LineUp(1)
		case "down", "j":
			m.vp.LineDown(1)
			m.follow = m.vp.AtBottom()
		case "pgup", "b":
			m.follow = false
			m.vp.ViewUp()
		case "pgdown", "f":
			m.vp.ViewDown()
			m.follow = m.vp.AtBottom()
		case "G", "end":
			m.follow = true
			m.vp.GotoBottom()
		}
	case tabSessions:
		switch msg.String() {
		case "up", "k":
			if m.sessSel > 0 {
				m.sessSel--
			}
		case "down", "j":
			if m.sessSel < len(m.sessions)-1 {
				m.sessSel++
			}
		case "enter":
			if m.sessSel < len(m.sessions) {
				s := m.sessions[m.sessSel]
				hist := m.curAgent.SessionID == nil || s.ID != *m.curAgent.SessionID
				m.tab = tabTranscript
				return m, tea.Batch(m.openSession(s.ID, hist)...)
			}
		}
	case tabUsage:
		if msg.String() == "R" {
			return m, usageCmd(m.client, m.curProject.ID, m.curProject.Name)
		}
	}
	return m, nil
}

// ── composer ─────────────────────────────────────────────────────────

func (m model) onComposerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.composer.Blur()
		m.focus = focusMain
		return m, nil
	case tea.KeyTab:
		m.cycleFocus()
		return m, nil
	case tea.KeyEnter:
		content := strings.TrimSpace(m.composer.Value())
		if content == "" || !m.connOK {
			return m, nil
		}
		m.composer.Reset()
		return m, submitTurnCmd(m.client, m.curProject.Name, m.curAgent.Name, content)
	case tea.KeyCtrlJ:
		m.composer.InsertString("\n")
		m.syncComposerHeight()
		return m, nil
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.syncComposerHeight()
	return m, cmd
}

// syncComposerHeight grows the composer up to 5 lines with its content.
func (m *model) syncComposerHeight() {
	lines := m.composer.LineCount()
	if lines < 1 {
		lines = 1
	}
	if lines > 5 {
		lines = 5
	}
	if m.composer.Height() != lines {
		m.composer.SetHeight(lines)
		m.layout()
	}
}

// ── modal form ───────────────────────────────────────────────────────

func (m model) onFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	f := m.form
	done, submit, cmd := f.update(msg)
	if !done {
		return m, cmd
	}
	m.form = nil
	if !submit {
		return m, nil
	}
	switch f.kind {
	case formProject:
		req := wire.CreateProjectRequest{
			Name:   f.fields[0].value(),
			Origin: f.fields[1].value(),
			Stack:  f.fields[2].value(),
		}
		return m, actionCmd("create project "+req.Name, func(ctx context.Context) error {
			_, err := m.client.CreateProject(ctx, req)
			return err
		})
	case formAgent:
		req := wire.CreateAgentRequest{
			Name:       f.fields[0].value(),
			RolePrompt: f.fields[1].value(),
			Model:      f.fields[2].value(),
		}
		if kind := f.fields[3].value(); kind != "none" {
			req.CredentialKind = kind
		}
		project := f.project
		return m, actionCmd("create agent "+req.Name, func(ctx context.Context) error {
			_, err := m.client.CreateAgent(ctx, project, req)
			return err
		})
	case formRetire:
		project, agent := f.project, f.agent
		req := wire.RetireAgentRequest{Reason: f.fields[0].value(), Seed: f.fields[1].value()}
		return m, actionCmd("retire "+agent, func(ctx context.Context) error {
			_, err := m.client.RetireAgent(ctx, project, agent, req)
			return err
		})
	}
	return m, nil
}
