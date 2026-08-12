package streamjson

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodeFile decodes every line of a fixture, failing the test on any
// decode error.
func decodeFile(t *testing.T, name string) []Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := NewDecoder(f)
	var events []Event
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("%s: event %d: %v", name, len(events), err)
		}
		events = append(events, ev)
	}
}

var fixtures = []string{"simple.jsonl", "tool_use.jsonl", "unknown_kind.jsonl"}

func TestFixturesDecode(t *testing.T) {
	for _, name := range fixtures {
		events := decodeFile(t, name)
		if len(events) == 0 {
			t.Fatalf("%s: no events", name)
		}
		for i, ev := range events {
			if ev.Kind == "" {
				t.Errorf("%s: event %d has empty Kind", name, i)
			}
			if ev.SessionID() == "" {
				t.Errorf("%s: event %d (%s) has empty session ID", name, i, ev.Kind)
			}
		}
	}
}

// TestLossless re-encodes each fixture from Event.Raw and requires the
// result to be byte-identical to the file.
func TestLossless(t *testing.T) {
	for _, name := range fixtures {
		orig, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(orig) == 0 || orig[len(orig)-1] != '\n' {
			t.Fatalf("%s: fixture must end with a newline", name)
		}
		var buf bytes.Buffer
		enc := NewEncoder(&buf)
		for _, ev := range decodeFile(t, name) {
			if err := enc.RawLine(ev.Raw); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(orig, buf.Bytes()) {
			t.Errorf("%s: decode → re-encode is not byte-identical", name)
		}
	}
}

func TestSimpleFixture(t *testing.T) {
	events := decodeFile(t, "simple.jsonl")

	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	want := []string{"system", "assistant", "assistant", "rate_limit_event", "result"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}

	const sessionID = "02bf18c8-77ad-46f8-ad27-be2d4733ddf4"

	init, err := events[0].Init()
	if err != nil {
		t.Fatal(err)
	}
	if init.SessionID != sessionID {
		t.Errorf("init.SessionID = %q", init.SessionID)
	}
	if init.Model != "claude-fable-5" {
		t.Errorf("init.Model = %q", init.Model)
	}
	if len(init.Tools) == 0 {
		t.Error("init.Tools is empty")
	}

	// First assistant event carries only a thinking block; second the text.
	if got := events[1].Text(); got != "" {
		t.Errorf("thinking-only event Text() = %q, want empty", got)
	}
	if got := events[2].Text(); !strings.HasPrefix(got, "Hi!") {
		t.Errorf("assistant Text() = %q, want prefix \"Hi!\"", got)
	}
	msg, err := events[2].Message()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != "assistant" || msg.Model != "claude-fable-5" {
		t.Errorf("message role/model = %q/%q", msg.Role, msg.Model)
	}

	// The rate_limit_event kind is unknown to the package but still
	// carries the envelope.
	if events[3].Subtype != "" || events[3].SessionID() != sessionID {
		t.Errorf("rate_limit_event envelope: subtype=%q session=%q",
			events[3].Subtype, events[3].SessionID())
	}

	res, err := events[4].Result()
	if err != nil {
		t.Fatal(err)
	}
	if res.Subtype != SubtypeSuccess || res.IsError {
		t.Errorf("result subtype/is_error = %q/%v", res.Subtype, res.IsError)
	}
	if res.DurationMS != 3170 || res.NumTurns != 1 {
		t.Errorf("result duration/turns = %d/%d", res.DurationMS, res.NumTurns)
	}
	if res.TotalCostUSD != 0.154465 {
		t.Errorf("result cost = %v", res.TotalCostUSD)
	}
	if res.SessionID != sessionID {
		t.Errorf("result session = %q", res.SessionID)
	}
	wantUsage := Usage{
		InputTokens:              2,
		OutputTokens:             63,
		CacheCreationInputTokens: 6766,
		CacheReadInputTokens:     15975,
	}
	if res.Usage != wantUsage {
		t.Errorf("result usage = %+v, want %+v", res.Usage, wantUsage)
	}
}

func TestToolUseFixture(t *testing.T) {
	events := decodeFile(t, "tool_use.jsonl")

	var uses []ToolUse
	for _, ev := range events {
		uses = append(uses, ev.ToolUses()...)
	}
	if len(uses) != 1 {
		t.Fatalf("found %d tool uses, want 1", len(uses))
	}
	if uses[0].Name != "Bash" || uses[0].ID == "" {
		t.Errorf("tool use = %+v", uses[0])
	}
	if !strings.Contains(string(uses[0].Input), "echo hello") {
		t.Errorf("tool input = %s", uses[0].Input)
	}

	// The tool_result comes back as a user event whose block references
	// the tool_use ID.
	var results int
	for _, ev := range events {
		if ev.Kind != KindUser {
			continue
		}
		msg, err := ev.Message()
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range msg.Content {
			if b.Type == "tool_result" {
				results++
				if b.ToolUseID != uses[0].ID {
					t.Errorf("tool_result.ToolUseID = %q, want %q", b.ToolUseID, uses[0].ID)
				}
				if !strings.Contains(string(b.Content), "hello") {
					t.Errorf("tool_result content = %s", b.Content)
				}
			}
		}
	}
	if results != 1 {
		t.Fatalf("found %d tool_result blocks, want 1", results)
	}

	// system/thinking_tokens is a known kind with an unknown subtype;
	// it must decode but refuse the Init accessor.
	var sawThinking bool
	for _, ev := range events {
		if ev.Kind == KindSystem && ev.Subtype == "thinking_tokens" {
			sawThinking = true
			if _, err := ev.Init(); err == nil {
				t.Error("Init() on system/thinking_tokens should error")
			}
		}
	}
	if !sawThinking {
		t.Error("no system/thinking_tokens event in fixture")
	}
}

func TestUnknownKindFixture(t *testing.T) {
	events := decodeFile(t, "unknown_kind.jsonl")
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5", len(events))
	}
	wantKinds := []string{"stream_event", "control_response", "compaction", "permission_request", "future_thing"}
	for i, ev := range events {
		if ev.Kind != wantKinds[i] {
			t.Errorf("event %d Kind = %q, want %q", i, ev.Kind, wantKinds[i])
		}
	}
	if events[2].Subtype != "auto" {
		t.Errorf("compaction Subtype = %q", events[2].Subtype)
	}
	// Typed accessors refuse unknown kinds instead of guessing.
	if _, err := events[0].Message(); err == nil {
		t.Error("Message() on unknown kind should error")
	}
	if _, err := events[0].Result(); err == nil {
		t.Error("Result() on unknown kind should error")
	}
	if events[0].Text() != "" || events[0].ToolUses() != nil {
		t.Error("Text/ToolUses on unknown kind should be zero-valued")
	}
}

