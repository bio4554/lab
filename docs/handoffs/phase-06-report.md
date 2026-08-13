# Phase 6 report — labd APIs & agent tokens

Branch `phase-06-api`, off `development`. `make check` green; the full
suite passes under `-race -count=1` (two consecutive runs) with
Postgres + Docker up; Postgres-dependent tests skip when it is down.
The curl demo below ran live against a real OAuth credential
(2026-08-12); transcript scrubbed of all secret material.

## What was built

### `internal/wire`

JSON-tagged request/response structs for both APIs, no behavior:
projects, agents (with `session_id`, `running`, `status_text`), turns,
events (also the SSE `data:` body), daemon status, whoami, retire,
status-report. Phase 7's TUI and Phase 11's `lab-agent` import these.

### Migration `00003` + store

- `lab.agent_tokens(id, agent_id fk, secret_hash bytea, created_at,
  revoked_at)` with a unique index on `secret_hash`, and
  `lab.agents.status_text text` (nullable).
- `MintAgentToken` — 32 random bytes, hex plaintext returned exactly
  once; only the SHA-256 hash is stored. `VerifyAgentToken` — lookup
  by hash via the unique index, then a `subtle.ConstantTimeCompare`
  recheck of the fetched hash; unknown and revoked are one
  indistinguishable `ErrTokenInvalid`. `RevokeAgentTokens` — all
  active tokens of an agent.
- `ActiveAgents` (state ∉ stopped/retired — daemon-boot restart),
  `SetAgentStatusText`, `MaxEventID` (SSE live-only start),
  and `EnqueueTurn` now also `pg_notify('lab_turns', agent_id)`
  (see stretch, below). All covered in the store suite.

### labd hosts drivers (`claude.Manager`)

`Manager` keeps one `Driver.Run` goroutine per started agent — pure
goroutine bookkeeping (start/stop/stop-all/is-running); all agent
state stays in the store, written by the driver as before. Driver
contexts are detached from request contexts; `Stop` cancels and waits,
so the Phase 5 grace-drain applies per agent. A driver that dies on
its own deregisters itself and can be started again. Tested against a
fake `Runner` seam (`TestManagerLifecycle`), no DB needed.

On daemon startup, `ActiveAgents` are restarted (simple version; full
crash reconciliation is Phase 12). After a *clean* shutdown nothing
restarts, because the driver marks agents `stopped` on cancellation —
only a daemon crash leaves states behind for this to pick up.

### Agent tokens in the container env

When the driver is constructed with `AgentAPIURL` (the daemon does;
labctl does not), every container create first revokes the agent's
active tokens, mints a fresh one, and injects `LAB_AGENT_ID`,
`LAB_AGENT_TOKEN`, `LAB_API_URL`
(`http://host.docker.internal:<agent-api port>`), `LAB_PROJECT`.
Credential injection is untouched. The plaintext token exists only in
the env array and the agent API caller's header — never logged,
never persisted (verified below).

### Client API (`cfg.Labd.ClientAPIAddr`, no auth)

`net/http` ServeMux patterns, names in paths, wire types on the wire:

```
GET  /healthz                                  (unchanged shape)
GET  /v1/status                                version, db health, stacks, hosted agents
POST /v1/projects                              validates stack, clones; rolls back row on git failure
GET  /v1/projects[/{project}]
DELETE /v1/projects/{project}                  409 while agents exist
POST /v1/projects/{p}/agents                   role/model/credential_kind (env-passthrough stand-in)
GET  /v1/projects/{p}/agents                   state + current session + status_text + running
POST /v1/projects/{p}/agents/{a}/start|stop    manager start/cancel (409 on already/not running)
POST /v1/projects/{p}/agents/{a}/retire        wraps Driver.Retire, returns new/prev session
POST /v1/projects/{p}/agents/{a}/turns         source_kind=user
GET  /v1/turns/{id}
GET  /v1/sessions/{id}/events?after_seq&limit  per-session backfill
GET  /v1/events?after_id&limit                 global backfill
GET  /v1/events/stream                         SSE
```

### SSE (`GET /v1/events/stream`)

One daemon-wide `LISTEN` connection (`api.Hub`) fans out to
subscribers. Each frame is `id: <event id>` + `data: <wire.Event>`.
Resume via `Last-Event-ID` (header wins) or `?after_id`; with neither
the stream starts live-only at the current head. Keepalive comment
every 15s.

**Deviation from the handoff's sketch, deliberate:** the handoff
suggested fetching rows by the NOTIFY payload's `event_id`. Instead a
notification is only a *wake-up*; each subscriber reads rows after its
own last-sent id from the store. Same data path, but gapless and
duplicate-free by construction — across notification loss, hub
reconnects, and client reconnects — with no per-subscriber buffering.
The payload's `event_id` is therefore not consumed; the lab_turns
payload (agent id) is.

### Agent API (`cfg.Labd.AgentAPIAddr`, bearer auth)

