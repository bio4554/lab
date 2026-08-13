package kbclient

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// TokenProvisioner is labd's side of getting kbase access into agent
// containers: at container create it registers the agent as a kbase
// principal (idempotent upsert) and mints a fresh project-scoped
// token, revoking the agent's previous tokens for that project — one
// live token per (agent, project), rotated on every container create,
// mirroring the lab agent-token lifecycle.
type TokenProvisioner struct {
	// Admin is a client authenticated with kbased's admin token.
	Admin *Client
	// AgentBaseURL is the kbased URL as reachable from inside
	// containers (config kbase_url_for_agents); returned as the
	// KBASE_URL value.
	AgentBaseURL string
}

// ProvisionAgentToken ensures a principal for the agent and mints its
// project-scoped token. It returns the container-visible base URL and
// the plaintext token (which exists only in memory and the container
// env — never at rest, never in logs).
func (p *TokenProvisioner) ProvisionAgentToken(ctx context.Context, agentID uuid.UUID, displayName string, projectID uuid.UUID) (url, token string, err error) {
	principal, err := p.Admin.RegisterPrincipal(ctx, RegisterPrincipalRequest{
		Kind:        "agent",
		ExternalID:  agentID.String(),
		DisplayName: displayName,
	})
	if err != nil {
		return "", "", fmt.Errorf("register kbase principal: %w", err)
	}
	resp, err := p.Admin.MintToken(ctx, MintTokenRequest{
		PrincipalID:    principal.ID,
		ProjectID:      &projectID,
		RevokeExisting: true,
	})
	if err != nil {
		return "", "", fmt.Errorf("mint kbase token: %w", err)
	}
	return p.AgentBaseURL, resp.Token, nil
}
