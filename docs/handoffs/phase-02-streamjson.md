# Phase 2 handoff — stream-json codec

You are the implementation agent for Phase 2 of the `lab` project. This
document is your complete brief. Read `docs/DESIGN.md` first —
especially "Agent runtime > Driving Claude Code" and "Event log".

Your job: `internal/streamjson` — a **pure, dependency-free, heavily
tested** package for the Claude Code CLI's stream-json interface
(`claude -p --input-format stream-json --output-format stream-json
--verbose`). Phase 5 builds the agent driver on top of it; Phase 1 (in
parallel) stores your decoded events as jsonb. You are the only phase
touching `internal/streamjson`.

## Ground rules

- Branch `phase-02-streamjson` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded in your
  report — no redesign.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop.
- **This repo is public on GitHub.** Fixtures must contain no secrets —
  scrub anything resembling a credential before committing; session
  IDs and local paths are acceptable.
- Allowed new deps: **none** (stdlib only).
- Finish with `docs/handoffs/phase-02-report.md`.

## Deliverables

### 1. Types

A typed `Event` model for the stream's output lines. Known kinds:

- `system` (notably `subtype: "init"` — carries `session_id`, model,
  tools; but other subtypes exist)
- `assistant` / `user` — wrap an Anthropic-API-shaped `message`
  (content blocks: text, tool_use, tool_result, thinking)
- `result` — terminal event per prompt: `subtype`
  (`success`/error kinds), `is_error`, `duration_ms`, `num_turns`,
  `total_cost_usd`, `usage` (input/output/cache tokens), `session_id`

Design requirements:

- **Lossless**: every event retains its raw line (`json.RawMessage`).
  Unknown event kinds and unknown fields inside known kinds must
  survive decode → re-encode byte-for-byte (raw passthrough — do not
  attempt field-level round-tripping).
- Typed accessors for what lab actually needs: session ID discovery
  (init), text extraction from assistant messages, tool_use
  name/input summaries, and a `Result` struct exposing usage tokens,
  `total_cost_usd`, `is_error`, `duration_ms`.
- `Kind` + `Subtype` always available, even for unknown events.

### 2. Decoder / Encoder

- `NewDecoder(io.Reader)` with `Next() (Event, error)` — line-delimited
  JSON, buffer sized for very long lines (tool results can be
  megabytes; make the limit explicit and generous, e.g. 32MB), `io.EOF`
  at stream end, and a policy for malformed lines (return a typed error
  carrying the raw line; the caller decides — don't silently drop).
- `NewEncoder(io.Writer)` producing **input** lines: a
  `UserMessage(text string)` constructor emitting the
  `{"type":"user","message":{"role":"user","content":[...]}}` shape,
  one line, flushed.

### 3. Fixtures (`internal/streamjson/testdata/`)

Capture real streams — you have the `claude` CLI available in your own
environment:

```sh
claude -p 'say hi' --output-format stream-json --verbose > testdata/simple.jsonl
claude -p 'run `echo hello` and tell me the output' --output-format stream-json --verbose > testdata/tool_use.jsonl
```

(Adjust prompts as needed to get: an init event, plain assistant text, a
tool_use/tool_result pair, and a result event. Review the captured files
line by line before committing — public repo.) Also hand-craft
`unknown_kind.jsonl` with plausible future event kinds.

### 4. Tests

- Fixture-driven: decode every fixture line; assert kinds, session-ID
  extraction, result usage/cost parsing, text extraction.
- Losslessness: for every fixture line, decode → re-encode raw →
  byte-equal.
- Encoder golden test for `UserMessage`.
- Oversized-line and malformed-line behavior.

## Acceptance criteria

- `make check` green; no new module deps in `go.mod`.
- Unknown-kind fixture decodes without error and round-trips
  losslessly.
- Package has no imports outside stdlib.

## Out of scope

Process management, containers, retries (Phase 5); persistence
(Phase 1); the input-side control protocol beyond user messages (Phase
5 owns interrupt/permission shapes if needed and may extend this
package then).
