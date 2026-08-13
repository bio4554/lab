package budget

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labd/store"
)

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

func TestParseLimits(t *testing.T) {
	l, err := ParseLimits(json.RawMessage(`{"max_cost_usd_day": 5.0, "max_tokens_day": 2000000, "max_turns_hour": 30}`))
	if err != nil {
		t.Fatal(err)
	}
	if *l.MaxCostUSDDay != 5.0 || *l.MaxTokensDay != 2000000 || *l.MaxTurnsHour != 30 {
		t.Fatalf("limits = %+v", l)
	}
	for _, raw := range []string{"", "{}"} {
		l, err := ParseLimits(json.RawMessage(raw))
		if err != nil || !l.IsZero() {
			t.Fatalf("ParseLimits(%q) = %+v, %v; want zero limits", raw, l, err)
		}
	}
	for _, raw := range []string{`{"max_turns_day": 1}`, `{"max_turns_hour": -1}`, `[1]`} {
		if _, err := ParseLimits(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseLimits(%q) should error", raw)
		}
	}
}

// gateHarness is a live-Postgres fixture: a credential and two agents
// bound to it (so credential-scope usage can aggregate across agents).
type gateHarness struct {
	st     *store.Store
	pool   *pgxpool.Pool
	cred   store.Credential
	proj   store.Project
	agent  store.Agent
	agent2 store.Agent
}

