# lab rust stack: base + rustup with a pinned stable toolchain,
# installed for the agent user (RUSTUP_HOME/CARGO_HOME in its home).
ARG BASE_IMAGE=lab/base:dev
FROM ${BASE_IMAGE}

ARG RUST_VERSION=1.90.0

USER agent
RUN curl -fsSL https://sh.rustup.rs \
        | sh -s -- -y --profile minimal --default-toolchain "${RUST_VERSION}" --no-modify-path
ENV PATH="/home/agent/.cargo/bin:${PATH}"
WORKDIR /work
