# lab go stack: base + pinned Go toolchain.
ARG BASE_IMAGE=lab/base:dev
FROM ${BASE_IMAGE}

ARG GO_VERSION=1.25.7
# Docker arch of the daemon (amd64|arm64); passed by the builder.
ARG TARGETARCH=amd64

USER root
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
        | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}" \
    GOPATH=/home/agent/go
USER agent
WORKDIR /work
