# artist-alley Go server image.
#
# Multi-stage: build the static binary in a Go toolchain image, then
# copy it into a minimal runtime. Runtime bundles ffmpeg + a pinned
# Blender 4.2 LTS so the preview.video and preview.model handlers can
# run inline — final image is ~1.4 GB. Still one container.

ARG GO_VERSION=1.26
ARG BUILD_VERSION=dev
# Match the in-container `app` user to the host UID/GID so the bind-
# mounted /var/lib/aa-storage volume stays writable across image
# rebuilds. Other services (php, web) already accept these via compose.
ARG UID=1000
ARG GID=1000

# ---- build stage ----------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src/app

# Cache the module download layer separately from sources.
COPY app/go.mod app/go.sum* ./
RUN go mod download

COPY app/ ./

ARG BUILD_VERSION
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
    -o /out/aa \
    ./cmd/aa

# ---- runtime stage --------------------------------------------------------
# debian-slim instead of alpine because the official Blender tarball
# (preview.model worker) is built against glibc. Final image lands
# around ~1.4 GB; the cost buys one container that handles every
# preview job kind (image / video / 3D) under a shared queue.
FROM debian:bookworm-slim AS runtime

# - ffmpeg: drives preview.video (HLS encode, sprite sheet, poster).
# - blender (Cycles CPU): drives preview.model (turntable + sprite
#   sheet). No GPU stack baked in so it runs anywhere; deployments with
#   GPUs can layer nvidia/cuda + reuse the same binary.
# - libgl1 / libegl1 / libxrender1 / libxi6 / libxxf86vm1 / libxfixes3 /
#   libxkbcommon0 / libxext6 / libsm6 / libice6: blender dlopens GL +
#   X libs on startup even in --background; without them the binary
#   aborts before our script runs.
# - turntable.py is baked into the image at /app/blender/turntable.py
#   via the COPY below — the Go handler defaults its ScriptPath to
#   that location.
ARG UID
ARG GID

# Blender pin. We deliberately do NOT use debian's `blender` package:
# bookworm ships 3.4.1 (2022) and its bundled glTF / FBX add-ons use
# `np.bool` which NumPy 1.20+ removed — version-skew that's been broken
# for years. Pulling the official portable tarball gives us a known
# Blender 4.2 LTS build that bundles its own Python + numpy, so the
# add-ons match the runtime they were written against. LTS line is
# supported through July 2027.
ARG BLENDER_VERSION=4.2.9
ARG BLENDER_SHA256=dfbc127a7d28f9c2175b23bf9d6701b2855f31eedfb391f9a6e60adb24572846

# Runtime libs blender dlopens on startup even in --background mode
# (libgl, libxrender, etc.); ffmpeg drives preview.video. xz-utils +
# curl are only needed for the extraction step.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl xz-utils ffmpeg \
        libgl1 libegl1 libxrender1 libxi6 libxxf86vm1 libxfixes3 \
        libxkbcommon0 libxext6 libsm6 libice6 \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fL -o /tmp/blender.tar.xz \
        "https://download.blender.org/release/Blender${BLENDER_VERSION%.*}/blender-${BLENDER_VERSION}-linux-x64.tar.xz" \
 && echo "${BLENDER_SHA256}  /tmp/blender.tar.xz" | sha256sum -c - \
 && mkdir -p /opt/blender \
 && tar -xJf /tmp/blender.tar.xz -C /opt/blender --strip-components=1 \
 && ln -s /opt/blender/blender /usr/local/bin/blender \
 && rm /tmp/blender.tar.xz \
 && apt-get purge -y curl xz-utils \
 && apt-get autoremove -y \
 && groupadd --gid "${GID}" app \
 && useradd --uid "${UID}" --gid app --no-create-home app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa
COPY --chown=app:app scripts/blender /app/blender

EXPOSE 8080

ENTRYPOINT ["/app/aa"]
