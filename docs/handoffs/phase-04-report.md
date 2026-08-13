# Phase 4 report — Docker runtime & stack images

Branch `phase-04-runtime`, off `development`. `make check` green with
Docker running and (verified via a bogus `DOCKER_HOST`) with the daemon
unreachable — every Docker-dependent test skips cleanly.

## What was built

### `stacks/` — image templates + embed

- `stacks/base/Dockerfile`: `debian:bookworm-slim`; git, curl, jq,
  ripgrep, ca-certificates, openssh-client; non-root `agent` (uid 1000,
  home `/home/agent`); `kbase` + `lab-agent` copied from the build
  context into `/usr/local/bin`; Claude Code via the native installer,
  pinned `ARG CLAUDE_CODE_VERSION=2.1.222` (current stable at build
  time). Verified installer invocation:
  `curl -fsSL https://claude.ai/install.sh | bash -s -- <version>` —
  the script takes `stable|latest|X.Y.Z` as `$1` and installs the
  launcher under `~/.local/bin`, which the image adds to `PATH`.
  `WORKDIR /work`. An `ARG SKIP_CLAUDE_INSTALL=1` skips the (network-
  dependent) install layer; tests use it, and it participates in the
  content hash so test images never alias real ones.
- `stacks/{go,node,python,rust}/Dockerfile`: `ARG BASE_IMAGE` +
  toolchain, all pinned and verified to exist upstream: Go 1.25.7
  (official tarball), Node 24.13.0 (active LTS, official tarball; the
  layer apt-installs `xz-utils` first — slim has no xz), Python 3
  (bookworm's 3.11 via apt) + uv 0.9.5 (pinned installer, lands in
  `~/.local/bin`), Rust 1.90.0 via rustup (minimal profile, installed
  as the `agent` user).
- `stacks/embed.go`: package `stacks` embeds `*/Dockerfile` so labd
  builds images without needing the repo checkout for templates.
  `runtime.Stacks()` derives the valid-stack list from the embedded FS
  (base, go, node, python, rust) — it cannot drift from `stacks/`.

### `cmd/kbase`, `cmd/lab-agent`

Stubs printing `<name> <version> (stub)`; real files at the real paths
in every image. Phases 9/11 replace them.

### `internal/labd/runtime`

- `runtime.go` — `Runtime` wrapping the official Docker client
  (`FromEnv` + API version negotiation): `Create` (container
  `lab-agent-<id>`, worktree bind at `/work` rw, named volume
  `lab-claude-<id>` at `/home/agent/.claude`, env passed through
  verbatim, `lab.agent-id`/`lab.project-id` labels,
  `host.docker.internal:host-gateway`, user `agent`, default cmd
  `sleep infinity`), `Start`, `Stop(timeoutSeconds)`, `Remove(force)`,
  `RemoveVolume`, `Inspect` (running/exit code), `List` (by label,
  optionally per project), `Ping`, `Stacks()`/`ValidStack`.
- `builder.go` — `Builder.EnsureImage(ctx, stack)`: cross-compiles the
  two stubs with the host toolchain (`GOOS=linux GOARCH=<daemon arch>`,
  arch from `docker info`, x86_64/aarch64 mapped), computes
  content-addressed tags, and lazily builds via the Engine API with an
  in-memory tar context. Base hash covers the base Dockerfile, build
  args, and both binaries' sha256; stack hash covers the stack
  Dockerfile and its args including `BASE_IMAGE=lab/base:<basehash>`,
  chaining the hashes. Existing tag ⇒ no build. Build output streams
  to `slog` at debug; failures surface the error plus the last 20
  output lines.
- `attach.go` — `Attach(ctx, id)` → `(stdin io.WriteCloser, stdout,
  stderr io.Reader)`, stdcopy-demultiplexed. Closing stdin half-closes
  the connection (container sees stdin EOF; later output still
  arrives); ctx cancel tears the attachment down and errors pending
  reads.

### Tests (`runtime_test.go`)

