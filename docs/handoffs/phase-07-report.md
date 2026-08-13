# Phase 7 report — TUI v1 (`cmd/lab`)

Branch `phase-07-tui`, off `development`. `make check` green (Postgres
up; the api/store/claude suites ran against the live dev database).
New dependencies: `charmbracelet/bubbletea`, `lipgloss`, `bubbles`
(the three allowed), plus their transitive deps via `go mod tidy`.

## What was built

### `internal/labclient`

Typed client for every client-API route, `wire` types on both sides:
status, project CRUD, agent list/create/start/stop/retire, turn
submit/get, session listing, project usage, per-session and global
event backfill (`AllSessionEvents` pages until drained). Non-2xx
responses become `*labclient.APIError{Status, Message}` from the
`wire.Error` body.

`StreamEvents(ctx, afterID)` is the SSE subscriber: it exposes
`Events <-chan wire.Event` and `Status <-chan StreamStatus`
(connected / disconnected+attempt+backoff transitions; statuses are
drop-oldest so a slow UI never blocks the stream). It reconnects
forever with exponential backoff (0.5s → 10s cap, reset after a
successful connect), resuming via `Last-Event-ID` from the last
delivered event so the feed is gapless and duplicate-free across
daemon restarts. `afterID < 0` starts live-only (no header), matching
the server; scrollback comes from backfill instead.

Unit tests (httptest fakes, `-race`): request/path-escaping/decoding
round-trips, error mapping, backfill pagination, and three stream
tests — resume header across a dropped connection with no gaps or
dupes, live-only omitting the header, and disconnect status reporting.

### `cmd/lab`

bubbletea TUI in chrome direction **1c**: flat frame, hairline
splits, focused element gets the box + accent edge. Layout: header
status line (`lab ● labd <version> · connected · drivers N`, address
right-aligned) / left project+agent tree / main pane / footer key
hints that change with focus.

- **Projects** — main pane table (name, origin, stack, agents,
  created) when no agent is open; `p` opens the create dialog (name,
  origin, stack picker populated from `/v1/status`).
- **Agents** — tree rows under each project with run glyph (● hosted
  driver / ○ not / ◌ retired), name, state; `n` create dialog (name,
  role prompt textarea, model, credential picker
  oauth_token/api_key/none), `s` start, `x` stop, `r` retire dialog
  (reason + seed prompt). Errors from actions surface in the footer.
- **Transcript** — the main tab: per-session event accumulation
  merged from REST backfill + the live SSE stream, deduplicated by
  seq (the overlap window between backfill and stream start is safe
  by construction). Rendering per the 1c vocabulary: `you ▌` /
  `<agent> ▌` role gutters with wrapped continuation indents, `⚙`
  tool one-liners (name + input summary, ✓/✗ once the tool_result
  arrives), `✔/✘` result lines (duration · in→out tok · cost), dim
  `·`-prefixed raw lines for unknown kinds, session-start marker with
  model. The CLI does not echo prompts as events, so the TUI injects
  each turn's prompt as the `you ▌` line itself (agent-sent turns get
  the source agent's name): turn ids seen in the stream are resolved
  once via `GET /v1/turns/{id}` and cached.
  Composer at the bottom (enter sends, ctrl+j newline, grows
  to 5 lines); the tab row shows the in-flight turn as
  `turn <id> · queued/running/done`, driven by stream events. The
  transcript follows the agent's current session across retires.
- **Sessions** — tab 2: session / span / end reason (`▶ running`
  while open) / event count / prev-chain, from the new sessions
  endpoint. Enter opens that session's transcript; historical
  sessions render read-only (BACKFILL badge, no composer, esc back
  to live).
- **Usage** — tab 3: per-agent last hour / today / total
  (tok in/out · cost, turns in total) plus a Σ project row, from the
  new usage endpoint. `R` refreshes.
- **Daemon down** — REST polling failure flips the header to
  `◌ labd unreachable · reconnecting` and centers a CONNECTION LOST
  box (last event id, auto-retry note, `r` retry now, `q` quit). The
  2s poll and the SSE subscriber keep retrying independently; on
  recovery the stream resumes from its last id — no gaps, no
  restart needed.
