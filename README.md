# lab

An orchestration harness for persistent Claude Code agents running in
Docker sandboxes. See [docs/DESIGN.md](docs/DESIGN.md) for the
architecture and [docs/PLAN.md](docs/PLAN.md) for the phased build plan.

## Prerequisites

- **Go 1.24+** and **git**
- **Docker** with the engine running (Docker Desktop on macOS is fine —
  agent containers reach host daemons via `host.docker.internal`)
- **An Anthropic credential**, either:
  - a subscription OAuth token — run `claude setup-token` in any
    terminal with the Claude Code CLI installed (needs a browser;
    Pro/Max/Team/Enterprise plans; token lasts one year), or
  - an API key.

Everything runs on one machine: Postgres in docker-compose, two host
daemons (`labd`, `kbased`), N agent containers, and the `lab` TUI.

## Bootstrap from a fresh clone

### 1. Start the lab daemon

```sh
make startd        # terminal A
```

This brings up Postgres (docker compose), applies both migration
streams, builds `labd`, and runs it in the foreground. First-time state
lands under `~/.lab/`: the credential-vault key (`secret.key`, created
0600 — labd refuses to start if it's readable beyond your user) and
project git data.

### 2. Onboard a credential (one time)

Since Phase 8, agent credentials are stored encrypted in the database —
labd does **not** read `ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN`
from its environment. Register yours once, secret on stdin so it never
lands in shell history:

```sh
make build                                              # binaries into bin/
./bin/labctl cred add -kind oauth_token -label personal # paste token, Enter, Ctrl-D
# or: ./bin/labctl cred add -kind api_key -label personal
```

OAuth tokens default to a one-year expiry (the `setup-token` lifetime);
override with `-expires <RFC3339>|never`. Check what's stored with
`./bin/labctl cred list` — secrets are never displayed, logged, or
returned by any API.

### 3. (Optional) Start kbased — the agent knowledge base

Agents get a `kbase` CLI (add/recall/show versioned notes, decisions,
architecture) when `kbased` is running and labd can mint tokens for it.
Both daemons must share one admin token:

```sh
# put this in ~/.lab/demo.env so startd picks it up automatically:
#   LAB_KBASED_ADMIN_TOKEN=<output of: openssl rand -hex 32>

set -a; . ~/.lab/demo.env; set +a
./bin/kbased       # terminal B
```

Restart `make startd` after adding the variable so labd sees it too.
Without kbased (or the token), agents still run — they just start
without kbase access.

### 4. Start the TUI and create your first agent

```sh
make startc        # terminal C
```

In the TUI:

1. **`p`** — create a project. Origin is a git URL or a local folder
   (local folders are converted to a lab-managed git repo). Pick a
   stack: `base`, `go`, `node`, `python`, or `rust` — it selects the
   toolchain baked into the project's agent image.
2. **`n`** (with the project selected) — create an agent. Give it a
   role prompt and set credential kind to `oauth_token` or `api_key`;
   with exactly one stored credential of that kind it binds
   automatically. (With several, create via
   `labctl agent create ... -cred-id <id>` instead.)
3. **`s`** — start the agent. The first start of a stack builds its
   Docker image (a few minutes); later starts reuse it.
4. **Enter** on the agent focuses the composer — type a prompt, Enter
   sends it. Events stream into the transcript live.

More keys: `Esc` back to the main pane, `1/2/3` switch
transcript/sessions/usage tabs, `x` stop, `r` retire a session
(successor session with a seed prompt), `q` quit.

Each agent runs in its own container on its own git branch
(`agent/<name>`), in its own worktree mounted at `/work`. Merging agent
branches back is an explicit step (`labctl merge`).

## Day to day

`make startd` + `make startc` is the whole loop — Postgres data,
credentials, and project repos persist across restarts. `make db-down`
stops Postgres. Budgets (per agent or per credential) and usage are
available via the API (`PUT .../budget`, `GET /v1/usage`) and the
usage tab.

## Development

```sh
make db-up                      # start dev Postgres (docker compose)
make migrate-lab migrate-kbase  # apply both migration streams
make check                      # gofmt check, go vet, go build, go test
make build                      # all binaries into bin/
```

Both daemons read `lab.toml` (see `lab.example.toml`; the file is
optional — defaults target the compose Postgres) and accept `LAB_*`
environment overrides. They shut down cleanly on SIGINT/SIGTERM.
`/healthz` reports the build version and whether the daemon's schema
is migration-current.

Tests run against the compose Postgres and skip cleanly when it is
down; Docker-dependent suites skip without the engine. This repo is
public: never commit tokens, key files, or a real `lab.toml`.
