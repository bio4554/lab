-- +goose Up
CREATE TABLE lab.credentials (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    kind       text NOT NULL CHECK (kind IN ('api_key', 'oauth_token')),
    secret_enc bytea NOT NULL,
    label      text NOT NULL,
    status     text NOT NULL DEFAULT 'active',
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE lab.projects (
    id                    uuid PRIMARY KEY DEFAULT uuidv7(),
    name                  text NOT NULL UNIQUE,
    origin_kind           text NOT NULL CHECK (origin_kind IN ('git_url', 'local_path')),
    origin                text NOT NULL,
    stack                 text NOT NULL,
    default_credential_id uuid REFERENCES lab.credentials (id),
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE lab.agents (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id    uuid NOT NULL REFERENCES lab.projects (id),
    name          text NOT NULL,
    role_prompt   text NOT NULL DEFAULT '',
    model         text,
    credential_id uuid REFERENCES lab.credentials (id),
    budget        jsonb NOT NULL DEFAULT '{}',
    state         text NOT NULL DEFAULT 'stopped'
        CHECK (state IN ('stopped', 'idle', 'working', 'paused', 'retired')),
    container_id  text,
    branch        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE TABLE lab.sessions (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    agent_id          uuid NOT NULL REFERENCES lab.agents (id),
    claude_session_id text,
    started_at        timestamptz NOT NULL DEFAULT now(),
    ended_at          timestamptz,
    end_reason        text,
    prev_session_id   uuid REFERENCES lab.sessions (id)
);

CREATE TABLE lab.turns (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    agent_id    uuid NOT NULL REFERENCES lab.agents (id),
    session_id  uuid REFERENCES lab.sessions (id),
    source_kind text NOT NULL CHECK (source_kind IN ('user', 'agent')),
    source_id   uuid,
    content     text NOT NULL,
    status      text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'error')),
    error       text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

-- Queue polling: NextQueuedTurn scans (agent_id, status) ordered by created_at.
CREATE INDEX turns_agent_status_created_at_idx
    ON lab.turns (agent_id, status, created_at);

CREATE TABLE lab.events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id   uuid NOT NULL REFERENCES lab.agents (id),
    session_id uuid NOT NULL REFERENCES lab.sessions (id),
    turn_id    uuid REFERENCES lab.turns (id),
    seq        bigint NOT NULL,
    kind       text NOT NULL,
    payload    jsonb NOT NULL,
    ts         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, seq)
);

-- Tailing an agent's full stream across sessions by global event id.
CREATE INDEX events_agent_id_id_idx ON lab.events (agent_id, id);

CREATE TABLE lab.usage_rollups (
    credential_id uuid NOT NULL REFERENCES lab.credentials (id),
    agent_id      uuid NOT NULL REFERENCES lab.agents (id),
    window_start  timestamptz NOT NULL,
    tokens_in     bigint NOT NULL DEFAULT 0,
    tokens_out    bigint NOT NULL DEFAULT 0,
    cost_usd      numeric NOT NULL DEFAULT 0,
    turns         int NOT NULL DEFAULT 0,
    PRIMARY KEY (credential_id, agent_id, window_start)
);

-- +goose Down
DROP TABLE lab.usage_rollups;
DROP TABLE lab.events;
DROP TABLE lab.turns;
DROP TABLE lab.sessions;
DROP TABLE lab.agents;
DROP TABLE lab.projects;
DROP TABLE lab.credentials;
