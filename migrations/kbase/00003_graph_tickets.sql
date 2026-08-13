-- +goose Up

-- Directed edges between component entries. An edge lives in a project
-- scope (NULL = global) like entries do; its endpoints must be entries
-- of type component readable in that scope — enforced server-side,
-- since a CHECK cannot reach across tables.
--
-- Edges are append-only with tombstones: removal sets tombstoned_by/
-- tombstoned_at instead of deleting, so the graph at any past time T is
-- reconstructible (live-at-T = created_at <= T AND (tombstoned_at IS
-- NULL OR tombstoned_at > T)).
CREATE TABLE kbase.edges (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id    uuid,
    from_entry    uuid NOT NULL REFERENCES kbase.entries (id),
    to_entry      uuid NOT NULL REFERENCES kbase.entries (id),
    label         text NOT NULL,
    created_by    uuid NOT NULL REFERENCES kbase.principals (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    tombstoned_by uuid REFERENCES kbase.principals (id),
    tombstoned_at timestamptz,
    CHECK ((tombstoned_by IS NULL) = (tombstoned_at IS NULL))
);

-- At most one *live* edge per (scope, from, to, label); a tombstoned
-- edge may be recreated as a new row. NULL project (global) is its own
-- scope — hence the pair of partial unique indexes, as with entry
-- slugs.
CREATE UNIQUE INDEX edges_live_project_idx
    ON kbase.edges (project_id, from_entry, to_entry, label)
    WHERE tombstoned_at IS NULL AND project_id IS NOT NULL;
CREATE UNIQUE INDEX edges_live_global_idx
    ON kbase.edges (from_entry, to_entry, label)
    WHERE tombstoned_at IS NULL AND project_id IS NULL;

-- Append-only is a schema property: the only UPDATE an edge row ever
-- accepts is its first tombstone (setting tombstoned_by/tombstoned_at,
-- everything else unchanged); DELETE and every other UPDATE error out.
-- +goose StatementBegin
CREATE FUNCTION kbase.edge_tombstone_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kbase.edges is append-only; remove edges by tombstoning';
    END IF;
    IF OLD.tombstoned_at IS NOT NULL
        OR NEW.tombstoned_at IS NULL
        OR NEW.tombstoned_by IS NULL
        OR NEW.id <> OLD.id
        OR NEW.project_id IS DISTINCT FROM OLD.project_id
        OR NEW.from_entry <> OLD.from_entry
        OR NEW.to_entry <> OLD.to_entry
        OR NEW.label <> OLD.label
        OR NEW.created_by <> OLD.created_by
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'kbase.edges permits only a first tombstone as UPDATE';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER edges_tombstone_only
    BEFORE UPDATE OR DELETE ON kbase.edges
    FOR EACH ROW EXECUTE FUNCTION kbase.edge_tombstone_only();

-- Tickets: the one sanctioned in-place mutation in kbase. Each ticket
-- is backed 1:1 by an entry of type ticket whose version chain carries
-- the body, comments and status history (append-only, with
-- provenance); this row holds only the coordination state, mutated via
-- compare-and-swap on cas_version.
CREATE TABLE kbase.tickets (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id  uuid,
    entry_id    uuid NOT NULL UNIQUE REFERENCES kbase.entries (id),
    status      text NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'claimed', 'in_progress', 'done', 'abandoned')),
    claimed_by  uuid REFERENCES kbase.principals (id),
    cas_version int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE kbase.tickets;
DROP TRIGGER edges_tombstone_only ON kbase.edges;
DROP FUNCTION kbase.edge_tombstone_only();
DROP TABLE kbase.edges;
