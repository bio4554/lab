# Phase 0 report — Scaffold & dev infrastructure

Branch: `phase-00-scaffold`. `make check` passes; the full acceptance
sequence was run against Docker (see below).

## What was built

- **Module**: `github.com/bio4554/lab` (confirmed against the git
  remote), go directive `1.25.7` (see deviations).
- **Layout**: `cmd/labd`, `cmd/kbased`, `cmd/migrate`,
  `internal/config`, `internal/daemon`, `internal/migrate`,
  `migrations/{lab,kbase}`. No speculative packages: directories from
  DESIGN.md whose phases haven't run are not created because the build
  doesn't need them.
- **Config** (`internal/config`): TOML via `BurntSushi/toml`, defaults
  → file → `LAB_*` env override precedence. Covers Postgres DSN, labd
  client/agent API addrs, kbased addr, data dir, log level. Unknown
  keys are rejected (catches typos). `lab.example.toml` documents every
  key and env var. Zero-config works against the compose Postgres.
- **Dev Postgres**: `docker-compose.yml`, `postgres:18`, named volume,
  port 5432, db/user/password `lab` (local-only credential). Healthcheck
  wired so `make db-up` (`docker compose up -d --wait`) blocks until
  ready.
- **Migrations** (`internal/migrate` + `migrations/embed.go`): two
  independent goose streams, each with its own schema-qualified version
  table (`lab.goose_version`, `kbase.goose_version`) via
  `goose.NewProvider` + custom `database.NewStore`. Migrations are
  embedded (`embed.FS`); `Stream.Current` powers the daemons'
  startup check (warn only, never auto-migrate) and `/healthz`.
  Initial migration in each stream creates its schema.
- **Daemons** (`cmd/labd`, `cmd/kbased`, shared `internal/daemon`):
  parse `-config`/`lab.toml`, pgx pool + ping, `GET /healthz` →
  `{"version": ..., "schema_current": bool}` (checked live per
  request), `log/slog` structured logging, SIGINT/SIGTERM → HTTP drain
  → pool close. labd serves only the client API addr.
- **Makefile**: `check` (gofmt, vet, build, test), `test`, `build`
  (version stamped via `-ldflags`), `db-up`, `db-down`, `migrate-lab`,
  `migrate-kbase`, `clean`. Plain make.
- **Housekeeping**: `.gitignore` (`bin/`, `lab.toml`), README
  "Development" section with the acceptance command sequence.

## Acceptance results (run 2026-08-12)

- `make db-up` → compose Postgres healthy.
- `make migrate-lab migrate-kbase` → both streams apply cleanly;
  independent version tables verified by test
  (`TestStreamsAreIndependent`).
- `make check` → gofmt clean, vet clean, build clean, all tests pass
  (config: defaults/file/env/unknown-key; migrate: currency check
  against live Postgres in a throwaway schema).
- `make build`; both daemons answered `/healthz` with
  `schema_current: true`; after `go run ./cmd/migrate -stream kbase
  down`, kbased reported `schema_current: false`, and `true` again
  after re-up. Both exited cleanly on SIGTERM ("shutdown complete"
  logged, processes gone).
- DB-backed tests skip with a hint when Postgres is unreachable, so
  `make check` also works without Docker.

## Deviations from the handoff

1. **Default DSN** is
   `postgres://lab:lab@localhost:5432/lab?sslmode=disable`, not
   `postgres://localhost:5432/lab`. The compose `postgres:18` requires
   a user/password, and zero-config-runs-against-the-dev-DB seemed like
   the actual intent. The `lab:lab` credential is local-dev only.
2. **`cmd/migrate` added** (not on the handoff's forbidden list).
   Custom version-table names + embedded FS made the stock goose CLI a
   poor fit; a ~90-line command driven by `make migrate-*` reuses the
   same embedded migrations and config loading as the daemons.
3. **Initial migrations' Down is a no-op**: dropping the schema would
   also drop the goose version table inside the same transaction,
   before goose records the rollback. Rolling back to zero therefore
   leaves an empty schema behind — harmless, and later real migrations
   get real Downs (exercised in tests).
4. **`Stream.Up` pre-creates the schema** (`CREATE SCHEMA IF NOT
   EXISTS`) before goose runs, because the version table lives inside
   the schema the first migration creates. The migration keeps its own
   `CREATE SCHEMA IF NOT EXISTS` so the SQL remains the source of
   truth.
5. **`internal/daemon`** is not in DESIGN.md's layout; it holds the
   startup/serve/shutdown skeleton shared by both daemons rather than
   duplicating it. Happy to inline it into the two mains at review if
   preferred.
6. **go directive is `1.25.7`**, not 1.24: `go mod tidy` raises it to
   the minimum the dependency graph requires (pgx v5.10 / goose
   v3.27.3 chain). Still satisfies "Go 1.24+" in spirit — the
   toolchain floor, not a pin.
7. **Listen ports** (unspecified in the handoff): client API
   `127.0.0.1:7710`, agent API `127.0.0.1:7711` (reserved, unused
   until Phase 6), kbased `127.0.0.1:7720`.
8. **compose volume mounts `/var/lib/postgresql`** (not `.../data`):
   the `postgres:18` image moved its data directory.

No dependencies beyond the allowed set were added directly
(`pgx/v5`, `goose/v3`, `BurntSushi/toml`); everything else in `go.sum`
is transitive.

## Open questions

1. Should the client API eventually prefer a unix socket over
   localhost TCP (DESIGN.md mentions both)? Phase 0 went TCP-only.
2. `data_dir` defaults to `~/.lab` — DESIGN.md never names a default.
   Fine to keep, or should it be XDG (`~/.local/share/lab`)?
3. `/healthz` runs the goose pending-check per request (two cheap
   queries). If that's too much for a liveness probe later, cache it
   with a short TTL.
