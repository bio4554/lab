package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/wire"
)

// loadFixtureEvents wraps the stream-json fixture lines as stored
// events of one session, seq in file order.
func loadFixtureEvents(t *testing.T, name string) []wire.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "internal", "streamjson", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	session := uuid.New()
	var events []wire.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	seq := int64(0)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		seq++
		var env struct {
			Type string `json:"type"`
		}
		kind := "unknown"
		if err := json.Unmarshal([]byte(line), &env); err == nil && env.Type != "" {
			kind = env.Type
		}
		events = append(events, wire.Event{
			ID: seq, SessionID: session, Seq: seq, Kind: kind,
			Payload: []byte(line), TS: time.Date(2026, 8, 12, 12, 4, 31, 0, time.UTC),
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestBuildTranscriptToolUseFixture(t *testing.T) {
	events := loadFixtureEvents(t, "tool_use.jsonl")
	entries := buildTranscript(events, "coder", nil)

	var kinds []entryKind
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	// init marker, tool_use (with outcome from the tool_result), agent
	// text, result — thinking-only assistant events and system noise
	// (thinking_tokens, rate_limit unknown kind → raw) interleave.
	var tool *TranscriptEntry
	var agent, result, marker, raw int
	for i := range entries {
		switch entries[i].Kind {
		case entryTool:
			tool = &entries[i]
		case entryAgent:
			agent++
		case entryResult:
			result++
		case entryMarker:
			marker++
		case entryRaw:
			raw++
		}
	}
	if marker != 1 {
		t.Errorf("markers = %d, want 1 (init); kinds %v", marker, kinds)
	}
	if tool == nil {
		t.Fatalf("no tool entry; kinds %v", kinds)
	}
	if tool.Tool != "Bash" || tool.ToolSummary != "echo hello" {
		t.Errorf("tool entry = %+v", *tool)
	}
	if tool.Outcome != "✓" {
		t.Errorf("tool outcome = %q, want ✓", tool.Outcome)
	}
	if agent != 1 {
		t.Errorf("agent text entries = %d, want 1", agent)
	}
	if result != 1 {
		t.Errorf("result entries = %d, want 1", result)
	}
	// rate_limit_event is an unknown kind → dim raw line.
	if raw < 1 {
		t.Errorf("raw entries = %d, want >= 1 (rate_limit_event)", raw)
	}
}

func TestBuildTranscriptUnknownKinds(t *testing.T) {
	events := loadFixtureEvents(t, "unknown_kind.jsonl")
	entries := buildTranscript(events, "a", nil)
	if len(entries) != len(events) {
		t.Fatalf("entries = %d, want %d (all raw)", len(entries), len(events))
	}
	for _, e := range entries {
		if e.Kind != entryRaw {
			t.Errorf("entry = %+v, want raw", e)
		}
		if !strings.HasPrefix(e.Text, "· ") {
			t.Errorf("raw line %q lacks · prefix", e.Text)
		}
	}
}

func TestBuildTranscriptSimpleFixture(t *testing.T) {
	events := loadFixtureEvents(t, "simple.jsonl")
	entries := buildTranscript(events, "coder", nil)
	var texts []string
	for _, e := range entries {
		if e.Kind == entryAgent {
			texts = append(texts, e.Text)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "orchestrator or implementation role") {
		t.Errorf("agent texts = %q", texts)
	}
	last := entries[len(entries)-1]
	if last.Kind != entryResult || last.IsError {
		t.Errorf("last entry = %+v, want success result", last)
	}
	if !strings.Contains(last.Text, "$0.154") {
		t.Errorf("result line %q lacks cost", last.Text)
	}
}

func TestBuildTranscriptSortsBySeq(t *testing.T) {
	events := loadFixtureEvents(t, "simple.jsonl")
	// Shuffle deterministically: reverse.
	rev := make([]wire.Event, len(events))
	for i, e := range events {
		rev[len(events)-1-i] = e
	}
	if got, want := buildTranscript(rev, "a", nil), buildTranscript(events, "a", nil); len(got) != len(want) || got[0] != want[0] {
		t.Errorf("reversed input produced a different transcript")
	}
}

func TestBuildTranscriptInjectsTurnPrompts(t *testing.T) {
	events := loadFixtureEvents(t, "tool_use.jsonl")
	// Stamp the substantive events (everything after init) with a turn.
	turnID := uuid.New()
	for i := range events {
		if i > 0 {
			events[i].TurnID = &turnID
		}
	}
	turns := map[uuid.UUID]TurnInfo{turnID: {Who: "you", Content: "echo hello please"}}
	entries := buildTranscript(events, "coder", turns)

	var userIdx = -1
	for i, e := range entries {
		if e.Kind == entryUser {
			if userIdx != -1 {
				t.Fatalf("prompt injected more than once: %+v", entries)
			}
			userIdx = i
		}
	}
	if userIdx == -1 {
		t.Fatalf("no user entry; entries %+v", entries)
	}
	e := entries[userIdx]
	if e.Who != "you" || e.Text != "echo hello please" || e.Time == "" {
		t.Errorf("user entry = %+v", e)
	}
	// The prompt precedes the tool call it caused.
	for i := range entries[:userIdx] {
		if entries[i].Kind == entryTool {
			t.Errorf("tool entry before the turn prompt: %+v", entries)
		}
	}

	// Unknown turn ids inject nothing (until the cache fills).
	if got := buildTranscript(events, "coder", nil); len(got) != len(entries)-1 {
		t.Errorf("nil turns: %d entries, want %d", len(got), len(entries)-1)
	}
}

func TestToolInputSummary(t *testing.T) {
	for in, want := range map[string]string{
		`{"command":"go test ./...","description":"Run tests"}`: "go test ./...",
		`{"file_path":"/a/b.go","old_string":"x"}`:              "/a/b.go",
		`{"zeta":"z","alpha":"a"}`:                              "a",
		`{"n":5}`:                                               `{"n":5}`,
		`{}`:                                                    "",
		`not json`:                                              "",
	} {
		if got := toolInputSummary([]byte(in)); got != want {
			t.Errorf("toolInputSummary(%s) = %q, want %q", in, got, want)
		}
	}
	long := `{"command":"` + strings.Repeat("x", 100) + `"}`
	if got := toolInputSummary([]byte(long)); len([]rune(got)) != 61 || !strings.HasSuffix(got, "…") {
		t.Errorf("long summary = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestFormatters(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 612: "612", 3100: "3.1k", 1000: "1k", 1_400_000: "1.4M",
	} {
		if got := fmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d) = %q, want %q", n, got, want)
		}
	}
	for ms, want := range map[int64]string{
		850: "850ms", 1800: "1.8s", 14200: "14.2s", 3000: "3s", 61000: "1m01s",
	} {
		if got := fmtDuration(ms); got != want {
			t.Errorf("fmtDuration(%d) = %q, want %q", ms, got, want)
		}
	}
	if got := fmtCost(0.0176); got != "$0.018" {
		t.Errorf("fmtCost = %q", got)
	}
	if got := fmtCost(31.8); got != "$31.80" {
		t.Errorf("fmtCost = %q", got)
	}
}

func TestTranscriptStateDedupe(t *testing.T) {
	ts := newTranscriptState()
	sess := uuid.New()
	ev := func(seq int64) wire.Event {
		return wire.Event{ID: seq, SessionID: sess, Seq: seq, Kind: "assistant", Payload: []byte(`{}`)}
	}
	if !ts.add(ev(2), ev(3)) {
		t.Fatal("first add reported no change")
	}
	// Backfill overlaps the stream: 1..3 arrive again plus 4.
	if !ts.add(ev(1), ev(2), ev(3), ev(4)) {
		t.Fatal("second add reported no change")
	}
	if ts.add(ev(2)) {
		t.Fatal("pure duplicate reported a change")
	}
	if len(ts.events) != 4 {
		t.Fatalf("events = %d, want 4", len(ts.events))
	}
	for i, e := range ts.events {
		if e.Seq != int64(i+1) {
			t.Fatalf("events out of order: %+v", ts.events)
		}
	}
}
