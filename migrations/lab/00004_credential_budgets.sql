-- +goose Up
-- Per-credential budget limits, same jsonb contract as lab.agents.budget:
--   {"max_cost_usd_day": 5.0, "max_tokens_day": 2000000, "max_turns_hour": 30}
-- All keys optional; agent and credential budgets both apply.
ALTER TABLE lab.credentials ADD COLUMN budget jsonb NOT NULL DEFAULT '{}';

-- Rate-limit hold: while set and in the future, turns on agents using
-- this credential stay queued (the budget gate denies delivery).
-- Cleared by the daemon sweep once passed, or by manual resume.
ALTER TABLE lab.credentials ADD COLUMN limited_until timestamptz;

-- +goose Down
ALTER TABLE lab.credentials DROP COLUMN limited_until;
ALTER TABLE lab.credentials DROP COLUMN budget;
