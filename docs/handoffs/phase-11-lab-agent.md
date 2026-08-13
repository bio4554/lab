# Phase 11 handoff — lab-agent & the orchestration loop

You are the implementation agent for Phase 11 of the `lab` project.
This document is your complete brief. Read `docs/DESIGN.md`
("Binaries", "Agent runtime", "Turns", and the orchestration framing
at the top) plus the phase 06 and 10 reports. This is the phase the
whole system exists for: **agents driving agents**. An orchestrator
agent is *just another agent* — a Claude Code instance whose role
prompt tells it to manage the others; labd owns mechanics, the
orchestrator owns judgment, and its hands are the `lab-agent` CLI
(agent control) and `kbase` (tickets — real since Phase 10).

**Your file set**: `cmd/lab-agent` (replace the Phase 4 stub),
`internal/agentclient` (new), `internal/labd/api/agent.go`,
`internal/labd/claude/` (retire seeding + occupancy check),
`internal/labd/store/`, `internal/wire`, `migrations/lab/00005_*.sql`,
`cmd/labctl` (flags), `cmd/lab` (one small usage-tab addition),
`roles/` (new). No other phase runs in parallel; branch off current
`development`.

## Ground rules

- Branch `phase-11-lab-agent` off `development`.
- Stay in scope; if a contract below is wrong or incomplete, make the
  smallest reasonable call and record it in the report — do not
  redesign. `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop; Postgres/Docker-dependent tests
  skip gracefully.
- **Public repo — no secrets.** Nothing here needs new secret
  material; agent tokens stay hashed-at-rest (Phase 6 pattern).
- Allowed new deps: none expected.
- Finish with `docs/handoffs/phase-11-report.md`.

## Established seams you build on (do not rebuild)

- **Agent API** (`internal/labd/api/agent.go`, host-bound
  `127.0.0.1:7711`, reachable from containers as
  `LAB_API_URL=http://host.docker.internal:7711`, bearer
  `LAB_AGENT_TOKEN`): `GET /v1/whoami`, `GET /v1/agents` (siblings,
  with Running flag), `POST /v1/agents/{agent}/turns` (enqueues with
  `source_kind=agent`, `source_id=<caller>` — the attribution the TUI
  already renders by sender name), `POST /v1/status`.
- **Retirement** (`internal/labd/claude/retire.go`):
  `Driver.Retire(ctx, agentID, reason, seedPrompt)` ends the current
  session, chains a successor via `prev_session_id`, enqueues
  seedPrompt as the successor's first turn, and stops the container so
  the driver re-provisions (no `--resume` — fresh context). Exposed at
  `POST /v1/projects/{p}/agents/{a}/retire` on the client API.
- **Turn delivery** (`internal/labd/claude/pump.go`): result events
  close turns; the budget `TurnGate` runs peek → check → claim before
  each delivery. Agent-sourced turns already pass through the same
  gate — don't special-case them.
- **Result usage** (`internal/streamjson`): result events carry
  `input_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens` (and output/cost, already rolled up).
- Container env contract: one credential var + `KBASE_URL`/
  `KBASE_TOKEN` + `LAB_AGENT_ID`/`LAB_AGENT_TOKEN`/`LAB_API_URL`/
  `LAB_PROJECT`.

## Deliverables

### 1. `cmd/lab-agent` — the real CLI (replaces the stub)

Reads `LAB_API_URL` + `LAB_AGENT_TOKEN` from env (flags override),
same conventions as `kbase` (plain flag parsing, interspersed
flags/positionals, tabular output readable by humans and agents, exit
codes 0 success / 1 API error / 2 usage):

```
lab-agent whoami
lab-agent agents                       # siblings: name, state, running, status text, context gauge
lab-agent send <agent> "<prompt>"      # or -m / stdin; prints the queued turn id
lab-agent status "<text>"              # report own status ("" clears)
lab-agent spawn --name <n> --role "<r>" [--role-file f] [--model m]
```

Keep the binary lean — it ships in every agent image (the embedded
stub becoming real changes the image content hash: one-time rebuild
per stack, note it in the report).

### 2. `internal/agentclient` — typed client for the agent API

Used by the CLI and tests (mirror `internal/kbclient`'s shape: typed
methods, `APIError`). Wire types live in `internal/wire` as usual.

### 3. Spawn (the worker-spawn policy)

- Migration `migrations/lab/00005_*.sql`: `agents` gains
  `can_spawn boolean NOT NULL DEFAULT false`.
- New agent-API endpoint `POST /v1/agents` (spawn): callable only when
  the **caller's** `can_spawn` is true (403 otherwise). Body: name,
  role prompt, optional model. The spawned agent:
  - lands in the caller's project, on its own branch/worktree as
    usual;
  - **inherits the caller's credential binding** (same credential id —
    the orchestrator can't mint access it doesn't have);
  - gets `can_spawn=false` (no transitive spawning — record if you
    deviate);
  - is created **and started** (the driver runs; that's the useful
    primitive — an orchestrator spawns workers to use them).
- Client-API create (`wire.CreateAgentRequest`) + `labctl agent
  create` gain a `can_spawn` flag so the user can mark an
  orchestrator at creation. Toggling it later is out of scope.
- Spawn is attributed: log line + the spawned agent's creation is
  visible via `GET /v1/agents` immediately.

### 4. Context-occupancy gauge

