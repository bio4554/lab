package claude

import (
	"context"
	"testing"
	"time"

	"github.com/bio4554/lab/internal/labd/store"
)

// TestResumeCrashLoopBreaker: a --resume process that exits instantly
// on every replacement clears the recorded claude_session_id after
// resumeFailLimit consecutive failures, so the next cycle starts
// fresh instead of wedging forever.
func TestResumeCrashLoopBreaker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetClaudeSessionID(ctx, sess.ID, "dead-session-id"); err != nil {
		t.Fatal(err)
	}

	d := New(Options{
		Store:          h.st,
		Logger:         testDriver(h.st).log,
		RestartBackoff: time.Millisecond,
	})

	// The fake runner records each cycle's resume state and dies
	// "instantly" (errProcessExited with no elapsed time). Once a cycle
	// arrives without a claude_session_id — the breaker fired — it
	// stops the driver.
	type cycle struct{ resuming bool }
	var cycles []cycle
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	d.runProc = func(ctx context.Context, project store.Project, agent store.Agent, sess store.Session, pending *store.Turn) (*store.Turn, error) {
		resuming := sess.ClaudeSessionID != nil && *sess.ClaudeSessionID != ""
		cycles = append(cycles, cycle{resuming: resuming})
		if !resuming || len(cycles) > 10 {
			stopRun()
			return pending, ctx.Err()
		}
		return pending, errProcessExited
	}

	if err := d.Run(runCtx, h.agent.ID); err != nil && runCtx.Err() == nil {
		t.Fatalf("Run: %v", err)
	}

	if len(cycles) != resumeFailLimit+1 {
		t.Fatalf("got %d cycles %+v, want %d resume failures then one fresh start",
			len(cycles), cycles, resumeFailLimit)
	}
	for i := 0; i < resumeFailLimit; i++ {
		if !cycles[i].resuming {
			t.Errorf("cycle %d: resuming = false, want true", i)
		}
	}
	if cycles[resumeFailLimit].resuming {
		t.Error("cycle after the breaker fired still passed --resume; claude_session_id was not cleared")
	}

	// The clear is persisted, not just in-memory.
	got, err := h.st.CurrentSession(ctx, h.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != sess.ID {
		t.Fatalf("current session = %+v, want %s", got, sess.ID)
	}
	if got.ClaudeSessionID != nil {
		t.Errorf("claude_session_id = %q, want cleared", *got.ClaudeSessionID)
	}
}
