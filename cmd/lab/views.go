package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const treeWidth = 28

// layout re-derives component sizes from the window and current pane
// contents.
func (m *model) layout() {
	mainW := m.mainWidth()
	m.vp.Width = mainW
	m.vp.Height = m.transcriptHeight()
	m.composer.SetWidth(mainW - 4)
	m.refreshTranscript()
}

func (m *model) mainWidth() int {
	return max(20, m.width-treeWidth-1)
}

func (m *model) bodyHeight() int {
	return max(4, m.height-2) // header + footer
}

// transcriptHeight: body minus tab row minus composer box (content +
// border) minus the session marker row.
func (m *model) transcriptHeight() int {
	h := m.bodyHeight() - 1
	if !m.histSession {
		h -= m.composer.Height() + 2
	} else {
		h -= 1 // read-only badge line
	}
	return max(1, h)
}

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	header := m.headerView()
	footer := m.footerView()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.treeView(), m.mainView())
	body = lipgloss.NewStyle().Height(m.bodyHeight()).MaxHeight(m.bodyHeight()).Render(body)

	// Modal dialogs and the connection-lost box take over the body
	// area, centered (the frame around them stays).
	switch {
	case m.form != nil:
		body = lipgloss.Place(m.width, m.bodyHeight(), lipgloss.Center, lipgloss.Center, m.form.render())
	case !m.connOK && m.loaded:
		body = lipgloss.Place(m.width, m.bodyHeight(), lipgloss.Center, lipgloss.Center, m.connLostView())
	case !m.connOK && !m.loaded:
		body = lipgloss.Place(m.width, m.bodyHeight(), lipgloss.Center, lipgloss.Center, m.connLostView())
	}
	return header + "\n" + body + "\n" + footer
}

// ── chrome ───────────────────────────────────────────────────────────

func (m model) headerView() string {
	left := sTitle.Render("lab") + "  "
	var state string
	if m.connOK {
		state = sAccent.Render("●") + sMuted.Render(
			fmt.Sprintf(" labd %s · connected · drivers %d", m.status.Version, len(m.status.Running)))
		if !m.status.DBHealthy {
			state += sBad.Render(" · db unhealthy")
		}
		if !m.streamSt.Connected && m.streamSt.Attempt > 0 {
			state += sDim.Render(" · stream reconnecting")
		}
	} else {
		state = sDim.Render("◌ labd unreachable · reconnecting")
	}
	right := sDim.Render(m.addr)
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(state)-lipgloss.Width(right))
	return left + state + strings.Repeat(" ", gap) + right
}

func (m model) footerView() string {
	var hints []string
	key := func(k, label string) string { return sKey.Render(k) + sDim.Render(" "+label) }
	switch {
	case m.form != nil:
		hints = []string{key("tab", "next field"), key("enter", "submit"), key("esc", "cancel")}
	case !m.connOK:
		hints = []string{key("r", "retry now"), key("q", "quit")}
	case m.focus == focusComposer:
		hints = []string{key("enter", "send"), key("ctrl+j", "newline"), key("tab", "panes"), key("esc", "blur")}
	case m.focus == focusTree:
		hints = []string{key("↑↓", "select"), key("enter", "open"), key("n", "new agent"), key("p", "new project"),
			key("s", "start"), key("x", "stop"), key("r", "retire"), key("tab", "panes")}
	case m.curAgent != nil && m.tab == tabSessions:
		hints = []string{key("↑↓", "select"), key("enter", "open"), key("1", "transcript"), key("3", "usage"), key("tab", "panes")}
	case m.curAgent != nil && m.tab == tabUsage:
		hints = []string{key("R", "refresh"), key("1", "transcript"), key("2", "sessions"), key("tab", "panes")}
	case m.curAgent != nil:
		hints = []string{key("↑↓", "scroll"), key("G", "bottom"), key("1/2/3", "tabs"), key("esc", "back"), key("tab", "panes")}
	default:
		hints = []string{key("tab", "panes"), key("p", "new project")}
	}
	line := strings.Join(hints, sFaint.Render(" · "))
	right := key("q", "quit")
	if m.lastErr != "" {
		right = sBad.Render(truncate(m.lastErr, m.width/2)) + "  " + right
	}
	gap := max(1, m.width-lipgloss.Width(line)-lipgloss.Width(right))
	return line + strings.Repeat(" ", gap) + right
}

