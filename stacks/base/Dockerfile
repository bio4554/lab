# lab shared base image: debian-slim + common tools, non-root `agent`
# user, Claude Code CLI (native installer, pinned), and the in-container
# lab CLIs. Built by labd (internal/labd/runtime), which assembles the
# build context in memory: `kbase` and `lab-agent` are cross-compiled
# linux binaries placed at the context root.
#
# No credentials are baked in at any layer; they arrive as env vars at
# container create.
FROM debian:bookworm-slim

# Pinned Claude Code CLI version (never "latest": image tags are
# content-addressed and must be reproducible).
ARG CLAUDE_CODE_VERSION=2.1.222
# Set to 1 to skip the Claude Code install layer — used by tests that
# must not depend on network access to claude.ai.
ARG SKIP_CLAUDE_INSTALL=0

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        jq \
        openssh-client \
        ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --uid 1000 --create-home --home-dir /home/agent --shell /bin/bash agent \
    && mkdir /work \
    && chown agent:agent /work

# The build context (assembled by internal/labd/runtime) carries these
# with mode 0755; COPY preserves it. No --chmod: the daemon-side
# classic builder the Engine API uses does not support BuildKit flags.
COPY kbase lab-agent /usr/local/bin/

USER agent

# Native installer: `bash -s -- <version>` passes the pinned version as
# $1 (stable|latest|X.Y.Z). It downloads the binary and installs the
# launcher under /home/agent/.local/bin.
RUN if [ "$SKIP_CLAUDE_INSTALL" != "1" ]; then \
        curl -fsSL https://claude.ai/install.sh | bash -s -- "$CLAUDE_CODE_VERSION"; \
    fi

ENV PATH="/home/agent/.local/bin:${PATH}"

WORKDIR /work
