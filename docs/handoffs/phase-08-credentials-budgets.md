# Phase 8 handoff — Credentials & budgets

You are the implementation agent for Phase 8 of the `lab` project.
This document is your complete brief. Read `docs/DESIGN.md`
("Credentials & budgets" especially), then the phase 05/06 reports —
you are replacing Phase 5's env-passthrough `CredentialSource` with
real storage, and adding the budget/rate-limit machinery around the
driver.

**Parallel phases**: 7 (TUI) and 9 (kbase) run concurrently. Your file
set: `internal/labd/creds/` (new), `internal/labd/budget/` (new),
`internal/labd/claude/` (driver seams), `internal/labd/store`,
`internal/labd/api` (credential + budget endpoints), `cmd/labd`,
`cmd/labctl` (onboarding command), `migrations/lab/`, `internal/wire`.
Do not touch `cmd/lab` (Phase 7 owns it; its credential screens come
later) or anything kbase.

## Ground rules

- Branch `phase-08-credentials-budgets` off `development`.
- Stay in scope; smallest calls recorded; DESIGN/PLAN read-only.
- `make check` green when you stop; dependent tests skip gracefully.
- **Public repo.** This phase handles real secrets: no secret may
  appear in a commit, a log line, an error message, an event payload,
  or an API response. Tests use fabricated secrets only.
- Allowed new deps: `golang.org/x/crypto` (nacl/secretbox).
- Finish with `docs/handoffs/phase-08-report.md`.

## Deliverables

### 1. Encrypted credential storage (`internal/labd/creds`)

- Key management: a 32-byte key in `<data_dir>/secret.key`, file mode
  0600, generated on first use; refuse to start with looser
  permissions (clear error telling the user what to fix).
- `Vault`: `Encrypt`/`Decrypt` (secretbox, random nonce prefix) around
  `credentials.secret_enc`; a `store`-backed CredentialSource
  implementation (`Resolve` decrypts and maps kind → env var exactly
  like Phase 5's env source) wired into `cmd/labd` in place of
  `EnvCredentialSource`. `labctl`'s in-process mode uses it too.
- API + wire: credential create (label, kind, secret in the request
  body — localhost-only API, acceptable for v1), list (id, label,
  kind, status, expires_at, created_at — **never** the secret), delete
  (refused while referenced by an agent), and set-expiry. Agent
  create/update gains `credential_id` (the Phase 6 API took a bare
  kind; keep it working by resolving kind → the only credential of
  that kind, error if ambiguous — record this compat shim).
- `labctl cred add -kind oauth_token -label personal` reading the
  secret from stdin (so it never lands in shell history), plus `cred
  list`. Onboarding docs in the report: `claude setup-token` →
  paste. For `expires_at`, default oauth tokens to created_at + 1 year
  (that's the documented lifetime; overridable).

### 2. Budgets (`internal/labd/budget`)

- Config: `budget` jsonb on agents (exists) and a new `budget` jsonb
  column on credentials (your migration). Schema (document as the
  contract):
  `{"max_cost_usd_day": 5.0, "max_tokens_day": 2000000,
  "max_turns_hour": 30}` — all optional; agent and credential budgets
  both apply (most restrictive wins).
- `Gate`: `Check(ctx, agent) (Verdict, error)` consulted by the driver
  **before each turn delivery** (add a driver seam like TurnWake — a
  `TurnGate` option; nil = allow). Verdict: allow, or deny with
  reason + retry-after. A denied turn stays queued (do not error it);
  the pump logs once and re-checks on the poll/wake cadence.
- Daily/hourly windows computed from `usage_rollups` (UTC).
- API + wire: get/set agent and credential budgets; a usage/budget
  status endpoint (current window usage vs limits per credential and
  per agent) — Phase 7's TUI will consume this later.

### 3. Rate-limit pause/resume

- Parse `rate_limit_event` payloads (real fixture:
  `internal/streamjson/testdata/tool_use.jsonl` line 2 — add a typed
  accessor in `internal/streamjson`, it stays stdlib-only) and
  limit-shaped `result` errors.
- On a credential hitting its window limit (`status` not allowed, or
  unmistakable limit error): **pause every agent using that
  credential** — driver-level hold (agents stay `paused` state, turns
  stay queued, containers stay up), record the reset time, and
  auto-resume when it passes. Manual resume via API too.
- Surface paused-state + reset time in agent/status listings.
- Simulate in tests by feeding synthetic rate_limit_event/result
  streams through the pump (Phase 5's fake-process harness does this
  well).

### 4. Expiry surfacing

Credentials past `expires_at` → status `expired` (checked lazily on
use + a periodic sweep in labd); agents resolving an expired/missing
credential fail their turn with a clear queued-side denial (same
non-destructive hold as budget denial), and the API lists the
credential as expired.

## Tests

- Vault round-trip; key-file permission enforcement; source resolves
  and injects correctly (fabricated secrets).
- Gate: table-driven verdicts (kinds × limits × usage states ×
  windows); most-restrictive-wins; unlimited when no budget set.
- Pump + gate: denied turn stays queued and delivers after the gate
  opens; rate-limit event pauses and timed resume works (fake clock or
  short windows).
- API: credential CRUD (secret never echoed — assert on the wire),
  budget get/set round-trip, status endpoint.
- Leak scan test: run a mint/resolve/deliver cycle and assert the
  secret string appears nowhere in captured logs or the events table.

## Acceptance criteria

- `make check` green in all configurations; new tests pass with
  `-race`.
- End-to-end (documented; I re-run): `labctl cred add` with a real
  token from stdin → agent bound to `credential_id` → labd (no
  credential in daemon env at all now) runs a turn successfully →
  `secret_enc` in the DB is ciphertext → tight `max_turns_hour: 1`
  budget on the agent → second turn stays queued with a deny reason
  visible in the status endpoint → raising the budget releases it.
- Key file created 0600; startup refuses 0644 (demonstrated).
- No plaintext secret anywhere but the key-file-encrypted column, the
  driver's env map in memory, and the container env.

## Out of scope

TUI screens (Phase 7 owns `cmd/lab`; wiring these endpoints into it is
a later pass); kbase tokens (Phase 9); per-model cost attribution
beyond what `result.total_cost_usd`/usage gives (note ideas for Phase
12); OS keychain storage (post-v1 — key file is the v1 decision).
