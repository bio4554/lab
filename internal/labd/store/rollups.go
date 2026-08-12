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
