# Phase 9 report — kbase core: daemon, CLI, recall

Branch: `phase-09-kbase`. `make check` passes; all new tests pass with
`-race`; `make migrate-kbase` up/down round-trips (verified twice,
including after the final schema edit).

## What was built

- **Schema** (`migrations/kbase/00002_core_tables.sql`): `principals`,
  `tokens`, `entries`, `entry_versions` in the `kbase` schema, uuidv7
  defaults, real Downs. Slug uniqueness is per project scope with NULL
  (global) its own scope via two partial unique indexes. Immutability
  is a schema property, not just an API convention: a trigger rejects
  any UPDATE/DELETE on `entry_versions` (tests assert direct SQL
  tampering fails too).
- **`internal/kbased`** (new): typed store (plain pgx, mirrors
  `labd/store` idioms) + the HTTP layer. `/v1`: create entry, append
  version, get (`?version`, `?history=1`, `?project` for scope-less
  disambiguation), list, recall. `/admin/v1`: principal upsert, token
  mint (plaintext returned once), token revoke.
- **`internal/kbclient`** (new): the API's JSON types + typed client
  for everything, plus `TokenProvisioner` — labd's ensure-principal +
  mint-project-token step. Integration-tested end to end (kbclient →
  httptest kbased → live Postgres).
- **`cmd/kbase`**: real CLI replacing the Phase 4 stub. `add`,
  `update`, `show [--version|--history]`, `list`, `recall`;
  `KBASE_URL`/`KBASE_TOKEN` env with `-url`/`-token` overrides;
  content from `-m` or stdin (trailing `-` forces stdin); exit codes
  0 ok / 1 error / 2 usage. Stdlib only — no new dependencies anywhere
  in the phase.
- **`cmd/kbased`**: mounts the API via a new optional
  `daemon.Options.Routes` hook (the Phase 0 skeleton otherwise
  unchanged); warns at startup when the admin API is disabled.
- **labd wiring**: `claude.Options.KBase` (a small `KBaseTokenSource`
  interface) + `addKBaseEnv` next to Phase 6's `addLabEnv` in
  `driver.go` — the file Phase 8 also touches; the addition is one
  call site, one interface, one method at the bottom. `cmd/labd`
  wires a `kbclient.TokenProvisioner` when an admin token is
  configured.
- **Config**: `[kbased]` gains `url`, `kbase_url_for_agents`,
  `admin_token` (+ `LAB_KBASED_URL`,
  `LAB_KBASED_KBASE_URL_FOR_AGENTS`, `LAB_KBASED_ADMIN_TOKEN`);
  `lab.example.toml` documents all three. `make build` now also
  builds `bin/kbase` (handy for the human-token demo flow).

## Decisions recorded (as the handoff asked)

1. **Admin auth = config-designated admin token**, not a
   localhost-only listener. On Docker Desktop (macOS — the dev
   platform) `host.docker.internal` reaches services bound to the
   host's `127.0.0.1`, so a localhost listener would *not* actually
   keep agent containers out. A shared secret in `lab.toml`
   (gitignored) or `LAB_KBASED_ADMIN_TOKEN` does. It is compared
   constant-time, never stored in the database, and has **no default**
   (public repo, no secrets): empty ⇒ the admin API answers 403 and
   both daemons log a startup warning.
2. **kbase is a dependency, not a hard requirement**: if provisioning
   fails at container create (kbased down, admin API disabled), labd
   logs a warning and starts the agent *without* `KBASE_URL`/
   `KBASE_TOKEN` (10s timeout so a hung kbased can't stall container
   create). Covered by a driver test with a failing provisioner.
3. **FTS shape**: a generated tsvector cannot reference another table,
   so "title + content + slug" is two generated columns — title
   (weight A) + content (weight B) on `entry_versions`, slug (weight
   A, dashes split) on `entries` — both GIN-indexed, concatenated at
   query time for match and `ts_rank`. Current-version-only via a
   max(version_no) subquery; excerpts via `ts_headline` with
   `**bold**` markers.
