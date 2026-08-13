package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/store"
)

// TestPumpContextThresholdRetires: a result whose context occupancy
// reaches retire_context_tokens retires the session between turns —
// exactly once — chains a successor via prev_session_id, enqueues the
// kbase-recall seed addressed to it, and the successor's occupancy
// reads 0.
func TestPumpContextThresholdRetires(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	threshold := int64(1000)
	h.agent.RetireContextTokens = &threshold

	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "fill the context",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	// 600 + 100 + 400 = 1100 ≥ 1000: crossing.
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-th","total_cost_usd":0,`+
		`"usage":{"input_tokens":600,"cache_creation_input_tokens":100,"cache_read_input_tokens":400}}`)

	r := waitPump(t, res)
	if !errors.Is(r.err, errSessionRetired) {
		t.Fatalf("pump returned %v, want errSessionRetired", r.err)
	}
	if got, err := h.st.GetTurn(ctx, turn.ID); err != nil || got.Status != store.TurnStatusDone {
		t.Fatalf("turn = %v, %v; want done (retirement must not land mid-turn)", got.Status, err)
	}

	// The old session ended with the threshold reason; exactly one
	// successor chained to it.
	old, err := h.st.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.EndedAt == nil || old.EndReason == nil || !strings.Contains(*old.EndReason, "context threshold (1000 tokens)") {
		t.Fatalf("old session end = %+v", old)
	}
	sessions, err := h.st.ListSessions(ctx, h.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("agent has %d sessions, want 2 (one retirement per crossing)", len(sessions))
	}
	next, err := h.st.CurrentSession(ctx, h.agent.ID)
	if err != nil || next == nil {
		t.Fatalf("current session = %v, %v", next, err)
	}
	if next.PrevSessionID == nil || *next.PrevSessionID != sess.ID {
		t.Fatalf("successor prev_session_id = %v, want %s", next.PrevSessionID, sess.ID)
	}

	// The seed turn: queued, addressed to the successor, and it teaches
	// memory recovery through kbase.
	seed, err := h.st.PeekQueuedTurn(ctx, h.agent.ID)
	if err != nil || seed == nil {
		t.Fatalf("seed turn = %v, %v", seed, err)
	}
	if seed.SessionID == nil || *seed.SessionID != next.ID {
		t.Fatalf("seed addressed to %v, want successor %s", seed.SessionID, next.ID)
	}
	for _, want := range []string{"kbase recall", "kbase ticket list", "lab-agent status", h.agent.Name} {
		if !strings.Contains(seed.Content, want) {
			t.Errorf("seed content missing %q: %s", want, seed.Content)
		}
	}

	// The fresh session starts empty.
	if got, err := h.st.SessionContextTokens(ctx, next.ID); err != nil || got != 0 {
		t.Fatalf("successor occupancy = %d, %v; want 0", got, err)
	}
}

// TestPumpBelowThresholdNoRetire: a result under the threshold leaves
// the session alone.
func TestPumpBelowThresholdNoRetire(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	threshold := int64(1000)
	h.agent.RetireContextTokens = &threshold

	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "small turn",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-nt","total_cost_usd":0,`+
		`"usage":{"input_tokens":600,"cache_read_input_tokens":399}}`)
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	if cur, err := h.st.CurrentSession(ctx, h.agent.ID); err != nil || cur == nil || cur.ID != sess.ID {
		t.Fatalf("current session = %v, %v; want original %s", cur, err, sess.ID)
	}
	f.endProcess()
	waitPump(t, res)
}

// TestPumpRestartPoke: a poked restart (credential rebind) is honored
// at the turn boundary — immediately when idle, and only after the
// in-flight turn's result when working.
func TestPumpRestartPoke(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		d := testDriver(h.st)
		sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := newFakeProc()
		res := startPump(ctx, d, h, sess, f.proc, nil)
		d.PokeRestart(h.agent.ID)
		r := waitPump(t, res)
		if !errors.Is(r.err, errRestartRequested) {
			t.Fatalf("pump returned %v, want errRestartRequested", r.err)
		}
		if d.takeRestartPoke(h.agent.ID) {
			t.Error("poke not consumed")
		}
	})

	t.Run("mid-turn waits for the result", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		d := testDriver(h.st)
		sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
			AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "long turn",
		})
		if err != nil {
			t.Fatal(err)
		}
		f := newFakeProc()
		res := startPump(ctx, d, h, sess, f.proc, nil)
		f.readStdin(t) // the turn is in flight
		d.PokeRestart(h.agent.ID)

		// Several poll ticks: the pump must not restart mid-turn.
		select {
		case r := <-res:
			t.Fatalf("pump returned mid-turn: %v", r.err)
		case <-time.After(150 * time.Millisecond):
		}

		f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-rp","total_cost_usd":0,"usage":{}}`)
		r := waitPump(t, res)
		if !errors.Is(r.err, errRestartRequested) {
			t.Fatalf("pump returned %v, want errRestartRequested", r.err)
		}
		if got, err := h.st.GetTurn(ctx, turn.ID); err != nil || got.Status != store.TurnStatusDone {
			t.Fatalf("turn = %v, %v; want done before restart", got.Status, err)
		}
	})
}

// TestPumpGateHoldsAgentSourcedTurn: the budget gate applies to
// agent-sourced turns (a lab-agent send) exactly like user turns — a
// denied turn stays queued and delivers when the gate opens.
func TestPumpGateHoldsAgentSourcedTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	gate := &fakeGate{verdict: budget.Verdict{Reason: "agent-turn test budget", RetryAfter: time.Now().Add(time.Hour)}}
	d.gate = gate
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := h.agent.ID // self-reference is fine: any agent uuid attributes the turn
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindAgent, SourceID: &sender,
		Content: "agent-sourced gated prompt",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	waitFor(t, "several gate checks", func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.checks >= 3
	})
	if got, err := h.st.GetTurn(ctx, turn.ID); err != nil || got.Status != store.TurnStatusQueued {
		t.Fatalf("denied agent turn = %v, %v; want queued", got.Status, err)
	}

	gate.set(budget.Verdict{Allowed: true})
	if line := f.readStdin(t); !strings.Contains(line, "agent-sourced gated prompt") {
		t.Fatalf("delivered line = %s", line)
	}
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-ag","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	f.endProcess()
	waitPump(t, res)
}
