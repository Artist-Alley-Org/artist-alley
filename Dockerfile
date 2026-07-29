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

# ---- threejs-deps ---------------------------------------------------------
#
# The headless three.js preview worker's node_modules (puppeteer + three)
# for preview.model's 3D renderer (#498, ADR 0069). Multi-arch: buildx
# resolves node:22-bookworm-slim per TARGETPLATFORM, so the node binary +
# modules copied into the runtime below match the target arch. Puppeteer's
# bundled Chromium download is skipped — the runtime uses the apt
# `chromium` package, which exists on amd64 AND arm64. That is why arm64
# has 3D previews at all: the Blender tarball it replaced (#500) was
# x64-glibc only, so arm64 images shipped with no 3D renderer.

FROM node:${NODE_VERSION}-bookworm-slim AS threejs-deps
WORKDIR /app/threejs
ENV PUPPETEER_SKIP_DOWNLOAD=1
COPY scripts/threejs/package.json scripts/threejs/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund

# ---- runtime --------------------------------------------------------------
#
# Debian-slim instead of Alpine: the binary is cgo'd against libwebp
# so it dynamically links libwebp.so.7 at runtime. Final image is
# larger with the 3D preview stack (Chromium + Node) baked in, but
# still one artifact that renders every preview kind. Curl stays for
# the HEALTHCHECK below.

FROM debian:bookworm-slim AS runtime

LABEL org.opencontainers.image.title="artist-alley"
LABEL org.opencontainers.image.description="Self-hosted art review and archival platform for artists, curators, and small studios."
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.source="https://github.com/mscrnt/artist-alley"

# chromium: headless renderer for the three.js preview worker (#498). The
# apt package pulls its full runtime dep tree (nss, gtk, fonts, libgbm, …)
# so puppeteer launches it on SwiftShader software WebGL — no GPU. Present
# on amd64 AND arm64, so this stage is now arch-uniform: #500 removed the
# amd64-only Blender block that used to make it differ per platform, along
# with the GL/X libs (libgl1, libegl1, libxrender1, libxi6, libxxf86vm1,
# libxfixes3, libxkbcommon0, libxext6, libsm6, libice6) that existed
# solely for Blender's dlopens. Chromium brings the X/GL libs it needs
# through its own dependency tree. Those are RENDER-time dependencies, so
# a green build proves nothing about them — scripts/threejs/smoke.mjs,
# which CI runs against this image, is what actually proves it.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl libwebp7 \
        ffmpeg librsvg2-bin poppler-utils ghostscript imagemagick unar \
        chromium \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system app && useradd --system --gid app --no-create-home app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

# three.js preview worker (#498): Node runtime + node_modules + script.
# The Go ModelHandler defaults ThreeJSScript to /app/threejs/worker.mjs.
# Since #500 this is the ONLY 3D renderer in the image — Blender is gone
# (it was 1.3 GB of a 3.64 GB image and, once #498 routed every renderable
# format here, nothing called it) and comes back as an opt-in converter
# plugin for proprietary formats, #499. See ADR 0069, amended.
COPY --from=threejs-deps /usr/local/bin/node /usr/local/bin/node
COPY --from=threejs-deps --chown=app:app /app/threejs/node_modules /app/threejs/node_modules
COPY --chown=app:app scripts/threejs/worker.mjs scripts/threejs/render.html \
                     scripts/threejs/smoke.mjs scripts/threejs/package.json /app/threejs/
# The load path + lighting constants render.html imports are the web
# app's own modules (#689) — worker.mjs serves this directory at
# /shared/. Their canonical home is web/src/lib/3d/ because that is the
# one directory both build worlds can reach (the dev web container
# bind-mounts only ./web).
COPY --chown=app:app web/src/lib/3d/modelLoader.js web/src/lib/3d/defaultLighting.js \
                     /app/threejs/shared/
ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium

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
