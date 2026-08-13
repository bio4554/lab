# lab — Design

An orchestration/management harness for persistent, long-lived Claude Code agents
running in Docker sandboxes, driven by "loop engineering": an orchestrator agent
and/or the user repeatedly prompt worker agents that hold specific roles,
identities, and memories.

Stack: Go + Postgres 18. Local-first: everything runs on one machine.

## Topology

At any time on the machine:

- **1 × Postgres** — single database, two schemas: `lab` and `kbase`, with
  independent migration streams.
- **1 × `labd`** (lab daemon) — owns projects, agents, containers, sessions,
  credentials, budgets, and the event log.
- **1 × `kbased`** (kbase daemon) — centralized knowledge base serving N
  projects. Standalone product, developed in this repo, shares the Postgres
  instance (own schema).
- **N × agent containers** — sandboxed Claude Code CLI instances, one per
  agent, driven over the stream-json interface.
- **N × `lab`** (TUI client) — connect to `labd`.

The orchestrator is **just another agent** — a Claude Code instance in a
container whose role prompt tells it to manage the others. `labd` owns
*mechanics* (lifecycle, budgets, event log, credential injection); the
orchestrator owns *judgment* (what to prompt next, when to spawn/retire
workers). It acts through the same agent-facing API workers use for kbase,
plus lab-specific control endpoints.

## Binaries

| Binary      | Runs           | Purpose                                                        |
| ----------- | -------------- | -------------------------------------------------------------- |
| `labd`      | host           | lab daemon                                                     |
| `lab`       | host           | TUI client                                                     |
| `kbased`    | host           | kbase daemon (auth, persistence, queries)                      |
| `kbase`     | agent container| CLI for agents: knowledge CRUD, graph, tickets                 |
| `lab-agent` | agent container| CLI for agents: agent control (send turn, list/spawn agents, report status). Primarily used by the orchestrator. |

`lab-agent` is an addition to the original four pieces, forced by
"orchestrator is just another agent": the orchestrator needs a tool with which
to drive other agents, and CLI-over-Bash is our standard agent tool interface
(cheaper in tokens than MCP, trivially versioned into the image).

## Repo layout

```
lab/
├── cmd/
│   ├── labd/
│   ├── lab/
│   ├── kbased/
│   ├── kbase/
│   ├── lab-agent/
│   └── migrate/          # dev tool: drives the two goose streams
├── internal/
│   ├── labd/
│   │   ├── api/          # client API (TUI) + agent API (lab-agent)
│   │   ├── runtime/      # docker: images, containers, attach
│   │   ├── claude/       # claude process mgmt, session lifecycle
│   │   ├── gitrepo/      # bare clones, worktrees, branches, merges
│   │   ├── creds/        # credential storage + injection
│   │   ├── budget/       # limits, rollups, rate-limit backoff
│   │   └── store/        # pgx queries for lab schema
│   ├── kbased/
│   │   ├── api/
│   │   └── store/
│   ├── kbclient/         # Go client for kbased API (used by kbase CLI, labd, lab)
│   ├── streamjson/       # Claude Code stream-json event types + codec
│   ├── wire/             # shared API types (labd <-> lab, labd <-> lab-agent)
│   ├── config/           # shared lab.toml loading (Phase 0)
│   ├── daemon/           # shared daemon skeleton: startup/serve/shutdown (Phase 0)
│   └── migrate/          # goose stream wiring for both schemas (Phase 0)
├── stacks/               # Dockerfile templates (see Stacks)
│   ├── base/
│   ├── go/
│   ├── node/
│   ├── python/
│   └── rust/
├── migrations/
│   ├── lab/
│   └── kbase/
└── docs/
```

Module: single Go module. Migrations via `goose`. Postgres via `pgx`. TUI via
`bubbletea` + `lipgloss`. Docker via the official Docker Engine API client
(no shelling out).

## Networking & auth between components

- `labd` client API: localhost TCP for the TUI (decided at Phase 0
  close: no unix socket; defaults 7710 client / 7711 agent / 7720
  kbased). Local machine only; no multi-user auth in v1.