4. **Content is `text` (markdown), not jsonb** — the handoff's
   explicit call overrides DESIGN.md's older `content jsonb`.
5. **Scope semantics**: project tokens read their project + global and
   write only their project (appending to a global entry is 403);
   scope-less tokens read/write everything, write global by default,
   and can target a project via `project_id`. Same slug in several
   scopes: project tokens prefer their own project's entry; scope-less
   callers get 409 and disambiguate with `project_id`/`?project=`.
   Token rotation is per (principal, project-scope): `revoke_existing`
   at mint revokes exactly that scope's live tokens.
6. **Provenance is server-derived only**: the author of every version
   is the principal behind the request token; no request field can set
   it. Token plaintext exists only in the mint response, memory, and
   the container env — at rest it is a SHA-256 hash (the token-auth
   test asserts no stored token is anything but a 32-byte hash).

## Tests (all live-Postgres, skipping gracefully; verified they skip)

`internal/kbased` (through kbclient over httptest, so it doubles as
the kbclient integration suite): immutability (append + verbatim old
versions + 4xx on mutation-shaped requests + trigger blocks direct
SQL), provenance (agent v1, human v2, both in history), scoping (full
matrix incl. shadowed slugs), recall (12 fixtures; keyword ranking,
phrase query, slug-word match, type filter, `n` cap, current-version-
only), token auth (401 matrix, rotation, admin-vs-agent tokens, no
plaintext at rest), admin-disabled 403, Slugify unit. `cmd/kbase`:
CLI round-trip add → show → update → history → recall → list + exit-
code checks. `claude`: `addKBaseEnv` inject/degrade/no-op.

## Demo (for re-run)

```sh
make db-up migrate-lab migrate-kbase build
export LAB_KBASED_ADMIN_TOKEN=$(openssl rand -hex 32)
./bin/kbased &            # or a second terminal with the env set
make startd               # labd needs the same LAB_KBASED_ADMIN_TOKEN
# create a project + agent in the TUI (make startc), start the agent, then:
docker exec -it <agent-container> kbase add decision --title "Use Postgres" -m "Single instance, two schemas."
docker exec -it <agent-container> kbase recall "postgres"
docker exec -it <agent-container> kbase show use-postgres --history
# human second version (host):
export KBASE_URL=http://127.0.0.1:7720
PRIN=$(curl -s -H "Authorization: Bearer $LAB_KBASED_ADMIN_TOKEN" -d '{"kind":"human","external_id":"you"}' $KBASE_URL/admin/v1/principals | jq -r .id)
export KBASE_TOKEN=$(curl -s -H "Authorization: Bearer $LAB_KBASED_ADMIN_TOKEN" -d "{\"principal_id\":\"$PRIN\"}" $KBASE_URL/admin/v1/tokens | jq -r .token)
./bin/kbase update use-postgres -m "Amended by a human."
./bin/kbase show use-postgres --history   # both authors visible
```

The host-side half of this (kbased + admin mint + full CLI cycle with
provenance in history) was executed live during the phase; the
in-container half needs a running agent, which is the orchestrator's
re-run. The provisioning path the container half depends on
(`RegisterPrincipal` + `MintToken` exactly as labd calls them) is what
the CLI round-trip test drives.

## Notes for the orchestrator

- **Image hashes change**: `cmd/kbase` went from a printf stub to a
  real client, so the builder's content hash (which covers the
  embedded CLI binaries) yields new tags and a one-time rebuild per
  stack on first use. The image story itself is unchanged — verified
  via the runtime builder tests, which cross-compile the new CLI.
- **`driver.go` merge with Phase 8**: my change is the one-line call
  after `addLabEnv`, the `KBase` option/field, and the
  `KBaseTokenSource` + `addKBaseEnv` block above `setStopped` —
  nothing inside `addLabEnv` itself.
- `daemon.Options` gained the optional `Routes` hook (labd doesn't use
  the daemon package; only kbased is affected).
- Out of scope per handoff: graph/edges, tickets/CAS behavior (the
  `component`/`ticket` types are accepted and stored already),
  pgvector, TUI views, `lab-agent`.
