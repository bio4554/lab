# Phase 7 handoff — TUI v1 (`cmd/lab`)

You are the implementation agent for Phase 7 of the `lab` project.
This document is your complete brief. Read `docs/DESIGN.md`, then the
phase 05 and 06 reports — the daemon and its client API (Phase 6) are
what you are building the face for.

**Parallel phases**: 8 (credentials/budgets) and 9 (kbase) run
concurrently on other branches. Your file set: `cmd/lab/`, a new
`internal/labclient/` (HTTP client for the labd client API), tests.
You may add small read-only endpoints to `internal/labd/api/client.go`
(e.g. `GET /v1/stacks`, a usage listing) and matching `internal/wire`
types — list every API addition in your report. Do **not** touch the
driver, store schema, or credentials (Phase 8 owns those).

## Ground rules

- Branch `phase-07-tui` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded — no
  redesign. `docs/DESIGN.md`/`docs/PLAN.md` read-only.
- `make check` green when you stop.
- **Public repo — no secrets**, including in screenshots/captures in
  the report.
- Allowed new deps: `github.com/charmbracelet/bubbletea`,
  `github.com/charmbracelet/lipgloss`, `github.com/charmbracelet/bubbles`.
- Finish with `docs/handoffs/phase-07-report.md`.

## Deliverables

### 1. `internal/labclient`

Typed HTTP client for the labd client API (uses `internal/wire`):
projects/agents CRUD, start/stop/retire, submit turn, events backfill,
and an SSE subscriber (`Last-Event-ID` resume, auto-reconnect with
backoff) exposing a channel of `wire.Event`. Unit-test against
`httptest` fakes (and/or the real API package — your call).

### 2. `cmd/lab` — the TUI

bubbletea + lipgloss. Views (suggested layout, adapt as sensible):

- **Projects** — list with agent counts; create form (name, origin
  path/URL, stack picker populated from the API).
- **Agents** (per project) — name, state, status_text, current session,
  running indicator; actions: create (name, role prompt, model,
  credential kind), start, stop, retire (reason + seed prompt form).
- **Transcript** (per agent) — the heart of the TUI: live event feed
  over SSE with scrollback backfill; render events like `labctl tail`
  but nicer (role-colored text blocks, tool_use one-liners with the
  tool name + input summary, result line with cost/duration/tokens;
  unknown kinds as dim raw lines). A composer at the bottom submits
  turns; show queued/running/done state of the in-flight turn.
- **Sessions** (per agent) — session history with start/end times, end
  reasons, prev-chain, event counts; selecting one backfills its
  transcript read-only.
- **Usage** — simple table from `usage_rollups` (per agent: tokens
  in/out, cost, turns, last hour / today / total). Add the read-only
  API endpoint you need.
- Global: daemon status line (reachable? version? running drivers),
  key help footer, quit cleanly.

Keep the model/update/view code testable: rendering pure functions
over view-model structs; API calls in messages/commands. Unit-test
the view models and event-rendering (fixture events from
`internal/streamjson/testdata/` are convenient).

### 3. Config

`lab` reads the same `lab.toml` (client API address) with `LAB_*` env
overrides; `-addr` flag wins.

## Acceptance criteria

- `make check` green; view-model/render/labclient tests pass.
- Manual script in your report — number the steps; I will run them
  against a live daemon with real agents: connect → create project +
  agent → start → send turn from composer → watch live transcript →
  restart daemon mid-view (TUI reconnects, resumes without gaps) →
  retire from the TUI → usage view shows the spend.
- TUI degrades gracefully when the daemon is down (clear error state,
  retries; no panic).

## Out of scope

Credential onboarding & budget dashboards beyond the usage table
(Phase 8 owns credential/budget mechanics; a later pass wires its
screens into the TUI); kbase views (Phase 10+); any write endpoint
additions to the API; mouse support, themes, and other polish.
