#!/usr/bin/env bash
# scripts/dogfood/run-ui.sh — UI dogfood runner (Playwright).
#
# Invokes the Playwright suite under scripts/dogfood/ui/ and prints
# a summary the same shape as run-all.sh produces for the shell
# scenarios.
#
# Two test sets:
#   standalone — works against the dev stack alone. No pair.sh
#                required. Default; runs as part of every PR /
#                pre-merge check.
#   federation — requires both studio-a (dev) and studio-b
#                (dogfood profile) running AND paired via
#                scripts/dogfood/pair.sh. Use during dogfood weeks
#                or before federation-touching merges.
#
# Usage:
#   ./scripts/dogfood/run-ui.sh [MODE] [-- playwright args...]
#
# Modes (positional, first arg):
#   standalone   standalone tests only (default if omitted)
#   federation   federation tests only
#   all          both projects
#
# Extra args after the mode are forwarded to `playwright test`
# verbatim — so `--grep PATTERN`, `--reporter line`, etc. work
# as usual.

set -uo pipefail

SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UI_DIR="${ROOT}/scripts/dogfood/ui"
cd "$UI_DIR"

# --- arg parsing ---------------------------------------------------------

# First positional arg selects the mode (defaults to "standalone").
# Everything after is passed through to `playwright test`.
mode="standalone"
case "${1:-}" in
    standalone) mode="standalone"; shift ;;
    federation) mode="federation"; shift ;;
    all)        mode="all"; shift ;;
    -h|--help)
        sed -n '2,26p' "$SCRIPT_PATH"
        exit 0
        ;;
    "" )        ;;  # no arg → default standalone
    -*)         ;;  # starts with a dash → playwright passthrough, keep mode default
    *)          printf 'Unknown mode: %s\n' "$1" >&2; exit 2 ;;
esac
passthrough=("$@")

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- 1. ensure deps -------------------------------------------------------

if ! command -v node >/dev/null 2>&1; then
    fail "node is required. Install via your package manager or nvm."
fi
if ! command -v npx >/dev/null 2>&1; then
    fail "npx is required (ships with node)."
fi

if [ ! -d node_modules ]; then
    step "Installing Playwright dependencies (first run only)"
    npm install --silent
fi

if [ ! -d node_modules/@playwright/test/.local-browsers ] && \
   [ ! -d "$HOME/.cache/ms-playwright" ]; then
    step "Installing Playwright Chromium browser"
    npx playwright install chromium
fi

# --- 1b. corpus census, before ------------------------------------------
#
# THE INVARIANT (#1245): a suite run must END with the corpus counts it
# BEGAN with. Every spec is supposed to clean up after itself, and for a
# long time most of them did not — the shared coding stack accumulated
# 1,544 fixture assets, 878 collections and 1,155 field definitions,
# until 44% of the asset table was litter and the newest-200 window held
# no raster image at all. Nine specs then failed there for reasons that
# had nothing to do with what they assert.
#
# Sprint 5 and #1198 both checked this per-spec. Per-spec is the weaker
# place for it: it only guards the specs that remembered to ask, and the
# leak always comes from the one that did not. At suite level it catches
# the NEXT leak without anyone opting in.
#
# It is a REPORT plus a non-zero exit, not a cleanup. Deleting whatever
# a run added would hide which spec added it, and the point is to name
# the leak while the run that caused it is still on screen.