The metric (decided 2026-08-12): **occupancy ≈ the latest result
event's `input_tokens + cache_creation_input_tokens +
cache_read_input_tokens`** for the agent's *current session* — what
the context window actually held on the last turn (distinct from the
rollup meter, which sums across turns).

- Store query: latest `result` event in the current session → the sum
  (0 when no result yet — fresh/retired sessions start empty).
- Surface it as `context_tokens` on `wire.Agent` so it appears in
  **both** listings (client API + agent API) — the orchestrator reads
  it through `lab-agent agents`, the human through the TUI.
- TUI: show it in the usage tab for the selected agent (one line,
  e.g. `context: 148k tokens` — no gauge widget needed; the TUI is
  otherwise out of scope).

### 5. Retirement threshold + kbase-seeded successor

- Migration (same file): `agents` gains
  `retire_context_tokens bigint NULL` (NULL = never auto-retire).
  Settable at create (`labctl agent create -retire-tokens N`, wire
  field); pick a sensible demo default of NULL.
- **Enforcement point**: in the pump, after a result event closes a
  turn — never mid-turn — if occupancy ≥ threshold, trigger
  `Retire(agent, "context threshold (<n> tokens)", seed)`. One
  retirement per crossing (the fresh session's occupancy resets to 0
  naturally); log it.
- **Seed prompt** (used by auto-retire AND as the default when the
  client API retire gets an empty seed): a short template —
  identity is already re-injected via `--append-system-prompt`, so
  the seed's job is *memory recovery through kbase* (this is what
  makes kbase load-bearing):

  > Your previous session was retired (<reason>). Recover your
  > working state: run `kbase recall "<agent role summary or name>"`,
  > `kbase ticket list`, and `kbase show` on anything relevant, then
  > continue your work. Report status with `lab-agent status`.

  The successor pulls its own memory via the CLI rather than labd
  embedding recall output server-side (fresher, and CLI-over-Bash is
  the standard agent interface — record if you deviate). An explicit
  seed passed to retire is used verbatim instead.
- `roles/` (new, top-level): `orchestrator.md` and `worker.md` role
  prompt templates (markdown, referenced by `labctl agent create
  -role-file`). The orchestrator template must teach the loop: check
  `lab-agent agents`, break work into kbase tickets, `lab-agent send`
  workers pointed at tickets, verify via `kbase ticket list` /
  `kbase show --history`, report status. The worker template: claim
  before working, comment progress, `done` when finished, recall
  before asking.

### 6. Credential rebind poke (small)

`PUT` of an agent's credential via the client API currently changes
the DB row only; a running driver keeps the old env until its process
naturally restarts. Close the gap minimally: on credential change,
if the agent's driver is running, restart its claude process at the
next turn boundary (container replace, `--resume` continuity — the
session survives, only the env is rebuilt). Record the mechanism you
choose.

## Tests

Live-Postgres (+ Docker where needed), skip gracefully:

- Agent API: spawn allowed/denied by `can_spawn` (403), spawned agent
  inherits credential + project and appears in siblings, no
  transitive spawn; auth matrix unchanged (401s).
- CLI round-trip against a live agent API (httptest or real listener):
  whoami → status → agents (status text visible) → send (turn row has
  `source_kind=agent`, `source_id` = caller) → spawn.
- Occupancy: seed synthetic result events → query returns the latest
  result's sum for the current session only (older sessions and
  older results ignored; 0 for fresh session).
- Threshold: simulated event stream (no real claude) crossing the
  threshold → exactly one retirement fires between turns; successor
  chained via `prev_session_id`; seed turn enqueued with the recall
  template; occupancy reads 0 after.
- Gate still applies to agent-sourced turns (a denied budget holds a
  `lab-agent send` turn exactly like a user turn).
- Rebind poke: credential change on a (simulated) running agent
  restarts the process at a turn boundary, not mid-turn.

## Acceptance criteria

- `make check` green; new tests pass with `-race`;
  `make migrate-lab` up/down round-trips on 00005.
- **The e2e demo** (documented step-by-step in your report; the
  orchestrator re-runs it against real Claude with a test
  credential): a project with an orchestrator agent (`can_spawn`,
  orchestrator role template) and one worker (worker template). One
  human turn to the orchestrator — e.g. "Ship two small docs
  improvements to this repo; split the work into tickets and drive
  the worker" — results in: tickets created in kbase, the worker
  driven via `lab-agent send`, ticket(s) claimed/done with
  provenance, and the orchestrator reporting completion. **Every
  cross-agent turn is attributed** (`source_kind=agent` +
  `source_id`) in the event log and renders with the sender's name in
  the TUI. Include at least one occupancy reading
  (`lab-agent agents` output) in the report.
- Retirement demo (scripted, may be part of the same run): retire the
  worker with a low `retire_context_tokens` or an explicit retire
  call → successor session boots, runs `kbase recall`/`ticket list`
  per the seed, and picks work back up.
- No plaintext tokens at rest or in logs; no secrets in the diff.

## Out of scope

TUI orchestration views beyond the one usage-tab line; permission
gating (`--permission-prompt-tool`, post-v1); multi-project
orchestration (agents see only their project); spawn quotas/budget
inheritance beyond credential binding; retiring *other* agents via
lab-agent (orchestrators prompt workers to wrap up instead — the
client API retire exists for humans); Phase 12 hardening items.
