#!/usr/bin/env bash
# scripts/dogfood/run-all.sh — automated dogfood runner.
#
# Walks the canonical scenario catalogue, runs each, and prints
# a summary table + writes a machine-readable JSON report.
#
# Behavior per scenario:
#   exit 0   → PASS
#   exit 2   → SKIPPED (scenario is an outline / gated on future phase)
#   anything else → FAIL
#
# Usage:
#   ./scripts/dogfood/run-all.sh                   run all scenarios
#   ./scripts/dogfood/run-all.sh --quick           skip slow scenarios
#   ./scripts/dogfood/run-all.sh 01 06             run a specific subset
#   ./scripts/dogfood/run-all.sh --report path.json   write JSON report
#
# Defaults to dropping per-run state under scripts/dogfood/
# .dogfood-runs/<timestamp>/.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCENARIOS_DIR="${ROOT}/dogfood/scenarios"

# --- arg parsing -----------------------------------------------------------

quick=0
report_path=""
filter=()
while [ $# -gt 0 ]; do
    case "$1" in
        --quick)  quick=1; shift ;;
        --report) report_path="$2"; shift 2 ;;
        -h|--help)
            sed -n '2,18p' "$0"
            exit 0
            ;;
        *) filter+=("$1"); shift ;;
    esac
done

ts="$(date -u +%Y%m%d-%H%M%S)"
run_dir="${ROOT}/dogfood/.dogfood-runs/${ts}"
mkdir -p "$run_dir"
[ -z "$report_path" ] && report_path="${run_dir}/report.json"
log_dir="${run_dir}/logs"
mkdir -p "$log_dir"

# Per-scenario report fragments append here.
fragments_path="${run_dir}/fragments.jsonl"
: > "$fragments_path"
export DOGFOOD_REPORT_PATH="$fragments_path"

# --- discover scenarios ---------------------------------------------------

# Pull scenario scripts in numeric order. Each scenario file is
# named NN-name.sh; the leading digits drive ordering.
all_scenarios=()
for f in "${SCENARIOS_DIR}"/[0-9]*.sh; do
    name=$(basename "$f" .sh)
    all_scenarios+=("$name")
done

# Apply filter if user passed scenario IDs.
scenarios=()
if [ "${#filter[@]}" -gt 0 ]; then
    for f in "${filter[@]}"; do
        for s in "${all_scenarios[@]}"; do
            if [[ "$s" == "${f}-"* || "$s" == "$f" ]]; then
                scenarios+=("$s")
            fi
        done
    done
else
    scenarios=("${all_scenarios[@]}")
fi

# --- helpers --------------------------------------------------------------

CYAN=$'\033[1;36m'
GREEN=$'\033[1;32m'
RED=$'\033[1;31m'
YELLOW=$'\033[1;33m'
DIM=$'\033[2m'
RESET=$'\033[0m'

# --- run loop -------------------------------------------------------------

total_pass=0
total_fail=0
total_skip=0
total_ms=0
declare -a row_summary

printf '\n%sArtist Alley dogfood runner%s\n' "$CYAN" "$RESET"
printf '%s%s%s scenario(s) selected. Logs: %s\n\n' "$DIM" "${#scenarios[@]}" "$RESET" "$log_dir"

for name in "${scenarios[@]}"; do
    log_file="${log_dir}/${name}.log"
    start_ms=$(date +%s%3N)

    printf '%s──▶%s %-40s ' "$CYAN" "$RESET" "$name"

    bash "${SCENARIOS_DIR}/${name}.sh" >"$log_file" 2>&1
    rc=$?
    elapsed_ms=$(( $(date +%s%3N) - start_ms ))
    total_ms=$(( total_ms + elapsed_ms ))

    case "$rc" in
        0)
            printf '%sPASS%s   %sms\n' "$GREEN" "$RESET" "$elapsed_ms"
            total_pass=$(( total_pass + 1 ))
            row_summary+=("$name|PASS|$elapsed_ms|$log_file")
            ;;
        2)
            printf '%sSKIP%s   (outline; needs product code)\n' "$YELLOW" "$RESET"
            total_skip=$(( total_skip + 1 ))
            row_summary+=("$name|SKIP|$elapsed_ms|$log_file")
            ;;
        *)
            printf '%sFAIL%s   rc=%s   log: %s\n' "$RED" "$RESET" "$rc" "$log_file"
            total_fail=$(( total_fail + 1 ))
            row_summary+=("$name|FAIL|$elapsed_ms|$log_file")
            # Surface the last 5 lines of the failure for context.
            tail -5 "$log_file" | sed "s/^/   ${DIM}│${RESET} /"
            ;;
    esac
done

# --- summary --------------------------------------------------------------

printf '\n%s%s%s\n' "$CYAN" "$(printf '%.0s─' $(seq 1 76))" "$RESET"
printf 'Total: %d   %sPASS:%s %d   %sFAIL:%s %d   %sSKIP:%s %d   wall: %dms\n' \
    "${#scenarios[@]}" \
    "$GREEN" "$RESET" "$total_pass" \
    "$RED" "$RESET" "$total_fail" \
    "$YELLOW" "$RESET" "$total_skip" \
    "$total_ms"

# --- JSON report ----------------------------------------------------------

python3 - <<PY > "$report_path"
import json, os, time

fragments = []
path = "${fragments_path}"
if os.path.exists(path):
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                fragments.append(json.loads(line))
            except Exception:
                fragments.append({"scenario": "unknown", "pass": False, "parse_error": line[:200]})

summary = {
    "run_id": "${ts}",
    "timestamp_unix": int(time.time()),
    "totals": {
        "scenarios": ${#scenarios[@]},
        "pass":      ${total_pass},
        "fail":      ${total_fail},
        "skip":      ${total_skip},
        "wall_ms":   ${total_ms},
    },
    "scenarios": [],
    "scenario_reports": fragments,
}

raw_rows = """$(printf '%s\n' "${row_summary[@]}")"""
for row in raw_rows.strip().splitlines():
    name, status, ms, log = row.split('|', 3)
    summary["scenarios"].append({
        "name": name,
        "status": status,
        "elapsed_ms": int(ms),
        "log_path": log,
    })

print(json.dumps(summary, indent=2))
PY

printf '\nJSON report: %s\n' "$report_path"

if [ "$total_fail" -gt 0 ]; then
    exit 1
fi