Unit: stack list, content-hash sensitivity (dockerfile/arg key/arg
value/binary each change the tag). Docker integration (skip when
unreachable): lazy `EnsureImage` (second call returns the same tag with
zero builds, asserted via a counting slog handler); unknown stack
rejected; the load-bearing round-trip — base container running
`sh -c 'cat /work/hello.txt; cat'`, mounted-file line read through the
attach, two stdin→stdout echo round-trips, env/labels/volume verified
by inspect, `List` by label (global, right project, wrong project),
write+close stdin ⇒ the post-close line still arrives and the stream
reaches EOF, `Stop`/`Remove`/`Inspect` afterwards; stdout/stderr demux
separation; `go` stack container running `go version` exits 0.
`TestAllStacksFull` (env-gated, `LAB_TEST_BUILD_ALL_STACKS=1`) builds
all five stacks with the real Claude install layer and verifies
`claude --version`, `kbase`, `lab-agent` inside the base image — run
manually when touching Dockerfiles; it passed on this machine.
All containers/volumes are cleaned up via `t.Cleanup` even on failure.

## Decisions & deviations (smallest-call gaps)

- **`StdinOnce: true` on agent containers.** Empirically (Docker 29 /
  Docker Desktop macOS): with `StdinOnce=false` the daemon treats the
  attached client's stdin EOF as a detach and drops the read side too,
  losing output — the handoff's "closing stdin must not kill the
  reads" is only satisfiable with `StdinOnce=true`, which closes the
  container process's stdin and keeps delivering its output. The
  consequence — a detach/labd restart ends the `claude` process — is
  the design's model anyway: continuity is `--resume` against the
  `.claude` volume, not process lifetime.
- **Classic builder, not BuildKit.** The Engine API's `/build` with the
  official client uses the legacy builder (BuildKit needs a session
  library well beyond scope). Consequences: no `COPY --chmod` (binary
  modes come from the tar context headers instead) and no automatic
  `TARGETARCH` — the builder passes it explicitly as a build arg.
  Works against Docker Engine 29; if moby ever removes the legacy
  build endpoint this is the one place to swap.
- **Stub cross-compilation needs the repo checkout + go toolchain** on
  the host at image-build time (`BuilderOptions.ModuleDir`, defaulting
  via `go env GOMOD`). That matches the v1 deployment mode (labd run
  from the checkout); DESIGN.md already assumes "built by labd's host
  toolchain".
- **`Attach` requires draining stdout and stderr concurrently** (like
  `exec.Cmd` pipes): the demultiplexer stalls if one stream's frames
  arrive while the other is unread. Documented on the method; the ctx-
  cancel path closes the pipe writers directly so cancellation unblocks
  even a stalled demux.
- **Stack "base" images** are the base image re-tagged
  `lab/agent-base:<hash>` so all agent images share the
  `lab/agent-<stack>` naming.
- **`RemoveVolume` added** beyond the handoff list — tests must clean
  up the implicitly-created `.claude` volumes, and agent retirement
  will need it.
- New dependency: `github.com/docker/docker v28.5.2+incompatible` (the
  allowed client) plus its transitive requirements; nothing else.

## Notes for the orchestrator

- Image sizes on arm64 (full builds): base ≈ 500MB with Claude Code
  installed (≈ 210MB without), python ≈ 610MB, node ≈ 690MB,
  go ≈ 700MB, rust ≈ 1.0GB. Toolchain layers dominate; nothing
  obviously trimmable at this altitude.
- The `CLAUDE_CODE_VERSION` pin (2.1.222) will age; bumping it is a
  one-line Dockerfile change and the content hash handles the rebuild.
- `TestGoStack` skips under `-short`; the full-stack build test is
  env-gated as above.

## Open questions

None blocking. Phase 5 gets `EnsureImage` → `Create` → `Start` →
`Attach` and the echo round-trip as its contract; if it wants to keep
`claude` alive across labd restarts rather than resuming, that would
need the `StdinOnce` decision revisited (see above).
