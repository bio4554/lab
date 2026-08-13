# Phase 12 handoff — integration hardening

You are the implementation agent for Phase 12 of the `lab` project —
the last planned phase. This document is your complete brief. Read
`docs/DESIGN.md` in full and skim the phase 05/06/08/11 reports for
the driver/API/budget/orchestration behavior you must not break.

The system works end-to-end (phases 0–11: real orchestrator + worker
completed ticketed work in the live demo). Phase 12 makes it **honest
software**: labd survives its own death cleanly, everything shuts
down gracefully, logs are uniform, a credential-free e2e suite proves
the whole loop deterministically, and a handful of known small
defects get fixed.

**Your file set**: effectively the whole repo *except* `docs/DESIGN.md`,
`docs/PLAN.md`, and other phases' semantics — this phase hardens
behavior, it does not redesign contracts. No schema changes are
expected; if you need a migration, record why.

## Ground rules

- Branch `phase-12-hardening` off `development`.
- Stay in scope; smallest reasonable calls, recorded in the report.
- `make check` green when you stop; Postgres/Docker-dependent tests
  skip gracefully.
- **Public repo — no secrets.** The e2e suite must need no real
  credential (that is part of its point).
- Allowed new deps: none expected.
- Finish with `docs/handoffs/phase-12-report.md`.

## Deliverables

### 1. Crash-recovery reconciliation (the headline)

`cmd/labd/main.go` currently does simple recovery (restart drivers
for previously-active agents; the comment marks full reconciliation
as Phase 12). Replace it with a boot **sweep** that reconciles
container reality against DB state before serving traffic:

- Enumerate `lab-agent-*` containers via the runtime's label filters.
  - Container whose agent no longer exists in the DB → remove it
    (and its `.claude` volume).
  - Agent rows with a stale `container_id` (container gone or
    replaced) → clear/correct the recorded id.
- **Turns stuck `running`** (their pump died with the old daemon) →
  fail them cleanly: `FinishTurn(..., error, "labd restarted
  mid-turn")`. Queued turns stay queued — they deliver once the
  driver is back.
- Agents in state `working` with no live driver → back to `idle`
  (they were mid-turn when the daemon died); `stopped`/`retired`/
  `paused` states are respected as-is.
- Then restart drivers for active agents (the existing behavior),
  which replace containers as they already do (StdinOnce means an
  unattached claude is dead by design — `--resume` carries
  continuity).
- The sweep must be idempotent and logged (one line per action taken,
  none when there is nothing to do).

### 2. Graceful shutdown audit + the port-bind race

- Audit every binary for SIGINT/SIGTERM behavior: labd (driver drain
  exists — verify the grace path end-to-end), kbased, the TUI
  (terminal restored, no orphaned SSE goroutines), labctl. Fix what
  you find; document the expected shutdown sequence per binary in the
  report.
- **Port-bind retry**: a labd started while the previous labd is
  still draining exits immediately with `bind: address already in
  use` (hit live during the phase-11 review). On startup, retry
  binding both listeners for a bounded window (~10s, logged) before
  giving up.

### 3. Structured-logging pass

slog everywhere, one line per event, consistent keys (`daemon`,
`agent`, `project`, `session`, `turn`, `container`, `error`), sensible
levels (state changes INFO, recoverable oddities WARN, failures
ERROR). No `fmt.Print*` diagnostics left in daemon code paths. Do not
log secrets (the existing reviews assert none; keep it that way).

### 4. Known-defect fixes (all small, all previously observed)

a. **`cmd/migrate` lies**: it prints "up to date" even when it just
   applied migrations (bitten three times across reviews). Make it
   report what actually happened ("applied N migration(s)" vs "up to
   date").
b. **Retire `labctl agent run`**: it runs an in-process driver that
   lacks labd's lab-API/kbase/poke wiring, so its containers get only
   the credential env var — a trap (bitten during the phase-10
   review). Remove the command (and labctl's driver construction);
   point users at the client API / TUI start path. Record the removal
   in the README's development section if it is mentioned anywhere.
c. **Spawn-name collision is a 500**: map the unique violation on
   `(project_id, name)` to a 409 with a clear message, on both the
   agent API spawn and the client API agent create.

### 5. `make e2e` — deterministic end-to-end suite (no credential)

A build-tagged suite (`//go:build e2e`, wired as `make e2e`) that
drives the **real stack** — labd + kbased + Postgres + real Docker
containers — with a **scripted stand-in for the `claude` binary**
that speaks valid stream-json deterministically (init → a few events
→ result with usage; honors stdin turns; exits on stdin close). How
the stand-in gets into the container is your call (a test-only stack,
a builder hook, or a PATH shim in the test image) — record it. The
suite must cover at least:

1. project create → agent create → start → turn → events persisted →
   result closes the turn (assert usage rollup rows appear);
2. second turn delivers without a restart;
3. **kill -9 labd mid-turn → restart → the §1 sweep runs**: the stuck
   turn is errored (not wedged), the agent recovers, and a fresh turn
   completes;
4. retire → successor session chained, seed turn delivered;
5. budget deny → raise → release (reuse the phase-8 shape against the
   stub).

Acceptance: `make e2e` green **twice consecutively** on a dev machine
with Docker + compose Postgres up. It must skip (not fail) when
Docker or Postgres is absent.

### 6. README true-up

After the above lands, re-verify the README's bootstrap and
development sections against reality (migrate output changed, `labctl
agent run` removed, `make e2e` exists). Small edits only — the
structure is fresh (rewritten at phase 9 close).

## Tests

Beyond the e2e suite: unit tests for the reconciliation decision
table (container-without-agent, stale container_id, running-turn
cleanup, working-state reset — against a live store with fake runtime
listings); port-bind retry (occupy the port, assert bounded retry
then success/failure); migrate output (applied vs up-to-date paths);
spawn 409s.

## Acceptance criteria

- `make check` green; new tests pass with `-race`.
- `make e2e` green twice consecutively (I will run it twice myself).
- Manual demo (documented; I re-run): with a real agent mid-turn,
  `kill -9` labd → restart → logs show the sweep's actions, the turn
  is errored with "labd restarted mid-turn", the agent answers the
  next turn normally.
- No behavior regressions in the phase 5–11 demos (I will spot-check
  the orchestration loop).
- No secrets anywhere; e2e needs no credential.

## Out of scope

Filing the backlog as kbase tickets (deferred by the user — the
curated list stays with the orchestrator for now); TUI features and
polish (credential screens, transcript ring buffer, tab-digit
behavior, SSE per-session filter, N+1 listing, usage display split,
cross-project rollup view); scoping the repo.git container mount
tighter; cross-process rebind pokes; `--permission-prompt-tool`;
egress policies; pgvector; multi-machine anything.
