// Package budget enforces spend and rate limits around the agent
// driver: per-agent and per-credential budget limits evaluated against
// usage_rollups windows, and the rate-limit hold recorded on
// credentials. The driver consults the Gate before delivering each
// queued turn; a denied turn stays queued and is re-checked on the
// pump's poll/wake cadence, so opening the gate (higher budget, window
// rollover, rate-limit reset, manual resume) releases it without any
// turn ever being errored.
package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Limits is the budget contract stored in lab.agents.budget and
// lab.credentials.budget (jsonb):
//
//	{"max_cost_usd_day": 5.0, "max_tokens_day": 2000000, "max_turns_hour": 30}
//
// All keys are optional; an absent key is unlimited. An agent budget
// is evaluated against the agent's own usage, a credential budget
// against the credential's usage across all its agents; every set
// limit must pass (most restrictive wins). Windows are computed from
// usage_rollups in UTC: "day" is the current UTC calendar day,
// "hour" the current UTC clock hour. Tokens count input + output.
type Limits struct {
	MaxCostUSDDay *float64 `json:"max_cost_usd_day,omitempty"`
	MaxTokensDay  *int64   `json:"max_tokens_day,omitempty"`
	MaxTurnsHour  *int64   `json:"max_turns_hour,omitempty"`
}

// ParseLimits decodes a budget jsonb value. Empty or "{}" means no
// limits. Unknown keys are rejected so typos deny loudly at set time,
// not silently at enforcement time.
func ParseLimits(raw json.RawMessage) (Limits, error) {
	var l Limits
	if len(raw) == 0 {
		return l, nil
	}
	if err := unmarshalStrict(raw, &l); err != nil {
		return Limits{}, fmt.Errorf("budget: invalid limits %s: %w", raw, err)
	}
	if l.MaxCostUSDDay != nil && *l.MaxCostUSDDay < 0 ||
		l.MaxTokensDay != nil && *l.MaxTokensDay < 0 ||
		l.MaxTurnsHour != nil && *l.MaxTurnsHour < 0 {
		return Limits{}, fmt.Errorf("budget: limits must be non-negative: %s", raw)
	}
	return l, nil
}

// IsZero reports whether no limit is set.
func (l Limits) IsZero() bool {
	return l.MaxCostUSDDay == nil && l.MaxTokensDay == nil && l.MaxTurnsHour == nil
}

// Verdict is the Gate's answer for one prospective turn delivery.
type Verdict struct {
	Allowed bool
	// Reason says why delivery is denied (empty when allowed).
	Reason string
	// RetryAfter is when a denied turn is worth re-checking: the next
	// window boundary or the rate-limit reset. Zero when unknown (e.g.
	// an expired credential, which never heals on its own).
	RetryAfter time.Time
}

// allow is the affirmative verdict.
func allow() Verdict { return Verdict{Allowed: true} }

// deny builds a denial.
func deny(reason string, retryAfter time.Time) Verdict {
	return Verdict{Reason: reason, RetryAfter: retryAfter}
}

// dayStart returns the start of now's UTC calendar day.
func dayStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// hourStart returns the start of now's UTC clock hour.
func hourStart(now time.Time) time.Time {
	return now.UTC().Truncate(time.Hour)
}

func unmarshalStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