func TestMalformedLine(t *testing.T) {
	input := "this is not json\n{\"type\":\"result\",\"subtype\":\"success\"}\n"
	dec := NewDecoder(strings.NewReader(input))

	_, err := dec.Next()
	var mal *MalformedLineError
	if !errors.As(err, &mal) {
		t.Fatalf("err = %v, want *MalformedLineError", err)
	}
	if string(mal.Line) != "this is not json" {
		t.Errorf("mal.Line = %q", mal.Line)
	}

	// The decoder has advanced past the bad line; the caller can go on.
	ev, err := dec.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindResult || ev.Subtype != SubtypeSuccess {
		t.Errorf("event after malformed line = %s/%s", ev.Kind, ev.Subtype)
	}
	if _, err := dec.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestBlankLinesSkipped(t *testing.T) {
	input := "\n{\"type\":\"system\",\"subtype\":\"init\"}\n\n   \n{\"type\":\"result\"}\n\n"
	dec := NewDecoder(strings.NewReader(input))
	var kinds []string
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, ev.Kind)
	}
	if strings.Join(kinds, ",") != "system,result" {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestOversizedLine(t *testing.T) {
	huge := `{"type":"user","pad":"` + strings.Repeat("a", MaxLineBytes+1) + `"}` + "\n"
	dec := NewDecoder(strings.NewReader(huge))
	_, err := dec.Next()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

// A line just under the limit must decode fine — tool results can be
// megabytes.
func TestLargeLineWithinLimit(t *testing.T) {
	big := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"` +
		strings.Repeat("a", 8<<20) + `"}]}}` + "\n"
	dec := NewDecoder(strings.NewReader(big))
	ev, err := dec.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindUser {
		t.Errorf("Kind = %q", ev.Kind)
	}
	msg, err := ev.Message()
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_result" {
		t.Errorf("content = %+v", msg.Content)
	}
}
