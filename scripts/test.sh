#!/usr/bin/env bash
# scripts/test.sh
#
# Run artist-alley's Go integration tests against the live
# docker-compose stack. Postgres must be up (docker compose up -d).
#
# Usage:
#   ./scripts/test.sh             # Go tests
#   ./scripts/test.sh --with-s3   # also bring up MinIO + run S3-backend tests
#
# (The legacy `--php` flag is gone — the PHP-side test harness was
# removed when the strangler-fig migration was abandoned. The upstream
# fork base is reference-only and there is no PHP code path in the
# runtime.)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ ! -f .env ]; then
    echo "ERROR: .env not found. Run ./scripts/bootstrap.sh first." >&2
    exit 2
fi
# shellcheck disable=SC2046
export $(grep -E '^(POSTGRES_DB|POSTGRES_USER|POSTGRES_PASSWORD|POSTGRES_HOST_PORT)=' .env | xargs)

# Inside the docker network, postgres is reachable on its service name
# at its native port.
export AA_DB_HOST=postgres
export AA_DB_PORT=5432
export AA_DB_USER="${POSTGRES_USER:-artist_alley}"
export AA_DB_PASSWORD="${POSTGRES_PASSWORD:-}"

# ── Test-database isolation (#291) ────────────────────────────────
# The Go suite TRUNCATEs / DELETEs shared tables (users, user_roles,
# jobs, goose_db_version). Pointed at the live dev database this has
# repeatedly corrupted dev state — wiped the admin's roles (locking
# login out), poisoned real background jobs, reset goose bookkeeping.
# So the whole suite runs against a dedicated, disposable
# `<dev>_test` database in the SAME postgres container, reset to a
# clean migrated state each run (see "Resetting isolated test
# database" below). Every openPool helper reads AA_DB_NAME, so this
# single override redirects all 43 test files with no per-file edits —
# nothing after this point touches the dev database.
DEV_DB_NAME="${POSTGRES_DB:-artist_alley}"
export AA_DB_NAME="${DEV_DB_NAME}_test"

if ! docker compose ps --status running --format json 2>/dev/null | grep -q '"postgres"'; then
    echo "ERROR: postgres container is not running. Start it with 'docker compose up -d'." >&2
    exit 2
fi

with_s3="no"
for arg in "$@"; do
    case "$arg" in
        --with-s3) with_s3="yes"; shift ;;
        # Back-compat: --go is a no-op now (Go is the only target).
        # --php is rejected loudly so a stale invocation surfaces
        # rather than silently doing nothing.
        --go) shift ;;
        --php)
            echo "ERROR: --php flag removed — PHP harness was deleted." >&2
            exit 2
            ;;
    esac
done

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }

failed=0

step "Go tests (app/...)"

# The dev app container can keep running. It is pinned to the dev
# database ($DEV_DB_NAME); this suite runs entirely against
# ${DEV_DB_NAME}_test, and LISTEN/NOTIFY channels (migration 00006 —
# federation_outbox / federation_inbox) are per-database, so the live
# dispatcher + delivery worker cannot race the tests. Before #291 the
# suite shared the dev DB and we had to stop `app` to avoid exactly
# that race; the dedicated test DB makes stopping it unnecessary.
#
# AA_E2E_ISOLATED opts the federation latency-contract e2e test
# (TestFederation_EndToEnd_ProductionDefaults_SubSecond) in — the
# dedicated test DB gives it the committed-state isolation it needs.
export AA_E2E_ISOLATED=1

# When --with-s3, bring MinIO up in the same compose stack and
# expose its credentials to the test container so the s3 backend's
# contract tests run. Otherwise those tests skip cleanly.
s3_env=()
if [ "$with_s3" = "yes" ]; then
    step "Bringing up MinIO (storage-s3 profile)"
    docker compose --profile storage-s3 up -d minio
    # Wait for the MinIO API to answer. ~30s budget.
    for i in $(seq 1 30); do
        if docker compose exec -T minio sh -c 'curl -fs http://localhost:9000/minio/health/live' >/dev/null 2>&1; then
            echo "MinIO ready"
            break
        fi
        sleep 1
    done
    # Pull MinIO creds from .env so the test container uses them.
    # shellcheck disable=SC2046
    export $(grep -E '^MINIO_ROOT_(USER|PASSWORD)=' .env | xargs)
    s3_env=(
        -e AA_S3_TEST_ENDPOINT=http://minio:9000
        -e AA_S3_TEST_BUCKET=artist-alley-test
        -e AA_S3_TEST_ACCESS_KEY="${MINIO_ROOT_USER:-aaminio}"
        -e AA_S3_TEST_SECRET_KEY="${MINIO_ROOT_PASSWORD}"
    )
fi

NET=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' \
    "$(docker compose ps -q postgres)")

# Reset the isolated test DB to a clean, migrated state. Drop +
# recreate gives every run an identical starting point regardless of
# what the previous run's destructive tests left behind; WITH (FORCE)
# terminates any leftover connections. The baseline migration creates
# the pgvector extension itself, so no separate CREATE EXTENSION step
# is needed. In CI this just creates the test DB alongside the
# app-migrated dev DB — CI's postgres is ephemeral, so it's a safe
# superset that keeps CI green.
step "Resetting isolated test database ($AA_DB_NAME)"
docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$AA_DB_USER" -d postgres \
    -c "DROP DATABASE IF EXISTS ${AA_DB_NAME} WITH (FORCE);" \
    -c "CREATE DATABASE ${AA_DB_NAME};"

# Migrate it with the app's own embedded goose migrations (via the
# aa-migrate helper) so the test schema can never drift from what the
# server applies at boot.
step "Migrating test database ($AA_DB_NAME)"
docker run --rm \
    --network "$NET" \
    -v "${ROOT}/app:/src/app" \
    -w /src/app \
    -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
    -e GOFLAGS -e GOMAXPROCS \
    golang:1.26 \
    go run ./cmd/aa-migrate

# -p 1 serialises package execution: every test binary writes to the
# same artist_alley DB, and parallel packages were racing on shared
# rows (e.g. setup tests wipe system.admin user_roles to assert
# needs_setup=true; a parallel teams/workflow test creating its own
# admin during that window flipped TestCompleteSetup_InputValidation
# to 409 + TestAssetCount_ExcludesSubtitleTracks similarly). Cost of
# serialisation is ~5min added CI duration on self-hosted runner where
# wall-clock is queue time, not minute-budget — cheap. Real fix is
# per-package schema isolation; serialise is the precursor.
#
# -timeout 10m states the per-package bound explicitly (#622). It is
# Go's own default made visible, and it is the ONLY timeout: the tests
# themselves use t.Context() rather than hardcoded context deadlines,
# because an inner N-second cap under -race -p 1 contention is an
# arbitrary second bound that fires on loaded runners and replaces the
# real error ("which query was slow") with `context deadline exceeded`.
if ! docker run --rm \
    --network "$NET" \
    -v "${ROOT}/app:/src/app" \
    -w /src/app \
    -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
    -e GOFLAGS -e GOMAXPROCS \
    "${s3_env[@]}" \
    golang:1.26 \
    go test -race -count=1 -p 1 -timeout 10m ./...; then
    failed=$((failed + 1))
fi

if [ "$failed" -gt 0 ]; then
    echo
    echo "${failed} test step(s) failed"
    exit 1
fi

echo
echo "All tests passed."
