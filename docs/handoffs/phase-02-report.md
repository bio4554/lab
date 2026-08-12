# Phase 2 report — stream-json codec

Status: **complete**. `make check` green; no new module deps; package
imports stdlib only (`bufio`, `bytes`, `encoding/json`, `errors`,
`fmt`, `io`).

## What was built

`internal/streamjson`:

- **`event.go`** — `Event{Kind, Subtype, Raw}` with the raw line
  retained verbatim (`json.RawMessage`). Kind/Subtype come from a
  minimal envelope parse and are populated for unknown kinds too.
  Typed accessors parse `Raw` on demand:
  - `SessionID()` — top-level `session_id` (present on every CLI
    output event).
  - `Init() (*Init, error)` — session ID, model, tools from
    `system/init`; errors on any other kind/subtype.
  - `Message() (*Message, error)` — the Anthropic-shaped message of
    assistant/user events; `ContentBlock` covers text, thinking,
    tool_use, tool_result and keeps each block's raw bytes.
  - `Text()` — concatenated text blocks (empty for non-messages and
    thinking-only events).
  - `ToolUses()` — id/name/input summaries of tool_use blocks.
  - `Result() (*Result, error)` — subtype, `is_error`, `duration_ms`,
    `num_turns`, `total_cost_usd`, `session_id`, and `Usage`
    (input/output/cache-creation/cache-read tokens).
- **`decoder.go`** — `NewDecoder(io.Reader)` / `Next() (Event, error)`.
  Explicit 32MB line limit (`MaxLineBytes`); `io.EOF` at stream end;
  blank lines skipped. Malformed lines return
  `*MalformedLineError{Line, Err}` carrying the raw line — the
  decoder has already advanced, so the caller chooses to log/persist
  and continue. A line over the limit returns a wrapped
  `ErrLineTooLong` (terminal — bufio.Scanner cannot advance past it).
- **`encoder.go`** — `NewEncoder(io.Writer)`. `UserMessage(text)`
  emits the `{"type":"user","message":{...}}` input line, newline
  included, in a single `Write`. `RawLine(raw)` writes an event's raw
  bytes back out — the re-encode half of the lossless guarantee.

## Fixtures (`testdata/`)

Captured from the real CLI (`claude -p ... --output-format stream-json
--verbose`), reviewed line by line for secrets before commit — they
contain only session IDs, local paths, and server-signed thinking
signatures (none of which are credentials; `apiKeySource` is `"none"`):

- `simple.jsonl` — init, thinking-only assistant event, assistant
  text, a real `rate_limit_event` (an unknown kind captured in the
  wild), result.
- `tool_use.jsonl` — init, `system/thinking_tokens` (known kind,
  unknown subtype), assistant tool_use (Bash `echo hello`), user
  tool_result, assistant text, result.
- `unknown_kind.jsonl` — hand-crafted plausible future kinds
  (`stream_event`, `control_response`, `compaction`,
  `permission_request`, `future_thing`), including nested unknowns
  and odd key ordering to stress raw passthrough.

## Tests

Fixture-driven decode (kinds, session IDs, init fields, text
extraction, tool_use/tool_result pairing, result usage/cost/duration
parsing); byte-for-byte losslessness of decode → `RawLine` re-encode
for every fixture; unknown-kind decode + accessor refusal; malformed
line (typed error, then stream continues); blank-line skipping;
oversized line (>32MB) rejection; 8MB line accepted; encoder golden
test, escaping/one-line invariant, and multi-message output.

## Notes / judgment calls (no redesign)

- Observed streams stamp `session_id` on every event, so
  `Event.SessionID()` works everywhere, not just init; init discovery
  still goes through `Init()`.
- The real `result` event carries far more than the spec'd fields
  (`modelUsage`, `permission_denials`, iteration breakdowns…). Those
  stay available via `Raw`/jsonb rather than typed fields — Phase 1
  persists the raw payload, so nothing is lost.
- Accessors called on the wrong kind return errors rather than zero
  values (except the convenience helpers `Text`/`ToolUses`, which are
  documented to return empty).
- Malformed-line recovery: the decoder advances past the bad line;
  policy (drop, persist, abort) is the caller's, per the brief.
- Oversized lines are terminal by nature of `bufio.Scanner`; this is
  documented on `ErrLineTooLong`. Phase 5 can treat it as a session
  failure.