func (m model) connLostView() string {
	addr := m.addr
	msg := sLabel.Render("✕ CONNECTION LOST") + "\n" +
		sText.Render("labd at "+addr+" is not responding.") + "\n"
	if m.connErr != nil {
		msg += sDim.Render(truncate(m.connErr.Error(), 56)) + "\n"
	}
	if m.streamSt.LastEventID > 0 {
		msg += sDim.Render(fmt.Sprintf("last event id %d — the stream resumes without gaps once the daemon returns.", m.streamSt.LastEventID)) + "\n"
	}
	msg += sFaint.Render("retrying automatically · ") + sKey.Render("r") + sFaint.Render(" retry now · ") + sKey.Render("q") + sFaint.Render(" quit")
	return sModal.Render(msg)
}

// ── left tree ────────────────────────────────────────────────────────

func (m model) treeView() string {
	var lines []string
	lines = append(lines, sLabel.Render("PROJECTS"))
	for i, row := range m.tree {
		var text string
		switch row.Kind {
		case rowProject:
			arrow := "▾"
			if row.Collapsed {
				arrow = "▸"
			}
			text = fmt.Sprintf("%s %s %s", arrow, row.Project.Name, sDim.Render(fmt.Sprint(row.Count)))
		case rowAgent:
			a := row.Agent
			glyph := agentGlyph(a)
			if a.Running {
				glyph = sAccent.Render(glyph)
			} else {
				glyph = sDim.Render(glyph)
			}
			text = "  " + glyph + " " + a.Name + " " + sDim.Render(a.State)
		}
		style := lipgloss.NewStyle().Width(treeWidth - 2).MaxWidth(treeWidth - 2)
		if i == m.treeSel && m.focus == focusTree {
			text = sSelRow.Render("▎") + text
		} else {
			text = " " + text
		}
		lines = append(lines, style.Render(text))
	}
	if len(m.tree) == 0 {
		if m.loaded {
			lines = append(lines, sDim.Render(" no projects — p creates"))
		} else {
			lines = append(lines, sDim.Render(" loading…"))
		}
	}
	pane := strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Width(treeWidth).
		Height(m.bodyHeight()).
		MaxHeight(m.bodyHeight()).
		Padding(0, 1).
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(cHair).
		Render(pane)
}

// ── main pane ────────────────────────────────────────────────────────

func (m model) mainView() string {
	w := m.mainWidth()
	var content string
	if m.curAgent == nil {
		content = m.projectsTable(w)
	} else {
		content = m.tabRow(w) + "\n" + m.tabContent(w)
	}
	return lipgloss.NewStyle().
		Width(w).MaxWidth(w).
		Height(m.bodyHeight()).MaxHeight(m.bodyHeight()).
		Padding(0, 1).
		Render(content)
}

func (m model) tabRow(w int) string {
	names := []string{"transcript", "sessions", "usage"}
	var tabs []string
	for i, n := range names {
		if mainTab(i) == m.tab {
			tabs = append(tabs, sTabOn.Render(n))
		} else {
			tabs = append(tabs, sTabOff.Render(n))
		}
	}
	left := strings.Join(tabs, " ")
	right := m.contextLine()
	gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return left + strings.Repeat(" ", gap) + right
}

// contextLine is the right side of the tab row: agent · session · turn
// state.
func (m model) contextLine() string {
	a := m.curAgent
	parts := []string{a.Name}
	if m.viewSession != [16]byte{} {
		parts = append(parts, shortID(m.viewSession))
	}
	if turn, ok := m.inFlight[a.ID]; ok {
		label := "turn " + shortID(turn.ID) + " · " + turn.Status
		switch turn.Status {
		case "done":
			return sDim.Render(strings.Join(parts, " · ")+" · ") + sGood.Render(label)
		case "running":
			return sDim.Render(strings.Join(parts, " · ")+" · ") + sAccent.Render("▶ "+label)
		default:
			return sDim.Render(strings.Join(parts, " · ") + " · " + label)
		}
	}
	return sDim.Render(strings.Join(parts, " · "))
}

func (m model) tabContent(w int) string {
	switch m.tab {
	case tabSessions:
		return m.sessionsTable(w)
	case tabUsage:
		return m.usageTable(w)
	default:
		return m.transcriptView(w)
	}
}

// ── projects table (main pane when no agent is open) ─────────────────

