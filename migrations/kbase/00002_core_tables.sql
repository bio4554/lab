-- +goose Up

-- Principals are the identities kbase attributes writes to: lab agents
-- (external_id = lab agent uuid, opaque to kbase) and humans
-- (external_id = a handle). kbase never FKs into the lab schema.
CREATE TABLE kbase.principals (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    kind         text NOT NULL CHECK (kind IN ('agent', 'human')),
    external_id  text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, external_id)
);

-- Bearer tokens. project_id NULL scopes the token to all projects
-- (human/admin tokens); non-NULL restricts reads to that project +
-- global and writes to that project. Only the SHA-256 of the secret is
-- stored; the plaintext is returned once at mint.
CREATE TABLE kbase.tokens (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    principal_id uuid NOT NULL REFERENCES kbase.principals (id),
    secret_hash  bytea NOT NULL,
    project_id   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);

-- Verification looks tokens up by the SHA-256 of the presented secret.
CREATE UNIQUE INDEX tokens_secret_hash_idx ON kbase.tokens (secret_hash);

-- Entries are the versioned units. project_id NULL = global scope.
-- Titles live on versions; the slug is the stable handle. Slugs are
-- unique per project scope, with NULL project (global) its own scope —
-- hence the pair of partial unique indexes.
CREATE TABLE kbase.entries (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id uuid,
    type       text NOT NULL CHECK (type IN ('decision', 'note', 'architecture', 'component', 'ticket')),
    slug       text NOT NULL,
    created_by uuid NOT NULL REFERENCES kbase.principals (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX entries_project_slug_idx ON kbase.entries (project_id, slug)
    WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX entries_global_slug_idx ON kbase.entries (slug)
    WHERE project_id IS NULL;

-- Slug half of the FTS pair: a generated tsvector cannot reference
-- another table, so the slug indexes here and title+content index on
-- entry_versions; recall concatenates the two. Same 'english' config
-- as the version vector so query stemming matches slug words too.
ALTER TABLE kbase.entries ADD COLUMN search tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', replace(slug, '-', ' ')), 'A')
    ) STORED;
CREATE INDEX entries_search_idx ON kbase.entries USING gin (search);

-- Append-only version chain; "current" = max(version_no). No row is
-- ever updated or deleted (enforced by trigger below).
CREATE TABLE kbase.entry_versions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    entry_id   uuid NOT NULL REFERENCES kbase.entries (id),
    version_no int NOT NULL,
    title      text NOT NULL,
    content    text NOT NULL,
    author     uuid NOT NULL REFERENCES kbase.principals (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    search     tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', title), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    UNIQUE (entry_id, version_no)
);

CREATE INDEX entry_versions_search_idx ON kbase.entry_versions USING gin (search);

-- Immutability is a schema property, not just an API convention: any
-- UPDATE or DELETE against a version row errors out.
-- +goose StatementBegin
CREATE FUNCTION kbase.forbid_version_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'kbase.entry_versions is append-only';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER entry_versions_append_only
    BEFORE UPDATE OR DELETE ON kbase.entry_versions
    FOR EACH ROW EXECUTE FUNCTION kbase.forbid_version_mutation();

-- +goose Down
DROP TRIGGER entry_versions_append_only ON kbase.entry_versions;
DROP FUNCTION kbase.forbid_version_mutation();
DROP TABLE kbase.entry_versions;
DROP TABLE kbase.entries;
DROP TABLE kbase.tokens;
DROP TABLE kbase.principals;
