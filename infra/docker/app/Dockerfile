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
FROM node:24-bookworm-slim AS threejs-deps
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
# tini: PID 1, so orphaned grandchildren get reaped (#890) — see ENTRYPOINT.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl ffmpeg tini \
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
# render.html imports the viewer's own load path + lighting constants
# (#689); worker.mjs serves this directory at /shared/. Canonical home is
# web/src/lib/3d/ — the one directory both build worlds can reach.
COPY --chown=app:app web/src/lib/3d/modelLoader.js web/src/lib/3d/defaultLighting.js \
                     /app/threejs/shared/
ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium

# Cap glibc's malloc arenas (#887).
#
# The binary is cgo'd: chai2010/webp compiles libwebp's C in, and every
# WebP variant the preview ladder writes allocates through glibc rather
# than the Go heap. glibc gives each thread its own arena, up to
# 8 × ncores — 192 on the 24-core CI host — and it only ever trims the
# MAIN arena's top. So the per-thread arenas grow to the storm's high
# water and stay there: measured 1.02 GB of non-Go anonymous RSS still
# resident at idle, with the Go heap collected down to 165 MB and every
# child process gone. With MALLOC_ARENA_MAX=2 the same storm settles at
# 0.14 GB, and it costs nothing measurable in time (163 s against 158 s
# for the rebuild, inside run-to-run spread).
#
# This is glibc-only. It works here because the runtime stage is
# debian-bookworm-slim (glibc 2.36, `ldd --version` inside the image);
# on a musl base it would be silently ignored, so an Alpine rebase has
# to re-measure rather than assume this line still does something.
ENV MALLOC_ARENA_MAX=2

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa

EXPOSE 8080

# tini as PID 1 rather than /app/aa directly (#890).
#
# The preview pipeline spawns process TREES, not single children:
# node → headless chromium → chrome_crashpad_handler, plus ffmpeg,
# ghostscript, pdftoppm, unar and ImageMagick. os/exec reaps the DIRECT
# child of each exec.Command; the grandchildren those children leave
# behind are orphaned and reparented to PID 1. With the Go binary as
# PID 1 nothing ever called wait() for them, so one measured render
# storm ended with 328 zombies climbing monotonically — and a container
# that runs for weeks walks into PID exhaustion, which surfaces as a
# fork failure somewhere unrelated rather than as "we leaked PIDs".
#
# Deliberately NOT a SIGCHLD reap loop inside the Go process. A
# wait4(-1, WNOHANG) loop in `aa` races os/exec's own Cmd.Wait for the
# same children: whichever call wakes first consumes the exit status
# and the loser gets ECHILD, so a FAILED ffmpeg run stops reporting its
# real exit code. That is a worse defect than the zombies. tini cannot
# hit that race — a process only reaps its own children, and `aa`'s
# subprocesses are tini's GRANDchildren. They reach tini only after
# being orphaned, which is after os/exec has finished with them.
#
# In the image rather than `init: true` in compose, because reaping is
# a property of this image: every deployment of it gets the fix, not
# only the operators who run our compose file.
ENTRYPOINT ["/usr/bin/tini", "--", "/app/aa"]
