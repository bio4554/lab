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

---

## Orchestrator review — BOUNCED (2026-08-13)

Most of the phase verified clean: full diff read; `make check` green;
new suites `-race` twice; secret scan clean; `make e2e` green twice
(13.4s / 12.6s); the boot sweep repaired real leftover state on its
first dev-machine run (four stale container ids); port-bind retry,
migrate output, spawn 409s, TUI signal handling, and `agent run`
removal all verified. One finding class blocks the close:

### Fix 1 (blocking): the boot sweep destroys other instances' agents

`make e2e` on a dev machine **deletes every real agent's container
and `.claude` session volume**. Two contributing defects, one root
assumption:

- The e2e harness teardown (`removeAgentContainers`) filtered on the
  *presence* of the `lab.agent-id` label with `All: true` — matching
  every lab container on the machine, not just the suite's. **Already
  fixed on this branch by the orchestrator** (cleanup now scoped to
  the agent ids in the `lab_e2e` database); a canary container +
  volume survives the suite. Keep this fix.
- The remaining and deeper defect: the e2e labd's **boot sweep** does
  the same thing by design — it lists all `lab.agent-id` containers,
  looks them up in *its* (empty `lab_e2e`) database, concludes
  "deleted agent", and removes container + volume. Observed live: a
  dev agent's conversation history destroyed; its driver then
  crash-looped on `--resume` ("No conversation found", replace, 3s
  backoff, forever).

The sweep's "unknown container ⇒ deleted agent ⇒ remove" rule is only
sound when one labd owns every lab container on the Docker daemon —
an assumption the handoff itself baked in (orchestrator's error, the
implementation followed spec) and that the e2e suite violates by
running a second labd against a second database on the same daemon.

**Required fix — deployment identity on containers:**

- `runtime.Spec`/`Create` stamps a new label (e.g.
  `lab.deployment=<id>`) on every agent container. Derive the id from
  the daemon's database identity — e.g. hash of
  `pg_control_system().system_identifier` + the database OID — so two
  labds on different databases can never claim each other's
  containers. No migration needed if derived; if you persist one
  instead, record why.
- The sweep treats containers **without a matching deployment label
  as foreign: skip and log (INFO), never remove.** Unlabeled
  (pre-fix) containers are foreign too — they get replaced naturally
  by their own daemon's driver; removal of true orphans can stay
  manual for now.
- The e2e suite asserts the property: plant a foreign-labeled (and an
  unlabeled) canary container + volume before the suite; they must
  survive the whole run including the kill-9 restart sweep.
- `runtime.List` should surface the label so both the sweep and the
  e2e teardown can filter on it (the teardown's DB-scoped fix can
  stay as defense in depth).

### Fix 2 (bounded, same branch): resume crash-loop

A deterministically unresumable session (claude exits immediately;
stderr "No conversation found with session ID") currently
replace-loops forever at 3s intervals and wedges the agent. Add a
bounded fallback: after N (suggest 3) consecutive immediate exits of
a `--resume` process, clear the session's `claude_session_id`, log
loudly (WARN), and start fresh — continuity is already lost at that
point; the agent should recover instead of wedging. Recovery via
`retire` remains for humans. Cover with a driver test (fake runner:
resume-exits-instantly ×3 → fresh start without resume).

### Not blocking, recorded

- Dev-machine recovery from the incident: demo11's agents were
  retired to fresh sessions (history unrecoverable); any other agent
  with a stale `claude_session_id` (`rev10`, `impl1`) will crash-loop
  on next start until retired — or until Fix 2 lands, which handles
  it automatically.
- The acceptance "manual kill -9 demo with a real agent" was
  pre-empted by this incident; the orchestrator will run it during
  the re-review.
