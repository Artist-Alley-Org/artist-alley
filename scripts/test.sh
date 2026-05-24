#!/usr/bin/env bash
# scripts/test.sh
#
# Run artist-alley's integration tests against a live local Postgres.
#
# Usage:
#   ./scripts/test.sh                   # run every integration test
#   ./scripts/test.sh tests/aa/...      # run a specific test file

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Pull DB credentials from .env into AA_DB_* so the test scripts have them.
if [ ! -f .env ]; then
    echo "ERROR: .env not found. Run ./scripts/bootstrap.sh first." >&2
    exit 2
fi
# shellcheck disable=SC2046
export $(grep -E '^(POSTGRES_DB|POSTGRES_USER|POSTGRES_PASSWORD|POSTGRES_HOST_PORT)=' .env | xargs)

export AA_DB_HOST=postgres
export AA_DB_PORT=5432
export AA_DB_NAME="${POSTGRES_DB:-artist_alley}"
export AA_DB_USER="${POSTGRES_USER:-artist_alley}"
export AA_DB_PASSWORD="${POSTGRES_PASSWORD:-}"

if ! docker compose ps --status running --format json 2>/dev/null | grep -q '"postgres"'; then
    echo "ERROR: postgres container is not running. Start it with 'docker compose up -d'." >&2
    exit 2
fi

if [ $# -gt 0 ]; then
    tests=("$@")
else
    mapfile -t tests < <(find tests/aa/integration -type f -name '*_test.php' | sort)
fi

failed=0
for t in "${tests[@]}"; do
    container_path="/var/www/html/${t}"
    echo "=> ${t}"
    if ! docker compose exec -T \
        -e AA_DB_HOST -e AA_DB_PORT -e AA_DB_NAME -e AA_DB_USER -e AA_DB_PASSWORD \
        php php "${container_path}"; then
        failed=$((failed + 1))
    fi
done

if [ "$failed" -gt 0 ]; then
    echo
    echo "${failed} test file(s) failed"
    exit 1
fi

echo
echo "All test files passed."
