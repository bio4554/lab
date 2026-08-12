# Phase 4 handoff — Docker runtime & stack images

You are the implementation agent for Phase 4 of the `lab` project. This
document is your complete brief. Read `docs/DESIGN.md` first —
especially "Stacks" and "Agent runtime > Container contract".
`docs/PLAN.md` shows where this phase fits.

Your job: `stacks/` (image templates) + `internal/labd/runtime`
(image build & container lifecycle via the Docker Engine API). Phase 5
drives `claude` through the stdio attach you provide; Phase 3
(parallel) produces the worktree paths you mount. You own
`stacks/`, `internal/labd/runtime`, and stub `cmd/kbase` /
`cmd/lab-agent` main packages.

## Ground rules

- Branch `phase-04-runtime` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded in your
  report — no redesign.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop; Docker-dependent tests must skip
  gracefully when the daemon is unreachable.
- **This repo is public on GitHub.** No secrets in images, templates,
  or tests. Images must never bake in credentials — they arrive as env
  at container create, from Phase 5+.
- Allowed new deps: `github.com/docker/docker` (client) and its
  transitive requirements. Anything else: justify in the report.
- Finish with `docs/handoffs/phase-04-report.md`.

## Deliverables

### 1. Stack templates (`stacks/`)

Per DESIGN.md: a shared **base** image and per-stack toolchain layers.

- `stacks/base/Dockerfile`: `debian:stable-slim` (or `-slim` bookworm);
  non-root user `agent` (uid 1000, home `/home/agent`); git, curl, jq,
  ripgrep, ca-certificates, openssh-client; Claude Code CLI via the
  native installer, **pinned** via `ARG CLAUDE_CODE_VERSION` (installer:
  `curl -fsSL https://claude.ai/install.sh | bash -s -- <version>` —
  verify the exact invocation when you implement, and record it);
  `kbase` and `lab-agent` binaries copied from the build context to
  `/usr/local/bin/`; `WORKDIR /work`.
- `stacks/<go|node|python|rust>/Dockerfile`: `FROM lab/base:<tag>`
  (parameterized via `ARG BASE_IMAGE`) + toolchain: Go (latest stable
  tarball, pinned), Node LTS (nodesource or tarball, pinned), Python 3
  + `uv`, Rust via rustup (pinned). Keep layers lean; toolchains must
  be usable by the `agent` user.
- A small manifest in code (`runtime.Stacks()`) enumerating valid stack
  names — Phase 5+ validates project creation against it.

### 2. Stub agent binaries

`cmd/kbase/main.go` and `cmd/lab-agent/main.go`: print
`<name> <version> (stub)` and exit 0. Phases 9/11 replace them; they
exist so base images carry real files at the real paths.

### 3. Image building (`internal/labd/runtime`)

- `Builder.EnsureImage(ctx, stack)` → image tag. Tag =
  `lab/agent-<stack>:<content-hash>` where the hash covers: the
  stack's Dockerfile, the base Dockerfile, pinned versions/args, and
  the embedded CLI binaries' hashes. If the tag exists locally, done
  (lazy); otherwise build via the Docker API (tar build context
  assembled in-memory/tempdir; cross-compile the two stubs
  `GOOS=linux GOARCH=<docker daemon's arch>` with `go build` into the
  context). Base image builds first, stack image `FROM` it.
- Build output streamed to a logger; failures surface the tail.

### 4. Container lifecycle (`internal/labd/runtime`)

`Runtime` (wraps the Docker client):

- `Create(ctx, spec)` where spec: agent ID (container name
  `lab-agent-<id>`), image tag, worktree host path → `/work` (rw),
  named volume `lab-claude-<agent-id>` → `/home/agent/.claude`, env
  map (**passed through verbatim** — credential selection is the
  caller's job), labels (`lab.agent-id`, `lab.project-id`),
  `ExtraHosts: host.docker.internal:host-gateway`, user `agent`,
  command from spec (Phase 5 supplies the real `claude ...` argv;
  default `sleep infinity` for tests), stdin open.
- `Start`, `Stop(timeout)`, `Remove(force)`, `Inspect` (running? exit
  code?), `List` (by label — labd restart reconciliation will need it).
- `Attach(ctx, containerID)` → `(stdin io.WriteCloser, stdout, stderr
  io.Reader)` — demultiplex the Docker stream (stdcopy) when the
  container has no TTY. This is the pipe Phase 5 pumps stream-json
  through — get lifetimes right (closing stdin must not kill the
  reads; context cancel detaches cleanly).

### 5. Tests (Docker required; skip when unreachable)

- Build the base image (small `CLAUDE_CODE_VERSION` pin; if the
  installer needs network and that's flaky in your environment, make
  the claude-install layer skippable via build arg for tests and note
  it).
- `EnsureImage` is lazy (second call: no rebuild) and hash-sensitive
  (mutate an input → new tag).
- Run a container from base with `cat` as command: write lines to
  attached stdin, read them back from stdout (round-trip through your
  demux); verify env vars, `/work` mount contents, labels, volume.
- `go` stack: `go version` succeeds as the container's command.
- `List` finds it by label; `Stop`/`Remove` clean up. Tests must clean
  up containers/volumes they create even on failure.

## Acceptance criteria

- `make check` green with and without Docker running.
- Fresh machine path: `EnsureImage(ctx, "go")` builds base + go from
  nothing, returns a tag Docker can run.
- stdin→stdout echo round-trip test green — this is the load-bearing
  proof for Phase 5.
- No credential material anywhere in `stacks/` or image layers.

## Out of scope

Running actual `claude` processes, sessions, turns (Phase 5); worktree
creation (Phase 3 — your tests fabricate plain temp dirs to mount);
custom user Dockerfiles (post-v1); network egress restriction
(post-v1); `cmd/kbase`/`cmd/lab-agent` real functionality (Phases
9/11).
