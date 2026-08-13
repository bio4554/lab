# Phase 5 handoff — Claude driver: sessions, turns, events

You are the implementation agent for Phase 5 of the `lab` project — the
**core loop**, where everything built so far becomes one real agent
driving Claude Code in a container. This document is your complete
brief. Read `docs/DESIGN.md` in full (especially "Agent runtime" and
"Event log"), then the phase reports for what you're integrating:
`docs/handoffs/phase-0{1,2,3,4}-report.md`.

Your building blocks, all merged on `development`:

- `internal/labd/store` — turn queue (serial per agent), append-only
  events with per-session seq + `pg_notify(lab_events)`, session
  chaining (`prev_session_id`), `SetClaudeSessionID`.
- `internal/streamjson` — lossless codec; `Decoder.Next()`,
  `Encoder.UserMessage`, `Event.Init()/Result()/SessionID()`.
- `internal/labd/gitrepo` — `CreateProject`, `AddWorktree` (returns the
  host path to mount), `Merge`.
- `internal/labd/runtime` — `Builder.EnsureImage`, `Runtime.Create/
  Start/Stop/Remove/Inspect/List`, `Attach` (demuxed stdio).

## Ground rules

- Branch `phase-05-claude` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded in your
  report — no redesign. If an upstream package is missing a small thing
  you need (a store query, an accessor), **add it minimally in place**
  with a test, and list every such addition in your report.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop; Docker/Postgres-dependent tests
  skip gracefully.
- **This repo is public on GitHub.** Never commit credentials. The
  demo takes its Anthropic credential from your environment at runtime.
- Allowed new deps: none expected; justify anything in the report.
- Finish with `docs/handoffs/phase-05-report.md`, including the full
  demo transcript commands.

## Deliverables

### 1. `internal/labd/claude` — the agent driver

A `Driver` (name yours) that owns one agent's runtime lifecycle.
Responsibilities:

**Provisioning** — given an agent (store row): ensure worktree
(`gitrepo.AddWorktree`), ensure image (`Builder.EnsureImage` for the
project's stack), create + start the container (`runtime.Create`,
main-process command below).

**The claude process** — the container's main process:

```
claude -p --input-format stream-json --output-format stream-json \
  --verbose --dangerously-skip-permissions \
  [--append-system-prompt <agents.role_prompt>] \
  [--model <agents.model>] \
  [--resume <sessions.claude_session_id>]
```

`--resume` is included iff the lab session already has a
`claude_session_id` (i.e. this is a container/process restart, not a
fresh session). With stream-json input the process serves many prompts
serially and stays alive until stdin closes. Verify flag behavior
against the real CLI early — if any flag combination doesn't work as
described here, record what you found and adapt minimally.

**Env injection** — pass through exactly one Anthropic credential env
var (`ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`), configured per
agent from the driver's own process env for now (credential storage is
Phase 8; define a small `CredentialSource` interface so Phase 8 can
slot in). Never log its value; never write it to the event log.

**The pump** — per running agent:

- stdout → `streamjson.Decoder` → `store.AppendEvent` (persist
  `Event.Raw` as the payload, `Event.Kind` as kind; unknown kinds
  persist like any other — losslessness is the point). Capture
  `claude_session_id` from the first event carrying it →
  `SetClaudeSessionID`.
- stderr → drain concurrently (Phase 4 report: the demux stalls
  otherwise); log lines at warn, and persist nothing.
- Turn delivery: when the agent is idle, `store.NextQueuedTurn`; on a
  turn, `Encoder.UserMessage(content)` → stdin, then consume events
  (appending each, tagged with the turn id) until the `result` event →
  `FinishTurn(done|error)` using `Result().IsError`. Then poll for the
  next turn (a modest poll interval is fine for this phase; LISTEN
  wiring can wait for Phase 6).
- `result` usage: if the agent has a credential configured, record
  `AddUsage` (tokens + `total_cost_usd`) into the current UTC-hour
  window. Budget *enforcement* is Phase 8; recording is cheap now.

