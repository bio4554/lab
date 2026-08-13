# Phase 11 report — lab-agent & the orchestration loop

Branch: `phase-11-lab-agent`. `make check` green; the new suites pass
with `-race`; `make migrate-lab` up/down round-trips on 00005 (covered
by `TestLabMigrationRoundTrip`, which now steps through all five
migrations in a throwaway schema). No new dependencies. No plaintext
tokens at rest or in logs — agent tokens keep the Phase 6
hashed-at-rest pattern, and spawn only copies a credential *id*, never
secret material.

## What was built

- **Schema** (`migrations/lab/00005_agent_spawn_retire.sql`):
  `lab.agents` gains `can_spawn boolean NOT NULL DEFAULT false` (the
  worker-spawn policy bit) and `retire_context_tokens bigint NULL`
  (auto-retire threshold; NULL = never — the default, including for
  demos).
- **`cmd/lab-agent`** — the real CLI, replacing the Phase 4 stub.
  Same conventions as `kbase` (interspersed flag parsing, tabular
  output, exit codes 0 API-success / 1 API-error / 2 usage;
  `LAB_API_URL` + `LAB_AGENT_TOKEN` from env, `-url`/`-token`
  override):
  - `whoami` — identity, project, state, session, status
  - `agents` — siblings table: NAME, STATE, RUNNING, CONTEXT
    (occupancy gauge, `148k`-style), STATUS
  - `send <agent> "<prompt>"` (or `-m`, or stdin) — prints the queued
    turn id
  - `status "<text>"` — self-report; `""` clears
  - `spawn --name <n> --role "<r>" [--role-file f] [--model m]`
  Stdlib + internal packages only; the binary stays lean.
- **`internal/agentclient`** — typed client for the agent API,
  mirroring `internal/kbclient` (typed methods over `internal/wire`
  types, non-2xx as `*APIError`). Used by the CLI and tests.
- **Spawn** (`POST /v1/agents` on the agent API): 403 unless the
  *caller's* `can_spawn` is true. The spawned agent lands in the
  caller's project on its own branch (`gitrepo.BranchName`),
  **inherits the caller's credential binding** (same credential id),
  gets `can_spawn = false` (no transitive spawning — no deviation),
  and is created **and started** through the daemon's driver manager.
  The spawn is logged with both identities and the new agent is
  immediately visible via `GET /v1/agents`. `wire.CreateAgentRequest`
  and `labctl agent create` gained `can_spawn` + `retire-tokens`
  flags (plus `-role-file`, so `roles/*.md` can be used directly).
- **Context-occupancy gauge**:
  `store.SessionContextTokens(sessionID)` reads the latest `result`
  event of the session and sums `input_tokens +
  cache_creation_input_tokens + cache_read_input_tokens` in SQL
  (COALESCEd per field; 0 when the session has no result yet).
  Surfaced as `context_tokens` on `wire.Agent` in **both** listings —
  `lab-agent agents` for the orchestrator, the TUI for the human —
  plus one line in the TUI usage tab for the selected agent
  (`context: 148k tokens`).
- **Retirement threshold**: enforced in the pump, only at the moment a
  result event closes a turn (never mid-turn, never during shutdown
  drain). The occupancy is read from that same result event, so no
  extra query is on the hot path; when it reaches the agent's
  threshold the pump calls `Retire(agent, "context threshold
  (<threshold> tokens)", RetireSeed(...))` and returns
  `errSessionRetired`, so the Run loop re-provisions against the
  successor session. Exactly one retirement per crossing: the
  successor starts at occupancy 0 by construction.
- **Seed template** (`claude.RetireSeed(reason, query)`): the
  kbase-recovery prompt from the handoff, with the agent's *name* as
  the recall query. Used verbatim by auto-retire, and as the default
  when the client-API retire gets an empty seed. The successor pulls
  its own memory via the CLI (no server-side recall embedding — no
  deviation).
- **`roles/`**: `orchestrator.md` (the loop: survey via `lab-agent
  agents`, break work into kbase tickets, `lab-agent send` workers at
  tickets, verify via `kbase ticket list` / `kbase show --history`,
  report status, spawn when short-handed) and `worker.md` (claim
  before working, comment progress, done/abandon honestly, recall
  before asking). Referenced by `labctl agent create -role-file`.
- **Credential rebind poke**: `PUT .../credential` now, when the agent
  has a hosted driver, calls `Driver.PokeRestart(agentID)`. The pump
  checks the poke only while idle (current turn nil) and returns
  `errRestartRequested`; Run re-reads the agent row (picking up the
  new credential id), replaces the container, and the claude process
  continues the same session via `--resume`. Mid-turn pokes wait for
  the result.

