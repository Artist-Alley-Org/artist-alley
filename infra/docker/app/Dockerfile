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
FROM alpine:3.20 AS runtime

# ffmpeg is the workhorse for preview.video / preview.audio jobs
# (HLS encoding, frame extraction, waveform render). Alpine's package
# ships with libx264 + libvpx + opus + aac. GPU encoders (NVENC,
# QSV, VAAPI) aren't built into the alpine ffmpeg; the worker probes
# at boot and falls back cleanly to libx264 when they're absent — so
# a CPU-only deployment still works, and a future debian-based image
# can drop in GPU encoders without code changes.
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
 && addgroup -S app && adduser -S -G app app \
 && mkdir -p /var/lib/aa-storage \
 && chown app:app /var/lib/aa-storage

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa

EXPOSE 8080

ENTRYPOINT ["/app/aa"]
