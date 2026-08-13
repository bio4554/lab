package main

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/wire"
)

// View-model builders: pure functions from wire data to the row
// structures the views render, unit-tested without a terminal.

type treeRowKind int

const (
	rowProject treeRowKind = iota
	rowAgent
)

// TreeRow is one selectable line of the left project/agent tree.
type TreeRow struct {
	Kind      treeRowKind
	Project   wire.Project
	Agent     wire.Agent // Kind == rowAgent only
	Collapsed bool       // Kind == rowProject only
	Count     int        // Kind == rowProject: agent count
}

// buildTree flattens projects and their agents into tree rows.
// Collapsed projects contribute only their header row.
func buildTree(projects []wire.Project, agents map[uuid.UUID][]wire.Agent, collapsed map[uuid.UUID]bool) []TreeRow {
	var rows []TreeRow
	for _, p := range projects {
		list := agents[p.ID]
		rows = append(rows, TreeRow{Kind: rowProject, Project: p, Collapsed: collapsed[p.ID], Count: len(list)})
		if collapsed[p.ID] {
			continue
		}
		for _, a := range list {
			rows = append(rows, TreeRow{Kind: rowAgent, Project: p, Agent: a})
		}
	}
	return rows
}

// agentGlyph is the run-state marker: ● hosted driver, ○ stopped/idle
// without a driver, ◌ retired.
func agentGlyph(a wire.Agent) string {
	switch {
	case a.State == "retired":
		return "◌"
	case a.Running:
		return "●"
	default:
		return "○"
	}
}

// shortID renders a UUID as its first 6 hex digits plus ellipsis
// (mockup adaptation: `019ff8…`).
func shortID(id uuid.UUID) string {
	return id.String()[:6] + "…"
}

func shortIDPtr(id *uuid.UUID) string {
	if id == nil {
		return "—"
	}
	return shortID(*id)
}

// sessionSpan renders "11:52 → now" / "09:14 → 11:41", with dates when
// the session started on another day.
func sessionSpan(s wire.Session, now time.Time) string {
	format := "15:04"
	if s.StartedAt.Local().Format("2006-01-02") != now.Local().Format("2006-01-02") {
		format = "Jan 2 15:04"
	}
	start := s.StartedAt.Local().Format(format)
	if s.EndedAt == nil {
		return start + " → now"
	}
	return start + " → " + s.EndedAt.Local().Format("15:04")
}

// sessionEndReason is the END REASON cell: "▶ running" while open.
func sessionEndReason(s wire.Session) string {
	if s.EndedAt == nil {
		return "▶ running"
	}
	if s.EndReason == nil || *s.EndReason == "" {
		return "—"
	}
	return *s.EndReason
}

// usageCell renders one window aggregate: "18.2k / 3.1k · $0.41", or
// "—" when the window saw nothing.
func usageCell(u wire.Usage) string {
	if u.TokensIn == 0 && u.TokensOut == 0 && u.Turns == 0 && u.CostUSD == 0 {
		return "—"
	}
	return fmt.Sprintf("%s / %s · %s", fmtTokens(u.TokensIn), fmtTokens(u.TokensOut), fmtCost(u.CostUSD))
}

// usageTotalCell is the TOTAL cell, which also carries the turn count.
func usageTotalCell(u wire.Usage) string {
	if u.TokensIn == 0 && u.TokensOut == 0 && u.Turns == 0 && u.CostUSD == 0 {
		return "—"
	}
	return fmt.Sprintf("%d · %s", u.Turns, usageCell(u))
}

// sumUsage folds per-agent aggregates into the Σ project row.
func sumUsage(rows []wire.AgentUsage) wire.AgentUsage {
	var sum wire.AgentUsage
	add := func(dst *wire.Usage, src wire.Usage) {
		dst.TokensIn += src.TokensIn
		dst.TokensOut += src.TokensOut
		dst.CostUSD += src.CostUSD
		dst.Turns += src.Turns
	}
	for _, r := range rows {
		add(&sum.LastHour, r.LastHour)
		add(&sum.Today, r.Today)
		add(&sum.Total, r.Total)
	}
	return sum
}