func (m model) projectsTable(w int) string {
	agentTotal := 0
	for _, list := range m.agents {
		agentTotal += len(list)
	}
	head := sTabOn.Render("projects")
	right := sDim.Render(fmt.Sprintf("%d projects · %d agents", len(m.projects), agentTotal))
	gap := max(1, w-lipgloss.Width(head)-lipgloss.Width(right)-2)
	rows := []string{head + strings.Repeat(" ", gap) + right, ""}

	cols := fmt.Sprintf("%%-18s %%-*s %%-8s %%-7s %%s")
	originW := max(10, w-18-8-7-16)
	rows = append(rows, sLabel.Render(fmt.Sprintf(cols, "NAME", originW, "ORIGIN", "STACK", "AGENTS", "CREATED")))
	rows = append(rows, hairline(w-2))
	for _, p := range m.projects {
		line := fmt.Sprintf(cols,
			truncate(p.Name, 17), originW, truncate(p.Origin, originW-1),
			p.Stack, fmt.Sprint(len(m.agents[p.ID])), p.CreatedAt.Local().Format("2006-01-02"))
		sel := m.treeSel < len(m.tree) && m.tree[m.treeSel].Kind == rowProject && m.tree[m.treeSel].Project.ID == p.ID
		if sel {
			rows = append(rows, sSelRow.Render(truncate(line, w-2)))
		} else {
			rows = append(rows, sText.Render(truncate(line, w-2)))
		}
	}
	if len(m.projects) == 0 && m.loaded {
		rows = append(rows, sDim.Render("no projects yet — press p to create one"))
	}
	return strings.Join(rows, "\n")
}

// ── transcript ───────────────────────────────────────────────────────

// transcriptContent renders the viewport body (used by refreshTranscript).
func (m *model) transcriptContent() string {
	if m.curAgent == nil {
		return ""
	}
	if m.viewSession == [16]byte{} {
		return sDim.Render("no session yet — press s to start the agent")
	}
	ts := m.transcripts[m.viewSession]
	if ts == nil || (len(ts.events) == 0 && !ts.backfilled) {
		return sDim.Render("loading transcript…")
	}
	if len(ts.events) == 0 {
		return sDim.Render("no events in this session yet")
	}
	entries := buildTranscript(ts.events, m.curAgent.Name)
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = renderEntry(e, m.curAgent.Name, m.vp.Width-2)
	}
	return strings.Join(lines, "\n")
}

func (m model) transcriptView(w int) string {
	body := m.vp.View()
	if m.histSession {
		badge := sLabel.Render(strings.ToUpper(shortID(m.viewSession))+" · BACKFILL") + " " +
			lipgloss.NewStyle().Background(cBadgeBG).Foreground(cMuted).Padding(0, 1).Render("read-only") +
			sFaint.Render("  esc returns to live")
		return badge + "\n" + body
	}
	return body + "\n" + m.composerView(w)
}

func (m model) composerView(w int) string {
	box := sBlurBox
	if m.focus == focusComposer {
		box = sFocusBox
	}
	return box.Width(w - 2).Render(m.composer.View())
}

// ── sessions table ───────────────────────────────────────────────────

func (m model) sessionsTable(w int) string {
	rows := []string{sLabel.Render(fmt.Sprintf("%-10s %-20s %-18s %-8s %s", "SESSION", "SPAN", "END REASON", "EVENTS", "PREV")), hairline(w - 2)}
	now := time.Now()
	for i, s := range m.sessions {
		line := fmt.Sprintf("%-10s %-20s %-18s %-8d %s",
			shortID(s.ID), sessionSpan(s, now), truncate(sessionEndReason(s), 18), s.EventCount, shortIDPtr(s.PrevSessionID))
		switch {
		case i == m.sessSel && m.focus == focusMain:
			rows = append(rows, sSelRow.Render(truncate(line, w-2)))
		case s.EndedAt == nil:
			rows = append(rows, sText.Render(truncate(line, w-2)))
		default:
			rows = append(rows, sMuted.Render(truncate(line, w-2)))
		}
	}
	if len(m.sessions) == 0 {
		rows = append(rows, sDim.Render("no sessions"))
	}
	return strings.Join(rows, "\n")
}

// ── usage table ──────────────────────────────────────────────────────

func (m model) usageTable(w int) string {
	colW := max(16, (w-14)/3)
	format := func(name, hour, day, total string) string {
		return fmt.Sprintf("%-12s %-*s %-*s %s", truncate(name, 12), colW, hour, colW, day, total)
	}
	rows := []string{
		sLabel.Render(format("AGENT", "LAST HOUR", "TODAY", "TOTAL (turns · tok · cost)")),
		hairline(w - 2),
	}
	for _, u := range m.usage {
		rows = append(rows, sText.Render(truncate(format(u.AgentName, usageCell(u.LastHour), usageCell(u.Today), usageTotalCell(u.Total)), w-2)))
	}
	if len(m.usage) == 0 {
		rows = append(rows, sDim.Render("no usage recorded"))
	} else {
		sum := sumUsage(m.usage)
		rows = append(rows, hairline(w-2))
		rows = append(rows, sAccent.Render(truncate(format("Σ project", usageCell(sum.LastHour), usageCell(sum.Today), usageTotalCell(sum.Total)), w-2)))
	}
	return strings.Join(rows, "\n")
}
