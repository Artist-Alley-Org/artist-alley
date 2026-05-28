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
# Cross-compile in pure Go (CGO_ENABLED=0). Go's standard library does
# cross-compilation without external toolchains, so we stay on the
# host platform and emit a static binary for the target.

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS go-build

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev

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

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -tags embed_web \
      -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
      -o /out/aa \
      ./cmd/aa

# ---- runtime --------------------------------------------------------------
#
# Final image: Alpine with ca-certs + tzdata, a non-root `app` user,
# and the binary. ~25 MB compressed. Storage volume mounted under
# /var/lib/aa-storage.

FROM alpine:3.20 AS runtime

LABEL org.opencontainers.image.title="artist-alley"
LABEL org.opencontainers.image.description="Self-hosted art review and archival platform — a modern reimagining of ResourceSpace."
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.source="https://github.com/mscrnt/artist-alley"

RUN apk add --no-cache ca-certificates tzdata curl \
 && addgroup -S app && adduser -S -G app app \
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
