# artist-alley Go server image.
#
# Multi-stage: build the static binary in a Go toolchain image, then
# copy it into a minimal runtime. Runtime bundles ffmpeg (preview.video)
# + Chromium/Node (the headless three.js preview.model renderer) so
# every preview kind runs inline. Still one container.

ARG GO_VERSION=1.26
ARG BUILD_VERSION=dev
# Match the in-container `app` user to the host UID/GID so the bind-
# mounted /var/lib/aa-storage volume stays writable across image
# rebuilds. Other services (php, web) already accept these via compose.
ARG UID=1000
ARG GID=1000

# ---- build stage ----------------------------------------------------------
# Debian-bookworm Go image instead of alpine so cgo links against the
# same glibc + libwebp7 the runtime uses. chai2010/webp (variant
# encoder) needs libwebp at build time; nothing else cgo here today.
FROM golang:${GO_VERSION}-bookworm AS build

# libwebp-dev for the chai2010/webp encoder, gcc/pkg-config for
# the C toolchain cgo invokes during the build.
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev pkg-config libwebp-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src/app

# Cache the module download layer separately from sources.
COPY app/go.mod app/go.sum* ./
RUN go mod download

COPY app/ ./

ARG BUILD_VERSION
# CI passes GOFLAGS=-p=N / GOMAXPROCS=N (from the job env, via the
# resources compose override's build args) to cap this compile's
# parallelism so several stacks can build at once on a shared runner
# without starving each other. Both default empty → local builds use
# every core exactly as before.
ARG GOFLAGS=""
ARG GOMAXPROCS=""
RUN CGO_ENABLED=1 GOOS=linux GOFLAGS="${GOFLAGS}" GOMAXPROCS="${GOMAXPROCS}" go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
    -o /out/aa \
    ./cmd/aa

# ---- threejs-deps stage ---------------------------------------------------
# Install the headless three.js preview worker's node_modules (puppeteer
# + three) for the preview.model 3D renderer (#498, ADR 0069). Skip
# puppeteer's bundled Chromium download — the runtime uses the apt
# `chromium` package instead (one Chromium for the image, kept current by
# Debian security updates). node_modules + the node binary are copied
# into the runtime below.
FROM node:22-bookworm-slim AS threejs-deps
WORKDIR /app/threejs
ENV PUPPETEER_SKIP_DOWNLOAD=1
COPY scripts/threejs/package.json scripts/threejs/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund

# ---- runtime stage --------------------------------------------------------
# debian-slim instead of alpine because the Go binary is cgo'd against
# glibc's libwebp. One container still handles every preview job kind
# (image / video / 3D) under a shared queue.
FROM debian:bookworm-slim AS runtime

# - ffmpeg: drives preview.video (HLS encode, sprite sheet, poster).
# - chromium + node: drive preview.model — the headless three.js
#   turntable renderer (#498, ADR 0069). Software WebGL (SwiftShader),
#   so no GPU stack is baked in and it runs anywhere.
#
# Blender is deliberately NOT here (#500). It was 1.3 GB of a 3.64 GB
# image and, since #498 routed every renderable format to the three.js
# worker, nothing invoked it. It returns as an opt-in *converter*
# plugin for proprietary formats (#499) — see ADR 0069, amended.
#
# The GL/X libs that used to be listed explicitly (libgl1, libegl1,
# libxrender1, libxi6, libxxf86vm1, libxfixes3, libxkbcommon0, libxext6,
# libsm6, libice6) were Blender's dlopen set and left with it. Chromium
# pulls the X/GL libs it actually needs through its own apt dependency
# tree, so there is no explicit list to maintain here; a turntable render
# on this image is the check that matters, because a missing dlopen dep
# fails at RENDER time, not build time (see scripts/threejs/smoke.mjs).
ARG UID
ARG GID

# curl: the app healthcheck (docker-compose.yml) shells out to it.
# chromium: the headless renderer for the three.js preview worker (#498).
# The apt package pulls its full runtime dep tree (nss, gtk, fonts, libgbm,
# …) so puppeteer can launch it on SwiftShader software WebGL — no GPU.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl ffmpeg \
        librsvg2-bin libwebp7 poppler-utils ghostscript imagemagick unar \
        chromium \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid "${GID}" app \
 && useradd --uid "${UID}" --gid app --no-create-home app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

# three.js preview worker (#498): Node runtime + its node_modules + the
# worker script. The Go handler defaults ThreeJSScript to
# /app/threejs/worker.mjs; with Blender gone (#500) this is the only 3D
# renderer, so a missing piece fails preview.model jobs retryably rather
# than degrading silently. PUPPETEER_EXECUTABLE_PATH points puppeteer at
# the apt Chromium instead of a bundled download.
COPY --from=threejs-deps /usr/local/bin/node /usr/local/bin/node
COPY --from=threejs-deps --chown=app:app /app/threejs/node_modules /app/threejs/node_modules
COPY --chown=app:app scripts/threejs/worker.mjs scripts/threejs/render.html \
                     scripts/threejs/smoke.mjs scripts/threejs/package.json /app/threejs/
ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa

EXPOSE 8080

ENTRYPOINT ["/app/aa"]
