-- +goose Up
CREATE SCHEMA IF NOT EXISTS kbase;

-- +goose Down
-- Intentionally a no-op: dropping the schema here would also drop
-- kbase.goose_version mid-transaction, before goose records the rollback.
SELECT 1;
