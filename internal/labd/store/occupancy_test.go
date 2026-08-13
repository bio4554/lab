package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// TestSessionContextTokens: occupancy is the latest result event's
// input + cache-creation + cache-read sum, scoped to one session —
// older results, non-result events and other sessions are ignored, and
// a session without a result reads 0.
func TestSessionContextTokens(t *testing.T) {
	s := testStore(t)
	f := createFixture(t, s)
	ctx := context.Background()

	sess1, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionContextTokens(ctx, sess1.ID); err != nil || got != 0 {
		t.Fatalf("fresh session occupancy = %d, %v; want 0", got, err)
	}

	appendEvent := func(sessID uuid.UUID, kind, payload string) {
		t.Helper()
		if _, err := s.AppendEvent(ctx, sessID, f.Agent.ID, nil, kind, json.RawMessage(payload)); err != nil {
			t.Fatal(err)
		}
	}

	// Non-result events never count.
	appendEvent(sess1.ID, "assistant", `{"type":"assistant","message":{"usage":{"input_tokens":9999}}}`)
	if got, _ := s.SessionContextTokens(ctx, sess1.ID); got != 0 {
		t.Fatalf("occupancy after assistant event = %d, want 0", got)
	}

	appendEvent(sess1.ID, "result",
		`{"type":"result","usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`)
	if got, _ := s.SessionContextTokens(ctx, sess1.ID); got != 115 {
		t.Fatalf("occupancy after first result = %d, want 115", got)
	}

	// The latest result governs, not the max or the sum.
	appendEvent(sess1.ID, "result",
		`{"type":"result","usage":{"input_tokens":200,"cache_creation_input_tokens":20,"cache_read_input_tokens":1000}}`)
	if got, _ := s.SessionContextTokens(ctx, sess1.ID); got != 1220 {
		t.Fatalf("occupancy after second result = %d, want 1220", got)
	}

	// A missing usage object reads 0 (fields are COALESCEd).
	appendEvent(sess1.ID, "result", `{"type":"result","subtype":"error_during_execution"}`)
	if got, _ := s.SessionContextTokens(ctx, sess1.ID); got != 0 {
		t.Fatalf("occupancy after usage-less result = %d, want 0", got)
	}

	// A successor session starts at 0; the predecessor keeps its value.
	if err := s.EndSession(ctx, sess1.ID, "test retirement"); err != nil {
		t.Fatal(err)
	}
	sess2, err := s.CreateSession(ctx, f.Agent.ID, &sess1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SessionContextTokens(ctx, sess2.ID); got != 0 {
		t.Fatalf("successor occupancy = %d, want 0", got)
	}
	if _, err := s.AppendEvent(ctx, sess2.ID, f.Agent.ID, nil, "result",
		json.RawMessage(`{"type":"result","usage":{"input_tokens":50}}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SessionContextTokens(ctx, sess2.ID); got != 50 {
		t.Fatalf("successor occupancy after result = %d, want 50", got)
	}
}
