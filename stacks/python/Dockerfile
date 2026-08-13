# lab python stack: base + distro Python 3 + pinned uv.
ARG BASE_IMAGE=lab/base:dev
FROM ${BASE_IMAGE}

ARG UV_VERSION=0.9.5

USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        python3-venv \
        python3-pip \
    && rm -rf /var/lib/apt/lists/*
USER agent
# uv via its pinned installer; installs under /home/agent/.local/bin,
# which the base image already has on PATH.
RUN curl -fsSL "https://astral.sh/uv/${UV_VERSION}/install.sh" | sh
WORKDIR /work
