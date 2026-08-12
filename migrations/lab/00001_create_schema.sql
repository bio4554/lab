-- +goose Up
CREATE SCHEMA IF NOT EXISTS lab;

-- +goose Down
-- Intentionally a no-op: dropping the schema here would also drop
-- lab.goose_version mid-transaction, before goose records the rollback.
SELECT 1;
