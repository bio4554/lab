# lab node stack: base + pinned Node.js LTS from the official tarball.
ARG BASE_IMAGE=lab/base:dev
FROM ${BASE_IMAGE}

# Node 24 is the active LTS line.
ARG NODE_VERSION=24.13.0
# Docker arch of the daemon (amd64|arm64); passed by the builder.
# Node tarballs use x64 for amd64, so map below.
ARG TARGETARCH=amd64

USER root
# xz-utils: the official Node tarballs are .tar.xz and the slim base
# has no xz.
RUN apt-get update && apt-get install -y --no-install-recommends xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN arch="${TARGETARCH}"; [ "$arch" = "amd64" ] && arch=x64; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
        | tar -C /usr/local -xJ --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'
USER agent
WORKDIR /work
