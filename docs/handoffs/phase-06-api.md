# Phase 6 handoff — labd APIs & agent tokens

You are the implementation agent for Phase 6 of the `lab` project. This
document is your complete brief. Read `docs/DESIGN.md` (especially
"Networking & auth between components", "Event log", "Binaries"), then
the phase reports 00–05 — 05 in particular: the driver you are hosting
was built there, and its report's "Phase 6 notes" are addressed to you.

Where things stand: `labctl` (Phase 5) runs the `claude.Driver`
in-process — there is no daemon in the loop yet. Your job is to make
`labd` the long-running host: drivers run inside the daemon, clients
talk HTTP, and containers get bearer tokens so agents can talk back.

## Ground rules

- Branch `phase-06-api` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded in your
  report — no redesign. Minimal, tested additions to upstream packages
  are allowed (list each in the report).
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop; Postgres/Docker-dependent tests
  skip gracefully.
- **This repo is public on GitHub.** No secrets committed; token
  secrets are stored hashed, returned once at mint, never logged.
- Allowed new deps: none expected (SSE is stdlib: `http.Flusher`).
  Justify anything in the report.
- Finish with `docs/handoffs/phase-06-report.md`.

## Deliverables

### 1. `internal/wire` — shared API types

Request/response structs for everything below, JSON-tagged, no
behavior. Both the client API and (Phase 7's) TUI import these; the
agent API types will also serve Phase 11's `lab-agent`.

### 2. labd hosts drivers

An agent manager inside `labd`: one `claude.Driver.Run` goroutine per
started agent, started/stopped via the client API. On daemon startup,
restart drivers for agents whose state is not `stopped`/`retired`
(they were running when the daemon died — full crash reconciliation is
Phase 12; this is the simple version). Clean daemon shutdown cancels
all drivers (the Phase 5 grace-drain then applies per agent).

### 3. Agent tokens

- Migration (next `migrations/lab/` number): `agent_tokens(id uuid pk,
  agent_id fk not null, secret_hash bytea not null, created_at,
  revoked_at timestamptz null)` + store methods (mint returns the
  plaintext secret exactly once; verify by SHA-256 hash with
  constant-time compare; revoke).
- The driver's container env gains `LAB_AGENT_ID`, `LAB_AGENT_TOKEN`
  (minted fresh per container create, previous tokens for the agent
  revoked), `LAB_API_URL` (`http://host.docker.internal:<agent-api
  port>`), and `LAB_PROJECT` (project name). Credential injection is
  untouched.

### 4. Client API (on `cfg.Labd.ClientAPIAddr`, localhost, no auth in v1)

JSON over REST-ish routes (`net/http` ServeMux patterns are fine):

- Projects: create (validates stack via `runtime.Stacks()`, calls
  `gitrepo.CreateProject`, rolls back the row on failure — port
  labctl's logic), list, get, delete.
- Agents: create, list (with state + current session), start (spawn
  driver), stop (cancel driver), retire (wraps `Driver.Retire`).
- Turns: submit (`source_kind=user`), get by id.
- Events:
  - Backfill: `GET .../events?after_seq=N&limit=M` (per session) and a
    global `?after_id=N` variant — thin wrappers over the store.
  - Live: `GET .../events/stream` — **SSE** backed by `LISTEN
    lab_events` (one daemon-wide listener connection fanning out to
    subscribers; remember the NOTIFY payload is
    `{event_id, agent_id, session_id}` — fetch rows by id). Each SSE
    event: `id:` = event id, `data:` = the stored payload row (wire
    struct with agent/session/turn/seq/kind/payload). Support
    `Last-Event-ID`/`?after_id` resume so a dropped client backfills
    then continues live without gaps.
- Daemon status: running drivers, DB health, version.

### 5. Agent API (on `cfg.Labd.AgentAPIAddr`, bearer-token auth)

`Authorization: Bearer <token>` on every route; the token maps to the
calling agent (401 unknown/revoked; agents act only within their own
project — cross-project anything is 403/404):

- `GET /v1/whoami` — agent id/name/project/state/session.
- `GET /v1/agents` — sibling agents in the caller's project (name,
  state, current session id).
- `POST /v1/agents/{name}/turns` — enqueue a turn for a sibling agent,
  `source_kind=agent`, `source_id=<caller>`; body: content. This is
  the orchestration primitive `lab-agent` (Phase 11) will call.
- `POST /v1/status` — agent self-reports a short status string; add a
  nullable `status_text` column to `lab.agents` in your migration and
  surface it in both APIs' agent listings.

### 6. Wiring & polish

- `cmd/labd` grows from the Phase 0 skeleton: config → pool → store →
  gitrepo/runtime/builder/driver manager → both HTTP servers →
  graceful shutdown of everything (drivers first, then servers).
  `/healthz` stays.
- Optional (stretch, only if clean): `EnqueueTurn` gains a
  `pg_notify('lab_turns', agent_id)` and the hosted driver's idle poll
  shortens by waking on it. The 1s poll stays as the fallback either
  way. Skip entirely if it means invasive pump surgery — note the
  decision.
- `labctl` stays as-is (it still works in-process and remains the
  fallback dev tool). Do not port it to the API — Phase 7's TUI is the
  API's real client; your integration tests are its first consumer.

### 7. Tests (live Postgres via `httptest`; skip gracefully)

- Client API: project/agent CRUD round-trip; turn submit → row
  appears queued; events backfill pagination; SSE — append events
  from the test (via store) and assert a subscribed client receives
  them in order, and that `Last-Event-ID` resume misses nothing
  (append while disconnected).
- Agent API: whoami with a minted token; 401 on bad/revoked token;
  cross-agent turn lands with correct `source_kind`/`source_id`;
  sibling listing excludes other projects; status report round-trip.
- Manager: start/stop lifecycle with a fake driver seam if practical,
  or document why it's covered by the demo instead.

### 8. Demo (documented in your report; I will re-run it)

Live end-to-end with a real credential (env passthrough still, via the
daemon's env): start `labd`, then with `curl`: create project + agent
(reuse the Phase 5 scratch-repo recipe), start the agent, submit a
turn, watch `events/stream` deliver it live (show the SSE lines),
whoami + cross-agent turn with a minted token (create a second agent
for the target), clean daemon shutdown mid-idle.

## Acceptance criteria

- `make check` green in all configurations; API + token tests pass
  under `-race`.
- SSE stream: no gaps and no duplicates across a
  disconnect/reconnect with `Last-Event-ID` (tested).
- Token auth enforced on every agent-API route (tested).
- The curl demo reproduces from the report on my machine.
- No plaintext token or credential in logs, DB (hash only), or event
  payloads.

## Out of scope

TUI (Phase 7); credential encryption, budgets, rate-limit handling
(Phase 8); kbase daemon/CLI and their tokens (Phase 9 — same pattern,
separate system); real `lab-agent` CLI (Phase 11 — you build the API
it calls, not the CLI); worker-spawn policy (Phase 11); full crash
reconciliation (Phase 12).