Auth middleware on every route: `Authorization: Bearer <token>` →
`VerifyAgentToken` → agent loaded into the request context; missing,
malformed, unknown, revoked are all a plain 401. Scope is the caller's
project; agents elsewhere are 404, indistinguishable from missing.

```
GET  /v1/whoami                agent id/name/project/state/session/status
GET  /v1/agents                same-project agents only
POST /v1/agents/{name}/turns   source_kind=agent, source_id=<caller>  (Phase 11's primitive)
POST /v1/status                sets agents.status_text; empty clears; shown in both APIs' listings
```

### `cmd/labd`

Full wiring: config → pool → store → gitrepo/runtime/builder →
driver (+`AgentAPIURL`, `TurnWake`) → manager → hub → active-agent
restart → both HTTP servers. Shutdown order: drivers first
(`StopAll`, 30s bound; per-driver grace-drain inside), then the HTTP
servers, then the LISTEN connection. Request contexts hang off a
daemon-owned context (`Server.BaseContext`) so open SSE streams end
when shutdown begins — `Server.Shutdown` alone would wait on them
forever. labd no longer uses `internal/daemon.Run` (too rigid for two
servers + manager); `kbased` still does, and `/healthz` kept its
shape.

### Stretch: LISTEN wake for the turn queue — done (small)

`EnqueueTurn` notifies `lab_turns` with the agent id; the Hub's single
LISTEN connection also subscribes to it and pokes a `claude.WakeHub`
(per-agent coalescing channels). The pump gained exactly one select
case (`Options.TurnWake`, nil = old behavior); the 1s poll stays as
the fallback. No pump surgery beyond that.

### labctl

