package streamjson

import (
	"bytes"
	"strings"
	"testing"
)

func TestUserMessageGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).UserMessage("run the tests"); err != nil {
		t.Fatal(err)
	}
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}` + "\n"
	if buf.String() != want {
		t.Errorf("got  %q\nwant %q", buf.String(), want)
	}
}

func TestUserMessageEscaping(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.UserMessage("line1\nline2 \"quoted\""); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("output must be exactly one newline-terminated line: %q", out)
	}
	// It must round-trip through the decoder as a user event with the
	// original text.
	dec := NewDecoder(strings.NewReader(out))
	ev, err := dec.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindUser {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if got := ev.Text(); got != "line1\nline2 \"quoted\"" {
		t.Errorf("Text() = %q", got)
	}
}

func TestUserMessageMultipleLines(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, msg := range []string{"first", "second"} {
		if err := enc.UserMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("wrote %d lines, want 2", got)
	}
}
