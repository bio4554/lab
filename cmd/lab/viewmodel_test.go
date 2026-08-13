package main

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/wire"
)

func TestBuildTree(t *testing.T) {
	p1 := wire.Project{ID: uuid.New(), Name: "omni"}
	p2 := wire.Project{ID: uuid.New(), Name: "docs"}
	agents := map[uuid.UUID][]wire.Agent{
		p1.ID: {{Name: "planner"}, {Name: "coder"}},
		p2.ID: {{Name: "scribe"}},
	}

	rows := buildTree([]wire.Project{p1, p2}, agents, map[uuid.UUID]bool{})
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if rows[0].Kind != rowProject || rows[0].Count != 2 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].Kind != rowAgent || rows[1].Agent.Name != "planner" || rows[1].Project.ID != p1.ID {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if rows[3].Kind != rowProject || rows[3].Project.Name != "docs" {
		t.Errorf("row 3 = %+v", rows[3])
	}

	// Collapsing a project hides its agents but keeps the count.
	rows = buildTree([]wire.Project{p1, p2}, agents, map[uuid.UUID]bool{p1.ID: true})
	if len(rows) != 3 || !rows[0].Collapsed || rows[0].Count != 2 || rows[1].Kind != rowProject {
		t.Errorf("collapsed rows = %+v", rows)
	}
}

func TestAgentGlyph(t *testing.T) {
	if g := agentGlyph(wire.Agent{State: "retired", Running: false}); g != "◌" {
		t.Errorf("retired glyph = %q", g)
	}
	if g := agentGlyph(wire.Agent{State: "working", Running: true}); g != "●" {
		t.Errorf("running glyph = %q", g)
	}
	if g := agentGlyph(wire.Agent{State: "stopped"}); g != "○" {
		t.Errorf("stopped glyph = %q", g)
	}
}

func TestSessionRows(t *testing.T) {
	now := time.Date(2026, 8, 12, 13, 0, 0, 0, time.Local)
	start := time.Date(2026, 8, 12, 11, 52, 0, 0, time.Local)
	end := time.Date(2026, 8, 12, 12, 41, 0, 0, time.Local)
	reason := "retired: context full"

	open := wire.Session{ID: uuid.New(), StartedAt: start}
	if got := sessionSpan(open, now); got != "11:52 → now" {
		t.Errorf("open span = %q", got)
	}
	if got := sessionEndReason(open); got != "▶ running" {
		t.Errorf("open reason = %q", got)
	}

	done := wire.Session{ID: uuid.New(), StartedAt: start, EndedAt: &end, EndReason: &reason}
	if got := sessionSpan(done, now); got != "11:52 → 12:41" {
		t.Errorf("done span = %q", got)
	}
	if got := sessionEndReason(done); got != reason {
		t.Errorf("done reason = %q", got)
	}

	// A session from another day carries its date.
	old := wire.Session{StartedAt: start.AddDate(0, 0, -3), EndedAt: &end}
	if got := sessionSpan(old, now); got != "Aug 9 11:52 → 12:41" {
		t.Errorf("old span = %q", got)
	}
}

func TestShortID(t *testing.T) {
	id := uuid.MustParse("019ff8b3-5e25-7c19-8ce8-000000000000")
	if got := shortID(id); got != "019ff8…" {
		t.Errorf("shortID = %q", got)
	}
	if got := shortIDPtr(nil); got != "—" {
		t.Errorf("shortIDPtr(nil) = %q", got)
	}
}

func TestUsageCells(t *testing.T) {
	u := wire.Usage{TokensIn: 18200, TokensOut: 3100, CostUSD: 0.41, Turns: 12}
	if got := usageCell(u); got != "18.2k / 3.1k · $0.410" {
		t.Errorf("usageCell = %q", got)
	}
	if got := usageCell(wire.Usage{}); got != "—" {
		t.Errorf("empty usageCell = %q", got)
	}
	if got := usageTotalCell(u); got != "12 · 18.2k / 3.1k · $0.410" {
		t.Errorf("usageTotalCell = %q", got)
	}
}

func TestSumUsage(t *testing.T) {
	rows := []wire.AgentUsage{
		{LastHour: wire.Usage{TokensIn: 1, CostUSD: 0.5}, Total: wire.Usage{Turns: 2}},
		{LastHour: wire.Usage{TokensIn: 2, CostUSD: 0.25}, Total: wire.Usage{Turns: 3}},
	}
	sum := sumUsage(rows)
	if sum.LastHour.TokensIn != 3 || sum.LastHour.CostUSD != 0.75 || sum.Total.Turns != 5 {
		t.Errorf("sum = %+v", sum)
	}
}
