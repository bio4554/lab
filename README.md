# lab

An orchestration harness for persistent Claude Code agents running in
Docker sandboxes. See [docs/DESIGN.md](docs/DESIGN.md) for the
architecture and [docs/PLAN.md](docs/PLAN.md) for the phased build plan.

## Quick start

Requires Go 1.24+, git, and Docker. From a fresh clone:

```sh
make startd    # terminal A: Postgres up, schema migrated, labd built & running
make startc    # terminal B: the lab TUI, built & connected to labd
```

`startd` sources `~/.lab/demo.env` when it exists — put your
`ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` there (outside the
repo) so agents with env-passthrough credentials can run.

## Development

Requires Go 1.24+, git, and Docker.

```sh
make db-up                      # start dev Postgres (docker compose)
make migrate-lab migrate-kbase  # apply both migration streams
make check                      # gofmt check, go vet, go build, go test
make build                      # binaries into bin/
./bin/labd &                    # answers GET /healthz on 127.0.0.1:7710
./bin/kbased &                  # answers GET /healthz on 127.0.0.1:7720
```

Both daemons read `lab.toml` (see `lab.example.toml`; the file is
optional — defaults target the compose Postgres) and accept `LAB_*`
environment overrides. They shut down cleanly on SIGINT/SIGTERM.
`/healthz` reports the build version and whether the daemon's schema
is migration-current. `make db-down` stops Postgres.
