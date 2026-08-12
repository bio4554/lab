# Phase 1 report — lab schema & store layer

Branch: `phase-01-store`. All acceptance criteria pass (see below).

## What was built

- **Migration** `migrations/lab/00002_core_tables.sql`: all seven
  tables from DESIGN.md "lab schema" (`credentials`, `projects`,
  `agents`, `sessions`, `turns`, `events`, `usage_rollups`), fully
  schema-qualified, `uuidv7()` PK defaults (Postgres 18). Indexes:
  the `(session_id, seq)` unique constraint on events, plus
  `events (agent_id, id)` and `turns (agent_id, status, created_at)`.
  Real Down drops all seven tables in dependency order.
- **Store package** `internal/labd/store`: `store.New(pool) *Store`,
  typed structs with `db` tags (scanning via pgx v5
  `RowToStructByName`), plain schema-qualified SQL, no ORM. Files
  split per table: `projects.go`, `credentials.go`, `agents.go`,
  `sessions.go`, `turns.go`, `events.go`, `rollups.go`; enum string
  constants in `types.go`.
- **Tests** (`internal/labd/store/*_test.go`): live-Postgres
  integration tests, `LAB_TEST_DSN` override, graceful skip when
  unreachable (verified). Cover migration up/down/up round-trip, CRUD
  + targeted updates, session lifecycle/chaining, turn queue FIFO +
  serial-per-agent + a 10-goroutine race, event seq under 25
  concurrent appenders (gapless 1..N), LISTEN/NOTIFY receipt, and
  rollup accumulation.

## Key implementation decisions

- **Turn queue serialization**: `NextQueuedTurn` takes a per-agent
  `pg_advisory_xact_lock` (key: `hashtextextended('lab.turn_queue:' ||
  agent_id)`) before flipping the oldest queued turn to running. A
  pure `FOR UPDATE SKIP LOCKED` / EvalPlanQual approach can admit two
  running turns in a rare interleaving (recheck moves the LIMIT scan
  to the next queued row while the NOT-EXISTS subquery still sees the
  old snapshot), so the lock is the simple, provably-serial choice.
  `FinishTurn` needs no lock — it only removes a running turn.
- **Event seq**: unique-constraint-plus-retry, per the handoff's
  first option. `INSERT ... SELECT COALESCE(MAX(seq),0)+1` races on
  the `(session_id, seq)` unique; losers retry (lock-free progress —
  every conflict means another writer committed). `pg_notify` fires
  in the same transaction as the insert, so the notification is only
  delivered on commit and carries the real event id.
- **Migration round-trip test is hermetic**: it rewrites the real
  `migrations/lab/*.sql` into a throwaway schema (string-replacing the
  `lab.` qualifier) and runs Up/Down/Up there, so the test never drops
  the dev database's actual lab tables while other packages' tests run
  in parallel. The real stream's down/up was exercised via
  `go run ./cmd/migrate -stream lab down` / `up` (acceptance).
- **Test hygiene**: tests create their own fixture rows (unique names
  via uuid suffix) and delete exactly what they created in cleanup —
  no truncation of shared tables, so a dev database with real data
  survives `make check`, and repeated runs stay green (verified: zero
  leftover rows after the suite).

## API surface notes (indicative names → final)

- "Nothing available" is `nil, nil` rather than an error for
  `NextQueuedTurn` and `CurrentSession` (normal states, not failures);
  everything else missing returns `store.ErrNotFound`.
- `FinishTurn(id, status, errMsg)` accepts only `done`/`error`,
  requires the turn be `running` (else `ErrNotFound`), stores errMsg
  only for `error`, and stamps `finished_at`.
- `EndSession` only ends open sessions (`ended_at IS NULL`), so it
  can't clobber an already-recorded end reason.
- `SetAgentContainer` takes `*string`; nil clears the column.
- Extras beyond the required list, added because tests/later phases
  want them: `GetProjectByName`, `GetSession`, `GetTurn`,
  `ListAgents(projectID)`.
- `Store` exposes `NotifyChannel = "lab_events"` for Phase 5/6
  listeners.

## Deviations & choices where the handoff was silent

1. **Column nullability finalized**: `status`, `state`, `budget`,
   `role_prompt` are NOT NULL with defaults; `model`, `container_id`,
   `claude_session_id`, `expires_at`, `error`, `source_id`,
   `session_id` (on turns), `turn_id` (on events) are nullable, per
   DESIGN.md semantics.
2. **No FK cascades**: deletes fail while dependent rows exist. labd
   owns teardown ordering; silent cascade of an agent's event history
   seemed wrong for an append-only log.
3. **`cost_usd` stays `numeric` in SQL** (accumulation happens
   server-side, no float drift) but is read/written as `float64` in
   Go (`::float8` on the aggregate) — monitoring precision, not
   accounting.
4. **`turns` column is `int`** (rollup counter); Go `UsageDelta.Turns`
   is `int32` to match.
5. **`credentials.status` is free-text** (default `'active'`, no CHECK)
   — DESIGN.md never enumerates its values; Phase 8 can constrain it.
6. One new dependency: `github.com/google/uuid` (allowed).

## Acceptance results (run 2026-08-12)

- `docker compose down -v && make db-up && make migrate-lab && make
  check` — green from a fresh volume.
- `go run ./cmd/migrate -stream lab down` → all seven tables dropped;
  `up` restores them.
- `go test -race -count=1 ./internal/labd/store/` — all 12 tests pass,
  including the concurrent-seq and concurrent-queue tests.
- Suite skips cleanly when Postgres is unreachable (verified with a
  bogus `LAB_TEST_DSN`).

## Open questions

1. `cmd/migrate` prints "up to date" even when it just applied
   migrations (Phase 0 message, unconditional). Cosmetic; left alone —
   out of my file set.
2. Should `DeleteProject`/`DeleteAgent` eventually cascade (or
   soft-delete)? Phase 5/6 will know what teardown really needs.
3. `AppendEvent` retries unique-violation losers indefinitely (each
   loss implies another writer's commit, and it honors context
   cancellation). If a bounded retry count is preferred, it's a
   two-line change.
