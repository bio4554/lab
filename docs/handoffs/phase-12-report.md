# Phase 12 report — integration hardening

Branch: `phase-12-hardening`. `make check` green; the new suites pass
with `-race`; `make e2e` green twice consecutively on a dev machine
with Docker + compose Postgres up. No new dependencies. No schema
changes. No secrets anywhere — the e2e suite runs the whole stack with
a fake API-key string and a scripted claude stand-in; nothing talks to
Anthropic.

## §1 Crash-recovery reconciliation (the headline)

`cmd/labd/main.go`'s "simple recovery" block is replaced by a boot
**sweep** (`claude.Sweeper.Sweep`, `internal/labd/claude/sweep.go`)
that reconciles container reality against DB state after the store is
reachable and **before any driver starts or a listener comes up**:

- containers labelled `lab.agent-id` whose agent no longer exists in
  the DB → force-removed, along with their `lab-claude-<id>` volume
  (volume removal is best-effort — it may legitimately not exist);
- agents with a recorded `container_id` that is gone from the listing
  → cleared; replaced by a different container → corrected to the
  actual id;
- turns stuck `running` (their pump died with the old daemon) →
  `FinishTurn(error, "labd restarted mid-turn")`; queued turns are
  untouched and deliver once the driver is back;
- agents in state `working` → back to `idle` (on boot, no driver is
  alive by definition); `stopped`/`paused`/`retired` respected as-is.

Then the existing behavior: drivers restart for active agents and
replace containers as they always did (`--resume` carries continuity).

Notes on the smallest-call decisions:

- The sweep is a separate `Sweeper` struct with a narrow
  `ContainerRuntime` interface (List/Remove/RemoveVolume), so the
  decision table is unit-testable against a **live store with a fake
  runtime listing** (`sweep_test.go`) — exactly the shape the handoff
  asked for. Because the sweep's queries are global (all agents, all
  running turns), its test runs in a dedicated throwaway database
  (`lab_sweep_test`, created/migrated/dropped by the test) rather than
  the shared dev DB, where it would clobber other package-parallel
  suites' in-flight rows.
- A failed sweep (e.g. Docker down) logs a warning and the daemon
  continues: labd without Docker can still serve the API, and the next
  boot repairs. Individual repair failures are logged per-row and
  joined; the sweep always visits everything it can.
- Idempotent and logged: one INFO line per action taken
  (`sweep: ...`), nothing when there is nothing to do — asserted by
  the decision-table test's second run.
- Store additions: `AllAgents`, `AllRunningTurns` (both trivial
  selects). `Driver.Run`'s per-agent orphaned-turn check stays as a
  safety net for the driver-restart-within-a-live-daemon case (its
  message remains "orphaned by driver restart").

## §2 Graceful shutdown audit + the port-bind race

**Port-bind retry** — `daemon.Listen`
(`internal/daemon/listen.go`): binds a TCP listener, retrying
`EADDRINUSE` every 500ms for a bounded window (default
`BindRetryWindow` = 10s), one INFO line per retry, immediate failure
on any other error. labd binds **both** listeners through it up front
(a failure on the second closes the first); `daemon.Run` (kbased) uses
it too. Tests (`listen_test.go`): occupied port released mid-window →
success; permanently occupied → bounded failure; free port → immediate
success.

**Audit findings and the expected shutdown sequence per binary:**

- **labd** — SIGINT/SIGTERM → drivers stop first (`Manager.StopAll`,
  30s bound; each pump closes stdin and grace-drains events ≤15s, so
  in-flight generations persist), then SSE request contexts are
  cancelled and both HTTP servers drain (10s bound), then the
  LISTEN/NOTIFY hub stops, then pools close. Verified end-to-end: the
  e2e teardown SIGTERMs labd mid-operation and the log shows the full
  sequence ending in "shutdown complete". No changes needed.
- **kbased** — shared `daemon.Run` skeleton: HTTP drain (10s) then
  pool close. No changes needed.
- **lab (TUI)** — fixed: the SSE stream was started with
  `context.Background()` and never cancelled, orphaning the stream
  goroutine on quit; the program also had no SIGTERM handling. Now one
  `signal.NotifyContext` bounds the whole client — it is passed to
  `tea.WithContext` (bubbletea restores the terminal on cancellation)
  and to `StreamEvents`, so quitting or a signal ends the stream
  goroutine deterministically; a signal-driven `ErrProgramKilled` is
  treated as a clean exit.
- **labctl** — already runs every subcommand under
  `signal.NotifyContext`; all remaining subcommands are short-lived
  (the long-running `agent run` is gone, §4b). No changes needed.

## §3 Structured logging

The daemon paths were already slog-structured; this pass verified and
tightened rather than rewrote. State: root loggers carry
`daemon=labd|kbased`; keys in use are the agreed set (`agent`,
`project`, `session`, `turn`, `container`, `credential`, `error`, plus
locals like `tag` for image builds); levels follow the contract (state
changes INFO, recoverable oddities WARN, failures ERROR, chatter
DEBUG). `grep` confirms **no `fmt.Print*` diagnostics in any daemon
code path** — the only hits are a hash writer and the SSE wire
protocol, both wire format, not diagnostics. No secrets are logged
(the phase 6/8/9 review assertions still hold; new code logs ids and
names only). New sweep/listen logging follows the same conventions.

## §4 Known-defect fixes

