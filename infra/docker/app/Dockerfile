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

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S app && adduser -S -G app app

USER app
WORKDIR /app
COPY --from=build --chown=app:app /out/aa /app/aa

EXPOSE 8080

ENTRYPOINT ["/app/aa"]