Untouched, still works in-process (constructed without `AgentAPIURL`,
so it injects no lab env and mints no tokens — its containers simply
can't call the agent API, which matches its throwaway-dev role).

## Upstream additions (minimal, tested)

- `store`: `tokens.go` (new), `ActiveAgents`, `SetAgentStatusText`,
  `MaxEventID`, `EnqueueTurn` notify, `status_text` in `Agent`/cols;
  fixture + migration round-trip test updated for migration 00003.
- `claude`: `Options.AgentAPIURL`/`Options.TurnWake`, `addLabEnv`,
  one pump select case; `manager.go` (new).
- No new dependencies (SSE is stdlib `http.Flusher`).

## Tests

`internal/labd/api` (httptest + live Postgres, skip when down; all
`-race`):

- `TestProjectAndAgentCRUD` — create (bad stack 400, git clone of a
  scratch repo), get/list, agent create/list, delete 409-then-204,
  daemon status.
- `TestTurnSubmitAndGet` — submit → row queued, get by id, 400/404s.
- `TestEventsBackfill` — per-session seq pagination; global id tail.
- `TestEventStreamLiveAndResume` — live order via real LISTEN/NOTIFY,
  then disconnect → append → reconnect with `Last-Event-ID`: the gap
  arrives exactly once, live continues, no trailing duplicates.
- `TestAgentAPI` — every route 401 without/with a bad token; whoami;
  sibling listing scoped to the project; cross-agent turn lands with
  `source_kind=agent` + caller `source_id`; other-project target 404;
  status round-trip incl. clearing; revocation kills the token.
- `claude`: `TestManagerLifecycle` (fake driver seam), `TestWakeHub`.
- `store`: `TestAgentTokens`, `TestAgentStatusTextAndActiveAgents`.

**Shared-database flakiness, found and fixed:** the API tests run in
parallel with the `claude`/`store` packages against the same dev
Postgres, so global-stream assertions must filter to the test's own
session — and an SSE client leaked on a `t.Fatal` path deadlocks
`httptest.Server.Close`. Stream cancels are registered via
`t.Cleanup` and all global assertions filter by session. (First
full-suite run hung exactly this way; subsequent full `-race` runs are
clean.)

## The demo (run 2026-08-12; re-runnable)

Prereqs: `make db-up && make migrate-lab`, Docker up, credential env
file outside the repo, `go build -o bin/labd ./cmd/labd`. Scratch
origin repo as in the Phase 5 report (`/tmp/demo-repo-p6`).

**1. Start labd (credential in the daemon's env only):**

```
$ set -a; source ~/.lab/demo.env; set +a; ./bin/labd
msg="http listening" api="client API" addr=127.0.0.1:7710
msg="http listening" api="agent API"  addr=127.0.0.1:7711
```

**2. Project + two agents + start (terminal B, no credential):**

```
$ curl -s -X POST localhost:7710/v1/projects -d '{"name":"demo6","origin":"/tmp/demo-repo-p6","stack":"base"}'
{"id":"019ff8b3-5e25-…","name":"demo6","origin_kind":"local_path",…}
$ curl -s -X POST localhost:7710/v1/projects/demo6/agents \
    -d '{"name":"impl1","role_prompt":"You are impl1, a careful implementation agent working in /work. Keep answers brief.","credential_kind":"oauth_token"}'
$ curl -s -X POST localhost:7710/v1/projects/demo6/agents -d '{"name":"impl2","role_prompt":"You are impl2. Keep answers brief.","credential_kind":"oauth_token"}'
$ curl -s -X POST localhost:7710/v1/projects/demo6/agents/impl1/start     # 204
# labd log: image built, then
msg="claude process started" agent=impl1 container=716d06ad9dca resume=false session=019ff8b3-779c-…
```

**3. Stream + turn — SSE delivers live:**

```
$ curl -sN localhost:7710/v1/events/stream        # terminal C
$ curl -s -X POST localhost:7710/v1/projects/demo6/agents/impl1/turns \
    -d '{"content":"Create FEATURE.md summarizing this repository purpose, then stop."}'
{"id":"019ff8b3-d074-…","source_kind":"user","status":"queued",…}

# terminal C, live (21 frames for the turn):
id: 1454
data: {"id":1454,"agent_id":"019ff8b3-5e9e-…","session_id":"019ff8b3-779c-…","turn_id":"019ff8b3-d074-…","seq":1,"kind":"system",…}
…
id: 1474
data: {…,"seq":21,"kind":"result",…"is_error":false,…"total_cost_usd":0.0728…}

$ curl -s localhost:7710/v1/turns/019ff8b3-d074-…   # "status":"done"
$ ls ~/.lab/projects/<proj>/worktrees/<impl1>/      # FEATURE.md README.md VERSION
```

**4. Agent API with the minted token (from inside the container —
the only place the plaintext lives):**

```
$ TOK=$(docker exec lab-agent-<impl1-id> printenv LAB_AGENT_TOKEN)   # 64 hex chars
$ docker exec lab-agent-<impl1-id> printenv | grep ^LAB_
LAB_AGENT_ID=019ff8b3-5e9e-…
LAB_AGENT_TOKEN=<token>
LAB_API_URL=http://host.docker.internal:7711
LAB_PROJECT=demo6

$ curl -s -H "Authorization: Bearer $TOK" localhost:7711/v1/whoami
{"agent_id":"019ff8b3-5e9e-…","name":"impl1","project":"demo6",…,"state":"idle","session_id":"019ff8b3-779c-…"}
$ curl -s -H "Authorization: Bearer wrong" localhost:7711/v1/whoami   # 401 {"error":"invalid token"}

$ curl -s -H "Authorization: Bearer $TOK" -X POST localhost:7711/v1/agents/impl2/turns \
    -d '{"content":"impl1 says hi — reply with one sentence."}'
{"id":"019ff8b4-5f5f-…","agent_id":"<impl2>","source_kind":"agent","source_id":"<impl1>","status":"queued",…}

$ curl -s -H "Authorization: Bearer $TOK" -X POST localhost:7711/v1/status -d '{"status":"writing FEATURE.md"}'  # 204
$ curl -s -H "Authorization: Bearer $TOK" localhost:7711/v1/agents
# impl1 shows "status_text":"writing FEATURE.md"; only demo6 agents listed

$ curl -s -X POST localhost:7710/v1/projects/demo6/agents/impl2/start   # impl2 consumes the turn
# events: impl2 reply "Hi impl1!"; turn 019ff8b4-5f5f-… → status done, source_kind=agent, source_id=<impl1>
```

**5. Clean shutdown mid-idle (Ctrl-C / SIGTERM, both drivers hosted):**

```
msg="shutting down: stopping agent drivers"
msg="shutting down: draining http servers"
msg="shutdown complete"                       # exit 0
$ docker ps -a --filter label=lab.agent-id    # both containers Exited (0)
```

## Acceptance results

- `make check` green (Docker + Postgres up); Postgres down → api,
  store, claude tests skip. Full `go test -race -count=1 ./...` green
  twice consecutively.
- SSE disconnect/reconnect with `Last-Event-ID`: no gaps, no
  duplicates — `TestEventStreamLiveAndResume`.
- Token auth enforced on every agent-API route —
  `TestAgentAPI` iterates all four routes with missing and bad
  tokens; revoked token 401 also covered.
- Demo reproduced from the commands above.
- No plaintext token or credential anywhere: grepped the daemon log,
  the SSE capture, `lab.events.payload`, and `lab.agent_tokens`
  (hash only) for the live token's prefix — zero hits. The transcript
  above was scrubbed before commit.

## Open questions / notes for later phases

1. Client API listings do one `CurrentSession` query per agent (N+1);
   fine at lab scale, easy to batch if the TUI ever cares.
2. Agent-state semantics: `state` reflects the driver's view
   (idle/working), while `running` reflects the manager. After a
   daemon kill -9, states linger (idle/working) until the next boot's
   ActiveAgents restart picks them up — Phase 12's reconciliation
   owns the rest.
3. `DELETE /v1/projects` refuses while agents exist rather than
   cascading; agent deletion over the API is deliberately absent
   until retirement semantics for worktrees/volumes land (Phase 11's
   spawn/retire policy may want it).
4. The one spontaneous claude exit noted in Phase 5 did not recur
   across this phase's runs.
