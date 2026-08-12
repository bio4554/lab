# Phase 0 handoff — Scaffold & dev infrastructure

You are the implementation agent for Phase 0 of the `lab` project. This
document is your complete brief; you do not have (and don't need) the
conversation history behind it.

## Context

`lab` is an orchestration harness for persistent Claude Code agents running
in Docker sandboxes, written in Go against Postgres. Read
`docs/DESIGN.md` in full before writing code — it defines the architecture
you are scaffolding for. `docs/PLAN.md` shows where this phase fits; the
"Phase 0" section there is the summary of this brief.

Your job is **only** the scaffold: repo structure, config, migration
tooling, build tooling, and two daemon skeletons. No business logic.

## Ground rules

- Work on branch `phase-00-scaffold` off `development`.
- Stay in scope. If an interface contract below seems wrong or incomplete,
  make the smallest reasonable call and record it in your report — do not
  redesign.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- Tests are part of the deliverable. `make check` must pass when you stop.
- Allowed third-party deps for this phase: `pgx/v5`, `goose/v3`,
  `BurntSushi/toml` (or stdlib-only config if you prefer). Anything else:
  justify in the report.
- Go 1.24+. Single module: `github.com/bio4554/lab` (confirm the module
  path from the git remote if one exists; otherwise use this).
- Finish by writing `docs/handoffs/phase-00-report.md`: what you built,
  deviations, open questions.

## Deliverables

1. **Directory layout** per DESIGN.md "Repo layout". Create `cmd/labd`,
   `cmd/kbased`, `internal/...` packages as listed there. For packages
   whose phase hasn't run yet, create the directory only if it needs to
   exist for the build; do not stub out speculative interfaces.

2. **Config**: `internal/config` loading TOML with env-var overrides
   (`LAB_` prefix), covering: Postgres DSN, labd listen addrs (client API,
   agent API), kbased listen addr, data dir root, log level. One config
   file shared by both daemons (`lab.toml`), section per daemon, sensible
   defaults so `labd` runs with zero config against
   `postgres://localhost:5432/lab`. Include `lab.example.toml`.

3. **Postgres dev environment**: `docker-compose.yml` with a `postgres:18`
   service (named volume, port 5432, db `lab`). `make db-up` / `make
   db-down`.

4. **Migrations**: goose wired for **two independent streams** —
   `migrations/lab` applied to schema `lab`, `migrations/kbase` applied to
   schema `kbase`, each with its own goose version table
   (`lab.goose_version` / `kbase.goose_version`). Initial migration in each
   stream creates the schema itself. `make migrate-lab`, `make
   migrate-kbase`. Embed migrations (`embed.FS`) so daemons can self-check
   schema currency at startup (check + warn, don't auto-migrate).

5. **Daemon skeletons**: `cmd/labd` and `cmd/kbased` each: parse config,
   connect to Postgres (pgx pool), serve HTTP with `GET /healthz`
   (200 + JSON: version, schema-current bool), structured logging
   (`log/slog`), clean shutdown on SIGINT/SIGTERM (drain HTTP, close
   pool). labd listens on the client-API addr only for now.

6. **Makefile**: `check` (gofmt check, `go vet`, `go build ./...`,
   `go test ./...`), `test`, `build` (binaries into `bin/`), `db-up`,
   `db-down`, `migrate-lab`, `migrate-kbase`. Keep it plain make, no
   wrapper tools.

7. **Housekeeping**: `.gitignore` (bin/, lab.toml), minimal README section
   "Development" documenting the acceptance command sequence below.

## Acceptance criteria

From a fresh clone with Docker running:

```
make db-up
make migrate-lab migrate-kbase
make check
make build && ./bin/labd &   # answers /healthz, exits cleanly on SIGTERM
              ./bin/kbased & # same
```

- Both migration streams apply cleanly and are independently versioned.
- `/healthz` reports schema-current correctly (true after migrate; false if
  you roll one back).
- No lint noise: `gofmt -l .` empty, `go vet` clean.
- Unit tests exist for config loading (defaults, file, env override) and
  for the schema-currency check.

## Out of scope

Any table beyond goose bookkeeping (Phase 1); stream-json (2); git (3);
Docker runtime beyond dev compose (4); any API beyond /healthz (6); TUI
(7); auth (6/9). Don't create `cmd/lab`, `cmd/kbase`, or `cmd/lab-agent`
yet.
