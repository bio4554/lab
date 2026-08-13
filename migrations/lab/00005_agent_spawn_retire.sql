-- +goose Up
-- Worker-spawn policy: only agents marked can_spawn may create workers
-- through the agent API. Spawned agents always get can_spawn = false
-- (no transitive spawning).
ALTER TABLE lab.agents ADD COLUMN can_spawn boolean NOT NULL DEFAULT false;

-- Auto-retirement threshold: when the current session's context
-- occupancy (latest result event's input + cache tokens) reaches this,
-- the driver retires the session between turns. NULL = never.
ALTER TABLE lab.agents ADD COLUMN retire_context_tokens bigint;

-- +goose Down
ALTER TABLE lab.agents DROP COLUMN retire_context_tokens;
ALTER TABLE lab.agents DROP COLUMN can_spawn;
