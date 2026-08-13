# Phase 10 report — kbase graph & tickets

Branch: `phase-10-graph-tickets`. `make check` green; the new suites
pass with `-race -count=1` (run twice); `make migrate-kbase` up/down
round-trips on 00003 (verified after the final schema edit). No new
dependencies, no labd/TUI/lab-agent files touched, no secret material
anywhere in the diff.

## What was built

- **Schema** (`migrations/kbase/00003_graph_tickets.sql`):
  - `edges`: directed labeled edges between component entries, with
    scope semantics matching entries (NULL project = global) and
    provenance on both ends of the lifecycle (`created_by`,
    `tombstoned_by` → principals). Append-only is a schema property: a
    plpgsql trigger permits exactly one UPDATE — the first tombstone,
    with every other column unchanged — and rejects DELETE and all
    other UPDATEs. Duplicate *live* edges are blocked by a pair of
    partial unique indexes on (scope, from, to, label) — NULL-scope
    gets its own index, as with entry slugs — so a tombstoned edge can
    be recreated as a new row.
  - `tickets`: the single sanctioned in-place mutation in kbase.
    1:1 with an entry of type `ticket` (unique `entry_id`);
    status/claimed_by/cas_version/updated_at live here, everything
    narrative lives in the entry's version chain.
- **`internal/kbased`**: `graph.go` + `tickets.go` (store + handlers,
  mirroring the Phase 9 file layout). New `/v1` routes:
  `POST/DELETE /v1/edges[/{id}]`, `GET /v1/graph?at=`,
  `POST/GET /v1/tickets[/{ref}]`, and
  `POST /v1/tickets/{ref}/{claim,start,done,abandon,comment}`.
  Claim is the atomic statement from DESIGN.md verbatim; every
  transition runs CAS + version-append in one transaction, and a
  zero-row CAS returns 409 with the ticket's current state in the
  body. The admin mux is untouched.
- **`internal/kbclient`**: wire types for edges/graph/tickets and
  typed methods (`CreateEdge`, `TombstoneEdge`, `Graph`,
  `CreateTicket`, `ListTickets`, `GetTicket`, `ClaimTicket`,
  `StartTicket`, `DoneTicket`, `AbandonTicket`, `CommentTicket`).
  `APIError` gained a `Ticket` field: ticket 409s decode the current
  state so callers retry without a second GET.
- **`cmd/kbase`**: `graph` (text/dot/mermaid, `--at`), the `component`
  group (add/link/unlink) and the `ticket` group
  (list/show/claim/start/done/abandon/comment), in two new files;
  still stdlib + uuid only. Claim performs exactly one CAS attempt —
  no internal retry — and a lost claim prints the current state and
  exits 1 (0 success / 1 API-or-CAS / 2 usage, as in Phase 9).

## Decisions recorded (smallest-reasonable-call, per the ground rules)

1. **Principal FKs, not `created_by_token`** — as the handoff
   instructed, edges and tickets record principals
   (`created_by`/`tombstoned_by`/`claimed_by` → `kbase.principals`),
   deviating from DESIGN.md's older edge sketch. Provenance stays
   server-derived from the request token.
2. **Bare ticket entries are rejected**: `POST /v1/entries` with type
   `ticket` is now a 400 pointing at `POST /v1/tickets` — a ticket
   entry without its row would be an unclaimable orphan and the 1:1
   invariant is worth a hard edge. `kbase add ticket` is sugar for the
   ticket API (prints `created ticket <slug> (open, cas 0)`), so the
   CLI surface from the handoff needed no new `ticket create` verb.
   The Phase 9 recall test seeded a bare ticket entry; that seed now
   goes through the ticket API (same searchable corpus).
3. **Activity-line format** (stable, agents read it): every transition
   appends a version whose content is the previous content plus
   `\n\n---\n_<verb> by <kind>:<name> — <RFC3339-UTC>_` with verbs
   `claimed`/`started`/`completed`/`abandoned`; comments append the
   same shape with verb `comment` followed by a newline and the text.
   Titles carry forward unchanged.
4. **Ticket endpoints take `{ref}` = ticket id *or* entry slug** (the
   handoff's CLI accepts `<id|slug>`; resolving server-side through
   the shared `resolveSlug` rules keeps the CLI lean and gives slugs
   the same shadowing semantics as everywhere else).
5. **Graph `?at=` filters nodes too** (`created_at <= at`), not just
   edges, so a reconstructed past topology doesn't show components
   that didn't exist yet. Components are still forever — no tombstones
   on nodes.
6. **`component add` body is optional**: without `-m` (or an explicit
   trailing `-` for stdin) the title doubles as the body, so
   registering a component is a one-liner and never hangs waiting on
   stdin.
7. **Edge endpoint resolution follows the edge's scope**: a
   project-scoped edge resolves endpoints project-first-then-global; a
   global edge (scope-less writer, no `project_id`) requires global
   endpoints. Cross-project endpoints are simply not visible → 404.
8. **Write-scope on mutations**: project tokens tombstone only their
   own project's edges and transition/comment only their own project's
   tickets (global tickets are readable but need a scope-less token to
   mutate) — the same read-wide/write-narrow rule as entries. CAS and
   claimant failures are 409 (with state); scope failures stay 403.
9. Tickets have no `--global` flag; a scope-less token's tickets
   default to global exactly like entries (`writeScope` unchanged).

## Tests (live Postgres via the shared fixture; skip gracefully)

