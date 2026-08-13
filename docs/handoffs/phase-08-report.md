# Phase 8 report — Credentials & budgets

Branch `phase-08-credentials-budgets`, off `development`. `make check`
green; the changed packages pass `go test -race -count=1` twice
consecutively (Postgres + Docker up; Postgres-dependent suites skip
when it is down). The full acceptance flow below ran live on
2026-08-12 with a real OAuth token; the transcript is scrubbed — the
token was piped, grepped-for by count, and never printed.

## What was built

### `internal/labd/creds` — encrypted credential storage

- `vault.go` — `LoadOrCreateKey(dataDir)`: 32-byte key at
  `<data_dir>/secret.key`, generated with `O_EXCL` + mode 0600 on
  first use; refuses any mode with group/other bits (`perm&0o077 != 0`)
  with an error naming the exact `chmod 600 <path>` fix; refuses a
  wrong-size (corrupt) key file rather than padding. `Vault`
  `Encrypt`/`Decrypt`: NaCl secretbox, random 24-byte nonce prefix +
  ciphertext into `credentials.secret_enc`. New dep:
  `golang.org/x/crypto` (the one allowed).
- `source.go` — `Source`, the store-backed `claude.CredentialSource`:
  decrypts and maps kind → env var exactly like Phase 5's env source
  (`api_key` → `ANTHROPIC_API_KEY`, `oauth_token` →
  `CLAUDE_CODE_OAUTH_TOKEN`, exactly one injected). Expiry is checked
  lazily on every resolution; an expired-but-active row is flipped to
  `expired` on the way out. The env-var names are deliberately
  duplicated from `claude` (creds must not import the driver package,
  so driver tests can import creds — the leak scan does).
- Wired into `cmd/labd` in place of `EnvCredentialSource`; `labctl`'s
  in-process driver uses it too. The daemon now needs **no** Anthropic
  credential in its environment (demonstrated below).

### Migration `00004` + store

- `credentials.budget jsonb NOT NULL DEFAULT '{}'` and
  `credentials.limited_until timestamptz` (the rate-limit hold; NULL =
  not held).
- Store additions: `SetCredentialBudget` / `SetAgentBudget`,
  `SetCredentialExpiry` (clearing or extending past-expiry flips
  `expired` back to `active`), `SetCredentialLimited` (nil clears),
  `PauseAgentsForCredential` / `ResumeAgentsForCredential`
  (idle/working ↔ paused), `ExpireCredentials` + `ReleaseExpiredLimits`
  (the sweep queries; the latter resumes agents and clears holds in
  one transaction), `CountCredentialRefs` (agents + project defaults),
  `SetAgentCredential`, `PeekQueuedTurn` (head of queue without
  claiming), `AgentUsageInWindow` (per-agent rollup sums).

### `internal/labd/budget` — limits + gate

