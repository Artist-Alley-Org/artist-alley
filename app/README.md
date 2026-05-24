# `app/` — the artist-alley Go server

This is the long-term home for artist-alley's backend. The plan (ADR 0006)
is to port RS PHP features into this Go binary one at a time until PHP is
fully retired; at the end state, `app` is the only backend process.

Phase 1.0 (this directory's current state) is the **skeleton only**: the
binary boots, connects to Postgres, serves a single `/healthz` endpoint,
and lets nginx route to it. No RS features have been ported yet — every
RS route still goes to PHP-FPM via the same nginx in front of the stack.

## Layout

```
app/
├── cmd/aa/                   # binary entrypoint (main package)
├── internal/
│   ├── config/               # env-based config
│   ├── db/                   # pgx connection pool
│   ├── logging/              # slog setup (JSON by default)
│   └── http/
│       ├── server.go         # the chi router + http.Server + graceful shutdown
│       ├── middleware/       # request-id, structured logger, panic recover
│       └── handlers/         # one file per route concern (health.go, ...)
├── migrations/               # goose .sql files for artist-alley-owned tables (none yet)
├── go.mod / go.sum
└── README.md
```

`internal/` is enforced by the Go toolchain: nothing outside `app/` can
import these packages. Use that to keep boundaries clean.

## Build

```bash
docker compose build app
```

Or, outside Docker:

```bash
cd app
go build ./cmd/aa
```

The Dockerfile at [infra/docker/app/Dockerfile](../infra/docker/app/Dockerfile)
produces a static binary in a multi-stage build and ships it in an
alpine runtime; the final image is around 20 MB.

## Run

The `docker compose up` of the whole stack starts the `app` service
automatically (see `docker-compose.yml`). Standalone runs require
`AA_DB_PASSWORD` and a reachable Postgres; see `internal/config/config.go`
for the full env-var list.

## Test

```bash
# everything (PHP integration + Go):
./scripts/test.sh

# Go only:
./scripts/test.sh --go
```

Tests under `app/...` run with `go test -race -count=1 ./...` in a
throwaway `golang:1.26-alpine` container on the same docker network as
the compose stack. They connect to Postgres at `postgres:5432` using the
credentials in `.env`.

## Adding a new endpoint

1. Write the handler in `internal/http/handlers/<feature>.go`.
2. Wire it up in `internal/http/server.go` via the chi router.
3. Add a route in [infra/nginx/default.conf](../infra/nginx/default.conf)
   so nginx sends the path to `app` (otherwise it falls through to PHP).
4. Add tests under the same `handlers` package or, for end-to-end tests,
   under `tests/go/` (build-tag-gated if they need the live stack).