- Config: `lab` reads the same `lab.toml` (`labd.client_api_addr`)
  with `LAB_*` env overrides via the shared loader; `-addr` wins.
  `-config` accepts an explicit path (missing file only errors when
  explicit, same semantics as the daemons).

Model/update/view separation for testability: `buildTranscript`,
`toolInputSummary`, the formatters, `buildTree`, session/usage cell
builders, and `transcriptState` merging are all pure and covered by
unit tests (`cmd/lab`), using the `internal/streamjson/testdata/`
fixtures (simple, tool_use, unknown_kind) wrapped as stored events —
including out-of-order input and backfill/stream overlap.

## API additions (all read-only, as invited by the handoff)

- `GET /v1/projects/{project}/agents/{agent}/sessions` →
  `[]wire.Session` — sessions newest-first with `event_count` and
  `prev_session_id`.
- `GET /v1/projects/{project}/usage` → `[]wire.AgentUsage` — per
  agent `{last_hour, today, total}` of `{tokens_in, tokens_out,
  cost_usd, turns}`; hour = rollup windows starting within the last
  hour, today = since local midnight (daemon clock), buckets by
  window start.
- `internal/wire`: `Session`, `Usage`, `AgentUsage` types.
- `internal/labd/store` (read-only queries): `ListSessions`
  (sessions + event counts), `ProjectUsage` (one grouped query with
  `FILTER` windows). Covered by `TestSessionListAndProjectUsage` in
  the api package (live Postgres, skips when down).
- No `GET /v1/stacks`: the mockup's "from /v1/stacks" is served by
  the stack list already present in `GET /v1/status`.

No write endpoints were added; driver, store schema (no migration),
and credentials untouched.

## Deviations from the mockups (noted per the brief)

- **Both create forms are modal dialogs** (2b style) — 2a draws the
  project form inline in the main pane; one dialog mechanism keeps
  the code smaller.
- **Modals replace the body area centered** rather than compositing
  over a dimmed frame — lipgloss v1 has no overlay primitive and
  ANSI-aware line merging wasn't worth the code; the header/footer
  frame stays visible.
- **Keybindings**: tab switching is `1/2/3` (with `t`/`u` kept as
  transcript/usage shortcuts per the mockups); `n` = new agent and
  `p` = new project instead of one context-dependent `n`; agent
  actions (`s`/`x`/`r`) live on the tree selection. `?` help overlay
  and "a: edit address" in the daemon-down box were dropped (footer
  hints cover the keys; address comes from config/flag).
- Session ids display as `019ff8…` (first 6 hex of the UUID), the
  suggested adaptation.
- The connection-lost box appears whenever REST polling fails;
  mid-transcript stream interruption additionally shows as
  `stream reconnecting` in the header rather than an inline rule in
  the transcript.

## Tests

- `internal/labclient`: `TestNewNormalizesAddr`,
  `TestRequestsAndDecoding`, `TestAPIError`,
  `TestAllSessionEventsPagination`, `TestStreamResumeAcrossReconnect`,
  `TestStreamLiveOnlyOmitsHeader`, `TestStreamReportsDisconnect`.
- `cmd/lab`: `TestBuildTranscript*` (tool_use fixture incl. outcome
  matching, unknown kinds, simple, seq-sort), `TestToolInputSummary`,
  `TestFormatters`, `TestTranscriptStateDedupe`, `TestBuildTree`,
  `TestAgentGlyph`, `TestSessionRows`, `TestShortID`,
  `TestUsageCells`, `TestSumUsage`.
- `internal/labd/api`: `TestSessionListAndProjectUsage` (new), the
  Phase 6 suite untouched and green.
- `make check` green; `go test -race -count=1` green on the new
  packages.

## Manual acceptance script

Prereqs: Docker up, credentials in `~/.lab/demo.env` (outside the
repo), scratch origin repo (e.g. `/tmp/demo-repo-p7` with one
commit). `make startd`/`make startc` handle Postgres, migrations, and
builds.

1. Terminal A: `make startd` (sources `~/.lab/demo.env`, runs labd).
2. Terminal B: `make startc` (or `./bin/lab -addr 127.0.0.1:7710`).
   Header shows `● labd <version> · connected · drivers 0`.
