package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/store"
)

// fakeGate is a TurnGate whose verdict the test flips at will.
type fakeGate struct {
	mu      sync.Mutex
	verdict budget.Verdict
	checks  int
}

func (g *fakeGate) set(v budget.Verdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verdict = v
}

func (g *fakeGate) Check(_ context.Context, _ uuid.UUID) (budget.Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	return g.verdict, nil
}

// TestPumpGateHoldsAndReleases: a denied turn stays queued (not
// running, not errored) and is delivered as soon as the gate opens.
func TestPumpGateHoldsAndReleases(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	gate := &fakeGate{verdict: budget.Verdict{Reason: "test budget reached", RetryAfter: time.Now().Add(time.Hour)}}
	d.gate = gate
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "gated prompt",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)

	// The pump re-checks on its poll cadence while denied; the turn
	// must remain queued the whole time.
	waitFor(t, "several gate checks", func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.checks >= 3
	})
	got, err := h.st.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusQueued {
		t.Fatalf("denied turn status = %s, want queued", got.Status)
	}

	// Open the gate: the queued turn delivers without a new enqueue.
	gate.set(budget.Verdict{Allowed: true})
	line := f.readStdin(t)
	if !strings.Contains(line, "gated prompt") {
		t.Fatalf("delivered line = %s", line)
	}
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-gate","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	f.endProcess()
	waitPump(t, res)
}

// TestPumpRateLimitPausesAndResumes: a limited rate_limit_event in the
// stream records the hold on the credential and pauses its agents;
// with the real budget gate, the next turn stays queued until the
// reset passes, then delivers (timed resume, short window).
func TestPumpRateLimitPausesAndResumes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	d.gate = &budget.Gate{St: h.st}
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A sibling agent on the same credential, currently idle: the
	// pause must reach it too.
	sibling, err := h.st.CreateAgent(ctx, store.NewAgent{
		ProjectID: h.proj.ID, Name: "sibling-" + uuid.NewString(),
		CredentialID: &h.cred.ID, Branch: "agent/sibling",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.pool.Exec(ctx, "DELETE FROM lab.agents WHERE id = $1", sibling.ID); err != nil {
			t.Errorf("cleanup sibling: %v", err)
		}
	})
	if err := h.st.UpdateAgentState(ctx, sibling.ID, store.AgentStateIdle); err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)

	// First turn delivers normally, then the stream reports the limit
	// hit with a reset ~2s out.
	turn1, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "turn one",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.readStdin(t)
	// resetsAt is unix seconds on the wire; keep the local copy at the
	// same resolution so the before/after assertion is exact.
	reset := time.Unix(time.Now().Add(2*time.Second).Unix(), 0)
	f.writeLine(t, fmt.Sprintf(
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%d,"rateLimitType":"five_hour"},"session_id":"sid-rl"}`,
		reset.Unix()))
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-rl","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn one done", func() bool {
		got, err := h.st.GetTurn(ctx, turn1.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})

	// The hold landed on the credential and the sibling is paused.
	waitFor(t, "credential limited", func() bool {
		cred, err := h.st.GetCredential(ctx, h.cred.ID)
		return err == nil && cred.LimitedUntil != nil
	})
	cred, err := h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := cred.LimitedUntil.Unix(); got != reset.Unix() {
		t.Fatalf("limited_until = %d, want %d", got, reset.Unix())
	}
	sib, err := h.st.GetAgent(ctx, sibling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sib.State != store.AgentStatePaused {
		t.Fatalf("sibling state = %s, want paused", sib.State)
	}

	// A second turn stays queued while the hold lasts, then delivers
	// once the reset passes — no sweep involved, the gate opens by
	// clock.
	turn2, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "turn two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.st.GetTurn(ctx, turn2.ID); err != nil || got.Status != store.TurnStatusQueued {
		t.Fatalf("turn two during hold = %v, %v; want queued", got.Status, err)
	}
	line := f.readStdin(t) // blocks until the gate opens at reset
	if !strings.Contains(line, "turn two") {
		t.Fatalf("post-reset line = %s", line)
	}
	if time.Now().Before(reset) {
		t.Error("turn two delivered before the reset time")
	}
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-rl","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn two done", func() bool {
		got, err := h.st.GetTurn(ctx, turn2.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})

	// The sweep releases the passed hold and resumes paused agents.
	released, err := h.st.ReleaseExpiredLimits(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	cred, err = h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.LimitedUntil != nil {
		t.Fatalf("limited_until after sweep = %v, want nil", cred.LimitedUntil)
	}
	sib, err = h.st.GetAgent(ctx, sibling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sib.State != store.AgentStateIdle {
		t.Fatalf("sibling state after sweep = %s, want idle", sib.State)
	}

	f.endProcess()
	waitPump(t, res)
}

// TestPumpLimitShapedResultError: an is_error result whose text is
// unmistakably a rate-limit error records a default-length hold.
func TestPumpLimitShapedResultError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "trip the limit",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	before := time.Now()
	f.writeLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 429 rate limit exceeded","session_id":"sid-lim","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn errored", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusError
	})
	waitFor(t, "credential held", func() bool {
		cred, err := h.st.GetCredential(ctx, h.cred.ID)
		return err == nil && cred.LimitedUntil != nil
	})
	cred, err := h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if until := *cred.LimitedUntil; until.Before(before.Add(4*time.Minute)) || until.After(before.Add(6*time.Minute)) {
		t.Fatalf("default hold = %v, want ~5m from %v", until, before)
	}
	f.endProcess()
	waitPump(t, res)
}

// TestPumpSuccessResultMentioningLimitsDoesNotHold: limit words in a
// successful result (assistant prose) must not trip the detector, and
// an "allowed" rate_limit_event must not either.
func TestPumpSuccessResultMentioningLimitsDoesNotHold(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "explain rate limits",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	f.writeLine(t, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1786590000,"rateLimitType":"five_hour"},"session_id":"sid-ok"}`)
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"result":"A rate limit is a cap on requests, e.g. HTTP 429 Too Many Requests.","session_id":"sid-ok","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	cred, err := h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.LimitedUntil != nil {
		t.Fatalf("limited_until = %v, want nil (no hold on success)", cred.LimitedUntil)
	}
	f.endProcess()
	waitPump(t, res)
}