func newGateHarness(t *testing.T) *gateHarness {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (run `make db-up`): %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool)

	cred, err := st.CreateCredential(ctx, store.NewCredential{
		Kind: store.CredentialKindOAuthToken, SecretEnc: []byte("x"), Label: "test " + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, store.NewProject{
		Name: "test-" + uuid.NewString(), OriginKind: store.OriginKindLocalPath,
		Origin: "/tmp/" + t.Name(), Stack: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	mkAgent := func(name string) store.Agent {
		a, err := st.CreateAgent(ctx, store.NewAgent{
			ProjectID: proj.ID, Name: name + "-" + uuid.NewString(),
			CredentialID: &cred.ID, Branch: "agent/test",
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	h := &gateHarness{st: st, pool: pool, cred: cred, proj: proj, agent: mkAgent("a1"), agent2: mkAgent("a2")}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM lab.usage_rollups WHERE credential_id = $1", cred.ID); err != nil {
			t.Errorf("cleanup rollups: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM lab.agents WHERE project_id = $1", proj.ID); err != nil {
			t.Errorf("cleanup agents: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM lab.projects WHERE id = $1", proj.ID); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
	})
	return h
}

func (h *gateHarness) setBudgets(t *testing.T, agentBudget, credBudget string) {
	t.Helper()
	ctx := context.Background()
	if err := h.st.SetAgentBudget(ctx, h.agent.ID, json.RawMessage(agentBudget)); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetCredentialBudget(ctx, h.cred.ID, json.RawMessage(credBudget)); err != nil {
		t.Fatal(err)
	}
}

// TestGateVerdicts is the table: kinds of limits × usage states ×
// windows, most-restrictive-wins, unlimited when no budget set.
func TestGateVerdicts(t *testing.T) {
	h := newGateHarness(t)
	ctx := context.Background()

	// Frozen clock at the middle of the current real UTC hour, so the
	// store's DB-clock comparisons (SetCredentialExpiry) stay coherent
	// with the gate's fake clock.
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	gate := &Gate{St: h.st, Now: func() time.Time { return now }}

	thisHour := now.Truncate(time.Hour)
	prevHour := thisHour.Add(-time.Hour) // same UTC day, earlier hour
	yesterday := thisHour.Add(-24 * time.Hour)

	cases := []struct {
		name          string
		agentBudget   string
		credBudget    string
		usage         map[time.Time]store.UsageDelta // window → delta for h.agent
		agent2Usage   map[time.Time]store.UsageDelta // window → delta for h.agent2 (same credential)
		wantAllowed   bool
		wantReason    string // substring
		wantRetryHour bool   // retry-after == next hour boundary
		wantRetryDay  bool   // retry-after == next UTC midnight
	}{
		{
			name: "no budgets, heavy usage, unlimited",
			usage: map[time.Time]store.UsageDelta{
				thisHour: {TokensIn: 1e9, TokensOut: 1e9, CostUSD: 1e6, Turns: 1e6},
			},
			wantAllowed: true,
		},
		{
			name:        "agent turns/hour under limit",
			agentBudget: `{"max_turns_hour": 3}`,
			usage:       map[time.Time]store.UsageDelta{thisHour: {Turns: 2}},
			wantAllowed: true,
		},
		{
			name:          "agent turns/hour reached",
			agentBudget:   `{"max_turns_hour": 1}`,
			usage:         map[time.Time]store.UsageDelta{thisHour: {Turns: 1}},
			wantAllowed:   false,
			wantReason:    "agent budget: max_turns_hour 1 reached",
			wantRetryHour: true,
		},
		{
			name:        "turns in the previous hour do not count",
			agentBudget: `{"max_turns_hour": 1}`,
			usage:       map[time.Time]store.UsageDelta{prevHour: {Turns: 5}},
			wantAllowed: true,
		},
		{
			name:         "agent daily cost reached",
			agentBudget:  `{"max_cost_usd_day": 0.5}`,
			usage:        map[time.Time]store.UsageDelta{thisHour: {CostUSD: 0.5}},
			wantAllowed:  false,
			wantReason:   "max_cost_usd_day 0.50 reached",
			wantRetryDay: true,
		},
		{
			name:        "yesterday's cost does not count",
			agentBudget: `{"max_cost_usd_day": 0.5}`,
			usage:       map[time.Time]store.UsageDelta{yesterday: {CostUSD: 100}},
			wantAllowed: true,
		},
		{
			name:         "agent daily tokens reached (in + out)",
			agentBudget:  `{"max_tokens_day": 100}`,
			usage:        map[time.Time]store.UsageDelta{thisHour: {TokensIn: 60, TokensOut: 40}},
			wantAllowed:  false,
			wantReason:   "max_tokens_day 100 reached (100 today)",
			wantRetryDay: true,
		},
		{
			name:        "credential budget counts sibling agents",
			credBudget:  `{"max_turns_hour": 2}`,
			usage:       map[time.Time]store.UsageDelta{thisHour: {Turns: 1}},
			agent2Usage: map[time.Time]store.UsageDelta{thisHour: {Turns: 1}},
			wantAllowed: false, wantReason: "credential", wantRetryHour: true,
		},
		{
			name:        "agent budget ignores sibling agents",
			agentBudget: `{"max_turns_hour": 2}`,
			agent2Usage: map[time.Time]store.UsageDelta{thisHour: {Turns: 10}},
			wantAllowed: true,
		},
		{
			name:          "most restrictive wins: agent trips before credential",
			agentBudget:   `{"max_turns_hour": 1}`,
			credBudget:    `{"max_turns_hour": 100}`,
			usage:         map[time.Time]store.UsageDelta{thisHour: {Turns: 1}},
			wantAllowed:   false,
			wantReason:    "agent budget",
			wantRetryHour: true,
		},
		{
			name:          "most restrictive wins: credential trips although agent allows",
			agentBudget:   `{"max_turns_hour": 100}`,
			credBudget:    `{"max_turns_hour": 1}`,
			usage:         map[time.Time]store.UsageDelta{thisHour: {Turns: 1}},
			wantAllowed:   false,
			wantReason:    "credential",
			wantRetryHour: true,
		},
		{
			name:        "mixed limits, all pass",
			agentBudget: `{"max_cost_usd_day": 10, "max_turns_hour": 10}`,
			credBudget:  `{"max_tokens_day": 1000}`,
			usage:       map[time.Time]store.UsageDelta{thisHour: {CostUSD: 1, Turns: 1, TokensIn: 100, TokensOut: 100}},
			wantAllowed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.pool.Exec(ctx, "DELETE FROM lab.usage_rollups WHERE credential_id = $1", h.cred.ID); err != nil {
				t.Fatal(err)
			}
			h.setBudgets(t, orEmpty(tc.agentBudget), orEmpty(tc.credBudget))
			for window, d := range tc.usage {
				if err := h.st.AddUsage(ctx, h.cred.ID, h.agent.ID, window, d); err != nil {
					t.Fatal(err)
				}
			}
			for window, d := range tc.agent2Usage {
				if err := h.st.AddUsage(ctx, h.cred.ID, h.agent2.ID, window, d); err != nil {
					t.Fatal(err)
				}
			}
			v, err := gate.Check(ctx, h.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if v.Allowed != tc.wantAllowed {
				t.Fatalf("verdict = %+v, want allowed=%v", v, tc.wantAllowed)
			}
			if tc.wantReason != "" && !strings.Contains(v.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want substring %q", v.Reason, tc.wantReason)
			}
			if tc.wantRetryHour {
				if want := thisHour.Add(time.Hour); !v.RetryAfter.Equal(want) {
					t.Errorf("retry after = %v, want next hour %v", v.RetryAfter, want)
				}
			}
			if tc.wantRetryDay {
				if want := dayStart(now).Add(24 * time.Hour); !v.RetryAfter.Equal(want) {
					t.Errorf("retry after = %v, want next UTC midnight %v", v.RetryAfter, want)
				}
			}
		})
	}
}

func orEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// TestGateCredentialStates covers the non-budget denials: no
// credential, expired (with lazy status flip), non-active status, and
// the rate-limit hold (including an already-passed hold).
func TestGateCredentialStates(t *testing.T) {
	h := newGateHarness(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	gate := &Gate{St: h.st, Now: func() time.Time { return now }}

	// Agent without credential: allowed.
	free, err := h.st.CreateAgent(ctx, store.NewAgent{
		ProjectID: h.proj.ID, Name: "free-" + uuid.NewString(), Branch: "agent/free",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := gate.Check(ctx, free.ID); err != nil || !v.Allowed {
		t.Fatalf("no-credential verdict = %+v, %v", v, err)
	}

	// Rate-limit hold in the future: denied with retry-after.
	reset := now.Add(30 * time.Minute)
	if err := h.st.SetCredentialLimited(ctx, h.cred.ID, &reset); err != nil {
		t.Fatal(err)
	}
	v, err := gate.Check(ctx, h.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Allowed || !strings.Contains(v.Reason, "rate limited") || !v.RetryAfter.Equal(reset) {
		t.Fatalf("rate-limited verdict = %+v", v)
	}

	// A hold already in the past opens without waiting for the sweep.
	past := now.Add(-time.Minute)
	if err := h.st.SetCredentialLimited(ctx, h.cred.ID, &past); err != nil {
		t.Fatal(err)
	}
	if v, err := gate.Check(ctx, h.agent.ID); err != nil || !v.Allowed {
		t.Fatalf("passed-hold verdict = %+v, %v", v, err)
	}
	if err := h.st.SetCredentialLimited(ctx, h.cred.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Expired: denied, and the status is flipped lazily.
	expiry := now.Add(-time.Hour)
	if err := h.st.SetCredentialExpiry(ctx, h.cred.ID, &expiry); err != nil {
		t.Fatal(err)
	}
	v, err = gate.Check(ctx, h.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Allowed || !strings.Contains(v.Reason, "expired") || !v.RetryAfter.IsZero() {
		t.Fatalf("expired verdict = %+v", v)
	}
	cred, err := h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Status != store.CredentialStatusExpired {
		t.Fatalf("status = %q, want expired (lazy flip)", cred.Status)
	}

	// Extending the expiry reactivates.
	future := now.Add(time.Hour)
	if err := h.st.SetCredentialExpiry(ctx, h.cred.ID, &future); err != nil {
		t.Fatal(err)
	}
	if v, err := gate.Check(ctx, h.agent.ID); err != nil || !v.Allowed {
		t.Fatalf("reactivated verdict = %+v, %v", v, err)
	}
}