`internal/kbased`: concurrent claim (8 principals through the real
API — exactly one wins, seven 409s each carrying current state, final
cas_version 1); full lifecycle create→claim→start→comment→done with
per-step authors asserted in history, plus abandon→re-claim by a
second principal; stale CAS and non-claimant 409s with no state
change; edge validation (non-component 400, duplicate live 409,
different-label ok, cross-project 404, own→global ok, scope-less
global edge needs global endpoints); tombstone lifecycle (provenance
recorded, second tombstone 409, recreation ok, foreign-project 403);
graph reconstruction via `?at=` against server timestamps; direct-SQL
edge UPDATE/DELETE and second-tombstone-by-SQL all rejected by the
trigger; ticket scope matrix. `cmd/kbase`: round-trip component add
×2 → link → graph in all three formats (dot/mermaid shape asserted) →
unlink → graph reflects it; ticket lifecycle through the CLI with a
losing second claim (exit 1, state printed) and `show --history`
provenance. The fixture cleanups now also sweep edges (trigger
disabled just for the delete) and tickets.

## Demo (for the orchestrator re-run)

```sh
make startd            # both daemons, shared admin token (Phase 9 flow)
# create/start an agent (make startc), then inside the container:
docker exec -it <agent> kbase component add --title "API Server"
docker exec -it <agent> kbase component add --title "Postgres DB"
docker exec -it <agent> kbase component link api-server postgres-db --label "stores state in"
docker exec -it <agent> kbase graph --format mermaid   # paste into docs; it renders
docker exec -it <agent> kbase add ticket --title "Wire the health check" -m "labd /healthz needs a kbased probe."
docker exec -it <agent> kbase ticket list
docker exec -it <agent> kbase ticket claim wire-the-health-check
docker exec -it <agent> kbase ticket start wire-the-health-check
docker exec -it <agent> kbase ticket done  wire-the-health-check
docker exec -it <agent> kbase show wire-the-health-check --history   # agent provenance on every step

# host: mint a human token (Phase 9 report has the curl pair), then:
export KBASE_URL=http://127.0.0.1:7720 KBASE_TOKEN=<human-token>
./bin/kbase add ticket --title "Race me" -m "contended"
./bin/kbase ticket claim race-me & ./bin/kbase ticket claim race-me & wait
# exactly one prints "claim: race-me is now claimed (cas 1)";
# the other prints the current state and exits 1
```

## Notes for the orchestrator

- **Image hashes change once**: `cmd/kbase` grew the graph/ticket
  commands, so the builder's content hash yields new tags and a
  one-time rebuild per stack on first use — same as Phase 9, nothing
  else about the image story moved.
- Phase 11 overlap: nothing under `cmd/lab-agent`, `internal/labd/` or
  `cmd/labd` was touched; the only shared file edited is
  `internal/kbclient` (additive types/methods) and `internal/kbased`
  (new files + additive `server.go` routes/error cases), so no merge
  conflict is expected beyond trivial adjacency.
- Behavior change worth knowing: `kbase add ticket` used to create a
  bare entry (Phase 9 stored it inertly); it now creates a real
  claimable ticket, and bare ticket entries are refused by the API.

---

## Orchestrator review — closed (2026-08-13)

Reviewed against the phase-10 handoff acceptance criteria. **Zero
bounces.** Merged to `development` as 9bb08f8.

**Code review**: full 15-file diff read (+2437/−12). Migration 00003
trigger checks every column (nullable ones via `IS DISTINCT FROM`) and
the CHECK ties `tombstoned_by`/`tombstoned_at` together; ticket
transitions hold the tickets-row lock inside the CAS transaction, so
version appends serialize without the AppendVersion retry loop;
comments take the same lock via their `updated_at` bump. Claim's
`claimed_by IS NULL` predicate correctly encodes open|abandoned (done
keeps its claimant, so finished tickets are not reclaimable). All nine
recorded decisions accepted — notably the bare-ticket-entry rejection
(hard 1:1 invariant) and reclaimable-abandoned semantics.

**Checks**: `make check` green on the branch and post-merge;
`internal/kbased` + `cmd/kbase` pass `-race -count=1` twice; migration
00003 down/up round-trips against live Postgres (tables + trigger
verified present after re-up). Secret scan of the diff: clean.

**Live demo re-run** (both daemons from the branch build, real agent
container): container env contract intact — exactly one credential var
+ KBASE_URL/KBASE_TOKEN + LAB_*. In-container: component add ×2 →
link → graph in all three formats (mermaid output is valid `graph LR`)
→ unlink → `--at` before the tombstone restores the old topology.
Full in-container ticket lifecycle (add ticket → list → claim → start
→ comment → done) with `agent:rev10` provenance on all five versions;
host-side human comment appended v6 (`human:Charles`) — both authors
in history. Direct SQL UPDATE and DELETE on edges rejected by the
trigger; second tombstone via API → 409, exit 1. Concurrent host-side
claim race: exactly one winner (cas 1), loser printed current state
and exited 1 with no retry. Logs: zero token material in either
daemon's output; all stored kbase tokens are 32-byte hashes. Clean
SIGTERM shutdown of both daemons. The predicted one-time agent-image
rebuild happened on first start.

**Review note (not a phase defect)**: `labctl agent run` runs its own
in-process driver, which lacks labd's lab-API and kbase wiring — a
container started that way gets only the credential env var. Agents
must be started through labd (TUI or client API) to receive the full
env contract. Filed to the Phase 12 backlog.
