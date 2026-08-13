# Phase 9 handoff — kbase core: daemon, CLI, recall

You are the implementation agent for Phase 9 of the `lab` project.
This document is your complete brief. Read `docs/DESIGN.md` ("kbase"
in full, plus "Networking & auth") and the phase 00/06 reports. kbase
is a standalone knowledge base — a dependency of lab, developed in
this repo, sharing the Postgres instance in its own schema. One kbase
serves N projects.

Everything in kbase is **versioned, append-only, with provenance**.
This phase delivers the core: entries, versions, tokens/principals,
recall, the `kbased` daemon, the `kbase` CLI, and the labd wiring that
gets tokens into agent containers. Graph + tickets are Phase 10.

**Parallel phases**: 7 (TUI) and 8 (credentials) run concurrently.
Your file set: `migrations/kbase/`, `internal/kbased/` (new),
`internal/kbclient/` (new), `cmd/kbase`, `cmd/kbased`,
`internal/labd/claude/driver.go` (KBASE env vars — a small, contained
addition next to Phase 6's `addLabEnv`; Phase 8 also edits this file,
so keep your change tight and expect the orchestrator to resolve the
merge), `cmd/labd` (kbase client wiring), config. Do not touch
`cmd/lab`, budgets, or credentials.

## Ground rules

- Branch `phase-09-kbase` off `development`.
- Stay in scope; smallest calls recorded; DESIGN/PLAN read-only.
- `make check` green when you stop; dependent tests skip gracefully.
- **Public repo — no secrets.** kbase tokens: hashed at rest,
  plaintext returned once at mint (follow the Phase 6
  `store/tokens.go` pattern).
- Allowed new deps: none expected (pgx/goose/uuid already present).
- Finish with `docs/handoffs/phase-09-report.md`.

## Deliverables

### 1. Schema (`migrations/kbase/00002_*.sql`)

In the `kbase` schema (uuidv7 defaults, real Downs):

- `principals(id, kind agent|human, external_id text, display_name
  text, created_at)` — `external_id` carries the lab agent id (or a
  human handle); unique `(kind, external_id)`.
- `tokens(id, principal_id fk, secret_hash bytea unique, project_id
  uuid null — null scopes to all projects (human/admin tokens),
  created_at, revoked_at)`.
- `entries(id, project_id uuid null — null = global scope, type text
  check in ('decision','note','architecture','component','ticket'),
  slug text, title-less — titles live on versions, created_by fk
  principals, created_at)`; unique `(project_id, slug)` treating null
  project as its own scope (partial indexes).
- `entry_versions(id, entry_id fk, version_no int, title text, content
  text not null — markdown body, author fk principals, created_at)`;
  unique `(entry_id, version_no)`; **no UPDATE or DELETE ever** —
  current = max version_no.
- FTS: generated tsvector over title + content + slug, GIN index,
  query via `websearch_to_tsquery`, ranked (`ts_rank`), most-recent
  version only.

Note: `project_id` is a lab project UUID but kbase does **not** FK
into the lab schema — kbase is standalone; lab project ids are opaque
identifiers to it.

### 2. `kbased` (grow the Phase 0 skeleton)

Bearer-token API (same auth shape as Phase 6's agent API; token →
principal + optional project scope; project-scoped tokens see their
project's entries plus global ones, and write only to their project):

- `POST /v1/entries` (type, slug — generate from title if absent,
  title, content, optional `global: true` for scope-less tokens only)
- `POST /v1/entries/{slug}/versions` (append: title, content)
- `GET /v1/entries/{slug}` (current version; `?version=n`;
  `?history=1` returns the version list with authors/timestamps)
- `GET /v1/entries?type=&limit=` (list, current versions, newest
  first)
- `GET /v1/recall?q=<query>&type=&n=8` — FTS as above; response
  includes slug, type, title, rank, and a content excerpt
  (`ts_headline`)
- Admin (localhost-only listener or a config-designated admin token —
  your call, record it): principal registration + token mint/revoke.
  labd is the caller: it registers agent principals and mints their
  project-scoped tokens.

Provenance: every write records the authoring principal from the
token; it never trusts a client-supplied author.

### 3. `internal/kbclient`

Typed Go client for all of the above (used by the CLI, labd, and later
phases). Integration-tested against a live `kbased` (httptest or real
listener + real Postgres; skip gracefully).

### 4. `cmd/kbase` — the agent-facing CLI (replaces the stub)

Reads `KBASE_URL` + `KBASE_TOKEN` from env (flags override). Commands
per DESIGN.md:

```
kbase add <type> --title t [--slug s] [--global] [-]   # content from stdin or -m
kbase update <slug> [-]                                # append version
kbase show <slug> [--version n] [--history]
kbase list [--type t]
kbase recall "<query>" [--type t] [-n 8]
```

Plain flag parsing, tabular/markdown output readable by both humans
and agents, exit codes that make sense in scripts. Keep the binary
lean — it ships in every agent image (Phase 4's builder already
embeds whatever `cmd/kbase` builds to; check the image story still
works and note the resulting image-hash change).

### 5. labd wiring

- Config: `[kbased]` gains the URL labd should use +
  `kbase_url_for_agents` (the host.docker.internal form), and
  whatever admin-auth setting your §2 decision needs.
- On agent container create (next to `addLabEnv`): ensure a kbase
  principal exists for the agent, mint a project-scoped token
  (revoking prior ones), inject `KBASE_URL` + `KBASE_TOKEN`. If
  kbased is unreachable, log a warning and start the container
  without kbase env rather than failing the agent (kbase is a
  dependency, not a hard requirement — record this decision).

## Tests

Live-Postgres (+ live kbased where needed), skip gracefully:

- Immutability: update appends; old versions still readable verbatim;
  no path mutates a version (attempt → 4xx/error).
- Provenance: every version carries the authoring principal derived
  from the token used.
- Scoping: project token cannot read another project's entries, can
  read global, cannot write global; scope-less token can.
- Recall: seed a dozen fixture entries; assert relevant slugs surface
  for phrase and keyword queries, ranked sensibly; type filter works.
- Token auth: 401 unknown/revoked (same matrix as Phase 6).
- CLI round-trip against a live daemon: add → show → update →
  history → recall.

## Acceptance criteria

- `make check` green in all configurations; new tests pass with
  `-race`; `make migrate-kbase` up/down round-trips.
- Demo (documented; I re-run): start `kbased` + `labd`; create a
  project + agent; start it; `docker exec` into the agent container
  and run `kbase add decision --title "Use Postgres" -m "…"`,
  `kbase recall "postgres"`, `kbase show use-postgres --history`
  from inside — provenance shows the agent principal. Then a second
  version from a human-scoped token shows both authors in history.
- No plaintext token at rest or in logs.

## Out of scope

Components/edges/graph and tickets/CAS (Phase 10 — but your entry
types already include `component`/`ticket` so the enum doesn't churn);
pgvector (post-v1; FTS only); kbase TUI views; `lab-agent` (Phase 11);
budget interactions.
