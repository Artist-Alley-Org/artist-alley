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

# ── Per-worktree test databases (#644) ────────────────────────────
# Every `git worktree` shares the primary checkout's gitignored .env,
# so POSTGRES_DB — and therefore `<dev>_test` — is identical in all of
# them. Two agents running this script at once both reset the SAME
# database, and the reset's WITH (FORCE) below terminates the other
# run's live connections mid-suite: the victim sees 40+ failures
# ("terminating connection due to administrator command", `relation
# "user" does not exist`) that look like catastrophic application
# breakage and have nothing to do with its change.
#
# So a *linked* worktree gets its own database, keyed on a short hash
# of its checkout path (stable for the life of the worktree, unlike
# the branch name, which can change mid-run). The primary checkout
# keeps plain `<dev>_test` — the single-checkout case is unchanged,
# and its database persists between runs as before.
#
# Isolation, not serialisation: parallel worktrees exist so agents can
# work in parallel, and a flock around the whole suite would just make
# them queue behind each other for ~10 minutes.
OWN_TEST_DB="no"
if [ "$(git rev-parse --git-dir 2>/dev/null || echo .)" \
     != "$(git rev-parse --git-common-dir 2>/dev/null || echo .)" ]; then
    wt_id="$(printf '%s' "$ROOT" | sha1sum | cut -c1-8)"
    export AA_DB_NAME="${AA_DB_NAME}_${wt_id}"
    # This run owns a disposable database nobody else will ever reuse,
    # so it must also drop it — see the trap below. Per-worktree
    # databases that pile up are their own problem (cf. #622).
    OWN_TEST_DB="yes"
fi

# Postgres truncates identifiers at 63 bytes, and a truncated name is a
# silent collision — exactly the failure mode this block exists to
# prevent. Refuse rather than truncate.
if [ "${#AA_DB_NAME}" -gt 63 ]; then
    echo "ERROR: test database name '${AA_DB_NAME}' exceeds Postgres' 63-byte identifier limit." >&2
    echo "       Shorten POSTGRES_DB in .env." >&2
    exit 2
fi

# Whatever name we landed on, claim it for the duration of the run. If
# a second run computes the same name — two invocations in one
# checkout, two clones at different paths, a hand-set POSTGRES_DB — it
# stops here with one clear message instead of quietly force-dropping
# the database out from under the run that got there first.
LOCK_FILE="${TMPDIR:-/tmp}/aa-test-db-${AA_DB_NAME}.lock"
if command -v flock >/dev/null 2>&1; then
    exec 9>"$LOCK_FILE"
    if ! flock -n 9; then
        echo "ERROR: test database '${AA_DB_NAME}' is already in use by another ./scripts/test.sh run." >&2
        echo "       Wait for it to finish, or give this checkout its own database by" >&2
        echo "       setting a distinct POSTGRES_DB in its .env." >&2
        exit 2
    fi
else
    echo "WARNING: flock not found — cannot detect a concurrent run on ${AA_DB_NAME}." >&2
fi

if ! docker compose ps --status running --format json 2>/dev/null | grep -q '"postgres"'; then
    echo "ERROR: postgres container is not running. Start it with 'docker compose up -d'." >&2
    exit 2
fi

# Drop a per-worktree database on the way out, however we exit — a
# throwaway database per checkout is only an improvement if it also
# goes away. Set AA_TEST_KEEP_DB=1 to keep it for a post-mortem.
# The primary checkout's `<dev>_test` is deliberately left alone: it
# is reset at the start of each run, not accumulated.
cleanup_test_db() {
    local rc=$?
    if [ "$OWN_TEST_DB" = "yes" ] && [ "${AA_TEST_KEEP_DB:-0}" != "1" ]; then
        docker compose exec -T postgres psql -U "$AA_DB_USER" -d postgres \
            -c "DROP DATABASE IF EXISTS ${AA_DB_NAME} WITH (FORCE);" >/dev/null 2>&1 || true
    fi
    return $rc
}
trap cleanup_test_db EXIT

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
# $AA_DB_NAME, and LISTEN/NOTIFY channels (migration 00006 —
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
