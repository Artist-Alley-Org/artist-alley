# artist-alley Go server image.
#
# Multi-stage: build the static binary in a Go toolchain image, then
# copy it into a minimal runtime. Final image is ~20 MB.

ARG GO_VERSION=1.26
ARG BUILD_VERSION=dev

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
# debian-slim instead of alpine because Blender (preview.model worker)
# doesn't ship as a clean apk and the binary is built against glibc.
# Adds ~50 MB over alpine before blender; with blender + ffmpeg the
# final image is ~700 MB. The trade-off buys us a single unified
# pipeline: one container produces image / video / 3D previews.
FROM debian:bookworm-slim AS runtime

# - ffmpeg: preview.video / preview.audio (HLS, sprite sheet, poster).
# - blender: preview.model (turntable render → sprite sheet + posters).
#   Headless via --background; uses Cycles CPU (no GPU stack baked in,
#   so it runs anywhere; deployments with GPUs can swap the base image
#   to nvidia/cuda + register the same blender binary).
# - libgl1 / libxrender1 / libxi6 / libxxf86vm1 / libxfixes3 / libxkbcommon0:
#   blender links these even in --background mode (it dlopens GL on
#   startup); without them the binary aborts before reaching the script.
# - python3-pil: post-process the 36 PNG frames into a single sprite
#   sheet without bundling extra Python with the blender install.
# - scripts/blender/turntable.py is mounted by the compose file (worker
#   role) rather than baked into the image so iteration is cheap.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata ffmpeg blender python3-pil \
        libgl1 libxrender1 libxi6 libxxf86vm1 libxfixes3 libxkbcommon0 \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system app && useradd --system --gid app app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa
COPY --chown=app:app scripts/blender /app/blender

EXPOSE 8080

ENTRYPOINT ["/app/aa"]
