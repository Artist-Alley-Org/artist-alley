#!/usr/bin/env bash
# scripts/verify-baseline.sh
#
# Verifies ADR 0046's baseline squash story before the v1.0.0 tag
# flips migrations to append-only forever. Runs three checks:
#
#   1. baseline_present   — 00001_baseline_v1.sql exists + is
#                            non-empty + carries the ADR-referenced
#                            header docstring.
#   2. baseline_applies   — goose can apply the baseline (+ every
#                            append migration) against a scratch
#                            Postgres 16 DB from a totally empty
#                            starting state.
#   3. no_gaps            — the migration filename sequence is
#                            contiguous (no holes between numeric
#                            prefixes). Gaps would indicate a
#                            missing migration or a squash that
#                            didn't renumber cleanly.
#
# Fails loud with a summary of which check failed + a hint what
# usually causes it. Success prints "baseline verified against N
# append migrations — ready for v1.0.0 tag."
#
# See §4.21 of docs/v0_1_readiness.md + ADR 0046.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MIG_DIR="app/internal/db/migrations"
BASELINE="${MIG_DIR}/00001_baseline_v1.sql"

step()   { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
ok()     { printf '\033[1;32mOK\033[0m  %s\n' "$*"; }
fail()   { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; }
warn()   { printf '\033[1;33mWARN\033[0m %s\n' "$*"; }

failed=0

# ---------------------------------------------------------------------------
# Check 1: baseline_present
# ---------------------------------------------------------------------------
step "1/3 baseline present + non-empty + ADR-referenced"

if [ ! -f "${BASELINE}" ]; then
    fail "baseline file missing: ${BASELINE}"
    failed=$((failed + 1))
elif [ ! -s "${BASELINE}" ]; then
    fail "baseline file empty: ${BASELINE}"
    failed=$((failed + 1))
elif ! grep -q "ADR 0046" "${BASELINE}"; then
    fail "baseline missing ADR 0046 reference in header docstring"
    failed=$((failed + 1))
else
    lines=$(wc -l <"${BASELINE}")
    ok "${BASELINE} — ${lines} lines, ADR 0046 referenced"
fi

# ---------------------------------------------------------------------------
# Check 2: baseline_applies (fresh-DB round-trip)
# ---------------------------------------------------------------------------
step "2/3 baseline applies from empty on a scratch Postgres 16"

# Spin up an isolated postgres container so we don't touch dev DB.
CTNR="aa-verify-baseline-$$"
PG_PORT=""

cleanup() {
    if docker inspect "${CTNR}" >/dev/null 2>&1; then
        docker rm -f "${CTNR}" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# Pick a random unbound port. Postgres 16 to match the compose stack.
docker run --rm -d \
    --name "${CTNR}" \
    -e POSTGRES_PASSWORD=verify \
    -e POSTGRES_USER=verify \
    -e POSTGRES_DB=verify \
    -p 0:5432 \
    pgvector/pgvector:pg16 >/dev/null

# Wait until postgres accepts connections.
for _ in $(seq 1 30); do
    if docker exec "${CTNR}" pg_isready -U verify >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if ! docker exec "${CTNR}" pg_isready -U verify >/dev/null 2>&1; then
    fail "scratch postgres never became ready"
    failed=$((failed + 1))
else
    # Apply every migration via goose from a fresh Go build. Reuses
    # the same UpContext path the real Migrate function uses so we
    # exercise the actual embed-FS bind + goose invocation.
    if docker run --rm \
        --network container:"${CTNR}" \
        -v "${ROOT}/app:/src/app" \
        -v go-mod-cache:/go/pkg/mod \
        -w /src/app \
        -e AA_DB_HOST=127.0.0.1 \
        -e AA_DB_PORT=5432 \
        -e AA_DB_NAME=verify \
        -e AA_DB_USER=verify \
        -e AA_DB_PASSWORD=verify \
        golang:1.26 \
        sh -c 'go run ./cmd/aa/verifybaseline/' >/tmp/verify-baseline.log 2>&1
    then
        applied=$(tail -1 /tmp/verify-baseline.log)
        ok "baseline + append chain applied clean; head=${applied}"
    else
        fail "baseline apply failed — see /tmp/verify-baseline.log"
        tail -30 /tmp/verify-baseline.log || true
        failed=$((failed + 1))
    fi
fi

# ---------------------------------------------------------------------------
# Check 3: no_gaps in filename sequence
# ---------------------------------------------------------------------------
step "3/3 migration filename sequence is contiguous"

mapfile -t versions < <(find "${MIG_DIR}" -maxdepth 1 -type f -name '*.sql' -printf '%f\n' | \
    sed -n 's/^\([0-9][0-9][0-9][0-9][0-9]\)_.*\.sql$/\1/p' | \
    sort -u)

if [ "${#versions[@]}" -eq 0 ]; then
    fail "no canonical migration filenames found"
    failed=$((failed + 1))
else
    prev=0
    gaps=0
    for v in "${versions[@]}"; do
        n=$((10#$v))
        if [ "$prev" -ne 0 ] && [ "$n" -ne $((prev + 1)) ]; then
            warn "gap between ${prev} and ${n}"
            gaps=$((gaps + 1))
        fi
        prev=$n
    done
    if [ "$gaps" -eq 0 ]; then
        head=${versions[-1]}
        ok "${#versions[@]} migrations, contiguous, head=${head}"
    else
        fail "${gaps} gap(s) in migration sequence"
        failed=$((failed + 1))
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
if [ "$failed" -eq 0 ]; then
    count=${#versions[@]}
    appends=$((count - 1))
    printf '\033[1;32mbaseline verified\033[0m against %d append migrations — ready for v1.0.0 tag.\n' "${appends}"
    exit 0
fi
printf '\033[1;31m%d check(s) failed\033[0m — see output above.\n' "${failed}" >&2
exit 1
