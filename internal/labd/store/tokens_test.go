package store

import (
	"context"
	"testing"
)

func TestAgentTokens(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	tok, secret, err := s.MintAgentToken(ctx, f.Agent.ID)
	if err != nil {
		t.Fatalf("MintAgentToken: %v", err)
	}
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars", len(secret))
	}
	if string(tok.SecretHash) == secret {
		t.Error("secret stored in plaintext")
	}
	if tok.RevokedAt != nil {
		t.Errorf("fresh token revoked_at = %v, want nil", tok.RevokedAt)
	}

	agentID, err := s.VerifyAgentToken(ctx, secret)
	if err != nil || agentID != f.Agent.ID {
		t.Fatalf("VerifyAgentToken = %v, %v; want %v", agentID, err, f.Agent.ID)
	}
	if _, err := s.VerifyAgentToken(ctx, "not-a-token"); err != ErrTokenInvalid {
		t.Errorf("VerifyAgentToken(bad) = %v, want ErrTokenInvalid", err)
	}

	// Second mint + revoke-previous is the container-create sequence.
	if n, err := s.RevokeAgentTokens(ctx, f.Agent.ID); err != nil || n != 1 {
		t.Fatalf("RevokeAgentTokens = %d, %v; want 1", n, err)
	}
	if _, err := s.VerifyAgentToken(ctx, secret); err != ErrTokenInvalid {
		t.Errorf("VerifyAgentToken(revoked) = %v, want ErrTokenInvalid", err)
	}
	_, secret2, err := s.MintAgentToken(ctx, f.Agent.ID)
	if err != nil {
		t.Fatalf("MintAgentToken(second): %v", err)
	}
	if secret2 == secret {
		t.Error("mint returned a repeated secret")
	}
	if agentID, err := s.VerifyAgentToken(ctx, secret2); err != nil || agentID != f.Agent.ID {
		t.Errorf("VerifyAgentToken(second) = %v, %v", agentID, err)
	}
}

func TestAgentStatusTextAndActiveAgents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	status := "compiling the widget"
	if err := s.SetAgentStatusText(ctx, f.Agent.ID, &status); err != nil {
		t.Fatalf("SetAgentStatusText: %v", err)
	}
	agent, err := s.GetAgent(ctx, f.Agent.ID)
	if err != nil || agent.StatusText == nil || *agent.StatusText != status {
		t.Fatalf("status_text = %v, %v; want %q", agent.StatusText, err, status)
	}
	if err := s.SetAgentStatusText(ctx, f.Agent.ID, nil); err != nil {
		t.Fatalf("SetAgentStatusText(nil): %v", err)
	}
	if agent, _ = s.GetAgent(ctx, f.Agent.ID); agent.StatusText != nil {
		t.Errorf("status_text not cleared: %v", *agent.StatusText)
	}

	// stopped (fixture default) → not active; idle → active.
	inActive := func() bool {
		agents, err := s.ActiveAgents(ctx)
		if err != nil {
			t.Fatalf("ActiveAgents: %v", err)
		}
		for _, a := range agents {
			if a.ID == f.Agent.ID {
				return true
			}
		}
		return false
	}
	if inActive() {
		t.Error("stopped agent listed as active")
	}
	if err := s.UpdateAgentState(ctx, f.Agent.ID, AgentStateIdle); err != nil {
		t.Fatal(err)
	}
	if !inActive() {
		t.Error("idle agent not listed as active")
	}
}
