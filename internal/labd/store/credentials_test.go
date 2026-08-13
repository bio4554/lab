package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestCredentialBudgetAndLimits covers the Phase 8 credential columns
// and their store methods: budget round-trip, rate-limit hold
// set/clear, pause/resume of the credential's agents, the expiry
// sweep, and the reference count that guards deletion.
func TestCredentialBudgetAndLimits(t *testing.T) {
	s := testStore(t)
	f := createFixture(t, s)
	ctx := context.Background()

	// Fresh credential: '{}' budget, no hold.
	if got := string(f.Credential.Budget); got != "{}" {
		t.Errorf("initial budget = %s, want {}", got)
	}
	if f.Credential.LimitedUntil != nil {
		t.Errorf("initial limited_until = %v", f.Credential.LimitedUntil)
	}

	// Budget round-trips on both credential and agent.
	if err := s.SetCredentialBudget(ctx, f.Credential.ID, json.RawMessage(`{"max_turns_hour": 3}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentBudget(ctx, f.Agent.ID, json.RawMessage(`{"max_cost_usd_day": 1.5}`)); err != nil {
		t.Fatal(err)
	}
	cred, err := s.GetCredential(ctx, f.Credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.GetAgent(ctx, f.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var credLim, agentLim map[string]float64
	if err := json.Unmarshal(cred.Budget, &credLim); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(agent.Budget, &agentLim); err != nil {
		t.Fatal(err)
	}
	if credLim["max_turns_hour"] != 3 || agentLim["max_cost_usd_day"] != 1.5 {
		t.Errorf("budgets = %s / %s", cred.Budget, agent.Budget)
	}

	// Rate-limit hold: set, pause, sweep-release (past holds only).
	future := time.Now().Add(time.Hour).UTC()
	if err := s.SetCredentialLimited(ctx, f.Credential.ID, &future); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAgentState(ctx, f.Agent.ID, AgentStateWorking); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PauseAgentsForCredential(ctx, f.Credential.ID); err != nil || n != 1 {
		t.Fatalf("pause = %d, %v; want 1", n, err)
	}
	agent, _ = s.GetAgent(ctx, f.Agent.ID)
	if agent.State != AgentStatePaused {
		t.Fatalf("state after pause = %s", agent.State)
	}
	// Pausing again is a no-op (already paused).
	if n, _ := s.PauseAgentsForCredential(ctx, f.Credential.ID); n != 0 {
		t.Errorf("second pause = %d, want 0", n)
	}
	// The sweep ignores holds still in the future.
	if n, err := s.ReleaseExpiredLimits(ctx, time.Now()); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("release of future hold = %d, want 0", n)
	}
	// Once passed, it releases the hold and resumes the agents.
	past := time.Now().Add(-time.Minute).UTC()
	if err := s.SetCredentialLimited(ctx, f.Credential.ID, &past); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReleaseExpiredLimits(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("release = %d, %v; want 1", n, err)
	}
	cred, _ = s.GetCredential(ctx, f.Credential.ID)
	agent, _ = s.GetAgent(ctx, f.Agent.ID)
	if cred.LimitedUntil != nil || agent.State != AgentStateIdle {
		t.Fatalf("after release: limited=%v state=%s", cred.LimitedUntil, agent.State)
	}

	// Manual resume path: clear + ResumeAgentsForCredential.
	if err := s.SetCredentialLimited(ctx, f.Credential.ID, &future); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PauseAgentsForCredential(ctx, f.Credential.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredentialLimited(ctx, f.Credential.ID, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ResumeAgentsForCredential(ctx, f.Credential.ID); err != nil || n != 1 {
		t.Fatalf("resume = %d, %v; want 1", n, err)
	}

	// Expiry sweep flips only past-expiry active credentials.
	exp := time.Now().Add(-time.Minute).UTC()
	if err := s.SetCredentialExpiry(ctx, f.Credential.ID, &exp); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireCredentials(ctx, time.Now()); err != nil || n < 1 {
		t.Fatalf("expire sweep = %d, %v; want >= 1", n, err)
	}
	cred, _ = s.GetCredential(ctx, f.Credential.ID)
	if cred.Status != CredentialStatusExpired {
		t.Fatalf("status after sweep = %s", cred.Status)
	}
	// Extending the expiry reactivates.
	futureExp := time.Now().Add(time.Hour).UTC()
	if err := s.SetCredentialExpiry(ctx, f.Credential.ID, &futureExp); err != nil {
		t.Fatal(err)
	}
	cred, _ = s.GetCredential(ctx, f.Credential.ID)
	if cred.Status != CredentialStatusActive {
		t.Fatalf("status after extend = %s", cred.Status)
	}

	// Reference count: agent + project default = 2, then 0 after unbinding.
	if n, err := s.CountCredentialRefs(ctx, f.Credential.ID); err != nil || n != 2 {
		t.Fatalf("refs = %d, %v; want 2", n, err)
	}
	if err := s.SetAgentCredential(ctx, f.Agent.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE lab.projects SET default_credential_id = NULL WHERE id = $1", f.Project.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountCredentialRefs(ctx, f.Credential.ID); err != nil || n != 0 {
		t.Fatalf("refs after unbind = %d, %v; want 0", n, err)
	}
	if err := s.SetAgentCredential(ctx, f.Agent.ID, &f.Credential.ID); err != nil {
		t.Fatal(err)
	}
}

// TestPeekQueuedTurn: peek returns the head without claiming it.
func TestPeekQueuedTurn(t *testing.T) {
	s := testStore(t)
	f := createFixture(t, s)
	ctx := context.Background()

	if turn, err := s.PeekQueuedTurn(ctx, f.Agent.ID); err != nil || turn != nil {
		t.Fatalf("peek empty queue = %v, %v", turn, err)
	}
	first, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueTurn(ctx, NewTurn{AgentID: f.Agent.ID, SourceKind: SourceKindUser, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // peeking twice returns the same head, still queued
		peeked, err := s.PeekQueuedTurn(ctx, f.Agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if peeked == nil || peeked.ID != first.ID || peeked.Status != TurnStatusQueued {
			t.Fatalf("peek = %+v, want queued turn %s", peeked, first.ID)
		}
	}
	// Claiming still works and gets the same turn.
	claimed, err := s.NextQueuedTurn(ctx, f.Agent.ID)
	if err != nil || claimed == nil || claimed.ID != first.ID {
		t.Fatalf("next after peek = %+v, %v", claimed, err)
	}
}

// TestAgentUsageInWindow: sums only the agent's rollups since the
// boundary.
func TestAgentUsageInWindow(t *testing.T) {
	s := testStore(t)
	f := createFixture(t, s)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour)
	if err := s.AddUsage(ctx, f.Credential.ID, f.Agent.ID, now, UsageDelta{TokensIn: 10, TokensOut: 5, CostUSD: 0.1, Turns: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, f.Credential.ID, f.Agent.ID, now.Add(-2*time.Hour), UsageDelta{TokensIn: 100, Turns: 3}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentUsageInWindow(ctx, f.Agent.ID, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 10 || got.TokensOut != 5 || got.Turns != 1 {
		t.Fatalf("windowed usage = %+v", got)
	}
	all, err := s.AgentUsageInWindow(ctx, f.Agent.ID, now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if all.TokensIn != 110 || all.Turns != 4 {
		t.Fatalf("full usage = %+v", all)
	}
}
