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

# --- 2. run tests --------------------------------------------------------

if [ "$mode" = "all" ]; then
    step "Running Playwright UI suite (both projects)"
else
    step "Running Playwright UI suite (mode: $mode)"
fi
mkdir -p .pw-results
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

exit $rc