// TestLimitShapedResult covers the detector patterns directly.
func TestLimitShapedResult(t *testing.T) {
	yes := []string{
		`{"result":"rate limit exceeded"}`,
		`{"result":"Rate-limited, retry later"}`,
		`{"result":"usage limit reached|resets at 5pm"}`,
		`{"result":"429 too many requests"}`,
	}
	no := []string{
		`{"result":"wrote 4290 bytes"}`,
		`{"result":"all done"}`,
		`{"result":"loop ran 1429 times"}`,
	}
	for _, s := range yes {
		if !limitShapedResult([]byte(s)) {
			t.Errorf("limitShapedResult(%s) = false, want true", s)
		}
	}
	for _, s := range no {
		if limitShapedResult([]byte(s)) {
			t.Errorf("limitShapedResult(%s) = true, want false", s)
		}
	}
}

// TestGateBudgetRaiseReleases wires the real budget gate end-to-end
// through the pump: a max_turns_hour 1 budget holds the second turn
// queued with the deny visible via Gate.Check; raising the budget
// releases it without touching the queue (the acceptance flow).
func TestGateBudgetRaiseReleases(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	gate := &budget.Gate{St: h.st}
	d.gate = gate
	if err := h.st.SetAgentBudget(ctx, h.agent.ID, json.RawMessage(`{"max_turns_hour": 1}`)); err != nil {
		t.Fatal(err)
	}
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)

	turn1, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.readStdin(t)
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-b","total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":5}}`)
	waitFor(t, "first turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn1.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})

	// Second turn: held queued, and the gate names the budget.
	turn2, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "deny verdict", func() bool {
		v, err := gate.Check(ctx, h.agent.ID)
		return err == nil && !v.Allowed && strings.Contains(v.Reason, "max_turns_hour 1 reached")
	})
	time.Sleep(100 * time.Millisecond) // several poll ticks
	if got, err := h.st.GetTurn(ctx, turn2.ID); err != nil || got.Status != store.TurnStatusQueued {
		t.Fatalf("held turn = %v, %v; want queued", got.Status, err)
	}

	// Raise the budget: the held turn delivers.
	if err := h.st.SetAgentBudget(ctx, h.agent.ID, json.RawMessage(`{"max_turns_hour": 10}`)); err != nil {
		t.Fatal(err)
	}
	line := f.readStdin(t)
	if !strings.Contains(line, "second") {
		t.Fatalf("released line = %s", line)
	}
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-b","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "second turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn2.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	f.endProcess()
	waitPump(t, res)
}
