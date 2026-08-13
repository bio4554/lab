# lab — Phased Build Plan

Companion to [DESIGN.md](DESIGN.md). This document is **orchestrator-owned**:
the orchestrator agent maintains it, impl agents must not edit it.

## Process contract

Roles:

- **Orchestrator** (Claude, this repo's managing session): writes handoff
  prompts, reviews completed work, closes phases, maintains PLAN.md/DESIGN.md.
  Does not implement phases.
- **Impl agent**: a fresh Claude Code session given one handoff prompt.
  Implements exactly one phase on a branch.
- **User**: starts impl agents, arbitrates when the orchestrator escalates.

Lifecycle of a phase:

1. **Handoff** — orchestrator writes `docs/handoffs/phase-NN-<slug>.md`:
   context, scope (in/out), interface contracts, deliverables, acceptance
   criteria, ground rules. The prompt is self-contained; the impl agent
   should not need conversation history.
2. **Implementation** — impl agent works on branch `phase-NN-<slug>` off
   `development`. Commits freely; finishes with the repo green
   (`make check` passes) and writes `docs/handoffs/phase-NN-report.md`
   (what was built, deviations from the handoff, open questions).
3. **Review** — orchestrator reviews the diff against acceptance criteria,
   builds, runs tests, exercises the deliverable. Outcome: **bounce**
   (fix-list appended to the report; impl agent or a fresh agent addresses
   it on the same branch) or **close**.
4. **Close-out** — orchestrator merges to `development`, updates the status
   table below, and records any decisions made during the phase in
   DESIGN.md (or kbase, once it exists — dogfooding).

Ground rules for impl agents (repeated in every handoff):

- Stay in scope. If the handoff's interface contracts are wrong or
  incomplete, record the problem in the report and make the smallest
  reasonable call — do not redesign.
- `docs/DESIGN.md`, `docs/PLAN.md`, and other phases' code are read-only
  unless the handoff says otherwise.
- Tests are part of the deliverable. `make check` (fmt, vet, build, test)
  must pass.
- No new third-party dependencies beyond those the handoff lists without
  recording justification in the report.
- **This repo is public on GitHub.** Never commit secrets: Anthropic API
  keys, OAuth tokens (`sk-ant-oat01-...`), agent/kbase bearer tokens,
  credential-store key files, or a real `lab.toml`. Local-only dev DB
  passwords (docker-compose Postgres) are explicitly fine. Example configs
  use obvious placeholders.

## Status

| Phase | Title                                  | Status  | Branch | Depends on |
| ----- | -------------------------------------- | ------- | ------ | ---------- |
| 0     | Scaffold & dev infrastructure          | **done** (6ce72f8, merged) | phase-00-scaffold | —          |
| 1     | lab schema & store layer               | **done** (82ce39f, merged) | phase-01-store | 0          |
| 2     | stream-json codec                      | **done** (776c569, merged) | phase-02-streamjson | 0          |
| 3     | gitrepo: projects, worktrees           | **done** (a0a8fa5, merged) | phase-03-gitrepo | 0          |
| 4     | Docker runtime & stack images          | **done** (2355d94, merged) | phase-04-runtime | 0          |
| 5     | Claude driver: sessions, turns, events | **done** (369f757, merged; demo re-run by orchestrator) | phase-05-claude | 1,2,3,4    |
| 6     | labd APIs & agent tokens               | handed off | phase-06-api | 5          |
| 7     | TUI v1                                 | pending | —      | 6          |
| 8     | Credentials & budgets                  | pending | —      | 6          |
| 9     | kbase core: daemon, CLI, recall        | pending | —      | 0 (schema), 6 (tokens pattern) |
| 10    | kbase graph & tickets                  | pending | —      | 9          |
| 11    | lab-agent & orchestration loop         | pending | —      | 6, 9       |
| 12    | Integration hardening                  | pending | —      | all        |

Phases 1–4 are independent of each other and can run as parallel impl
agents. So can 7/8, and 9 alongside 7/8.

## Phases

### Phase 0 — Scaffold & dev infrastructure

**Goal**: a repo where every later phase has an obvious home and one-command
verification.

**Deliverables**: Go module; directory layout per DESIGN.md; config loading
(`labd`, `kbased` read a TOML/env config); goose migration tooling wired for
both schemas with an initial empty migration each; `Makefile` (`check`,
`test`, `build`, `migrate-lab`, `migrate-kbase`, `db-up` via docker-compose
Postgres); `cmd/labd` and `cmd/kbased` skeletons that start, connect to
Postgres, serve `/healthz`, and shut down cleanly on SIGTERM.

**Acceptance**: fresh clone + `make db-up && make migrate-lab migrate-kbase
&& make check` passes; both daemons run and answer `/healthz`.

### Phase 1 — lab schema & store layer

**Goal**: the lab-side Postgres schema and a typed store package.

**Deliverables**: migrations for `projects`, `credentials`, `agents`,
`sessions`, `turns`, `events`, `usage_rollups` (per DESIGN.md);
`internal/labd/store` with pgx CRUD + the queries later phases need
(enqueue/dequeue turn, append event with per-session seq, session chaining);
`LISTEN/NOTIFY` on event append. Store tests against real Postgres
(docker-compose), guarded by a build tag or env var.

**Acceptance**: `make check` incl. store tests green; schema matches
DESIGN.md; event append is safely concurrent (seq has no gaps/dupes under
parallel writers — tested).

### Phase 2 — stream-json codec

**Goal**: `internal/streamjson` — types + codec for the Claude Code
stream-json interface, pure and heavily tested.

**Deliverables**: typed events (system/init, assistant, user, result, and a
lossless raw/unknown passthrough), decoder from an `io.Reader` stream,
encoder for user-turn input messages, usage/cost extraction from `result`.
Fixtures captured from a real `claude -p --output-format stream-json
--verbose` run (handoff includes instructions to capture them).

**Acceptance**: round-trip and fixture-based decode tests pass; unknown
event kinds survive decode/encode losslessly.

### Phase 3 — gitrepo: projects, worktrees

**Goal**: `internal/labd/gitrepo` — project repo management.

**Deliverables**: create project from git URL (bare clone) or local folder
(init if needed, then treat as origin); per-agent worktree + branch
create/remove; merge agent branch → project main with conflict reporting
(no auto-resolution); data-dir layout under a configurable root. Shell out
to `git` (documented decision — worktree support in go-git is not worth the
risk).

**Acceptance**: integration tests covering both origin kinds, two agents on
parallel worktrees, clean merge and conflicting merge paths.

### Phase 4 — Docker runtime & stack images

**Goal**: `internal/labd/runtime` + `stacks/` — images and containers.

**Deliverables**: stack Dockerfile templates (`base`, `go`, `node`,
`python`, `rust`) per DESIGN.md (pinned Claude Code CLI; `kbase`/`lab-agent`
copied in — stub main packages are enough this phase); content-hash image
tagging and lazy build via Docker API; container create/start/stop/remove
with the DESIGN.md contract (mounts, env, `host.docker.internal`); attach
to main process stdio.

**Acceptance**: tests (Docker required, tagged) build the `base` image, run
a container echoing stdin→stdout over the attach API, verify mounts/env;
`go` stack builds and `go version` works inside.

### Phase 5 — Claude driver: sessions, turns, events (core loop)

**Goal**: M1 — one real agent end-to-end. The riskiest phase; everything
before exists to de-risk it.

**Deliverables**: `internal/labd/claude` — start/attach `claude` in the
container (stream-json flags, `--append-system-prompt`), pump stdout →
codec → event store, deliver queued turns serially, close turns on
`result`, `--resume` on restart, session retirement operation (end session,
start successor with seed prompt); minimal `labctl` dev CLI (create
project/agent, send turn, tail events) — throwaway, replaced by the TUI.

**Acceptance**: scripted demo (documented in report): create project on a
sample repo, start agent (real credential from env), send "create
FEATURE.md summarizing this repo", watch events stream into Postgres, file
appears in the agent's worktree; agent survives container restart via
resume; retirement produces a linked successor session.

### Phase 6 — labd APIs & agent tokens

**Goal**: `internal/labd/api` — the daemon's two HTTP surfaces.

**Deliverables**: client API (localhost): projects/agents CRUD, turn
submit, SSE event stream (backed by LISTEN/NOTIFY), status; agent API
(host-bound for containers): whoami, send-turn-to-agent, list agents,
report-status — bearer-token auth with per-agent tokens minted at container
create; `internal/wire` request/response types shared with clients.

**Acceptance**: API integration tests; token auth enforced (wrong/revoked
token → 401/403); SSE stream delivers live events with backfill from a
cursor.

### Phase 7 — TUI v1

**Goal**: `cmd/lab` — bubbletea client.

**Deliverables**: project list/create (incl. stack picker), agent
list/create/start/stop, live transcript view (SSE), send-turn composer,
session history, basic usage/budget panel. Talks only through the client
API and `internal/wire`.

**Acceptance**: manual script in report (orchestrator will run it);
non-TUI logic (view models, API client) unit-tested.

### Phase 8 — Credentials & budgets

**Goal**: `internal/labd/creds` + `internal/labd/budget` complete.

**Deliverables**: encrypted credential storage (secretbox; key file);
TUI-side onboarding flows (`claude setup-token` walkthrough, API key
paste); env injection with the one-credential rule; usage rollups from
result events; kind-aware budget config + pre-dispatch enforcement;
subscription rate-limit detection → pause all agents on the credential,
auto-resume at window reset; expiry surfacing.

**Acceptance**: unit tests for enforcement decisions (table-driven: kinds ×
limits × usage states); rate-limit pause/resume covered by a simulated 429
event stream; credentials never appear in logs or the event store
(asserted).

### Phase 9 — kbase core: daemon, CLI, recall

**Goal**: standalone kbase per DESIGN.md — entries, versions, provenance,
recall.

**Deliverables**: kbase schema migrations (`entries`, `entry_versions`,
`tokens`); `kbased` HTTP API (bearer tokens, principals registered by
labd); `internal/kbclient`; `cmd/kbase` CLI: `add`, `update`, `show`
(incl. `--history`), `recall` (Postgres FTS); labd mints kbase tokens at
container create and injects `KBASE_URL`/`KBASE_TOKEN`.

**Acceptance**: CLI round-trip tests against a live kbased; version
history immutable (updates append); every version carries the authoring
principal; recall returns relevant entries for seeded fixtures.

### Phase 10 — kbase graph & tickets

**Goal**: architecture graphs and CAS ticketing.

**Deliverables**: `component` entries + append-only `edges` with
tombstones; `kbase graph` (text/dot/mermaid) and `component add|link|
unlink`; `tickets` table + CAS claim exactly per DESIGN.md; `kbase ticket
list|show|claim|start|done|abandon|comment`; ticket history in the entry
version chain.

**Acceptance**: concurrent-claim test (N parallel claimers, exactly one
wins); graph reconstructible at time T; mermaid output renders.

### Phase 11 — lab-agent & the orchestration loop

**Goal**: agents driving agents — the point of the whole system.

**Deliverables**: `cmd/lab-agent` (send, agents list, status, spawn if
enabled) against the agent API; orchestrator-role plumbing: role prompt
templates, worker-spawn policy flag per agent; session retirement wired to
kbase recall for successor seeding; end-to-end demo: an orchestrator agent
+ one worker agent complete a two-step task via tickets.

**Acceptance**: the demo runs scripted against real Claude with a test
credential; every cross-agent turn is attributed in the event log.

### Phase 12 — Integration hardening

**Goal**: make it honest software.

**Deliverables**: crash-recovery sweep (labd restart reconciles container
reality vs. DB state), graceful shutdown everywhere, structured logging,
`make e2e` running a tagged end-to-end suite, README with real setup
docs, backlog of known gaps filed as kbase tickets (dogfood).

**Acceptance**: kill -9 labd mid-turn → restart reconciles and the turn
errors cleanly rather than wedging; e2e suite green twice consecutively.
