#!/usr/bin/env bash
# scripts/dogfood/run-ui.sh — UI dogfood runner (Playwright).
#
# Invokes the Playwright suite under scripts/dogfood/ui/ and prints
# a summary the same shape as run-all.sh produces for the shell
# scenarios.
#
# First run installs Playwright + Chromium into
# scripts/dogfood/ui/node_modules/. Subsequent runs reuse the
# install — ~1s startup vs ~60s.
#
# Usage:
#   ./scripts/dogfood/run-ui.sh                  run all UI tests
#   ./scripts/dogfood/run-ui.sh --project studio-a
#   ./scripts/dogfood/run-ui.sh --grep federation

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UI_DIR="${ROOT}/scripts/dogfood/ui"
cd "$UI_DIR"

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

step "Running Playwright UI suite"
mkdir -p .pw-results
npx playwright test "$@"
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