3. Press `p` → create project `demo7`, origin `/tmp/demo-repo-p7`,
   stack `base` (←/→ on the picker), enter. The project appears in
   the tree and table.
4. Select `demo7` in the tree (↑↓), press `n` → agent `impl1`, a
   short role prompt, model empty, credential `oauth_token` (or
   `api_key` to match your env), enter.
5. Select `impl1`, press `s` (start). Within a few seconds the glyph
   goes ●, state `idle` (first start builds the stack image — watch
   terminal A).
6. Press enter on `impl1` → transcript opens, composer focused. Type
   `Create FEATURE.md summarizing this repository, then stop.` and
   press enter. The tab row shows `turn … · queued` → `▶ running`;
   the transcript streams live: `you ▌` line, `⚙` tool one-liners
   gaining ✓, agent text, then the `✔ done · … tok · $…` result and
   the indicator flips to `done`.
7. Restart mid-view: Ctrl-C labd in terminal A while a second turn is
   running (send one first). The TUI header flips to `◌ labd
   unreachable` with the CONNECTION LOST box (it names the last event
   id). Restart labd (step 1 command). Within ~2s the header
   reconnects and the transcript continues from where it stopped —
   scroll back: no missing and no duplicated lines.
8. Press `2` (sessions): one row, `▶ running`, event count matching.
   Press `r` with `impl1` selected in the tree → retire dialog:
   reason `manual test`, seed `Say hello and summarize your role.`,
   enter. Sessions now show the old session ended (`manual test`) and
   a new one chained (PREV column); the transcript tab follows the
   new session and streams the seeded turn. Select the old session,
   enter → read-only BACKFILL view; esc returns to live.
9. Press `3` (usage): `impl1` shows the spend (last hour ≈ today;
   totals with turn count) and the Σ row matches. `R` refreshes.
10. Press `x` to stop the agent (glyph ○), `q` quits cleanly
    (terminal restored).

## Live-run findings & post-review fixes (2026-08-12)

The script above was exercised against a live daemon with a real
credential; the core loop passed (turn done, 15 events, result
`is_error=false`, usage rollups correct via the new endpoint). Issues
found in that run, fixed on this branch:

- **Turn prompts were invisible** — the CLI does not echo prompts as
  stream events, so transcripts showed only tool calls and replies.
  Fixed by resolving turn ids seen in events via `GET /v1/turns/{id}`
  (cached, fetched once) and injecting each prompt as the `you ▌`
  line at the turn's first event; agent-sent turns are labeled with
  the source agent's name. Covered by
  `TestBuildTranscriptInjectsTurnPrompts`.
- **Composer border clipped** — lipgloss `Width()` excludes borders,
  so the box overflowed the pane and lost its right edge. Width math
  fixed for the composer box and transcript viewport.
- **Focus was hard to see** — focus indicators added in the 1c
  idiom: the `PROJECTS` label and active tab pill render accent only
  while their pane holds focus; tree/sessions selections stay visible
  when unfocused (bar drops accent → dim); the footer key hints are
  prefixed with the focused pane's name.
- **One-command startup** — `make startd` (Postgres up + migrate +
  build + run labd, sourcing `~/.lab/demo.env` when present) and
  `make startc` (build + run the TUI); README gained a Quick start.
  A fresh clone needs only those two commands.

Also observed in the live run, not a bug: submitting a turn to an
agent that was never started leaves the indicator on `queued`
(labd queues durably; the turn drains on start). A "not started"
hint in the composer area is a possible later nicety.

## Notes / open questions for later phases

1. Transcript retention is unbounded per viewed session (fine at lab
   scale; a ring buffer is a small follow-up if long sessions get
   heavy).
2. The global SSE feed delivers all agents' events; the TUI keeps
   only sessions it has viewed. A per-session stream filter
   server-side would shrink traffic if that ever matters.
3. The usage view is per-project (selected agent's project). A
   cross-project rollup ("g group by project" in mockup 2d) is a
   natural Phase 8 extension once budgets land.
4. Credential onboarding is out of scope as briefed: the create-agent
   dialog only records the env-passthrough kind (Phase 8 wires real
   storage and its screens).
5. `refreshCmd` lists agents per project sequentially (N+1 noted in
   the Phase 6 report); at TUI poll cadence this is negligible
   locally.
