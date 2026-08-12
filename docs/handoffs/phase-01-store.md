# Phase 1 handoff — lab schema & store layer

You are the implementation agent for Phase 1 of the `lab` project. This
document is your complete brief. Read `docs/DESIGN.md` in full first —
especially "lab schema", "Event log", "Agent runtime > Turns", and
"Credentials & budgets". `docs/PLAN.md` shows where this phase fits.

Phase 0 landed the scaffold: config (`internal/config`), migration
streams (`internal/migrate`, `migrations/lab/`), daemon skeletons, and a
dev Postgres (`make db-up`). Read `docs/handoffs/phase-00-report.md` for
its decisions. You are building the **lab schema migrations and the
typed store package** on top of that.

Phases 2, 3, and 4 run in parallel with you on other branches. You will
not conflict with them if you stay in your file set: `migrations/lab/`,
`internal/labd/store/`. Don't touch `go.mod` beyond your allowed deps.

## Ground rules

- Branch `phase-01-store` off `development`.
- Stay in scope. If an interface contract below seems wrong or
  incomplete, make the smallest reasonable call and record it in your
  report — do not redesign.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- Tests are part of the deliverable. `make check` must pass when you
  stop (DB-backed tests must skip gracefully without Postgres, like
  Phase 0's).
- **This repo is public on GitHub.** Never commit secrets. Local dev DB
  credentials (`lab:lab`) are fine.
- Allowed new deps: `github.com/google/uuid`. Anything else: justify in
  the report.
- Finish by writing `docs/handoffs/phase-01-report.md`: what you built,
  deviations, open questions.

## Deliverables

### 1. Migrations (`migrations/lab/000NN_*.sql`)

Tables per DESIGN.md "lab schema", in the `lab` schema (qualify all
names). Postgres 18 — use `uuidv7()` for UUID primary key defaults.
Concretely:

- `projects(id uuid pk, name text unique not null, origin_kind text
  check (origin_kind in ('git_url','local_path')), origin text not
  null, stack text not null, default_credential_id uuid null references
  credentials, created_at timestamptz not null default now())`
- `credentials(id uuid pk, kind text check (kind in
  ('api_key','oauth_token')), secret_enc bytea not null, label text not
  null, status text not null default 'active', expires_at timestamptz
  null, created_at ...)`
- `agents(id uuid pk, project_id fk not null, name text not null,
  role_prompt text not null default '', model text null, credential_id
  fk null, budget jsonb not null default '{}', state text check (state
  in ('stopped','idle','working','paused','retired')) default
  'stopped', container_id text null, branch text not null, created_at
  ...)`, unique `(project_id, name)`.
- `sessions(id uuid pk, agent_id fk not null, claude_session_id text
  null, started_at ..., ended_at timestamptz null, end_reason text
  null, prev_session_id uuid null references sessions)`
- `turns(id uuid pk, agent_id fk not null, session_id fk null,
  source_kind text check (source_kind in ('user','agent')), source_id
  uuid null, content text not null, status text check (status in
  ('queued','running','done','error')) default 'queued', error text
  null, created_at ..., finished_at timestamptz null)`
- `events(id bigint generated always as identity pk, agent_id fk not
  null, session_id fk not null, turn_id fk null, seq bigint not null,
  kind text not null, payload jsonb not null, ts timestamptz not null
  default now())`, unique `(session_id, seq)`.
- `usage_rollups(credential_id fk, agent_id fk, window_start
  timestamptz, tokens_in bigint, tokens_out bigint, cost_usd numeric,
  turns int, pk (credential_id, agent_id, window_start))`
- Indexes to support: events by `(session_id, seq)` (the unique
  covers it), events by `(agent_id, id)`, turns by `(agent_id, status,
  created_at)` for queue polling.

Real `Down` migrations (drop the tables). Column details are yours to
finalize where unstated; record choices in the report.

### 2. Store package (`internal/labd/store`)

`store.New(pool *pgxpool.Pool) *Store` plus typed methods and structs.
Required surface (names indicative; adjust idiomatically and record):

- Projects/credentials/agents: Create/Get/List/Delete plus targeted
  updates the later phases need (`UpdateAgentState`,
  `SetAgentContainer`, `UpdateCredentialStatus`).
- Sessions: `CreateSession` (optionally chained via `prev_session_id`),
  `EndSession(id, reason)`, `SetClaudeSessionID`, `CurrentSession
  (agentID)`.
- Turn queue: `EnqueueTurn`, `NextQueuedTurn(agentID)` →
  atomically flips exactly one `queued` → `running` (only when the
  agent has no other `running` turn — turns are serial per agent),
  `FinishTurn(id, status done|error, errMsg)`.
- Events: `AppendEvent(sessionID, agentID, turnID, kind, payload)` —
  assigns `seq` atomically per session (**no gaps or duplicates under
  concurrent writers**; unique constraint + retry, or a
  per-session counter — your call, tested either way), then `NOTIFY
  lab_events` with a small JSON payload `{event_id, agent_id,
  session_id}` (use `pg_notify`). Also `EventsSince(sessionID, afterSeq,
  limit)` and `EventsSinceID(afterEventID, limit)` for tailing.
- Rollups: `AddUsage(credentialID, agentID, windowStart, deltas...)` —
  upsert-accumulate; `UsageInWindow(credentialID, since)`.

All queries schema-qualified (`lab.events`); no ORM, plain pgx.

### 3. Tests

Live-Postgres integration tests (skip gracefully when unreachable,
`LAB_TEST_DSN` override, like Phase 0's) covering at minimum:

- Migration up/down round-trip for the new files.
- Turn queue: serial-per-agent guarantee (a second `NextQueuedTurn`
  returns nothing while one turn is `running`), FIFO order.
- **Event seq under concurrency**: N goroutines appending to one
  session produce exactly N events with seq 1..N.
- LISTEN receives a notification on append.
- Rollup accumulation.

Tests must leave the dev database clean (dedicated throwaway schema, or
truncate what you touched) so repeated runs pass.

## Acceptance criteria

- `make db-up && make migrate-lab && make check` green from clean.
- `go run ./cmd/migrate -stream lab down` steps back cleanly (real
  Downs), `up` restores.
- The concurrency test above passes with `-race`.
- Schema matches DESIGN.md; deviations recorded in the report.

## Out of scope

kbase schema (Phase 9); any daemon/API wiring (Phases 5–6); encryption
of `secret_enc` contents (Phase 8 — the store treats it as opaque
bytes); docker/git anything.
