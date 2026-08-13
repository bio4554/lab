package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// oauthTokenLifetime is the documented lifetime of a `claude
// setup-token` credential; it becomes the default expiry for
// oauth_token credentials created without one.
const oauthTokenLifetime = 365 * 24 * time.Hour

func toWireCredential(c store.Credential) wire.Credential {
	return wire.Credential{
		ID:           c.ID,
		Kind:         c.Kind,
		Label:        c.Label,
		Status:       c.Status,
		ExpiresAt:    c.ExpiresAt,
		Budget:       c.Budget,
		LimitedUntil: c.LimitedUntil,
		CreatedAt:    c.CreatedAt,
	}
}

// findCredential resolves the {credential} path value.
func (s *ClientServer) findCredential(ctx context.Context, r *http.Request) (store.Credential, error) {
	id, err := uuid.Parse(r.PathValue("credential"))
	if err != nil {
		return store.Credential{}, fmt.Errorf("invalid credential id: %w", store.ErrNotFound)
	}
	return s.Store.GetCredential(ctx, id)
}

func (s *ClientServer) credentialCreate(w http.ResponseWriter, r *http.Request) {
	var req wire.CreateCredentialRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.Kind != store.CredentialKindAPIKey && req.Kind != store.CredentialKindOAuthToken {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("kind must be api_key or oauth_token"))
		return
	}
	if req.Label == "" || req.Secret == "" {
		writeError(s.Log, w, http.StatusBadRequest, errors.New("label and secret are required"))
		return
	}
	if _, err := budget.ParseLimits(req.Budget); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	expiresAt := req.ExpiresAt
	if expiresAt == nil && !req.NoExpiry && req.Kind == store.CredentialKindOAuthToken {
		t := time.Now().UTC().Add(oauthTokenLifetime)
		expiresAt = &t
	}
	enc, err := s.Vault.Encrypt(req.Secret)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	cred, err := s.Store.CreateCredential(r.Context(), store.NewCredential{
		Kind: req.Kind, SecretEnc: enc, Label: req.Label, ExpiresAt: expiresAt, Budget: req.Budget,
	})
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusCreated, toWireCredential(cred))
}

func (s *ClientServer) credentialList(w http.ResponseWriter, r *http.Request) {
	creds, err := s.Store.ListCredentials(r.Context())
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	out := make([]wire.Credential, len(creds))
	for i, c := range creds {
		out[i] = toWireCredential(c)
	}
	writeJSON(s.Log, w, http.StatusOK, out)
}

func (s *ClientServer) credentialDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cred, err := s.findCredential(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	refs, err := s.Store.CountCredentialRefs(ctx, cred.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if refs > 0 {
		writeError(s.Log, w, http.StatusConflict,
			fmt.Errorf("credential %q is referenced by %d agent(s)/project default(s)", cred.Label, refs))
		return
	}
	if err := s.Store.DeleteCredential(ctx, cred.ID); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ClientServer) credentialSetExpiry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cred, err := s.findCredential(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	var req wire.SetExpiryRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetCredentialExpiry(ctx, cred.ID, req.ExpiresAt); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	updated, err := s.Store.GetCredential(ctx, cred.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, toWireCredential(updated))
}

// credentialResume is the manual rate-limit release: it clears the
// hold and resumes the credential's paused agents without waiting for
// the recorded reset.
func (s *ClientServer) credentialResume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cred, err := s.findCredential(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.SetCredentialLimited(ctx, cred.ID, nil); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	resumed, err := s.Store.ResumeAgentsForCredential(ctx, cred.ID)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("credential manually resumed", "credential", cred.ID, "agents_resumed", resumed)
	w.WriteHeader(http.StatusNoContent)
}

// ── budgets ──────────────────────────────────────────────────────────

func (s *ClientServer) credentialBudgetGet(w http.ResponseWriter, r *http.Request) {
	cred, err := s.findCredential(r.Context(), r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, wire.BudgetPayload{Budget: cred.Budget})
}

func (s *ClientServer) credentialBudgetSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cred, err := s.findCredential(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	budgetJSON, err := decodeBudget(r)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetCredentialBudget(ctx, cred.ID, budgetJSON); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, wire.BudgetPayload{Budget: budgetJSON})
}

func (s *ClientServer) agentBudgetGet(w http.ResponseWriter, r *http.Request) {
	_, agent, err := s.findAgent(r.Context(), r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, wire.BudgetPayload{Budget: agent.Budget})
}