- `labd` agent API and `kbased` API: TCP bound to the host, reachable from
  containers via `host.docker.internal`. Both authenticate with **per-agent
  bearer tokens minted by `labd`** at container creation and injected as env
  vars. Every kbase write is therefore attributable to an agent identity (or
  a human via the TUI) without self-reporting.
- Token model (kbase schema, as built in Phase 9):
  `principals(id, kind agent|human, external_id, display_name)` +
  `tokens(id, principal_id, secret_hash sha256, project_id nullable,
  created_at, revoked_at)` — one live token per (principal, scope),
  rotated at container create. `labd` registers agents as principals
  via kbased's **admin API**, which is guarded by a config-designated
  admin token (`[kbased] admin_token` / `LAB_KBASED_ADMIN_TOKEN`, no
  default, constant-time compared, never stored in the database) rather
  than a localhost-only listener — on Docker Desktop
  `host.docker.internal` reaches host-localhost services, so a second
  listener would not actually exclude agent containers. Empty token ⇒
  admin API answers 403 and agents run without kbase access (kbase is a
  dependency, not a hard requirement: provisioning failures at
  container create log a warning and start the agent without
  `KBASE_URL`/`KBASE_TOKEN`).

## Projects & git

- A project points at a **git URL** or a **local folder**. Local folders are
  converted internally: `labd` inits/uses a git repo so the machinery is
  uniform.
- `labd` keeps a **bare clone per project** under its data dir. Each agent
  gets its **own worktree on its own branch**, bind-mounted into its
  container at `/work`. Agents never share a checkout; integration is an
  explicit merge step driven by the orchestrator or the user.

## Stacks

A **stack** is chosen at project creation and selects the toolchain baked
into that project's agent image.