**Restart & resume** — if the container dies (or labd restarts and
finds it dead via `Inspect`/`List`), replace it: same worktree, same
`.claude` volume, `--resume` with the stored `claude_session_id`. The
lab session row continues — do not create a new session for a resume.

**Session retirement** — a first-class driver operation:
`Retire(reason, seedPrompt)` stops the process cleanly (close stdin,
stop container), `EndSession(reason)`, `CreateSession(prev=old)`,
starts a fresh claude process (no `--resume`), and enqueues
`seedPrompt` as the new session's first turn. (Phase 11 will build
kbase-recall seeds on top; here the seed is just text.)

**Shutdown** — context-driven: close stdin, give the process a grace
period, stop the container. In-flight turns are marked `error` with a
recorded reason on hard shutdown. (Remember Phase 4's `StdinOnce`
semantics: detach/stdin-EOF ends the process — that's the designed
behavior; resumption carries continuity.)

### 2. `cmd/labctl` — throwaway dev CLI

Explicitly disposable (Phase 7's TUI replaces it; keep it under ~400
lines, no cobra — flag + subcommand switch is fine). It links the
internals directly (config → pgx pool → store/gitrepo/runtime/driver);
there is no daemon API yet. Subcommands:

- `labctl project create -name X -origin <url|path> -stack go`
- `labctl agent create -project X -name impl1 [-role "..."] [-cred api_key|oauth_token]`
- `labctl agent run -project X -name impl1` — foreground: provision +
  pump loop until Ctrl-C (credential env var read here)
- `labctl send -project X -agent impl1 "prompt text"` — enqueue turn
- `labctl tail -project X -agent impl1` — follow the event log (poll
  `EventsSince`; print kind + text/tool summaries via streamjson
  accessors)
- `labctl retire -project X -agent impl1 -seed "..."` — retire session
- `labctl merge -project X -agent impl1` — merge the agent's branch

`labctl send`/`tail` in one terminal while `agent run` sits in another,
communicating through Postgres — that's the architecture demo.

### 3. Tests

Unit-test what's testable without Claude: pump logic against a fake
process (feed fixture streams from `internal/streamjson/testdata/`
through an in-memory pipe and assert events/turns/sessions land
correctly in a live-Postgres store — reuse the fixture files),
turn-tagging, result→FinishTurn mapping, resume-arg construction,
retirement chaining. Docker/Postgres tests skip gracefully as usual.

The end-to-end path needs a real credential and real Claude — that's
the scripted demo below, run manually, not CI-style tests.

### 4. The demo (documented step-by-step in your report)

With a real credential in your env (never committed), against a
scratch git repo:

1. `labctl project create` (local folder origin, `base` stack) +
   `agent create` + `agent run`.
2. `labctl send`: "Create FEATURE.md summarizing this repository's
   purpose, then stop."
3. Show: events streaming into `lab.events` (via `labctl tail`),
   `FEATURE.md` committed... (the agent may or may not commit — that's
   fine; the file existing in the worktree is the assertion),
   the turn finishing `done`.
4. `docker restart` the container mid-idle; `labctl send` again; show
   the reply demonstrates memory of step 2 (same claude session via
   `--resume`).
5. `labctl retire -seed "Introduce yourself"`; show the new session
   row chains to the old via `prev_session_id` and the seed turn runs.

Record the transcript (trim noise) in the report. **Scrub the
transcript before committing** — it must contain no credential
fragments.

## Acceptance criteria

- `make check` green everywhere (no Docker, no Postgres, both).
- Pump unit tests: fixture stream in → correct events (kind, seq,
  turn_id), correct turn status transitions, correct session-id
  capture — all under `-race`.
- The demo, which I will re-run from your report's instructions with
  my own credential. Steps 1–5 must work as described.
- No credential material in any commit, log line, or event payload.

## Out of scope

HTTP APIs and tokens (Phase 6); TUI (Phase 7); credential encryption,
budget enforcement, rate-limit pause (Phase 8 — you only record
usage); kbase anything (Phase 9+); `lab-agent` real implementation
(Phase 11); multi-agent orchestration (Phase 11).
