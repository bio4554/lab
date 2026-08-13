# Phase 5 report — Claude driver: sessions, turns, events

Branch `phase-05-claude`, off `development`. `make check` green with
Docker + Postgres up, with Postgres down, and with Docker down (all
dependent tests skip cleanly). Pump tests pass under `-race -count=1`.
The full demo (steps 1–5) ran against a real OAuth credential; the
scrubbed transcript is below.

## What was built

### `internal/labd/claude` — the agent driver

- `driver.go` — `Driver` (`New(Options)`), `Run(ctx, agentID)`:
  provision (worktree via `gitrepo.AddWorktree`, image via
  `Builder.EnsureImage`, container via `runtime.Create` with the
  claude argv as main process) and pump, in a replace loop. `claudeArgs`
  builds `claude -p --input-format stream-json --output-format
  stream-json --verbose --dangerously-skip-permissions
  [--append-system-prompt role] [--model m] [--resume sid]`; `--resume`
  iff the lab session already has a `claude_session_id`. On process
  death: replace container (same worktree, same `.claude` volume,
  `--resume`), same lab session row, small backoff (default 3s). On
  `Run` start, a turn left `running` by a driver crash is errored
  (`orphaned by driver restart`) so the serial queue can't wedge.
- `pump.go` — the per-process loop. stdout → `streamjson.Decoder` →
  `store.AppendEvent` (raw payload, envelope kind; unknown kinds
  persist like any other); malformed lines are logged and skipped.
  claude session id is captured from the first event carrying one (and
  re-captured if it ever changes) → `SetClaudeSessionID`. stderr is
  drained concurrently and logged at warn, never persisted. When idle,
  `NextQueuedTurn` (1s poll; also checked immediately after each
  result) → `Encoder.UserMessage` → events tagged with the turn id →
  `result` → `FinishTurn(done|error)` from `Result().IsError` →
  `AddUsage` into the UTC-hour window when the agent has a credential.
  Shutdown is cooperative: close stdin, keep persisting events for a
  grace period (default 15s; a late result still finishes the turn
  `done`), then error the in-flight turn (`hard shutdown`).
- `creds.go` — `CredentialSource` interface
  (`Resolve(ctx, store.Credential) (envVar, secret, error)`) for Phase
  8 to slot in; `EnvCredentialSource` maps kind `api_key` →
  `ANTHROPIC_API_KEY`, `oauth_token` → `CLAUDE_CODE_OAUTH_TOKEN` and
  reads the value from the driver's process env. Exactly one variable
  is injected; the secret is never logged or persisted.
