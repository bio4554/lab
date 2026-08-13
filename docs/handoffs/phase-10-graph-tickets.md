# Phase 10 handoff — kbase graph & tickets

You are the implementation agent for Phase 10 of the `lab` project.
This document is your complete brief. Read `docs/DESIGN.md` ("kbase"
in full) and `docs/handoffs/phase-09-report.md` — Phase 9 built the
kbase core (entries, versions, principals/tokens, recall, `kbased`,
`kbclient`, the `kbase` CLI) and this phase extends it with the two
remaining kbase features: **architecture graphs** (components + edges
with tombstones) and **CAS ticketing**. The `component` and `ticket`
entry types already exist in the schema enum but are inert.

**Your file set**: `migrations/kbase/00003_*.sql`, `internal/kbased/`,
`internal/kbclient/`, `cmd/kbase`. You should not need to touch labd,
the TUI, or the agent image plumbing — the `kbase` binary is already
embedded in agent images and rebuilding it is automatic (the image
content-hash changes once; note it in your report). Phase 11
(`lab-agent`, labd orchestration) may run in parallel — do not touch
`cmd/lab-agent`, `internal/labd/`, or `cmd/labd`.

## Ground rules

- Branch `phase-10-graph-tickets` off `development`.
- Stay in scope; if a contract below is wrong or incomplete, make the
  smallest reasonable call and record it in the report — do not
  redesign. `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop; Postgres-dependent tests skip
  gracefully when the compose DB is down.
- **Public repo — no secrets.** Nothing in this phase should need new
  secret material; keep it that way.
- Allowed new deps: none expected (pgx/goose/uuid already present).
- Finish with `docs/handoffs/phase-10-report.md`.

## Established conventions you must follow

From Phase 9 (all in `internal/kbased/`):

- **Provenance is server-derived.** The authoring principal always
  comes from the request token (`Caller` in the auth middleware) —
  never from a request field. DESIGN.md's edge sketch says
  `created_by_token`; the as-built convention is principal FKs
  (`created_by → principals`), so use principals for edges and tickets
  too and note the deviation-from-sketch in your report.
- **Scope rules** (see `writeScope`/`resolveSlug` in `server.go`):
  project tokens read their project + global, write only their
  project; scope-less tokens read/write everything and must
  disambiguate shadowed slugs with an explicit `project_id` (409
  otherwise). Edges and tickets follow the same rules.
- **Append-only is a schema property**: `00002_core_tables.sql` uses a
  plpgsql trigger to reject UPDATE/DELETE on `entry_versions`. Edge
  immutability (below) gets the same treatment.
- Errors: `writeStoreError` mapping, plain 401 for auth, 409 for
  conflicts. HTTP routes use Go 1.22 method patterns on the `/v1` mux
  behind `authenticate`; admin mux is not involved this phase.

## Deliverables

### 1. Schema (`migrations/kbase/00003_*.sql`)

In the `kbase` schema (uuidv7 defaults, real Downs):

- `edges(id, project_id uuid null, from_entry fk entries, to_entry fk
  entries, label text not null, created_by fk principals, created_at,
  tombstoned_by fk principals null, tombstoned_at timestamptz null)`.
  - Both endpoints must be entries of type `component` (validate
    server-side; a CHECK can't reach across tables).
  - `project_id` follows entry scope semantics (NULL = global). An
    edge lives in the writer's scope; its endpoints must be readable
    in that scope (own project or global).
  - **Append-only with tombstones**: a trigger permits exactly one
    UPDATE — setting `tombstoned_by`/`tombstoned_at` when both are
    NULL — and rejects DELETE and every other UPDATE. Removal is a
    tombstone so the graph at any past time T is reconstructible:
    live-at-T = `created_at <= T AND (tombstoned_at IS NULL OR
    tombstoned_at > T)`.
  - Duplicate live edge (same from, to, label, scope) → 409; after
    tombstoning, the same edge may be recreated as a new row.
- `tickets(id, project_id uuid null, entry_id fk entries unique,
  status text check in
  ('open','claimed','in_progress','done','abandoned') default 'open',
  claimed_by fk principals null, cas_version int not null default 0,
  created_at, updated_at)`.
  - Every ticket is backed by an entry of type `ticket` (1:1 via the
    unique `entry_id`); the entry's version chain is the ticket's
    body + comment + status history, so tickets get the same
    provenance and immutability as everything else.

### 2. Ticket semantics (exactly per DESIGN.md)

- **Claim is one atomic statement**:
  `UPDATE tickets SET claimed_by=$principal, status='claimed',
  cas_version=cas_version+1, updated_at=now() WHERE id=$id AND
  cas_version=$expected AND claimed_by IS NULL` — zero rows updated ⇒
  lost the race; the API returns 409 with the current ticket state so
  the caller can re-list and retry. Claim is valid from `open` or
  `abandoned` (both have `claimed_by IS NULL`).
- Transitions (each bumps `cas_version` and requires the caller's
  expected version; `start`/`done`/`abandon` additionally require the
  caller to be the claimant):
  - `claim`: open|abandoned → claimed (sets claimed_by)
  - `start`: claimed → in_progress
  - `done`: claimed|in_progress → done
  - `abandon`: claimed|in_progress → abandoned (clears claimed_by so
    the ticket is claimable again; history shows it was abandoned)
- **Every mutation appends a version** to the ticket's entry, authored
  by the acting principal: creation writes v1 (title + markdown body);
  comments and status transitions append a version whose content is
  the previous content plus a trailing activity line (e.g.
  `\n\n---\n_claimed by agent:impl1 — 2026-08-13T??:??Z_` or the
  comment text with the same attribution shape). Keep the format
  simple and stable — agents will read it; exact formatting is your
  smallest-call to make, record it.
- Ticket status/claimed_by/cas_version live only in `tickets` (the
  single sanctioned in-place mutation in kbase); everything narrative
  lives in the version chain.

### 3. `kbased` API (all under the existing authenticated `/v1` mux)

Graph:

- `POST /v1/edges` — `{from, to, label, project_id?}`; from/to are
  component slugs resolved with the existing `resolveSlug` semantics.
- `DELETE /v1/edges/{id}` — tombstone (records the acting principal);
  tombstoning an already-tombstoned edge → 409.
- `GET /v1/graph?at=<RFC3339>` — nodes (component entries visible to
  the caller: slug, current title, project scope) + edges live at
  `at` (default: now). This is the data endpoint; rendering is the
  CLI's job.

Tickets:

- `POST /v1/tickets` — `{title, body, slug?, project_id?}`; creates
  the entry (type `ticket`, slug generated from title if absent, v1 =
  body) and the ticket row in one transaction.
- `GET /v1/tickets?status=&limit=` — newest first, includes slug,
  title, status, claimed_by display name, cas_version.
- `GET /v1/tickets/{id}` — ticket row + current entry version.
- `POST /v1/tickets/{id}/claim|start|done|abandon` — each takes
  `{cas_version}`; CAS/permission failures → 409 with current state.
- `POST /v1/tickets/{id}/comment` — `{text}`; appends a version, does
  not touch status (no CAS needed, but bump `updated_at`).

### 4. `internal/kbclient`

Typed methods for all of the above (`CreateEdge`, `TombstoneEdge`,
`Graph`, `CreateTicket`, `ListTickets`, `GetTicket`, `ClaimTicket`,
`StartTicket`, `DoneTicket`, `AbandonTicket`, `CommentTicket` — names
indicative). Wire types in `types.go`; 409 surfaces as `APIError` with
the current ticket state decoded so callers can retry without a second
GET.

### 5. `cmd/kbase` CLI (extend the existing custom `parse`)

Per DESIGN.md's v1 surface:

```
kbase graph [--format text|dot|mermaid] [--at <RFC3339>]
kbase component add --title t [--slug s] [-m body | stdin]   # sugar for add component
kbase component link <from> <to> --label l
kbase component unlink <edge-id>            # or <from> <to> --label l if unambiguous
kbase ticket list [--status s]
kbase ticket show <id|slug>
kbase ticket claim <id|slug>       # GET → CAS claim once; on 409 print current state, exit 1
kbase ticket start|done|abandon <id|slug>
kbase ticket comment <id|slug> -m "text"
```

- `graph --format text` = readable adjacency list; `dot` = valid
  Graphviz; `mermaid` = a valid `graph LR`/`flowchart` block (this
  gets pasted into docs — it must render).
- Claim does **not** retry internally — exactly one CAS attempt, so
  scripted concurrent claimers behave predictably; exit codes stay
  0 success / 1 API-or-CAS failure / 2 usage, as in Phase 9.
- Keep the binary lean; it ships in every agent image.

## Tests

Live-Postgres (+ live kbased where needed), skip gracefully:

- **Concurrent claim**: N parallel claimers (goroutines through the
  real API or CLI) on one ticket — exactly one wins, all others get
  409; ticket ends with one claimant and cas_version advanced once.
- Full ticket lifecycle: create → claim → start → comment → done;
  history shows every step with the acting principal; abandon path
  ends claimable-again and a second principal successfully re-claims.
- Stale CAS: transition with an old cas_version → 409, no state
  change.
- Edge immutability: direct SQL UPDATE (non-tombstone) and DELETE on
  edges rejected by the trigger; second tombstone attempt → 409.
- Graph reconstruction: build a graph, tombstone an edge, add
  another; `?at=` before the tombstone returns the old topology,
  default returns the new one.
- Edge validation: non-component endpoint → 4xx; duplicate live edge
  → 409; scope rules (project token can't link another project's
  components; can link own-project → global).
- CLI round-trip: component add ×2 → link → graph in all three
  formats (assert dot/mermaid syntactic shape) → unlink → graph
  reflects it.

## Acceptance criteria

- `make check` green; new tests pass with `-race`;
  `make migrate-kbase` up/down round-trips on 00003.
- Demo (documented in your report; the orchestrator re-runs it):
  with both daemons up and an agent container running, from inside
  the container: `kbase component add` twice, `component link`,
  `kbase graph --format mermaid` (output renders), then
  `kbase ticket list` → `claim` → `start` → `done` with
  `kbase ticket show --history`-equivalent (`kbase show <slug>
  --history`) showing agent provenance on every step. From the host,
  two concurrent `claim` invocations on a fresh ticket: exactly one
  succeeds.
- No plaintext tokens introduced anywhere; no secrets in the diff.

## Out of scope

labd/TUI graph rendering; ticket assignment/notifications (agents
poll via `ticket list`); `lab-agent` (Phase 11); pgvector; edge
attributes beyond a single label; component deletion (entries are
forever — tombstoning is for edges only).
