#!/usr/bin/env bash
# scripts/test.sh
#
# Run artist-alley's integration tests against the live docker-compose
# stack. Covers both the PHP integration tests (under tests/aa/) and the
# Go tests (under app/...).
#
# Usage:
#   ./scripts/test.sh             # PHP + Go
#   ./scripts/test.sh --php       # PHP only
#   ./scripts/test.sh --go        # Go only

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
# at its native port. Both the PHP container and the Go test runner use
# these values.
export AA_DB_HOST=postgres
export AA_DB_PORT=5432
export AA_DB_NAME="${POSTGRES_DB:-artist_alley}"
export AA_DB_USER="${POSTGRES_USER:-artist_alley}"
export AA_DB_PASSWORD="${POSTGRES_PASSWORD:-}"

if ! docker compose ps --status running --format json 2>/dev/null | grep -q '"postgres"'; then
    echo "ERROR: postgres container is not running. Start it with 'docker compose up -d'." >&2
    exit 2
fi

run_php="yes"
run_go="yes"
with_s3="no"
for arg in "$@"; do
    case "$arg" in
        --php)     run_go="no";  shift ;;
        --go)      run_php="no"; shift ;;
        --with-s3) with_s3="yes"; shift ;;
    esac
done

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }

failed=0

if [ "$run_php" = "yes" ]; then
    step "PHP integration tests (tests/aa/integration/)"
    mapfile -t php_tests < <(find tests/aa/integration -type f -name '*_test.php' | sort)
    for t in "${php_tests[@]}"; do
        echo "=> ${t}"
        if ! docker compose exec -T \
            -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
            php php "/var/www/html/${t}"; then
            failed=$((failed + 1))
        fi
    done
fi

if [ "$run_go" = "yes" ]; then
    step "Go tests (app/...)"

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
    if ! docker run --rm \
        --network "$NET" \
        -v "${ROOT}/app:/src/app" \
        -w /src/app \
        -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
        "${s3_env[@]}" \
        golang:1.26 \
        go test -race -count=1 ./...; then
        failed=$((failed + 1))
    fi
fi

if [ "$failed" -gt 0 ]; then
    echo
    echo "${failed} test step(s) failed"
    exit 1
fi

echo
echo "All tests passed."
