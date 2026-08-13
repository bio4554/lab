-- +goose Up
CREATE TABLE lab.agent_tokens (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    agent_id    uuid NOT NULL REFERENCES lab.agents (id),
    secret_hash bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

-- Verification looks tokens up by the SHA-256 of the presented secret.
CREATE UNIQUE INDEX agent_tokens_secret_hash_idx ON lab.agent_tokens (secret_hash);

-- Agent self-reported status (POST /v1/status on the agent API).
ALTER TABLE lab.agents ADD COLUMN status_text text;

-- +goose Down
ALTER TABLE lab.agents DROP COLUMN status_text;
DROP TABLE lab.agent_tokens;
