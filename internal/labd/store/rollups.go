package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AddUsage accumulates a usage delta into the (credential, agent,
// window) rollup row, creating it on first use.
func (s *Store) AddUsage(ctx context.Context, credentialID, agentID uuid.UUID, windowStart time.Time, d UsageDelta) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO lab.usage_rollups (credential_id, agent_id, window_start, tokens_in, tokens_out, cost_usd, turns)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (credential_id, agent_id, window_start) DO UPDATE SET
			tokens_in  = lab.usage_rollups.tokens_in  + EXCLUDED.tokens_in,
			tokens_out = lab.usage_rollups.tokens_out + EXCLUDED.tokens_out,
			cost_usd   = lab.usage_rollups.cost_usd   + EXCLUDED.cost_usd,
			turns      = lab.usage_rollups.turns      + EXCLUDED.turns`,
		credentialID, agentID, windowStart, d.TokensIn, d.TokensOut, d.CostUSD, d.Turns)
	if err != nil {
		return fmt.Errorf("add usage: %w", err)
	}
	return nil
}

// AgentUsage is one agent's rollup aggregates over three windows, for
// the TUI usage view. Rollup windows that straddle a boundary count
// toward the later bucket in full (window_start >= boundary).
type AgentUsage struct {
	AgentID   uuid.UUID `db:"agent_id"`
	AgentName string    `db:"agent_name"`
	HourIn    int64     `db:"hour_in"`
	HourOut   int64     `db:"hour_out"`
	HourCost  float64   `db:"hour_cost"`
	HourTurns int64     `db:"hour_turns"`
	DayIn     int64     `db:"day_in"`
	DayOut    int64     `db:"day_out"`
	DayCost   float64   `db:"day_cost"`
	DayTurns  int64     `db:"day_turns"`
	TotalIn   int64     `db:"total_in"`
	TotalOut  int64     `db:"total_out"`
	TotalCost float64   `db:"total_cost"`
	TotalTurn int64     `db:"total_turns"`
}

// ProjectUsage aggregates rollups per agent of a project: windows
// starting at or after hourStart, at or after dayStart, and all time.
// Every agent of the project gets a row, zeroes when it has no usage.
func (s *Store) ProjectUsage(ctx context.Context, projectID uuid.UUID, hourStart, dayStart time.Time) ([]AgentUsage, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT a.id AS agent_id, a.name AS agent_name,
			COALESCE(SUM(u.tokens_in)  FILTER (WHERE u.window_start >= $2), 0)::bigint  AS hour_in,
			COALESCE(SUM(u.tokens_out) FILTER (WHERE u.window_start >= $2), 0)::bigint  AS hour_out,
			COALESCE(SUM(u.cost_usd)   FILTER (WHERE u.window_start >= $2), 0)::float8  AS hour_cost,
			COALESCE(SUM(u.turns)      FILTER (WHERE u.window_start >= $2), 0)::bigint  AS hour_turns,
			COALESCE(SUM(u.tokens_in)  FILTER (WHERE u.window_start >= $3), 0)::bigint  AS day_in,
			COALESCE(SUM(u.tokens_out) FILTER (WHERE u.window_start >= $3), 0)::bigint  AS day_out,
			COALESCE(SUM(u.cost_usd)   FILTER (WHERE u.window_start >= $3), 0)::float8  AS day_cost,
			COALESCE(SUM(u.turns)      FILTER (WHERE u.window_start >= $3), 0)::bigint  AS day_turns,
			COALESCE(SUM(u.tokens_in),  0)::bigint  AS total_in,
			COALESCE(SUM(u.tokens_out), 0)::bigint  AS total_out,
			COALESCE(SUM(u.cost_usd),   0)::float8  AS total_cost,
			COALESCE(SUM(u.turns),      0)::bigint  AS total_turns
		FROM lab.agents a
		LEFT JOIN lab.usage_rollups u ON u.agent_id = a.id
		WHERE a.project_id = $1
		GROUP BY a.id, a.name
		ORDER BY a.name`,
		projectID, hourStart, dayStart)
	usage, err := pgx.CollectRows(rows, pgx.RowToStructByName[AgentUsage])
	if err != nil {
		return nil, fmt.Errorf("project usage: %w", err)
	}
	return usage, nil
}

// AgentUsageInWindow sums one agent's rollups (across credentials) for
// windows starting at or after since. Zero-valued Usage when none.
func (s *Store) AgentUsageInWindow(ctx context.Context, agentID uuid.UUID, since time.Time) (Usage, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT
			COALESCE(SUM(tokens_in),  0)::bigint  AS tokens_in,
			COALESCE(SUM(tokens_out), 0)::bigint  AS tokens_out,
			COALESCE(SUM(cost_usd),   0)::float8  AS cost_usd,
			COALESCE(SUM(turns),      0)::bigint  AS turns
		FROM lab.usage_rollups
		WHERE agent_id = $1 AND window_start >= $2`,
		agentID, since)
	usage, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Usage])
	if err != nil {
		return Usage{}, fmt.Errorf("agent usage in window: %w", err)
	}
	return usage, nil
}

// UsageInWindow sums a credential's rollups across all agents for
// windows starting at or after since. Zero-valued Usage when none.
func (s *Store) UsageInWindow(ctx context.Context, credentialID uuid.UUID, since time.Time) (Usage, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT
			COALESCE(SUM(tokens_in),  0)::bigint  AS tokens_in,
			COALESCE(SUM(tokens_out), 0)::bigint  AS tokens_out,
			COALESCE(SUM(cost_usd),   0)::float8  AS cost_usd,
			COALESCE(SUM(turns),      0)::bigint  AS turns
		FROM lab.usage_rollups
		WHERE credential_id = $1 AND window_start >= $2`,
		credentialID, since)
	usage, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Usage])
	if err != nil {
		return Usage{}, fmt.Errorf("usage in window: %w", err)
	}
	return usage, nil
}
