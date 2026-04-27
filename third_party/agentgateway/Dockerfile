# syntax=docker/dockerfile:1.11
ARG BUILDER=base
ARG BUILDPLATFORM

FROM --platform=$BUILDPLATFORM docker.io/library/node:23.11.0-bookworm AS node

WORKDIR /app

COPY ui .

RUN --mount=type=cache,target=/app/npm/cache npm install

RUN --mount=type=cache,target=/app/npm/cache npm run build

FROM docker.io/library/rust:1.95.0-trixie AS musl-builder

ARG TARGETARCH

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache && \
    apt-get update && apt-get install -y --no-install-recommends \
    make musl-tools

RUN <<EOF
mkdir /build
if [ "$TARGETARCH" = "arm64" ]; then
  rustup target add aarch64-unknown-linux-musl;
  echo aarch64-unknown-linux-musl > /build/target
else
  rustup target add x86_64-unknown-linux-musl;
  echo x86_64-unknown-linux-musl > /build/target
fi
EOF

FROM --platform=$BUILDPLATFORM docker.io/library/rust:1.95.0-trixie AS base-builder

ARG TARGETARCH

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache && \
    apt-get update && apt-get install -y --no-install-recommends \
    gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

RUN <<EOF
mkdir /build
if [ "$TARGETARCH" = "arm64" ]; then
  rustup target add aarch64-unknown-linux-gnu;
  echo aarch64-unknown-linux-gnu > /build/target
else
  rustup target add x86_64-unknown-linux-gnu;
  echo x86_64-unknown-linux-gnu > /build/target
fi
EOF

FROM ${BUILDER}-builder AS builder
ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETARCH
ARG PROFILE=release
ARG VERSION
ARG GIT_REVISION

WORKDIR /app

COPY Makefile Cargo.toml Cargo.lock ./
COPY .cargo ./.cargo
COPY crates ./crates
COPY tools ./tools
COPY --from=node /app/out ./ui/out

RUN \
    --mount=type=cache,id=cargo,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    cargo fetch --locked
RUN --mount=type=cache,target=/app/target \
    --mount=type=cache,id=cargo,target=/usr/local/cargo/registry  \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    <<EOF
export VERSION="${VERSION}"
export GIT_REVISION="${GIT_REVISION}"
read -r target < /build/target
if [ "${TARGETARCH}" = "arm64" ]; then
  export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc
  export CC_aarch64_unknown_linux_gnu=aarch64-linux-gnu-gcc
  export CXX_aarch64_unknown_linux_gnu=aarch64-linux-gnu-g++
fi
cargo build --features ui --target "${target}" --profile ${PROFILE} || exit 1
mkdir /out
mv /app/target/${target}/${PROFILE}/agentgateway /out
if [ "${BUILDPLATFORM}" = "${TARGETPLATFORM}" ]; then
  /out/agentgateway --version
  if /out/agentgateway --version | grep -q '"unknown"'; then
    exit 1
  fi
fi
EOF

FROM cgr.dev/chainguard/glibc-dynamic AS runner

ARG TARGETARCH

WORKDIR /

COPY --from=builder /out/agentgateway /app/agentgateway

LABEL org.opencontainers.image.source=https://github.com/agentgateway/agentgateway
LABEL org.opencontainers.image.description="Agentgateway is an open source project that is built on AI-native protocols to connect, secure, and observe agent-to-agent and agent-to-tool communication across any agent framework and environment."

ENTRYPOINT ["/app/agentgateway"]