func (s *ClientServer) agentBudgetSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	budgetJSON, err := decodeBudget(r)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetAgentBudget(ctx, agent.ID, budgetJSON); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(s.Log, w, http.StatusOK, wire.BudgetPayload{Budget: budgetJSON})
}

// agentCredentialSet rebinds an agent to a credential (or unbinds with
// null). The new binding applies from the agent's next container
// provision; a hosted driver picks it up on its next restart.
func (s *ClientServer) agentCredentialSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, agent, err := s.findAgent(ctx, r)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	var req wire.SetAgentCredentialRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if req.CredentialID != nil {
		if _, err := s.Store.GetCredential(ctx, *req.CredentialID); err != nil {
			writeError(s.Log, w, http.StatusBadRequest, fmt.Errorf("credential %s: %w", req.CredentialID, err))
			return
		}
	}
	if err := s.Store.SetAgentCredential(ctx, agent.ID, req.CredentialID); err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeBudget reads and validates a BudgetPayload body.
func decodeBudget(r *http.Request) ([]byte, error) {
	var req wire.BudgetPayload
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	if _, err := budget.ParseLimits(req.Budget); err != nil {
		return nil, err
	}
	if len(req.Budget) == 0 {
		return []byte("{}"), nil
	}
	return req.Budget, nil
}

// ── usage/budget status ──────────────────────────────────────────────

// usageStatus reports current-window usage vs limits per credential
// and per agent, with the live gate verdict per agent. This is the
// endpoint the TUI's budget view will consume.
func (s *ClientServer) usageStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	hourStart := now.Truncate(time.Hour)

	creds, err := s.Store.ListCredentials(ctx)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	status := wire.UsageStatus{Credentials: []wire.CredentialUsage{}, Agents: []wire.AgentBudgetStatus{}}
	for _, c := range creds {
		day, err := s.Store.UsageInWindow(ctx, c.ID, dayStart)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		hour, err := s.Store.UsageInWindow(ctx, c.ID, hourStart)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		status.Credentials = append(status.Credentials, wire.CredentialUsage{
			ID: c.ID, Label: c.Label, Kind: c.Kind, Status: c.Status,
			ExpiresAt: c.ExpiresAt, LimitedUntil: c.LimitedUntil, Budget: c.Budget,
			Today: toWireUsage(day), ThisHour: toWireUsage(hour),
		})
	}

	projects, err := s.Store.ListProjects(ctx)
	if err != nil {
		writeError(s.Log, w, http.StatusInternalServerError, err)
		return
	}
	for _, proj := range projects {
		agents, err := s.Store.ListAgents(ctx, proj.ID)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		for _, a := range agents {
			day, err := s.Store.AgentUsageInWindow(ctx, a.ID, dayStart)
			if err != nil {
				writeError(s.Log, w, http.StatusInternalServerError, err)
				return
			}
			hour, err := s.Store.AgentUsageInWindow(ctx, a.ID, hourStart)
			if err != nil {
				writeError(s.Log, w, http.StatusInternalServerError, err)
				return
			}
			verdict := wire.BudgetVerdict{Allowed: true}
			if s.Gate != nil {
				v, err := s.Gate.Check(ctx, a.ID)
				if errors.Is(err, store.ErrNotFound) {
					// The agent was deleted between the listing and the
					// check; drop its row rather than failing the report.
					continue
				}
				if err != nil {
					writeError(s.Log, w, http.StatusInternalServerError, err)
					return
				}
				verdict.Allowed = v.Allowed
				verdict.Reason = v.Reason
				if !v.RetryAfter.IsZero() {
					retry := v.RetryAfter
					verdict.RetryAfter = &retry
				}
			}
			status.Agents = append(status.Agents, wire.AgentBudgetStatus{
				ID: a.ID, Name: a.Name, Project: proj.Name, State: a.State,
				CredentialID: a.CredentialID, Budget: a.Budget,
				Today: toWireUsage(day), ThisHour: toWireUsage(hour),
				Verdict: verdict,
			})
		}
	}
	writeJSON(s.Log, w, http.StatusOK, status)
}

func toWireUsage(u store.Usage) wire.Usage {
	return wire.Usage{TokensIn: u.TokensIn, TokensOut: u.TokensOut, CostUSD: u.CostUSD, Turns: u.Turns}
}
