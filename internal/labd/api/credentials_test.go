package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// doRaw performs a request and returns status + raw body, for
// assertions on the wire bytes themselves.
func doRaw(t *testing.T, client *http.Client, method, url string, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestCredentialAPI: create/list/expiry/resume/delete, with the secret
// asserted absent from every response body, and the create-time
// validation and oauth default expiry.
func TestCredentialAPI(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	ctx := context.Background()
	const secret = "fabricated-secret-vLx9-do-not-echo"
	label := "api test " + uuid.NewString()[:8]

	// Validation: bad kind, missing label/secret, bad budget.
	status, _ := doRaw(t, client, "POST", srv.URL+"/v1/credentials",
		`{"kind":"weird","label":"x","secret":"y"}`)
	if status != http.StatusBadRequest {
		t.Errorf("bad kind = %d, want 400", status)
	}
	status, _ = doRaw(t, client, "POST", srv.URL+"/v1/credentials", `{"kind":"api_key"}`)
	if status != http.StatusBadRequest {
		t.Errorf("missing fields = %d, want 400", status)
	}
	status, _ = doRaw(t, client, "POST", srv.URL+"/v1/credentials",
		`{"kind":"api_key","label":"x","secret":"y","budget":{"max_turns_day":1}}`)
	if status != http.StatusBadRequest {
		t.Errorf("bad budget key = %d, want 400", status)
	}

	// Create an oauth token: default expiry ≈ +1 year, secret not echoed.
	status, body := doRaw(t, client, "POST", srv.URL+"/v1/credentials",
		`{"kind":"oauth_token","label":"`+label+`","secret":"`+secret+`","budget":{"max_turns_hour":5}}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, body)
	}
	if strings.Contains(body, secret) {
		t.Fatal("create response echoes the secret")
	}
	var cred wire.Credential
	if err := json.Unmarshal([]byte(body), &cred); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID)
	})
	if cred.Kind != "oauth_token" || cred.Label != label || cred.Status != store.CredentialStatusActive {
		t.Errorf("created credential = %+v", cred)
	}
	if cred.ExpiresAt == nil {
		t.Fatal("oauth_token default expiry missing")
	}
	if d := time.Until(*cred.ExpiresAt); d < 364*24*time.Hour || d > 366*24*time.Hour {
		t.Errorf("default expiry = %v (%v out), want ~1 year", cred.ExpiresAt, d)
	}

	// The stored secret_enc is ciphertext, and the vault round-trips it.
	row, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.SecretEnc), secret) {
		t.Fatal("secret_enc contains the plaintext")
	}

	// List: present, no secret anywhere in the body.
	status, body = doRaw(t, client, "GET", srv.URL+"/v1/credentials", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d", status)
	}
	if !strings.Contains(body, label) {
		t.Error("created credential missing from list")
	}
	if strings.Contains(body, secret) || strings.Contains(body, "secret_enc") {
		t.Fatal("list response leaks secret material")
	}

	// Budget get/set round-trip.
	var bp wire.BudgetPayload
	doJSON(t, client, "GET", srv.URL+"/v1/credentials/"+cred.ID.String()+"/budget", nil, &bp, http.StatusOK, nil)
	if !strings.Contains(string(bp.Budget), `"max_turns_hour": 5`) && !strings.Contains(string(bp.Budget), `"max_turns_hour":5`) {
		t.Errorf("budget = %s", bp.Budget)
	}
	doJSON(t, client, "PUT", srv.URL+"/v1/credentials/"+cred.ID.String()+"/budget",
		wire.BudgetPayload{Budget: json.RawMessage(`{"max_cost_usd_day":2.5}`)}, &bp, http.StatusOK, nil)
	doJSON(t, client, "GET", srv.URL+"/v1/credentials/"+cred.ID.String()+"/budget", nil, &bp, http.StatusOK, nil)
	var lim struct {
		MaxCost *float64 `json:"max_cost_usd_day"`
		Turns   *int64   `json:"max_turns_hour"`
	}
	if err := json.Unmarshal(bp.Budget, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.MaxCost == nil || *lim.MaxCost != 2.5 || lim.Turns != nil {
		t.Errorf("budget after set = %s", bp.Budget)
	}
	status, _ = doRaw(t, client, "PUT", srv.URL+"/v1/credentials/"+cred.ID.String()+"/budget",
		`{"budget":{"max_tokens_day":-1}}`)
	if status != http.StatusBadRequest {
		t.Errorf("negative budget = %d, want 400", status)
	}

	// Expiry set + clear.
	past := time.Now().Add(-time.Hour).UTC()
	var updated wire.Credential
	doJSON(t, client, "PUT", srv.URL+"/v1/credentials/"+cred.ID.String()+"/expiry",
		wire.SetExpiryRequest{ExpiresAt: &past}, &updated, http.StatusOK, nil)
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(past) {
		t.Errorf("expiry after set = %v", updated.ExpiresAt)
	}
	var cleared wire.Credential // fresh var: expires_at is omitempty on the wire
	doJSON(t, client, "PUT", srv.URL+"/v1/credentials/"+cred.ID.String()+"/expiry",
		wire.SetExpiryRequest{ExpiresAt: nil}, &cleared, http.StatusOK, nil)
	if cleared.ExpiresAt != nil || cleared.Status != store.CredentialStatusActive {
		t.Errorf("expiry after clear = %+v", cleared)
	}

	// Manual resume clears a rate-limit hold and unpauses agents.
	fix := createFixture(t, st, pool)
	if err := st.SetAgentCredential(ctx, fix.Agent.ID, &cred.ID); err != nil {
		t.Fatal(err)
	}
	hold := time.Now().Add(time.Hour)
	if err := st.SetCredentialLimited(ctx, cred.ID, &hold); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAgentState(ctx, fix.Agent.ID, store.AgentStatePaused); err != nil {
		t.Fatal(err)
	}

	// While paused, the agent listing surfaces paused_until.
	var agents []wire.Agent
	doJSON(t, client, "GET", srv.URL+"/v1/projects/"+fix.Project.Name+"/agents", nil, &agents, http.StatusOK, nil)
	if len(agents) != 1 || agents[0].State != store.AgentStatePaused ||
		agents[0].PausedUntil == nil || !agents[0].PausedUntil.Equal(hold) {
		t.Errorf("paused agent listing = %+v", agents)
	}

	doJSON(t, client, "POST", srv.URL+"/v1/credentials/"+cred.ID.String()+"/resume", nil, nil, http.StatusNoContent, nil)
	resumed, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.LimitedUntil != nil {
		t.Errorf("limited_until after resume = %v", resumed.LimitedUntil)
	}
	ag, err := st.GetAgent(ctx, fix.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ag.State != store.AgentStateIdle {
		t.Errorf("agent state after resume = %s, want idle", ag.State)
	}

	// Delete: refused while referenced, allowed once unbound.
	status, body = doRaw(t, client, "DELETE", srv.URL+"/v1/credentials/"+cred.ID.String(), "")
	if status != http.StatusConflict {
		t.Fatalf("delete while referenced = %d: %s", status, body)
	}
	if err := st.SetAgentCredential(ctx, fix.Agent.ID, nil); err != nil {
		t.Fatal(err)
	}
	doJSON(t, client, "DELETE", srv.URL+"/v1/credentials/"+cred.ID.String(), nil, nil, http.StatusNoContent, nil)
	if _, err := st.GetCredential(ctx, cred.ID); err != store.ErrNotFound {
		t.Errorf("credential after delete = %v, want ErrNotFound", err)
	}
}

// TestAgentBudgetAndUsageStatus: agent budget get/set round-trip and
// the usage/budget status endpoint reflecting usage and a live deny
// verdict.
func TestAgentBudgetAndUsageStatus(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	ctx := context.Background()

	var cred wire.Credential
	doJSON(t, client, "POST", srv.URL+"/v1/credentials",
		wire.CreateCredentialRequest{Kind: "api_key", Label: "usage test " + uuid.NewString()[:8], Secret: "fabricated-usage-secret"},
		&cred, http.StatusCreated, nil)
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM lab.usage_rollups WHERE credential_id = $1", cred.ID)
		pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID)
	})
	fix := createFixture(t, st, pool)
	if err := st.SetAgentCredential(ctx, fix.Agent.ID, &cred.ID); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/v1/projects/" + fix.Project.Name + "/agents/" + fix.Agent.Name

	// Agent budget round-trip.
	var bp wire.BudgetPayload
	doJSON(t, client, "GET", base+"/budget", nil, &bp, http.StatusOK, nil)
	if strings.ReplaceAll(string(bp.Budget), " ", "") != "{}" {
		t.Errorf("initial agent budget = %s", bp.Budget)
	}
	doJSON(t, client, "PUT", base+"/budget",
		wire.BudgetPayload{Budget: json.RawMessage(`{"max_turns_hour":1}`)}, &bp, http.StatusOK, nil)

	// One turn of usage this hour: the gate must now deny.
	window := time.Now().UTC().Truncate(time.Hour)
	if err := st.AddUsage(ctx, cred.ID, fix.Agent.ID, window, store.UsageDelta{
		TokensIn: 100, TokensOut: 50, CostUSD: 0.25, Turns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var status wire.UsageStatus
	doJSON(t, client, "GET", srv.URL+"/v1/usage", nil, &status, http.StatusOK, nil)
	var foundCred, foundAgent bool
	for _, c := range status.Credentials {
		if c.ID != cred.ID {
			continue
		}
		foundCred = true
		if c.Today.Turns != 1 || c.ThisHour.CostUSD != 0.25 || c.ThisHour.TokensIn != 100 {
			t.Errorf("credential usage = %+v", c)
		}
	}
	for _, a := range status.Agents {
		if a.ID != fix.Agent.ID {
			continue
		}
		foundAgent = true
		if a.ThisHour.Turns != 1 || a.Project != fix.Project.Name {
			t.Errorf("agent usage = %+v", a)
		}
		if a.Verdict.Allowed || !strings.Contains(a.Verdict.Reason, "max_turns_hour 1 reached") {
			t.Errorf("agent verdict = %+v, want deny naming the budget", a.Verdict)
		}
		if a.Verdict.RetryAfter == nil || !a.Verdict.RetryAfter.Equal(window.Add(time.Hour)) {
			t.Errorf("retry after = %v, want %v", a.Verdict.RetryAfter, window.Add(time.Hour))
		}
	}
	if !foundCred || !foundAgent {
		t.Fatalf("status missing rows: cred=%v agent=%v", foundCred, foundAgent)
	}

	// Raising the budget flips the verdict to allowed.
	doJSON(t, client, "PUT", base+"/budget",
		wire.BudgetPayload{Budget: json.RawMessage(`{"max_turns_hour":100}`)}, &bp, http.StatusOK, nil)
	doJSON(t, client, "GET", srv.URL+"/v1/usage", nil, &status, http.StatusOK, nil)
	for _, a := range status.Agents {
		if a.ID == fix.Agent.ID && !a.Verdict.Allowed {
			t.Errorf("verdict after raise = %+v, want allowed", a.Verdict)
		}
	}

	// Rebinding the credential via the API endpoint.
	status2, _ := doRaw(t, client, "PUT", base+"/credential", `{"credential_id":null}`)
	if status2 != http.StatusNoContent {
		t.Fatalf("unbind credential = %d", status2)
	}
	ag, err := st.GetAgent(ctx, fix.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ag.CredentialID != nil {
		t.Errorf("credential after unbind = %v", ag.CredentialID)
	}
	doJSON(t, client, "PUT", base+"/credential",
		wire.SetAgentCredentialRequest{CredentialID: &cred.ID}, nil, http.StatusNoContent, nil)
	ag, err = st.GetAgent(ctx, fix.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ag.CredentialID == nil || *ag.CredentialID != cred.ID {
		t.Errorf("credential after rebind = %v", ag.CredentialID)
	}
}
