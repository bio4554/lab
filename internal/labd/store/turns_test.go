package store

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestTurnQueueFIFO(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	for _, content := range []string{"first", "second", "third"} {
		if _, err := s.EnqueueTurn(ctx, NewTurn{
			AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: content,
		}); err != nil {
			t.Fatalf("EnqueueTurn(%s): %v", content, err)
		}
	}

	for _, want := range []string{"first", "second", "third"} {
		turn, err := s.NextQueuedTurn(ctx, f.Agent.ID)
		if err != nil {
			t.Fatalf("NextQueuedTurn: %v", err)
		}
		if turn == nil || turn.Content != want {
			t.Fatalf("NextQueuedTurn = %+v, want content %q", turn, want)
		}
		if turn.Status != TurnStatusRunning {
			t.Fatalf("status = %q, want running", turn.Status)
		}
		if err := s.FinishTurn(ctx, turn.ID, TurnStatusDone, ""); err != nil {
			t.Fatalf("FinishTurn: %v", err)
		}
	}

	if turn, err := s.NextQueuedTurn(ctx, f.Agent.ID); err != nil || turn != nil {
		t.Fatalf("NextQueuedTurn(empty) = %+v, %v; want nil, nil", turn, err)
	}
}

// TestTurnQueueSerialPerAgent checks the core guarantee: while a turn
// is running, NextQueuedTurn hands out nothing else for that agent.
func TestTurnQueueSerialPerAgent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	a, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "b"}); err != nil {
		t.Fatal(err)
	}

	first, err := s.NextQueuedTurn(ctx, f.Agent.ID)
	if err != nil || first == nil || first.ID != a.ID {
		t.Fatalf("NextQueuedTurn = %+v, %v; want turn a", first, err)
	}
	if blocked, err := s.NextQueuedTurn(ctx, f.Agent.ID); err != nil || blocked != nil {
		t.Fatalf("NextQueuedTurn(while running) = %+v, %v; want nil, nil", blocked, err)
	}

	errMsg := "boom"
	if err := s.FinishTurn(ctx, first.ID, TurnStatusError, errMsg); err != nil {
		t.Fatalf("FinishTurn(error): %v", err)
	}
	got, err := s.GetTurn(ctx, first.ID)
	if err != nil || got.Status != TurnStatusError || got.Error == nil || *got.Error != errMsg || got.FinishedAt == nil {
		t.Fatalf("finished turn = %+v, %v", got, err)
	}

	second, err := s.NextQueuedTurn(ctx, f.Agent.ID)
	if err != nil || second == nil || second.Content != "b" {
		t.Fatalf("NextQueuedTurn after finish = %+v, %v; want turn b", second, err)
	}
}

// TestTurnQueueConcurrent races NextQueuedTurn from many goroutines;
// exactly one may win while the rest see either the running turn block
// or an empty queue. Run with -race.
func TestTurnQueueConcurrent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	for i := 0; i < 5; i++ {
		if _, err := s.EnqueueTurn(ctx, NewTurn{
			AgentID: f.Agent.ID, SourceKind: SourceKindAgent, Content: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 10
	var wg sync.WaitGroup
	wins := make(chan uuid.UUID, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn, err := s.NextQueuedTurn(ctx, f.Agent.ID)
			if err != nil {
				t.Errorf("NextQueuedTurn: %v", err)
				return
			}
			if turn != nil {
				wins <- turn.ID
			}
		}()
	}
	wg.Wait()
	close(wins)

	var got []uuid.UUID
	for id := range wins {
		got = append(got, id)
	}
	if len(got) != 1 {
		t.Fatalf("%d turns went running concurrently, want exactly 1", len(got))
	}

	var running int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM lab.turns WHERE agent_id = $1 AND status = 'running'",
		f.Agent.ID).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 1 {
		t.Fatalf("running turns in db = %d, want 1", running)
	}

	if err := s.FinishTurn(ctx, got[0], TurnStatusDone, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFinishTurnValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	turn, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(ctx, turn.ID, TurnStatusQueued, ""); err == nil {
		t.Error("FinishTurn(queued): want validation error")
	}
	// Not running yet: FinishTurn must refuse.
	if err := s.FinishTurn(ctx, turn.ID, TurnStatusDone, ""); err != ErrNotFound {
		t.Errorf("FinishTurn(not running) = %v, want ErrNotFound", err)
	}
}

func TestRunningTurnAndSetTurnSession(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	if got, err := s.RunningTurn(ctx, f.Agent.ID); err != nil || got != nil {
		t.Fatalf("RunningTurn(none) = %+v, %v; want nil, nil", got, err)
	}

	turn, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Queued turns are not running.
	if got, err := s.RunningTurn(ctx, f.Agent.ID); err != nil || got != nil {
		t.Fatalf("RunningTurn(queued only) = %+v, %v; want nil, nil", got, err)
	}
	if _, err := s.NextQueuedTurn(ctx, f.Agent.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunningTurn(ctx, f.Agent.ID)
	if err != nil || got == nil || got.ID != turn.ID || got.Status != TurnStatusRunning {
		t.Fatalf("RunningTurn = %+v, %v; want turn %s running", got, err, turn.ID)
	}

	sess, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnSession(ctx, turn.ID, sess.ID); err != nil {
		t.Fatalf("SetTurnSession: %v", err)
	}
	stamped, err := s.GetTurn(ctx, turn.ID)
	if err != nil || stamped.SessionID == nil || *stamped.SessionID != sess.ID {
		t.Fatalf("turn after SetTurnSession = %+v, %v; want session %s", stamped, err, sess.ID)
	}
	if err := s.SetTurnSession(ctx, uuid.New(), sess.ID); err != ErrNotFound {
		t.Fatalf("SetTurnSession(missing) = %v, want ErrNotFound", err)
	}

	if err := s.FinishTurn(ctx, turn.ID, TurnStatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RunningTurn(ctx, f.Agent.ID); err != nil || got != nil {
		t.Fatalf("RunningTurn(after finish) = %+v, %v; want nil, nil", got, err)
	}
}
