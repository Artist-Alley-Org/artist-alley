# artist-alley — production image.
#
# Single artifact: the Go binary with the SvelteKit frontend baked in
# via //go:embed (-tags embed_web). Multi-arch ready via docker buildx
# — set TARGETOS / TARGETARCH on the build to pick a platform.
#
# Stages
#   web-build   build the SvelteKit bundle (npm ci + npm run build)
#   go-build    compile the Go binary with the embedded bundle
#   runtime     minimal Alpine + non-root user + the binary
#
# Layer caching follows the canonical Go + npm patterns: copy lock
# files first to cache the deps install layer, copy sources after.
#
# Build args
#   GO_VERSION       Go toolchain image tag (default: pin in sync with
#                    app/go.mod's `go` directive)
#   NODE_VERSION     Node toolchain image tag
#   BUILD_VERSION    embedded into the binary via `-X main.Version=`

# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26
ARG NODE_VERSION=22

# ---- web-build ------------------------------------------------------------
#
# BUILDPLATFORM = the host running buildx (not the target). The
# SvelteKit bundle is platform-agnostic so we always build it on the
# host to avoid emulation overhead for arm64 cross-builds.

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web-build

WORKDIR /web

# Lockfile first → install layer caches across source changes.
COPY web/package.json web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

# The OpenAPI spec lives outside web/ but is needed by the
# `generate:api` step that runs as a postinstall hook. Mount it at the
# path the script expects.
COPY app/api/openapi.yaml /opt/openapi.yaml
ENV AA_OPENAPI_SPEC=/opt/openapi.yaml

COPY web/ ./
RUN npm run build

# ---- go-build -------------------------------------------------------------
#
# Debian-bookworm base instead of alpine: the WebP variant encoder
# (chai2010/webp) is cgo'd against libwebp, and matching glibc on
# both build and runtime keeps the binary portable. We still build
# on $BUILDPLATFORM and emit for $TARGETPLATFORM; cross-arch builds
# need the appropriate libwebp-dev:arch — buildx pulls it via
# Debian's multiarch when TARGETARCH != $(dpkg --print-architecture).

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS go-build

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev

# Build-time C toolchain + libwebp headers for chai2010/webp. The
# go-build stage is pinned to $BUILDPLATFORM (host amd64) for speed
# — Go itself cross-compiles fine — but cgo shells out to `cc`,
# which must match TARGETARCH. So we add-architecture the target
# and install the cross-gcc + arch-scoped libwebp-dev.
#
# pkg-config is what chai2010/webp uses to resolve the lib at link
# time. `PKG_CONFIG_PATH` picks the right per-arch lib pkgconfig dir.
RUN dpkg --add-architecture arm64 \
 && apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev pkg-config libwebp-dev \
        gcc-aarch64-linux-gnu libc6-dev-arm64-cross libwebp-dev:arm64 \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src/app

# Module download caches independently from sources.
COPY app/go.mod app/go.sum* ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Sources, then the embedded frontend. The Go //go:embed directive
# reads from app/internal/http/static_assets/; copy the SvelteKit
# output into that directory so the next `go build` picks it up.
COPY app/ ./
COPY --from=web-build /web/build/ ./internal/http/static_assets/

# Per-arch cc + pkg-config selection. amd64 uses host gcc; arm64
# uses the aarch64-linux-gnu cross-gcc so cgo can assemble the
# arm64 .S emitted by mscrnt/webp.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    case "${TARGETARCH}" in \
      amd64) CC=gcc                    PKG_CFG=/usr/lib/x86_64-linux-gnu/pkgconfig ;; \
      arm64) CC=aarch64-linux-gnu-gcc  PKG_CFG=/usr/lib/aarch64-linux-gnu/pkgconfig ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && CGO_ENABLED=1 CC=$CC PKG_CONFIG_PATH=$PKG_CFG \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -tags embed_web \
      -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
      -o /out/aa \
      ./cmd/aa

# ---- runtime --------------------------------------------------------------
#
# Debian-slim instead of Alpine: the binary is cgo'd against libwebp
# so it dynamically links libwebp.so.7 at runtime. Final image lands
# around ~80 MB — bigger than the prior pure-Go Alpine cut (~25 MB)
# but gains lossy + lossless WebP encoding for the variant ladder
# (~25-35% smaller variants than JPEG at perceptually equivalent
# quality). Curl stays for the HEALTHCHECK below.

FROM debian:bookworm-slim AS runtime

LABEL org.opencontainers.image.title="artist-alley"
LABEL org.opencontainers.image.description="Self-hosted art review and archival platform for artists, curators, and small studios."
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.source="https://github.com/mscrnt/artist-alley"

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl libwebp7 \
        ffmpeg librsvg2-bin poppler-utils ghostscript imagemagick unar \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system app && useradd --system --gid app --no-create-home app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

USER app
WORKDIR /app

COPY --from=go-build --chown=app:app /out/aa /app/aa

ENV AA_HTTP_ADDR=":8080" \
    AA_STORAGE_BACKEND="fs" \
    AA_STORAGE_FS_ROOT="/var/lib/aa-storage"

EXPOSE 8080
VOLUME ["/var/lib/aa-storage"]

# Lightweight HTTP health check against /healthz (the Go server's
# liveness endpoint).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/aa"]