- Each stack under `stacks/<name>/` is a Dockerfile template composed of:
  1. **base layer** (shared): debian-slim + git, curl, ripgrep, jq,
     ca-certificates, a non-root `agent` user, Claude Code CLI (native
     installer, pinned version), `kbase` and `lab-agent` binaries (built by
     `labd`'s host toolchain via `GOOS=linux go build`, copied in).
  2. **toolchain layer**: per-stack (Go toolchain, Node LTS, Python + uv,
     Rust, or nothing for `base`).
- Initial stacks: `base`, `go`, `node`, `python`, `rust`. Escape hatch
  (post-v1): `custom` — user supplies a Dockerfile that must start
  `FROM lab/base`.
- Images tagged `lab/agent-<stack>:<content-hash>`; `labd` builds lazily on
  first agent start and rebuilds when the stack template, pinned Claude Code
  version, or embedded CLI binaries change.
- `projects.stack` records the choice; per-agent stack override is possible
  later but not v1.

## Agent runtime

### Container contract

Per agent, `labd` runs one container:

- **Image**: the project's stack image.
- **Mounts**:
  - `/work` — the agent's worktree (rw).
  - `/home/agent/.claude` — named volume per agent: Claude Code session
    state survives container/host restarts.
- **Env** (exactly these credentials, nothing else, to avoid precedence
  surprises): one of `ANTHROPIC_API_KEY` **or** `CLAUDE_CODE_OAUTH_TOKEN`;
  plus `LAB_AGENT_ID`, `LAB_AGENT_TOKEN`, `LAB_API_URL`, `KBASE_URL`,
  `KBASE_TOKEN`, `LAB_PROJECT`.
- **Network**: default bridge + `host.docker.internal`. (Tighter egress
  policies are a post-v1 concern.)

### Driving Claude Code

- `labd` starts `claude -p --input-format stream-json --output-format
  stream-json --verbose --dangerously-skip-permissions
  --append-system-prompt <role>` as the container's main process, attached
  via the Docker API. User/orchestrator turns are written to stdin; events
  stream from stdout into the event log.
- **Persistence = resumption, not process lifetime.** The `claude` process
  may die (container restart, host reboot); the agent's continuity comes
  from `--resume <claude_session_id>` against the mounted `.claude` volume.
- **Context lifecycle**: long-lived agents exhaust context. Policy per
  agent: rely on auto-compaction, or **retire the session** and boot a
  fresh one seeded with the identity prompt + kbase recall. Session
  retirement is a first-class daemon operation (records reason, links old →
  new session). This makes kbase load-bearing for memory, which is the
  point.
- **Identity injection**: `--append-system-prompt` (role/persona, stored per
  agent) + `CLAUDE.md` in the worktree for project conventions.
- Permission gating hook (post-v1): `--permission-prompt-tool` served by
  `labd` so the daemon/orchestrator can approve individual tool calls.

### Turns

A **turn** is one prompt delivered to an agent, from the user (TUI) or from
another agent (`lab-agent send`). Turns are queued in Postgres and delivered
serially per agent — an agent processes one turn at a time; the `result`
event closes the turn.

## Event log

Every stream-json event is persisted append-only:

`events(id bigserial, agent_id, session_id, turn_id, seq, kind, payload
jsonb, ts)`

This is the spine: TUI live transcripts tail it (Postgres `LISTEN/NOTIFY` →
SSE to clients), the orchestrator can inspect worker activity through
`lab-agent`, `result` events feed usage rollups, and everything is
replayable for debugging.

## Credentials & budgets

- `credentials(id, kind api_key|oauth_token, secret_enc, label, status,
  expires_at, budget jsonb, limited_until, created_at)` — encrypted at
  rest with NaCl secretbox; the 32-byte key lives at
  `<data_dir>/secret.key` (created 0600 on first use; labd refuses to
  start on a key file readable beyond its owner). Plaintext exists only
  in memory and the agent container's env; no API response ever carries
  it (asserted on wire bytes in tests).
- **Acquisition**: `claude setup-token` in any terminal (subscription;
  one-year token — the default expiry) or a pasted API key, registered
  via `POST /v1/credentials` (secret in the body — acceptable because
  the client API is localhost-only) or `labctl cred add` (secret on
  stdin). Expiry is swept every 30s and checked lazily on use;
  extending/clearing it reactivates an expired credential.
- Each agent references a credential by id (`credential_id` at create,
  `PUT .../credential` to rebind; applies at next container provision).
  Which agents ride the subscription vs. an API key is an explicit
  per-agent routing choice. The Phase 6 `credential_kind` API field
  survives as a compat shim: it binds the only stored credential of
  that kind.
- **Budget contract** (`agents.budget` and `credentials.budget` jsonb):
  `{"max_cost_usd_day": 5.0, "max_tokens_day": 2000000,
  "max_turns_hour": 30}` — all keys optional, unknown keys rejected at
  set time. Windows are UTC (calendar day / clock hour, the rollup
  grain). An agent budget is evaluated against the agent's own usage, a
  credential budget against the credential's usage across all its
  agents; every set limit must pass (most restrictive wins).
- **Enforcement** is the driver's `TurnGate` seam (`budget.Gate`),
  consulted between peek and claim: a denied turn stays queued —
  never errored — and delivers the moment the gate opens (budget
  raise, window rollover, reset passing, manual resume). The gate
  re-reads agent + credential rows per check so changes apply without
  a driver restart.
- **Rate-limit detection**: a limited `rate_limit_event` (or an
  unmistakably limit-shaped `is_error` result, default 5-minute hold)
  records `limited_until` on the credential and pauses every
  idle/working agent riding it — never let loops spin against 429s.
  Resume is threefold: the gate's clock, labd's 30s sweep, or
  `POST /v1/credentials/{id}/resume`.
- Rollups (`usage_rollups(credential_id, agent_id, window_start,
  tokens_in, tokens_out, cost_usd, turns)`) power `GET /v1/usage`
  (per-credential and per-agent windows + live gate verdicts) and the
  TUI monitoring views.

## lab schema (Postgres)

- `projects(id, name, origin_kind git_url|local_path, origin, stack,
  default_credential_id, created_at)`
- `credentials(...)` — above
- `agents(id, project_id, name, role_prompt, model, credential_id,
  budget jsonb, state stopped|idle|working|paused|retired, container_id,
  branch, created_at)`
- `sessions(id, agent_id, claude_session_id, started_at, ended_at,
  end_reason, prev_session_id)`
- `turns(id, agent_id, session_id, source_kind user|agent, source_id,
  content, status queued|running|done|error, created_at, finished_at)`
- `events(...)` — above
- `usage_rollups(...)` — above

## kbase

Standalone knowledge base; a dependency of lab, developed alongside it.
Serves N projects; entries are project-scoped with an optional global scope
(`project_id` nullable).

### Data model

Everything is **versioned, append-only, with provenance**. Two-table core
(as built in Phase 9):

- `entries(id, project_id nullable, type decision|note|architecture|
  component|ticket, slug, created_by → principals, created_at)` — slugs
  unique per project scope, with NULL (global) its own scope via two
  partial unique indexes.
- `entry_versions(id, entry_id, version_no, title, content text
  (markdown), author → principals, created_at)` — "current" = max
  `version_no`; nothing is updated in place (single exception: ticket
  claim, below). Append-only is a **schema property**: a trigger
  rejects UPDATE/DELETE on version rows, even via direct SQL. The
  author is always the principal behind the request token — no request
  field can set it.

### Architecture graphs (first-class)

- **Components** are entries of type `component` (versioned like everything
  else).
- **Directed edges** between components: `edges(id, project_id,
  from_entry, to_entry, label, created_by_token, created_at,
  tombstoned_by_token, tombstoned_at)` — append-only; removal is a
  tombstone, so the graph at any time T is reconstructible.
- `kbase graph` renders the current graph (text/DOT/mermaid) for agents;
  the TUI can render it for humans.

### Ticketing (CAS)

Simple ticket system so impl agents can take work without stepping on each
other:

- `tickets(id, project_id, entry_id, status open|claimed|in_progress|done|
  abandoned, claimed_by, cas_version int)`
- Claiming is one atomic statement:
  `UPDATE tickets SET claimed_by=$agent, status='claimed',
  cas_version=cas_version+1 WHERE id=$id AND cas_version=$expected AND
  claimed_by IS NULL` — zero rows updated ⇒ lost the race, re-list and
  retry.
- Ticket body/comments/status history live in the entry's version chain, so
  tickets get the same provenance as everything else.

### Recall

The API is designed around the agent's recall loop, not fancy querying:
`kbase recall "<task description>" [--type ...] [-n 8]` → the N most
relevant current versions. Postgres full-text search first; pgvector only
if FTS proves insufficient. As built: weighted generated tsvectors —
title (A) + content (B) on `entry_versions`, slug words (A) on
`entries`, both GIN-indexed, concatenated at query time —
`websearch_to_tsquery` + `ts_rank`, current versions only, `ts_headline`
excerpts. Scope rules: project tokens read their project + global and
write only their project; scope-less tokens read/write everything and
disambiguate shadowed slugs with an explicit project.

### CLI surface (v1)

```
kbase add <type> [--slug s] [--title t] [-]      # new entry (content on stdin)
kbase update <slug> [-]                          # append new version
kbase show <slug> [--version n] [--history]
kbase recall "<query>" [--type t] [-n 8]
kbase graph [--format text|dot|mermaid]
kbase component add|link|unlink ...
kbase ticket list|show|claim|start|done|abandon|comment ...
```

## Milestones

- **M0** — scaffold: repo layout, config, migrations, `labd`/`kbased`
  skeletons with health endpoints.
- **M1** — single agent end-to-end: project create (clone/init), stack
  image build, container up, stream-json pipe, event log, turns via a
  minimal client. *Proves the core loop.*
- **M2** — TUI: project/agent views, live transcript, send turn, credential
  onboarding (setup-token flow), budget dashboards.
- **M3** — kbase: entries/versions/recall, tokens/provenance, `kbase` CLI
  in the image.
- **M4** — kbase: architecture graph + ticketing (CAS).
- **M5** — orchestration: `lab-agent` CLI (send/list/spawn/status),
  multi-agent worktrees + merge flow, budget enforcement + subscription
  rate-limit pause/resume, session retirement with kbase-seeded reboot.

## Deferred (explicitly post-v1)

Custom stacks via user Dockerfile; `--permission-prompt-tool` gating;
network egress policies; per-agent stack overrides; pgvector recall;
multi-machine anything.