- The budget jsonb contract (both `agents.budget` and
  `credentials.budget`):

  ```json
  {"max_cost_usd_day": 5.0, "max_tokens_day": 2000000, "max_turns_hour": 30}
  ```

  All keys optional (absent = unlimited); unknown keys and negative
  values are rejected at set time. **Windows are UTC, computed from
  `usage_rollups`**: "day" = the current UTC calendar day, "hour" = the
  current UTC clock hour (matching the rollups' hourly window grain).
  "Tokens" = tokens_in + tokens_out. An **agent budget is evaluated
  against the agent's own usage; a credential budget against the
  credential's usage across all its agents; every set limit must pass**
  — that is the operational meaning of "both apply, most restrictive
  wins".
- `Gate.Check(ctx, agentID) (Verdict, error)` — deny carries a reason
  string and a retry-after (next hour/next UTC midnight/rate-limit
  reset; zero for expiry, which never heals on its own). Check order:
  no credential → allow (nothing metered); missing/expired/non-active
  credential → deny (expiry flips status lazily, same as the source);
  rate-limit hold in the future → deny with the reset as retry-after
  (a hold already in the past opens immediately, without waiting for
  the sweep); then budgets.
- **Deviation from the handoff sketch**: `Check` takes the agent *id*,
  not the agent row, and re-reads agent + credential every check. The
  pump's agent struct is loaded once per process; a stale row would
  mean budget raises/rate-limit releases only apply after a driver
  restart — the acceptance flow ("raising the budget releases it")
  requires live re-reads. Two extra point queries per idle-poll-with-
  queued-turn, only when a turn is actually waiting.

### Driver: `TurnGate` seam + rate-limit pause/resume

- `claude.Options.TurnGate` (interface in `claude`, implemented by
  `budget.Gate`; nil = allow — every pre-existing test and labctl path
  is unchanged when unset).
- The pump's `tryNextTurn` now: **peek** (never claims), gate check,
  and only on allow `NextQueuedTurn` → deliver. A denied turn is
  therefore never flipped to running and never errored; the denial is
  logged once per distinct reason and re-checked on the existing
  poll/wake cadence, so the turn delivers the moment the gate opens
  (budget raise, window rollover, reset passing, manual resume).
- `rate_limit_event` handling in the stream: parsed via the new typed
  accessor; a limited status (`allowed`/`allowed_warning` pass,
  anything else is limited) records `limited_until = resetsAt` on the
  credential and **pauses every idle/working agent using it**
  (containers stay up, turns stay queued). Also on **is_error results**
  whose text matches an unmistakable limit pattern
  (`rate limit|usage limit|too many requests|429` as a word), with a
  default 5-minute hold when no reset time is available — short on
  purpose; re-checking a few times beats stalling on a guess, and
  nothing spins against 429s at that cadence. The pattern is only
  consulted on error results, so assistant prose about rate limits in
  successful turns can't trip it (tested).
- Auto-resume is two independent paths: the gate treats a passed
  `limited_until` as open (turn delivery resumes immediately and
  flips the agent working), and labd's 30s sweep clears passed holds +
  resumes paused agents (so agents with empty queues don't linger
  paused) and expires past-expiry credentials. Manual resume:
  `POST /v1/credentials/{id}/resume`.

### `internal/streamjson` — typed rate-limit accessor (stdlib only)

`KindRateLimitEvent`, `RateLimit` struct
(status/resetsAt/rateLimitType/overage fields), `Event.RateLimit()`,
`RateLimit.Limited()` and `.ResetTime()`. Tested against the real
fixture line (`tool_use.jsonl` line 2).

### API + wire

```
POST   /v1/credentials                      create {kind,label,secret,expires_at?,no_expiry?,budget?}
GET    /v1/credentials                      list — id,label,kind,status,expires_at,budget,limited_until; never the secret
DELETE /v1/credentials/{id}                 409 while referenced (agents or project defaults)
PUT    /v1/credentials/{id}/expiry          set/clear expires_at (clearing/extending reactivates)
POST   /v1/credentials/{id}/resume          manual rate-limit release (clears hold, resumes agents)
GET|PUT /v1/credentials/{id}/budget         credential budget
GET|PUT /v1/projects/{p}/agents/{a}/budget  agent budget
PUT    /v1/projects/{p}/agents/{a}/credential  rebind (null unbinds; applies at next provision)
GET    /v1/usage                            usage/budget status (below)
```

- Secrets travel **into** the API exactly once, in the create body —
  localhost-only client API, accepted for v1 per the handoff. No
  response anywhere carries secret material (asserted on raw wire
  bytes in tests).
- `GET /v1/usage` (for Phase 7's TUI later): per credential and per
  agent, today's and this hour's usage plus the stored budget, the
  credential's status/hold, and a **live gate verdict per agent**
  (allowed, or reason + retry_after) — the deny reason in the
  acceptance flow is visible here.
- Agent create: gains `credential_id` (validated) and `budget`.
  **Compat shim, recorded**: `credential_kind` no longer creates an
  env-passthrough row (Phase 5/6 behavior) — it now resolves to *the
  only stored credential of that kind*, erroring when none or several
  exist. Same shim in `labctl agent create -cred`; `-cred-id` is the
  precise form. Old env-passthrough credential rows in a dev database
  have empty `secret_enc` and will fail decryption with a clear error
  if an agent still references one — rebind those agents or recreate
  them.
- Agent listings gain `credential_id` always, and `paused_until`
  (the credential's recorded reset) while the agent is paused.
- `wire`: Credential, CreateCredentialRequest, SetExpiryRequest,
  BudgetPayload, BudgetVerdict, CredentialUsage, AgentBudgetStatus,
  UsageStatus, SetAgentCredentialRequest; Agent/CreateAgentRequest
  extended.

### `cmd/labd`

Vault opened at startup (**a loose key file fails startup** with the
chmod hint — demonstrated below), gate + source wired into the driver,
vault + gate into the client server, and the 30-second
expiry/rate-limit sweep goroutine (stops with the hub context).

### `cmd/labctl`

- `cred add -kind api_key|oauth_token -label personal [-expires
  RFC3339|never]` — **secret read from stdin** (prompt only when stdin
  is a TTY), trimmed, encrypted, stored; never echoed, never in argv or
  shell history. `expires_at` defaults for `oauth_token` to now + 1
  year (the documented `claude setup-token` lifetime), overridable
  with `-expires`, suppressible with `-expires never`.
- `cred list` — id, kind, status, expiry, label, current hold.
- `agent create`: `-cred` resolves an existing credential by kind (the
  shim), `-cred-id` binds exactly; the in-process driver now carries
  the vault source and a real gate.

**Onboarding**: `claude setup-token` in any terminal (needs a
browser), copy the token, then
`./bin/labctl cred add -kind oauth_token -label personal` and paste at
the prompt (Enter, Ctrl-D). API keys the same with `-kind api_key`
(no default expiry). Then bind agents via `credential_id` at create,
or `PUT .../credential`.

## Tests (all live-Postgres suites skip when it's down; all pass -race)

- `creds`: vault round-trip (incl. nonce uniqueness, tamper/truncate/
  wrong-key failures), key-file creation mode + 0644 refusal + corrupt
  key refusal, kind→env mapping, source resolve/expiry-flip/
  reactivation/wrong-key against the live store (fabricated secrets).
- `budget`: `ParseLimits` (incl. unknown-key and negative rejection);
  `TestGateVerdicts` — the table (limit kinds × usage states × windows
  × scopes): unlimited-when-unset, hour/day windows, sibling-agent
  aggregation on credential scope vs isolation on agent scope,
  most-restrictive-wins both directions, retry-after boundaries;
  `TestGateCredentialStates` — no-credential allow, hold deny + passed-
  hold open, expiry deny + lazy flip + reactivation.
- `claude` (fake-process harness): gate-denied turn stays queued
  across repeated checks and delivers on open; rate_limit_event →
  hold + sibling paused → second turn held → **timed delivery after
  the reset** → sweep releases hold and resumes the sibling;
  limit-shaped error result → ~5m default hold; success results
  mentioning rate limits and `allowed` events trip nothing; detector
  pattern table; the full acceptance budget flow
  (`max_turns_hour: 1` deny naming the budget → raise → release);
  `TestNoSecretLeaks` — mint → resolve → deliver a full fixture turn
  with all driver logs captured, then scan logs, every event payload,
  the turn row, and `secret_enc` for the canary.
- `api`: credential CRUD with raw-body assertions that no response
  ever contains the secret (or `secret_enc`); oauth default expiry;
  budget round-trips (both scopes) incl. validation 400s; expiry
  set/clear; manual resume unpausing a paused agent;
  paused_until surfaced in agent listings; delete 409-while-referenced
  then 204; `/v1/usage` reflecting usage, a live deny verdict with
  retry_after, and the flip back to allowed after a raise.
- `store`: new-method coverage (budgets, holds, pause/resume, both
  sweeps, refs count, peek-doesn't-claim, agent usage windows);
  migration round-trip extended for 00004's columns.

## The demo (ran live 2026-08-12; re-runnable)

```
$ make db-up && make migrate-lab && go build -o bin/labd ./cmd/labd && go build -o bin/labctl ./cmd/labctl

# 1. labd with NO credential in its environment at all:
$ env -u ANTHROPIC_API_KEY -u CLAUDE_CODE_OAUTH_TOKEN ./bin/labd &
$ ls -la ~/.lab/secret.key
-rw-------  1 bio4554  staff  32 ...        # created 0600 on first use

# 2. real token from stdin (source ~/.lab/demo.env only in this shell):
$ set -a; source ~/.lab/demo.env; set +a; printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" | ./bin/labctl cred add -kind oauth_token -label demo8
credential demo8 added (id 019ff91b-3fe7-…, kind oauth_token, expires 2027-08-13T03:12:20Z)   # +1y default

# 3. project + agent bound to credential_id → turn runs:
$ curl -s -X POST localhost:7710/v1/projects -d '{"name":"demo8","origin":"<scratch repo>","stack":"base"}'
$ curl -s -X POST localhost:7710/v1/projects/demo8/agents -d '{"name":"impl1","role_prompt":"…","credential_id":"019ff91b-3fe7-…"}'
$ curl -s -X POST localhost:7710/v1/projects/demo8/agents/impl1/start   # 204
# labd: msg="claude process started" agent=impl1 …
$ curl … /turns -d '{"content":"…read /work/README.md…"}'   # → status done

# 4. secret_enc is ciphertext (148 bytes for a fabricated-length token):
$ psql … "SELECT length(secret_enc), left(encode(secret_enc,'hex'),40) FROM lab.credentials WHERE id='019ff91b…'"
148 | 9aba278baa5d2e0b65684654da30fcede5223b0e

# 5. tight budget holds; deny visible; raise releases:
$ curl -X PUT …/agents/impl1/budget -d '{"budget":{"max_turns_hour":1}}'
$ curl … /turns -d '{"content":"Say hi in one word."}'      # stays "queued"
$ curl -s localhost:7710/v1/usage    # for impl1:
verdict: {allowed: false, reason: "agent budget: max_turns_hour 1 reached (1 this hour)", retry_after: "2026-08-13T04:00:00Z"}
# labd logged exactly one "turn held by budget gate" line
$ curl -X PUT …/budget -d '{"budget":{"max_turns_hour":30}}'   # → held turn runs to done

# 6. leak scan on the live run (token grepped by count, never printed):
log hits: 0   events hits: 0   api list hits: 0
container env: exactly one of CLAUDE_CODE_OAUTH_TOKEN/ANTHROPIC_API_KEY present

# 7. SIGTERM → clean shutdown; then the permission refusal:
$ chmod 644 ~/.lab/secret.key && env -u … ./bin/labd
error="creds: key file /Users/bio4554/.lab/secret.key has mode 0644, readable beyond its owner; fix with: chmod 600 /Users/bio4554/.lab/secret.key"
$ chmod 600 ~/.lab/secret.key
```

Demo container + `lab-claude-*` volume removed afterwards; dev-DB rows
left in place per convention.

## Decisions & deviations (smallest-call gaps)

- **Gate re-reads rows by agent id** rather than taking the pump's
  agent struct — see above; required by the raise-releases acceptance.
- **Peek-then-gate-then-claim** ordering so a denied turn is never
  flipped to running; the peek is one cheap indexed query and only
  happens when the pump is idle.
- **Rate-limit statuses**: `allowed` and `allowed_warning` pass; any
  other non-empty status is limited. The fixture only shows `allowed`;
  if the CLI grows more benign statuses the list is one switch arm.
- **`limit-shaped result` default hold = 5 minutes** (no reset time in
  those errors). Manual resume and the gate's clock-based opening
  bound the cost of a wrong guess in both directions.
- **Budget semantics**: scope-local usage per budget (agent budget ↔
  agent usage, credential budget ↔ credential usage); documented in
  `budget.Limits`. Hour windows are UTC clock hours (rollup grain),
  not rolling 60 minutes.
- **Expiry default applied server-side too** (API create), not just in
  labctl, with `no_expiry` to opt out; `SetCredentialExpiry`
  reactivates an expired credential when the new expiry is future/none.
- **Old env-passthrough rows** (empty `secret_enc`) now fail
  resolution with a decryption error; agents bound to them need
  rebinding. Deliberate: silent env fallback is exactly what this
  phase removes.
- The pre-existing `cmd/migrate` prints "up to date" even when it
  applied migrations (already on the Phase-12 backlog; bitten again
  this phase).

## Acceptance results

- `make check` green in all configurations (Docker+Postgres up;
  Postgres down → creds/budget/store/api/claude suites skip; Docker
  down → runtime-dependent tests skip). Changed packages `-race
  -count=1` twice consecutively, green.
- End-to-end ran live exactly as the criteria list (transcript above):
  stdin token → `credential_id`-bound agent → **credential-free labd
  env** → turn done → ciphertext in DB → `max_turns_hour: 1` deny
  visible in `/v1/usage` with the held turn queued → raise → release.
- Key file 0600 on creation; 0644 startup refusal demonstrated.
- No plaintext secret outside the encrypted column, the driver's
  in-memory env map, and the container env — leak-scanned in tests
  (canary) and on the live run (real token, count-only greps).

## Notes for later phases

1. Phase 7 TUI: `GET /v1/usage` and `paused_until`/`credential_id` on
   agent listings are ready to consume; credential screens can drive
   the CRUD endpoints as-is.
2. Phase 12 (cost attribution): rollups still record
   `result.total_cost_usd` verbatim and input-side tokens summed;
   per-model attribution would need the model captured per result
   event (it's in the assistant events already) — idea: a
   `model` column on usage_rollups keyed windows.
3. The sweep is 30s/labd-wide; if agent counts grow, holds/expiry
   could move to LISTEN-driven precision, but the gate's lazy checks
   already bound staleness to one poll interval for queued work.
4. `PUT .../agents/{a}/credential` applies at next provision; a hosted
   driver keeps its current container env until restart — acceptable
   now, worth a driver poke when Phase 11 automates rebinding.

---

## Orchestrator review (2026-08-12) — closed

Full diff read; `make check` green; changed packages pass `-race
-count=1` twice; diff secret-scan clean (only fabricated test strings).
Live demo re-run end-to-end with a real token: credential-free labd
env → `cred add` from stdin (+1y default expiry) → `credential_id`-bound
agent ran a turn → `secret_enc` = 148-byte ciphertext →
`max_turns_hour: 1` held the second turn queued (deny reason +
retry_after in `/v1/usage`; exactly one "turn held by budget gate" log
line across continuous polling) → raise → released to done → leak scan
0 hits (logs, events, credential list, usage; container env held
exactly one credential var) → SIGTERM clean shutdown → 0644 key file
refused at startup with the chmod hint.

Accepted deviations: gate re-reads rows per check (required by the
raise-releases acceptance), labctl-only onboarding (TUI screens
explicitly out of scope), `credential_kind` shim now binding stored
credentials (old env-passthrough rows fail loudly — deliberate).
DESIGN.md's Credentials & budgets section updated to the as-built
contract. Merged as 664ecd3.
