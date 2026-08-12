package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAppendEventAndTail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	sess, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}

	ev1, err := s.AppendEvent(ctx, sess.ID, f.Agent.ID, &turn.ID, "system", json.RawMessage(`{"subtype":"init"}`))
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev1.Seq != 1 || ev1.TurnID == nil || *ev1.TurnID != turn.ID {
		t.Errorf("first event = %+v", ev1)
	}
	ev2, err := s.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "assistant", nil)
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev2.Seq != 2 || string(ev2.Payload) != "{}" {
		t.Errorf("second event = %+v payload=%s", ev2, ev2.Payload)
	}

	events, err := s.EventsSince(ctx, sess.ID, 0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("EventsSince(0) = %d, %v; want 2", len(events), err)
	}
	if events[0].Seq != 1 || events[1].Seq != 2 || events[0].Kind != "system" {
		t.Errorf("EventsSince order: %+v", events)
	}
	tail, err := s.EventsSince(ctx, sess.ID, 1, 10)
	if err != nil || len(tail) != 1 || tail[0].Seq != 2 {
		t.Fatalf("EventsSince(1) = %+v, %v", tail, err)
	}

	byID, err := s.EventsSinceID(ctx, ev1.ID-1, 1000)
	if err != nil {
		t.Fatalf("EventsSinceID: %v", err)
	}
	var seen int
	for _, ev := range byID {
		if ev.SessionID == sess.ID {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("EventsSinceID saw %d session events, want 2", seen)
	}
}

// TestAppendEventConcurrentSeq is the core event-log guarantee: N
// concurrent appenders to one session produce exactly N events with
// gapless seq 1..N. Run with -race.
func TestAppendEventConcurrentSeq(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	sess, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := json.RawMessage(fmt.Sprintf(`{"writer":%d}`, i))
			if _, err := s.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "assistant", payload); err != nil {
				t.Errorf("AppendEvent(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	events, err := s.EventsSince(ctx, sess.ID, 0, n+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != n {
		t.Fatalf("got %d events, want %d", len(events), n)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("events[%d].Seq = %d, want %d (gap or duplicate)", i, ev.Seq, i+1)
		}
	}
}

// TestAppendEventNotifies checks LISTEN lab_events receives a
// notification carrying the appended event's ids.
func TestAppendEventNotifies(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	sess, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	ev, err := s.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "result", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		notice, err := listener.Conn().WaitForNotification(deadline)
		if err != nil {
			t.Fatalf("no notification received: %v", err)
		}
		var got struct {
			EventID   int64  `json:"event_id"`
			AgentID   string `json:"agent_id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(notice.Payload), &got); err != nil {
			t.Fatalf("bad payload %q: %v", notice.Payload, err)
		}
		// Other tests may notify concurrently; wait for ours.
		if got.SessionID != sess.ID.String() {
			continue
		}
		if got.EventID != ev.ID || got.AgentID != f.Agent.ID.String() {
			t.Fatalf("payload = %q, want event %d agent %s", notice.Payload, ev.ID, f.Agent.ID)
		}
		return
	}
}