## Decisions recorded (smallest-reasonable-call)

1. **Seed defaulting lives in the callers, not in `Driver.Retire`**:
   `Retire` keeps its existing contract (empty seed = no seed turn),
   so labctl's low-level `retire -seed ""` behavior is unchanged. The
   client API and auto-retire supply `RetireSeed` when no explicit
   seed is given.
2. **The rebind poke is in-process shared memory** (a flag map on the
   Driver, consumed by the pump between turns). labd hosts both the
   API and the drivers, so this covers the deployed topology; a
   driver run in another process (labctl dev mode) doesn't see pokes
   and picks the new credential up on its next natural process
   restart. Chosen over a Postgres signal as the minimal mechanism
   the handoff asked for.
3. **Threshold reads the result event, not the store**: the pump's
   occupancy check uses the just-received result's usage rather than
   re-querying `SessionContextTokens` — same number (that query
   returns the latest result's sum), zero extra latency. The store
   query is what the listings use.
4. **Agent snapshot per process, not per turn**: the pump keeps the
   agent row it was started with; `Run` re-reads it on every
   provision cycle (retire, restart, process death). A threshold
   changed mid-process applies from the next process, which is
   acceptable for a between-turns mechanism.
5. **Spawn name collisions surface as 500** (unique violation on
   `(project_id, name)`), not a mapped 409 — kept minimal; the CLI
   prints the constraint error either way. Candidate for Phase 12
   polish.
6. **`formatTokens` in the CLI truncates k-values** (`148k`) while the
   TUI reuses its existing `fmtTokens` (`148.2k` style); both render
   the same gauge.

## Image note

The stub binary in every stack image is now the real CLI: the image
content hash changes, so the first agent start after this lands does a
**one-time rebuild per stack** (the builder detects the changed
`lab-agent` binary in the build context). No action needed beyond the
wait.

## Tests (all live-Postgres, skip gracefully; no Docker needed)

- `store`: `TestSessionContextTokens` — latest-result-wins, non-result
  events ignored, usage-less result reads 0, successor session starts
  at 0 while the predecessor keeps its value; migration round-trip
  extended through 00005.
- `api`: `TestAgentAPISpawn` — 401 unauthenticated, 403 without
  `can_spawn`, spawned agent inherits project + credential, appears in
  siblings, has `can_spawn=false`, and its own token gets 403 (no
  transitive spawn); 400 on missing name. `TestAgentAPIContextTokens`
  — both listings report the gauge from seeded result events. The
  Phase 6 auth matrix is unchanged (`TestAgentAPI` still passes; the
  spawn route 401s like the rest).
- `cmd/lab-agent`: `TestCLIRoundTrip` — whoami → status → agents
  (status text + CONTEXT column visible) → send (turn row has
  `source_kind=agent`, `source_id` = caller; positional and stdin
  prompts) → spawn → status clear, against a real `AgentServer` on a
  live store. `TestCLIUsageErrors` — exit 2 paths.
- `claude`: `TestPumpContextThresholdRetires` — simulated stream
  crosses the threshold → exactly one retirement between turns (turn
  is done first), successor chained via `prev_session_id`, seed turn
  addressed to the successor with the recall template, occupancy 0
  after. `TestPumpBelowThresholdNoRetire`. `TestPumpRestartPoke` —
  idle poke restarts immediately; mid-turn poke waits for the result
  (and the turn finishes done). `TestPumpGateHoldsAgentSourcedTurn` —
  a denied budget holds a `lab-agent send` turn exactly like a user
  turn, and it delivers when the gate opens.

## E2E demo (for the orchestrator to run against real Claude)

Prereqs: `make db-up && make migrate-lab && make migrate-kbase`,
kbased + labd running (`lab.toml` with a kbased admin token), Docker
up, and a test credential sourced from `~/.lab/demo.env`.

1. **Project + credential**
   ```
   labctl project create -name demo -origin <repo-or-path> -stack base
   labctl cred add -kind oauth_token -label demo   # paste token, Ctrl-D
   ```
2. **Orchestrator + worker**
   ```
   labctl agent create -project demo -name orchestrator \
     -role-file roles/orchestrator.md -cred oauth_token -can-spawn
   labctl agent create -project demo -name worker1 \
     -role-file roles/worker.md -cred oauth_token
   ```
   Start both via the TUI (`lab`) or the client API
   (`POST /v1/projects/demo/agents/{name}/start`).
3. **One human turn to the orchestrator** (TUI composer, or
   `POST .../orchestrator/turns`):
   > Ship two small docs improvements to this repo; split the work
   > into tickets and drive the worker.
4. **Watch the loop** (TUI transcript + `kbase ticket list`):
   tickets appear (`kbase add ticket` by the orchestrator), worker
   turns arrive attributed — `source_kind=agent`,
   `source_id=<orchestrator>` — and render with the sender's name in
   the TUI; the worker claims/comments/dones tickets with provenance
   (`kbase show <slug> --history`); the orchestrator reports
   completion via `lab-agent status`.
5. **Occupancy reading**: from inside either container (or `docker
   exec`), `lab-agent agents` — include this output in the demo
   record; the CONTEXT column is the gauge, which the TUI usage tab
   mirrors for the selected agent.
6. **Retirement leg** (scripted, same run): either
   `labctl agent create ... -retire-tokens 30000` on a third agent
   and let a couple of turns cross it, or retire explicitly with an
   empty seed to exercise the default:
   `curl -X POST localhost:7710/v1/projects/demo/agents/worker1/retire -d '{}'`.
   The successor session boots (chained via `prev_session_id`), its
   first turn is the recall seed, and the transcript shows it running
   `kbase recall` / `kbase ticket list` before picking work back up.
   `lab-agent agents` shows the fresh session's CONTEXT at ~0.

## Out of scope / left for Phase 12

Toggling `can_spawn` after creation; spawn-name 409 mapping; spawn
quotas / budget inheritance beyond the credential binding; TUI
orchestration views beyond the usage-tab line; cross-process rebind
pokes; retiring other agents via lab-agent.

---

## Orchestrator review — closed (2026-08-13)

Reviewed against the phase-11 handoff acceptance criteria. **Zero
bounces** on the phase's own scope. Merged to `development` as
83f2300; one harness fix landed alongside (317edc1, below).

**Code review**: full 24-file diff read (+1742/−42). Spawn checks the
*caller's* flag before decoding, forces `can_spawn=false` on workers,
and copies only the credential id; the occupancy query is
latest-result-wins per session with COALESCEd fields; the pump's
threshold check reuses the just-received result (no extra query) and
fires only after the turn is closed; restart pokes are consumed only
while idle; `Run` re-reads the agent row each provision cycle so
rebinds/thresholds apply on the next process. All six recorded
decisions accepted (seed defaulting in callers, in-process poke map,
spawn-name 409 mapping deferred to Phase 12).

**Checks**: `make check` green on branch and post-merge; the four new
suites pass `-race -count=1` twice; migration 00005 down/up
round-trips (columns verified). Secret scan clean.

**Live e2e demo (real Claude, oauth credential)**: orchestrator
(`roles/orchestrator.md`, can_spawn) + worker1 (`roles/worker.md`) on
a sample project. One human turn produced: three kbase tickets (the
third opened *by the orchestrator on its own* after it noticed
setup.md exceeded the 30-line cap), worker turns attributed
(`source_kind=agent`, sender rendered by name), claim/start/comment/
done provenance across principals (v1 `agent:orchestrator`, v2+
`agent:worker1`), real commits on `agent/worker1` (fdb854b, 1a0f968),
and a verified completion report. Occupancy gauge live throughout
(`lab-agent agents`: orchestrator 92k → 1.2M, worker 500k).
Retirement leg: empty-seed client-API retire → successor chained via
`prev_session_id`, default RetireSeed enqueued, and the successor
actually ran `kbase recall` + `kbase ticket list`, concluded all
tickets complete, cleared its status. Gauge reset with the new
session. Zero token material in daemon logs or the event store; clean
SIGTERM teardown.

**Latent harness bug found by the demo (not a phase-11 defect)**: git
had never worked inside agent containers — a worktree's `.git` links
to the host `repo.git`, which was never mounted. The worker hit it on
its first commit, correctly refused to fake success, and left the
tickets `in_progress` with explanatory comments. Fixed in 317edc1
(orchestrator): `repo.git` is bind-mounted at its identical host path
(runtime spec + driver + gitrepo accessor + runtime test); verified
live, after which the worker committed and finished. DESIGN's
container contract updated. Follow-ups filed for Phase 12: scope the
repo.git mount tighter, and the labd restart port-bind race (a new
labd racing the old one's drain exits on "address already in use").

**Second finding**: the orchestrator ended its turn "waiting" for the
worker, but nothing re-prompts an agent — the loop stalled until a
human nudge. Root cause: the worker template never told workers to
report back. Fixed in 317edc1: workers now send a completion turn to
their orchestrator; the orchestrator template states explicitly that a
worker's message is what resumes its loop.
