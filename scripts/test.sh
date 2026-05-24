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
case "${1:-}" in
    --php) run_go="no";  shift ;;
    --go)  run_php="no"; shift ;;
esac

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
    # Resolve the compose network name by inspecting one of its services.
    NET=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' \
        "$(docker compose ps -q postgres)")
    if ! docker run --rm \
        --network "$NET" \
        -v "${ROOT}/app:/src/app" \
        -w /src/app \
        -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
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