- `retire.go` — `Retire(ctx, agentID, reason, seedPrompt)`:
  `EndSession(reason)` → `CreateSession(prev=old)` → enqueue seed
  **addressed to the new session** (`turns.session_id`) → stop the
  agent's container. Works from a separate process than `agent run`
  (the demo's shape) and with no driver running at all.

### Retirement/turn race, made deterministic

`labctl retire` and `labctl agent run` are separate processes, so the
running pump could fetch the seed turn before the container stop lands.
The pump therefore checks a fetched turn's `session_id`: a turn
addressed to a different session means "this session was retired" —
the pump hands the still-`running` turn back to the Run loop, which
re-provisions against the new session (fresh process, no `--resume`)
and delivers the carried turn first. Ordinary `labctl send` turns have
NULL `session_id` (deliverable by any session) and are stamped with the
delivering session via the new `SetTurnSession`.

### Store additions (minimal, in place, tested)

- `RunningTurn(ctx, agentID)` — the agent's running turn or nil
  (orphan cleanup needs it).
- `SetTurnSession(ctx, turnID, sessionID)` — stamp the delivering
  session on a queue-wide turn.
- Both covered by `TestRunningTurnAndSetTurnSession` in
  `internal/labd/store/turns_test.go`.

### `cmd/labctl` (377 lines, flag + switch, no cobra — disposable)

`project create` (origin auto-classified git URL vs local path, made
absolute; stack validated against `runtime.Stacks()`; the store row is
rolled back if `gitrepo.CreateProject` fails), `agent create`
(`-cred api_key|oauth_token` creates a credential row with empty
`secret_enc` as the Phase 5 env-passthrough stand-in), `agent run`
(foreground driver until Ctrl-C), `send`, `tail` (polls
`CurrentSession` + `EventsSince`, follows across retirement, renders
text/tool_use/result via streamjson accessors), `retire`, `merge`.

### Tests (`internal/labd/claude/claude_test.go`, live Postgres, skip when unreachable)

Pump against an in-memory fake process fed the Phase 2 fixtures:
`simple.jsonl` end-to-end (kinds, gapless seqs, turn tagging incl.
pre-turn events untagged, payload equality, session-id capture, turn
done, usage sums match the fixture's result), `unknown_kind.jsonl`
(losslessness for future kinds), error result → turn `error`, process
death mid-turn → turn `error` + replace signal, retirement carry-over
(mismatched turn returned running, redelivered by the next pump),
shutdown drain (late result → `done`), hard shutdown (grace expiry →
`error`), `Retire` chaining, orphaned-turn cleanup, `claudeArgs`
construction, credential env mapping. All run with `-race`.

## Decisions & deviations (smallest-call gaps)

- **jsonb normalizes payloads.** `events.payload` is jsonb (Phase 1
  schema), which canonicalizes key order/whitespace/duplicates, so
  "lossless" is semantic, not byte-for-byte. Tests compare parsed
  JSON. If byte-fidelity ever matters, the column would need to be
  `text`/`bytea` — not this phase's call.
- **Flag behavior verified against CLI 2.1.229 (host)**: one process
  serves N prompts serially over stream-json stdin; exits on stdin
  EOF; `-p --resume <sid>` continues the *same* session id with memory
  intact. Matches the handoff; no adaptation needed.
- **One spontaneous process exit observed** (container CLI pinned at
  2.1.222): the first demo process exited ~2s after its first result.
  Cause not pinned down; the replace loop absorbed it (`--resume`,
  same lab session) which is the designed behavior either way.
- **`docker restart` and the attach**: on Docker Desktop/Engine 29 the
  driver's attach *survived* `docker restart` — the pump saw no EOF,
  and the restarted main process (whose argv already contained
  `--resume`, since the container was created after the session id was
  known) answered the next turn with full memory. So restart
  continuity holds on both paths: attach death → driver replaces with
  `--resume`; attach survival → the restarted argv resumes by itself.
  Corner: a container created *before* the session id exists (fresh
  session) that is externally restarted would boot an unresumed
  process; the pump re-captures the new session id, so tracking stays
  correct even though conversational memory would reset. Reaching that
  corner requires an external restart in the seconds before the first
  event arrives; left alone at this altitude.
- **`tokens_in` counts input + cache-creation + cache-read tokens**
  (all input-side); `tokens_out` is output tokens; `cost_usd` is
  `total_cost_usd` verbatim. Phase 8 can refine.
- **Store writes use `context.WithoutCancel`** inside the pump so
  shutdown never loses drained events or turn finishes mid-write.
- **Defaults**: 1s turn poll (LISTEN is Phase 6), 15s shutdown grace,
  3s restart backoff — all `Options` fields.
- **Credential rows with empty `secret_enc`** mark kind + identity for
  usage attribution; the secret stays in the driver's env until Phase
  8. No new dependencies.

## The demo (run 2026-08-12; re-runnable from these commands)

Prereqs: `make db-up && make migrate-lab`, Docker running, and a real
credential in the environment **only for the `agent run` shell** — e.g.
a file outside the repo:

```
$ cat ~/.lab/demo.env        # never committed; CLAUDE_CODE_OAUTH_TOKEN=... or ANTHROPIC_API_KEY=...
$ go build -o bin/labctl ./cmd/labctl
```

Scratch origin repo:

```
$ mkdir /tmp/demo-repo && cd /tmp/demo-repo && git init -b main
$ echo '# demo-repo — a tiny scratch repository used to demo the lab agent loop.' > README.md
$ echo 1.0.0 > VERSION && git add -A && git commit -m initial
```

**Step 1 — project + agent + run.**

```
$ ./bin/labctl project create -name demo -origin /tmp/demo-repo -stack base
project demo created (id 019ff886-402f-…, local_path /tmp/demo-repo, stack base)
$ ./bin/labctl agent create -project demo -name impl1 \
    -role "You are impl1, a careful implementation agent working in /work. Keep answers brief." \
    -cred oauth_token
agent impl1 created (id 019ff88d-7969-…, branch agent/impl1)

# terminal A:
$ set -a; source ~/.lab/demo.env; set +a; ./bin/labctl agent run -project demo -name impl1
running agent impl1 (019ff88d-7969-…); Ctrl-C to stop
level=INFO msg="claude process started" agent=impl1 container=e183d3b93b43 resume=false session=019ff88d-8d35-…
```

**Step 2 — send a turn (terminal B).**

```
$ ./bin/labctl send -project demo -agent impl1 \
    "Create FEATURE.md summarizing this repository's purpose, then stop."
queued turn 019ff88d-f01e-…
```

**Step 3 — events stream, file lands, turn finishes done.**

```
$ ./bin/labctl tail -project demo -agent impl1
--- session 019ff88d-8d35-… (prev <nil>)
[   1] system/init        turn=019ff88d
[   2] rate_limit_event   turn=019ff88d
[   5] assistant          turn=019ff88d
[   6] assistant          turn=019ff88d [tool_use Bash {"command": "ls -la /work && …"}]
[  11] assistant          turn=019ff88d [tool_use Read {"file_path": "/work/README.md"}]
[  18] assistant          turn=019ff88d [tool_use Write {"content": "# FEATURE.md\n\n## Purpose\n\nThis is `demo-repo`, …"}]
[  20] assistant          turn=019ff88d Created `FEATURE.md`: this repo is just a minimal scratch/demo repository …
[  21] result/success     turn=019ff88d is_error=false turns=6 cost=$0.1066 in=8 out=755

$ ls ~/.lab/projects/<project-id>/worktrees/<agent-id>/
FEATURE.md  README.md  VERSION          # file exists in the worktree (agent did not commit — allowed)
# turns row: status=done, finished_at set; usage_rollups gained the turn's tokens+cost.
```

**Step 4 — container restart mid-idle, memory intact.**

```
$ docker restart lab-agent-<agent-id>
$ ./bin/labctl send -project demo -agent impl1 \
    "Without looking at any files: what file did you create earlier in this conversation, and what was its heading? One sentence."
$ # tail shows, same lab session:
assistant  I created `FEATURE.md` earlier in this conversation, and its heading was "# FEATURE.md".
result/success  is_error=false
```

(Terminal A also demonstrated the replace path during the run:
`msg="claude process exited; replacing"` → `msg="claude process
started" … resume=true session=019ff88d-8d35-…` — same session id,
`--resume` in the new container's argv.)

**Step 5 — retirement with seed.**

```
$ ./bin/labctl retire -project demo -agent impl1 -reason "demo retirement" -seed "Introduce yourself in one sentence."
msg="stopping container for retirement" agent=impl1 container=61b34dbeb3ea
new session 019ff890-98f1-… (prev 019ff88d-8d35-…)

# terminal A, seconds later:
msg="claude process exited; replacing"
msg="claude process started" … resume=false session=019ff890-98f1-…

# sessions table:
019ff88d-8d35-… | ended, end_reason=demo retirement, prev=NULL
019ff890-98f1-… | open, prev_session_id=019ff88d-8d35-…
# seed turn: session_id=019ff890-98f1-…, status=done; reply:
I'm Claude Code, Anthropic's CLI-based AI assistant for software engineering tasks.
```

Ctrl-C in terminal A: `stopped`; agent state `stopped`; container
`Exited (0)`.

## Acceptance results

- `make check` green (Docker+Postgres up, and each down — dependent
  tests skip).
- `go test -race -count=1 ./internal/labd/claude/ ./internal/labd/store/`
  — all pass (11 driver tests + store suite).
- Demo steps 1–5 ran as documented above.
- No credential material in any commit, log line, or event payload
  (the driver only ever passes the secret inside the container env
  array; transcript reviewed before commit).

## Open questions

1. The one spontaneous claude exit (2.1.222) after a tool-using turn —
   worth watching in Phase 6/7; if it becomes a pattern, bump the
   pinned CLI version in `stacks/base/Dockerfile`.
2. `labctl tail` renders from persisted events only; nothing shows for
   an agent that has never had a session. Fine for a throwaway.
3. Phase 6 should replace the 1s queue poll with LISTEN/NOTIFY (channel
   already exposed as `store.NotifyChannel`) and move Retire/Run
   coordination behind the daemon API.