corpus_census() {
    docker compose exec -T postgres psql -U "${POSTGRES_USER:-artist_alley}" \
        -d "${POSTGRES_DB:-artist_alley}" -tA -F'|' -c "
        SELECT (SELECT count(*) FROM assets)
            || '|' || (SELECT count(*) FROM posts)
            || '|' || (SELECT count(*) FROM collections)
            || '|' || (SELECT count(*) FROM field_definition)
            || '|' || (SELECT count(*) FROM \"user\");" 2>/dev/null | tr -d '[:space:]'
}

# The census reads the LOCAL compose stack. If the suite is pointed at a
# different host, the two would be describing different databases and a
# clean diff would mean nothing — so check they agree, and skip loudly
# rather than report a comparison that is not a comparison. This is the
# same class of mistake as running the suite against the wrong stack;
# it just fails silently instead of visibly.
census_enabled="yes"
census_host_port="$(printf '%s' "${STUDIO_A_HOST:-http://localhost:5173}" | sed -E 's#.*:([0-9]+).*#\1#')"
expected_port="$(grep -E '^VITE_HOST_PORT=' "${ROOT}/.env" 2>/dev/null | tail -1 | cut -d= -f2)"
expected_port="${expected_port:-5173}"
if [ "$census_host_port" != "$expected_port" ]; then
    warn "corpus invariant SKIPPED: suite targets port ${census_host_port} but this checkout's compose stack is on ${expected_port}."
    warn "  The census would read a different database than the tests drive."
    census_enabled="no"
fi
if [ "$census_enabled" = "yes" ]; then
    # shellcheck disable=SC2046
    export $(grep -E '^(POSTGRES_DB|POSTGRES_USER)=' "${ROOT}/.env" 2>/dev/null | tail -2 | xargs) 2>/dev/null || true
    corpus_before="$(corpus_census)"
    if [ -z "$corpus_before" ]; then
        warn "corpus invariant SKIPPED: could not read the database (is the stack up?)"
        census_enabled="no"
    else
        step "Corpus census (before): assets|posts|collections|fields|users = ${corpus_before}"
    fi
fi

# --- 2. run tests --------------------------------------------------------

if [ "$mode" = "all" ]; then
    step "Running Playwright UI suite (both projects)"
else
    step "Running Playwright UI suite (mode: $mode)"
fi
mkdir -p .pw-results

# Where the cross-file instance-config lock records each hold (#1248).
# Truncated per run so the audit below describes THIS run and not a
# fortnight of them.
export AA_LOCK_AUDIT="${UI_DIR}/.pw-results/instance-locks.jsonl"
rm -f "$AA_LOCK_AUDIT"

pw_args=()
if [ "$mode" != "all" ]; then
    pw_args+=(--project "$mode")
fi
if [ "${#passthrough[@]}" -gt 0 ]; then
    pw_args+=("${passthrough[@]}")
fi
npx playwright test "${pw_args[@]}"
rc=$?

# --- 3. summary ----------------------------------------------------------

if [ -f .pw-results/report.json ]; then
    step "Summary"
    node -e '
const r = require("./.pw-results/report.json");
const stats = r.stats ?? {};
const total = (stats.expected ?? 0) + (stats.unexpected ?? 0) + (stats.skipped ?? 0) + (stats.flaky ?? 0);
const pass  = stats.expected ?? 0;
const fail  = stats.unexpected ?? 0;
const skip  = stats.skipped ?? 0;
const flaky = stats.flaky ?? 0;
const ms    = stats.duration ?? 0;
const fmt = (n) => String(n).padStart(3);
const c = (s, n) => `[${s}m${n}[0m`;
console.log(`  Total: ${total}   ${c("1;32", "PASS:")} ${fmt(pass)}   ${c("1;31", "FAIL:")} ${fmt(fail)}   ${c("1;33", "SKIP:")} ${fmt(skip)}   FLAKY: ${flaky}   wall: ${Math.round(ms)}ms`);
'
    printf '\nHTML report: %s/.pw-report/index.html\n' "$UI_DIR"
fi

# --- 3b. shared-instance-config locks ------------------------------------
#
# Four specs write `system.public_mode` and each one reads the prior
# value, sets what it needs and puts the prior back — a contract that is
# correct for one writer and a lost update for two (#1248). The lock in
# helpers/instance-lock.ts makes those windows mutually exclusive ACROSS
# FILES, which the `mode: 'serial'` they all already declare does not.
#
# "They cannot interleave" is a claim about a race, and a race that is
# merely unlikely passes most runs — so it is checked from the run's own
# audit rather than asserted. Exit 4 is distinct from the ratchet's 3 and
# from Playwright's own, so the failure kinds are told apart by status.
if [ -f "$AA_LOCK_AUDIT" ]; then
    node "${UI_DIR}/instance-lock-audit.mjs" "$AA_LOCK_AUDIT"
    audit_rc=$?
    if [ "$audit_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then rc=$audit_rc; fi
fi

# --- 4. corpus census, after --------------------------------------------

if [ "$census_enabled" = "yes" ]; then
    corpus_after="$(corpus_census)"
    step "Corpus census (after):  assets|posts|collections|fields|users = ${corpus_after}"
    if [ -z "$corpus_after" ]; then
        warn "corpus invariant INCONCLUSIVE: the database was unreadable after the run."
    elif [ "$corpus_after" != "$corpus_before" ]; then
        # The RATCHET (scripts/dogfood/corpus-budget.txt). Roughly 25
        # specs still leave rows behind, so failing on any drift would
        # make this suite permanently red — and a guard that is always
        # red is one nobody reads. The budget records the leak that
        # already exists; only a BIGGER leak fails the run.
        budget_file="${ROOT}/scripts/dogfood/corpus-budget.txt"
        printf '\n\033[1;33mCORPUS DRIFT\033[0m — this run did not fully clean up after itself.\n'
        printf '  %-18s %10s %10s %10s %10s\n' "TABLE" "BEFORE" "AFTER" "DELTA" "BUDGET"
        IFS='|' read -r b1 b2 b3 b4 b5 <<< "$corpus_before"
        IFS='|' read -r a1 a2 a3 a4 a5 <<< "$corpus_after"
        over=0
        i=1
        for t in assets posts collections field_definition user; do
            eval "b=\$b$i; a=\$a$i"
            delta=$((a - b))
            allowed="$(grep -E "^${t} " "$budget_file" 2>/dev/null | awk '{print $2}')"
            allowed="${allowed:-0}"
            flag=""
            if [ "$delta" -gt "$allowed" ]; then flag="  <== OVER BUDGET"; over=1; fi
            printf '  %-18s %10s %10s %+10d %10s%s\n' "$t" "$b" "$a" "$delta" "$allowed" "$flag"
            i=$((i + 1))
        done
        if [ "$over" -eq 1 ]; then
            printf '\n\033[1;31mA NEW LEAK\033[0m — a table drifted further than %s allows.\n' "$budget_file"
            printf 'Find the spec that stopped cleaning up. Do NOT raise the budget to make this\n'
            printf 'pass: the ratchet only turns down. Do not reseed, and do not delete the\n'
            printf 'difference by hand — `aa sweep-fixtures` (dry run by default) clears the backlog.\n'
            # Distinct from Playwright's own failure code so a leak is not
            # mistaken for a failing assertion.
            [ "$rc" -eq 0 ] && rc=3
        else
            printf '\nWithin the recorded budget — no NEW leak. The drift itself is still debt:\n'
            printf 'the target in %s is all zeroes.\n' "$budget_file"
        fi
    else
        printf '\n\033[1;32mCorpus invariant holds\033[0m — the run left the database as it found it.\n'
    fi
fi

exit $rc
