package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
)

// Gate decides whether an agent may be delivered its next turn. It
// re-reads the agent and credential rows on every check so budget
// edits, expiry changes, rate-limit holds and manual resumes apply
// immediately, without a driver restart.
type Gate struct {
	St  *store.Store
	Log *slog.Logger
	// Now is a test seam; nil means time.Now.
	Now func() time.Time
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Check returns the verdict for delivering the agent's next turn.
// Order: credential presence → expiry → status → rate-limit hold →
// budget limits (agent scope, then credential scope). An agent with no
// credential is allowed — nothing is metered against it.
func (g *Gate) Check(ctx context.Context, agentID uuid.UUID) (Verdict, error) {
	agent, err := g.St.GetAgent(ctx, agentID)
	if err != nil {
		return Verdict{}, err
	}
	if agent.CredentialID == nil {
		return allow(), nil
	}
	cred, err := g.St.GetCredential(ctx, *agent.CredentialID)
	if errors.Is(err, store.ErrNotFound) {
		return deny("credential missing", time.Time{}), nil
	}
	if err != nil {
		return Verdict{}, err
	}
	now := g.now()

	// Expiry: lazy check on use; flip the row so listings agree.
	if cred.ExpiresAt != nil && !now.Before(*cred.ExpiresAt) {
		if cred.Status == store.CredentialStatusActive {
			if err := g.St.UpdateCredentialStatus(ctx, cred.ID, store.CredentialStatusExpired); err != nil && g.Log != nil {
				g.Log.Warn("marking credential expired", "credential", cred.ID, "error", err)
			}
		}
		return deny(fmt.Sprintf("credential %q expired %s", cred.Label, cred.ExpiresAt.UTC().Format(time.RFC3339)), time.Time{}), nil
	}
	if cred.Status != store.CredentialStatusActive {
		return deny(fmt.Sprintf("credential %q has status %s", cred.Label, cred.Status), time.Time{}), nil
	}

	// Rate-limit hold recorded by the driver (or already past — the
	// sweep clears it eventually, the gate opens immediately).
	if cred.LimitedUntil != nil && now.Before(*cred.LimitedUntil) {
		return deny(fmt.Sprintf("credential %q rate limited until %s", cred.Label, cred.LimitedUntil.UTC().Format(time.RFC3339)),
			*cred.LimitedUntil), nil
	}

	agentLimits, err := ParseLimits(agent.Budget)
	if err != nil {
		return Verdict{}, err
	}
	credLimits, err := ParseLimits(cred.Budget)
	if err != nil {
		return Verdict{}, err
	}
	if !agentLimits.IsZero() {
		usage := func(since time.Time) (store.Usage, error) { return g.St.AgentUsageInWindow(ctx, agent.ID, since) }
		if v, err := checkScope(ctx, "agent budget", agentLimits, usage, now); err != nil || !v.Allowed {
			return v, err
		}
	}
	if !credLimits.IsZero() {
		usage := func(since time.Time) (store.Usage, error) { return g.St.UsageInWindow(ctx, cred.ID, since) }
		if v, err := checkScope(ctx, fmt.Sprintf("credential %q budget", cred.Label), credLimits, usage, now); err != nil || !v.Allowed {
			return v, err
		}
	}
	return allow(), nil
}

// checkScope evaluates one scope's limits against its usage windows.
func checkScope(_ context.Context, scope string, lim Limits, usage func(time.Time) (store.Usage, error), now time.Time) (Verdict, error) {
	if lim.MaxCostUSDDay != nil || lim.MaxTokensDay != nil {
		day, err := usage(dayStart(now))
		if err != nil {
			return Verdict{}, err
		}
		nextDay := dayStart(now).Add(24 * time.Hour)
		if lim.MaxCostUSDDay != nil && day.CostUSD >= *lim.MaxCostUSDDay {
			return deny(fmt.Sprintf("%s: max_cost_usd_day %.2f reached ($%.4f today)", scope, *lim.MaxCostUSDDay, day.CostUSD), nextDay), nil
		}
		if lim.MaxTokensDay != nil && day.TokensIn+day.TokensOut >= *lim.MaxTokensDay {
			return deny(fmt.Sprintf("%s: max_tokens_day %d reached (%d today)", scope, *lim.MaxTokensDay, day.TokensIn+day.TokensOut), nextDay), nil
		}
	}
	if lim.MaxTurnsHour != nil {
		hour, err := usage(hourStart(now))
		if err != nil {
			return Verdict{}, err
		}
		if hour.Turns >= *lim.MaxTurnsHour {
			return deny(fmt.Sprintf("%s: max_turns_hour %d reached (%d this hour)", scope, *lim.MaxTurnsHour, hour.Turns), hourStart(now).Add(time.Hour)), nil
		}
	}
	return allow(), nil
}