a. **`cmd/migrate` now tells the truth**: `migrate.Stream.Up` returns
   the applied-migration count (from goose's results), and the CLI
   prints `applied N migration(s)` vs `up to date`.
   `TestSchemaCurrency` asserts 2-then-0 across consecutive Ups.
b. **`labctl agent run` removed**: the subcommand now returns a
   pointed error directing to labd (TUI /
   `POST .../agents/{agent}/start`), and the full in-process driver
   construction is gone. `labctl retire` — the one remaining driver
   user — now builds a minimal Store+Runtime driver (`retireDriver`),
   which is all `Retire` needs (it only stops a container, never
   provisions). README development section records the removal.
c. **Spawn-name collision is a 409**: `store.CreateAgent` (and
   `CreateProject`, same class) detect SQLSTATE 23505 and return a
   wrapped `store.ErrDuplicateName` with a clear message ("agent %q
   already exists in this project"); `writeError` maps it to 409 for
   both APIs. Covered in `spawn_test.go` (agent-API duplicate spawn →
   409; client-API duplicate create → 409).

## §5 `make e2e` — the deterministic end-to-end suite

`e2e/e2e_test.go` (`//go:build e2e`, wired as `make e2e`:
`go test -tags e2e -count=1 -timeout 30m ./e2e`). It drives the **real
stack**: real `labd` + `kbased` binaries (built by the suite), the
compose Postgres, real Docker containers running a scripted claude
stand-in.

**How the stand-in gets into the container** (the recorded call): a
builder hook, not a test-only stack. `runtime.BuilderOptions` gains
`ClaudeStubPath`; when set, the base image build (a) skips the real
CLI install layer and (b) installs the given host binary as
`/usr/local/bin/claude`. The build context always carries a `stub/`
directory (empty in real builds — Dockerfiles have no conditional
COPY, so the Dockerfile installs `stub/claude` iff present); the
stub's content hash joins the image tag, so **a stub image can never
alias a real one**. labd exposes the knob as
`[labd] claude_stub_path` / `LAB_LABD_CLAUDE_STUB_PATH` and logs a
loud WARN when it is set. The stub itself (`e2e/claudestub`) is a
~150-line Go program speaking faithful minimal stream-json: init with
session id (honoring `--resume`), per stdin user-message an assistant
echo + a result with fixed usage numbers; `sleep:N` delays the result
(the kill test's lever); exits on stdin close (StdinOnce semantics).

**Isolation**: dedicated `lab_e2e` database (dropped/recreated per
run, both streams migrated), temp data dir, ephemeral ports, every
config key pinned via env so no `lab.toml` leaks in. Teardown:
SIGTERM both daemons (exercising the graceful path every run), then
force-remove agent containers + volumes. Skips (not fails) without
Docker or Postgres.

**Coverage** (ordered subtests, each building on the last):

1. `01_core_loop` — credential (fake key) → project (local git
   origin) → agent (kind-bound credential) → start → turn → events
   persisted with the echo → result closes the turn → usage_rollups
   rows exist.
2. `02_second_turn_same_process` — second turn completes, same
   session.
3. `03_kill9_recovery_sweep` — `sleep:600` turn reaches `running`,
   labd is SIGKILLed, restarted; because the sweep runs before the
   listeners come up, a healthy `/healthz` implies it finished: the
   stuck turn is errored with exactly "labd restarted mid-turn" and a
   fresh turn completes.
4. `04_retire_chains_successor` — retire → response chains
   `prev_session_id` → the seed turn's echo appears in the successor
   session.
5. `05_budget_deny_raise_release` — `max_turns_hour: 1` with several
   turns already used this hour → submitted turn stays `queued`
   through multiple pump cycles (denied, never errored) → raising the
   budget releases it to completion with no other intervention.

Observed: first run builds the base image (network for apt; the real
CLI layer is skipped); subsequent runs hit the content-addressed cache
and the suite completes in well under a minute. Run twice
consecutively: green, green.

## §6 README true-up

Development section: `make e2e` added with a one-line description;
migrate output change noted on the `migrate-lab`/`migrate-kbase` line;
`labctl agent run` removal recorded (agents start through labd only).
Bootstrap section re-read against reality — no changes needed (it
already described the TUI/labd start path, never `agent run`).

## Deviations & smallest-call log

- `CreateProject` also maps 23505 → `ErrDuplicateName` (same defect
  class, two lines, shared helper); handoff named agents only.
- Sweep failure is WARN-and-continue rather than fatal, so labd still
  serves without Docker (see §1 notes).
- `daemon.Listen` is shared with kbased rather than labd-only: the
  same restart race exists there and the helper is one function.
- The e2e suite skips the *real* Claude Code install layer in its
  images; `make e2e` therefore needs network only for apt on the
  first build, and never touches claude.ai.
- Config gains one key (`labd.claude_stub_path`) — test-only, loudly
  logged, documented in the struct comment; not added to
  `lab.example.toml` on purpose (never set it in real deployments).

## For the reviewer

- Manual demo (as promised in the handoff): with a real agent
  mid-turn, `kill -9 <labd-pid>` → restart labd → boot logs show
  `sweep: erroring turn orphaned mid-turn` (and any container-id
  repairs) before "http listening"; the turn shows
  `error: labd restarted mid-turn`; the next turn answers normally.
  `03_kill9_recovery_sweep` is this exact scenario, automated.
- `make e2e` twice is the acceptance run; on first execution expect
  the one-time base-image apt build.
- The pending-review backlog (phases 8, 9, driver.go conflict) is
  unchanged by this phase except where §1 touched `cmd/labd/main.go`
  and `internal/labd/claude/` — the sweep is additive (new file) and
  the main.go recovery block replacement is localized.
