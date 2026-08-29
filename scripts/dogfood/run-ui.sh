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
#
# Exit codes (each failure kind gets its own, so a wrapper can tell them
# apart by status alone):
#   0  every selected test passed
#   1  a test failed, or a prerequisite is missing (fail())
#   2  unknown mode
#   3  the corpus ratchet — a table leaked more than corpus-budget.txt allows
#   4  the instance-lock audit — two files held shared config at once
#   5  NO TESTS RAN — the selection matched nothing (#1272)
#   6  the denominator audit: a test that was declared did not run, or
#      skipped itself without being in skip-manifest.txt (#1348, #1344)
#   7  the ledger/census reconciliation — the two instruments below
#      disagree about how many rows the run left behind (#1351)

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
        sed -n '2,40p' "$SCRIPT_PATH"
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

# --- 1a. the accounting checks itself first (#1351) ----------------------
#
# ⛔ AN INSTRUMENT THAT HAS NEVER BEEN TESTED IS NOT EVIDENCE. #1351 was
# a bug in the reporter, not in the suite: `created - deleted` counted
# API CALLS and was read as if it counted ROWS, so a deduplicating create
# and an idempotent delete both made it disagree with the census — and
# for weeks the disagreement was printed and nobody's tooling compared
# the two. These cases pin every shape that was found, and they fail
# against the pre-#1351 arithmetic.
#
# Run here rather than only in a workflow, so it holds wherever the suite
# is driven from, and BEFORE the census so the accounting is trusted
# before it is used. It is sub-second and needs no stack.
step "Fixture-accounting self-test (#1351)"
if ! node --test "${UI_DIR}/fixture-ledger.test.mjs" > /tmp/aa-ledger-selftest.$$ 2>&1; then
    cat /tmp/aa-ledger-selftest.$$
    rm -f /tmp/aa-ledger-selftest.$$
    fail "the fixture ledger's own accounting is broken — its report and the corpus census cannot be trusted to mean what they say."
fi
grep -E '^# (tests|pass|fail)' /tmp/aa-ledger-selftest.$$ | sed 's/^/  /'
rm -f /tmp/aa-ledger-selftest.$$

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

# ⛔ IT COUNTS LIVE ROWS, AND THE FIRST VERSION DID NOT — which made the
# invariant unsatisfiable and mis-reported every well-behaved spec as a
# leak (#1247).
#
# `DELETE` is a SOFT delete on all three of assets, posts and collections
# (`SET deleted_at = NOW()` — assets/queries.sql:93, posts:56,
# collections:138) and an ARCHIVE on field_definition (`SET status =
# 'archived'` — metadata/queries.sql:141, whose own comment says "we
# never DELETE field defs in normal flow"). `user` has no delete at all.
#
# So `count(*)` can never come back down through any API a spec can call.
# A spec that created three assets and deleted all three still drifted
# +3, no teardown could have fixed it, and the ratchet's stated target of
# all zeroes was unreachable by construction. "Roughly 25 specs leak" was
# partly measuring the instrument.
#
# LIVE rows are also what the invariant is FOR. The harm #1245 records —
# the newest-200 window holding no raster image, the bootstrap admin off
# page 1 of /admin/users — is caused by rows the app can still see. Every
# index that feeds those surfaces is partial on `deleted_at IS NULL`. A
# soft-deleted row is invisible to browse, to search and to every admin
# list; it is disk, and `aa sweep-fixtures` is what reclaims disk.
#
# The raw totals are still reported, as a second line and ungated: they
# are the sweep's backlog, and losing sight of them would be a different
# mistake.
# ⛔ COMPOSE IS RUN FROM THE REPO ROOT, NOT FROM HERE (#1263).
#
# This script `cd`s into the UI dir at the top, and CI sets COMPOSE_FILE
# to paths RELATIVE to the repo root
# (`docker-compose.yml:infra/docker/ci/…`, ui-pr.yml + ui-nightly.yml
# job env). Compose resolves those against the current directory, so from
# here every call died with `stat …/scripts/dogfood/ui/docker-compose.yml:
# no such file or directory` — verified by reproducing it locally. On a
# workstation it happened to work anyway, because with COMPOSE_FILE unset
# compose walks up to find the file; that is the whole reason this never
# showed up outside CI.
#
# Subshell rather than a `cd` here: the Playwright invocation below has to
# stay in the UI dir.
compose() { ( cd "$ROOT" && docker compose "$@" ); }

psql_tuple() {
    compose exec -T postgres psql -U "${POSTGRES_USER:-artist_alley}" \
        -d "${POSTGRES_DB:-artist_alley}" -tA "$@"
}

corpus_census() {
    psql_tuple -F'|' -c "
        SELECT (SELECT count(*) FROM assets WHERE deleted_at IS NULL)
            || '|' || (SELECT count(*) FROM posts WHERE deleted_at IS NULL)
            || '|' || (SELECT count(*) FROM collections WHERE deleted_at IS NULL)
            || '|' || (SELECT count(*) FROM field_definition WHERE status <> 'archived')
            || '|' || (SELECT count(*) FROM \"user\");" 2>/dev/null | tr -d '[:space:]'
}

# The same five tables counted RAW — soft-deleted and archived rows
# included. Reported, never gated: this is the sweep's backlog, not the
# suite's tidiness.
corpus_total() {
    psql_tuple -F'|' -c "
        SELECT (SELECT count(*) FROM assets)
            || '|' || (SELECT count(*) FROM posts)
            || '|' || (SELECT count(*) FROM collections)
            || '|' || (SELECT count(*) FROM field_definition)
            || '|' || (SELECT count(*) FROM \"user\");" 2>/dev/null | tr -d '[:space:]'
}

# ── DOES THE CENSUS READ THE DATABASE THE TESTS DRIVE? (#1263) ───────
#
# The census reads the LOCAL compose stack directly. If the suite is
# pointed somewhere else, the two describe different databases and a
# clean diff means nothing — so they are checked to agree, and the run
# skips loudly rather than reporting a comparison that is not one.
#
# ⛔ THE PORT WAS THE WRONG PROXY FOR THAT QUESTION, and it answered NO
# on every CI run this guard has ever seen. It compared the port in
# STUDIO_A_HOST against VITE_HOST_PORT from the checkout's `.env`; CI
# drives `http://app.aa:8080` (ui-pr.yml, ui-nightly.yml) against a
# default of 5173, so `census_enabled` was `no` in the pipeline from the
# day it was written. The stacks were the SAME one all along — reached
# over the compose network by alias, because CI publishes no host ports
# at all. The ratchet guarded one workstation and the all-zeroes budget
# made CI's silence look like agreement.
#
# So ask the question directly instead of by proxy: every instance holds
# an Ed25519 keypair generated at boot (federation.instance_identity in
# system_config), and serves its PUBLIC half unauthenticated at
# /api/v1/federation/instance. Fetch it from the host the suite drives,
# read it out of the database the census reads, and compare. Same key,
# same instance, same database — whatever hostname or port either side
# was reached by.
#
# `node` rather than `curl` for the fetch: node is already a hard
# dependency checked at the top of this script, and the answer needs JSON
# parsing either way.
census_enabled="yes"
census_skip_reason=""
census_host="${STUDIO_A_HOST:-http://localhost:5173}"

census_skip() {
    census_enabled="no"
    census_skip_reason="$1"
    warn "corpus invariant SKIPPED: $1"
    warn "  A SKIPPED census is not a passed one — the ratchet checked nothing on this run."
    # An annotation, so a skip surfaces in the run summary rather than
    # only in 20 minutes of scrollback. This is the failure mode #1263
    # was: nobody reads a warning nobody is looking for.
    if [ -n "${CI:-}" ]; then
        printf '::warning title=corpus census skipped::%s\n' "$1"
    fi
}

# The instance the SUITE drives, by its published public key. Whitespace
# stripped on both sides because one arrives as JSON-escaped PEM and the
# other as a psql tuple with real newlines in it.
census_target_identity() {
    node -e '
      const base = process.argv[1].replace(/\/+$/, "");
      fetch(base + "/api/v1/federation/instance", { signal: AbortSignal.timeout(15000) })
        .then((r) => (r.ok ? r.json() : null))
        .then((j) => process.stdout.write(String((j && j.public_key_pem) || "").replace(/\s+/g, "")))
        .catch(() => {});
    ' "$1" 2>/dev/null
}

# The instance the CENSUS reads, out of its own database.
census_local_identity() {
    psql_tuple -c \
        "SELECT value->>'public_key_pem' FROM system_config WHERE key = 'federation.instance_identity';" \
        2>/dev/null | tr -d '[:space:]'
}

# Needed by every psql call below, including the identity read, so it is
# exported before the guard rather than inside it.
# shellcheck disable=SC2046
export $(grep -E '^(POSTGRES_DB|POSTGRES_USER)=' "${ROOT}/.env" 2>/dev/null | tail -2 | xargs) 2>/dev/null || true

census_local_key="$(census_local_identity)"
census_target_key="$(census_target_identity "$census_host")"

if [ -z "$census_local_key" ]; then
    census_skip "could not read this checkout's compose database (is the stack up, and is 'postgres' its service name?)"
elif [ -z "$census_target_key" ]; then
    census_skip "${census_host} did not answer GET /api/v1/federation/instance, so there is no way to tell whether it is this stack."
elif [ "$census_local_key" != "$census_target_key" ]; then
    census_skip "${census_host} is a DIFFERENT instance from this checkout's compose stack (their federation identities differ), so the census would count a database the tests never touched."
fi

if [ "$census_enabled" = "yes" ]; then
    corpus_before="$(corpus_census)"
    total_before="$(corpus_total)"
    if [ -z "$corpus_before" ]; then
        census_skip "the database went unreadable between the identity check and the census."
    else
        step "Corpus census (before): assets|posts|collections|fields|users = ${corpus_before}   (raw incl. soft-deleted: ${total_before})"
    fi
fi


# --- 2. run tests --------------------------------------------------------

if [ "$mode" = "all" ]; then
    step "Running Playwright UI suite (both projects)"
else
    step "Running Playwright UI suite (mode: $mode)"
fi
mkdir -p .pw-results

# ⛔ THE RUN'S OWN EVIDENCE, AND ONLY THIS RUN'S (#1272). The summary and
# the zero-test guard below both read .pw-results/report.json, and an
# absent one is a load-bearing signal — "Playwright wrote no report" is
# how a run that never started is told from one that finished. Playwright
# cleans its outputDir per run, but that is its promise and not ours; a
# leftover report from the last invocation would be summarised as this
# one's, so it is removed here rather than trusted to be gone.
rm -f .pw-results/report.json

# Where the cross-file instance-config lock records each hold (#1248).
# Truncated per run so the audit below describes THIS run and not a
# fortnight of them.
export AA_LOCK_AUDIT="${UI_DIR}/.pw-artifacts/instance-locks.jsonl"
rm -f "$AA_LOCK_AUDIT"

# Where helpers/fixture-ledger.ts records who created and who deleted
# each row (#1247). Truncated per run for the same reason.
export AA_FIXTURE_LEDGER="${UI_DIR}/.pw-artifacts/fixture-ledger.jsonl"
rm -f "$AA_FIXTURE_LEDGER"

pw_args=()
if [ "$mode" != "all" ]; then
    pw_args+=(--project "$mode")
fi
if [ "${#passthrough[@]}" -gt 0 ]; then
    pw_args+=("${passthrough[@]}")
fi
npx playwright test "${pw_args[@]}"
rc=$?

# --- 2b. did it actually RUN anything? (#1272) ---------------------------
#
# A run that executed nothing must not be reported like a run that passed
# everything. A typo'd --grep, a bad --project, or a renamed spec file all
# select zero tests, and every downstream reader — the summary below, the
# corpus invariant below that, a human scrolling the log — describes the
# result in the language of success: `PASS: 0  FAIL: 0`, then `Corpus
# invariant holds`. Nothing in that output says the suite never started.
#
# ⚠️ MEASURED HERE, NOT ASSUMED. #1272 reports the script exiting 0 on
# `Error: No tests found`; on @playwright/test 1.62.1 it exits 1, because
# Playwright itself started treating an empty selection as an error in
# 1.44 (that is what `--pass-with-no-tests` opts out of). So the hole this
# closes is not the exit code alone — it is that "ran nothing" and
# "assertions failed" arrive as the SAME status 1, from a log that reads
# green either way. Both halves of #1272's acceptance need a code of its
# own, and 1 through 4 are taken (fail, unknown mode, corpus ratchet,
# instance-lock audit).
#
# The count comes from the run's own report rather than from grepping
# Playwright's stderr for a message string, which changes between minor
# versions — and the report was deleted before the run, so a missing one
# means THIS run produced none.
NO_TESTS_RC=5
ran="unknown"
if [ -f .pw-results/report.json ]; then
    ran="$(node -e '
      try {
        const s = require("./.pw-results/report.json").stats ?? {};
        const n = (s.expected ?? 0) + (s.unexpected ?? 0) + (s.skipped ?? 0) + (s.flaky ?? 0);
        process.stdout.write(String(n));
      } catch { process.stdout.write("unknown"); }
    ' 2>/dev/null)"
    ran="${ran:-unknown}"
fi

no_tests="no"
if [ "$ran" = "0" ]; then
    no_tests="yes"
elif [ "$ran" = "unknown" ] && [ "$rc" -eq 0 ]; then
    # Exit 0 with no report at all is a green backed by no evidence
    # whatsoever, which is the same claim this guard refuses. A non-zero
    # rc with no report is Playwright failing loudly on its own terms —
    # keep its code and its message.
    no_tests="yes"
fi

if [ "$no_tests" = "yes" ]; then
    printf '\n\033[1;31mNO TESTS RAN\033[0m — this run executed nothing.\n'
    printf '  mode      : %s\n' "$mode"
    if [ "${#passthrough[@]}" -gt 0 ]; then
        printf '  passed on : %s\n' "${passthrough[*]}"
    fi
    printf '  reported  : %s test(s) in .pw-results/report.json\n' "$ran"
    printf '\nA selection that matches nothing is a typo, not a pass. Check the --grep\n'
    printf 'pattern, the --project name, and that the spec files it names still exist.\n'
    printf 'Exit %d is distinct from a failing assertion (Playwright'"'"'s own 1), the\n' "$NO_TESTS_RC"
    printf 'corpus ratchet (3) and the instance-lock audit (4), so a wrapper can tell\n'
    printf '"ran nothing" from "ran and failed".\n'
    rc=$NO_TESTS_RC
fi

# --- 3. summary + the denominator audit ----------------------------------
#
# STATS.SKIPPED IS TWO DIFFERENT EVENTS ADDED TOGETHER, and printing it
# as one number is the whole of #1348. Playwright's own reporter keeps
# them apart; this summary did not. Attempt 1 of run 33198346487:
#
#     2 failed
#     3 skipped        <- tests that ran a `test.skip()` guard
#     6 did not run    <- tests a failure in their serial block prevented
#     386 passed
#
# `stats.skipped` is 3 + 6 = 9, which is the figure #1348 was filed on as
# "2 became 9". Two of the nine were the ordinary environment-dependent
# skips the quiet run also had; ONE was a guard reading `.count()` before
# the page had drawn (fixed in kind-filter-1166); and SIX were tests that
# never executed at all, which is not a skip in any sense a reader would
# recognise. Folding them together turned "six questions went unasked"
# into a word that sounds deliberate.
#
# So the summary line is produced by skip-audit.mjs rather than here. It
# classifies exactly the way Playwright's own `generateSummary()` does,
# and one classifier means the header line and the audit under it cannot
# disagree about what a word means.

# A manifest entry naming a test that is not in the report is STALE, and
# stale is a failure, but only when this run selected the whole suite.
# Under `--grep` an absent test means the selection and not a deletion,
# and failing there would make the flag unusable.
audit_full=""
if [ "${#passthrough[@]}" -eq 0 ] && [ "$mode" != "federation" ]; then
    audit_full="--full"
fi

# Not when nothing ran: exit 5 already owns that verdict, and an audit of
# an empty report would report every manifest entry as deleted.
if [ "$no_tests" != "yes" ]; then
    step "Summary"
    node "${UI_DIR}/skip-audit.mjs" \
        .pw-results/report.json \
        "${ROOT}/scripts/dogfood/skip-manifest.txt" \
        $audit_full
    skip_rc=$?
    if [ "$skip_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then rc=$skip_rc; fi
fi

if [ -f .pw-results/report.json ]; then
    printf '\nHTML report: %s/.pw-report/index.html\n' "$UI_DIR"
fi

# --- 3b. shared-instance-config locks ------------------------------------
#
# Five specs write `system.public_mode` and each one reads the prior
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

# --- 3c. who left the rows -----------------------------------------------
#
# The census below reports drift per TABLE. This says which SPEC it
# belongs to (#1247) — recorded as each row was created rather than
# inferred from names afterwards, because a naming rule is safe for a
# report and is the exact rule that nearly deleted five real assets when
# it was used for deletion (fixturesweep/rules.go).
#
# ⛔ AND IT IS NOW COMPARED WITH THE CENSUS RATHER THAN PRINTED BESIDE IT
# (#1351). The two ran over the same rows and contradicted each other in
# the same output for weeks — `assets LEFT 1` here, "invariant holds"
# below — because nothing but a reader ever put the two numbers side by
# side. The summary written here is what step 4 reconciles against.
AA_LEDGER_SUMMARY="${UI_DIR}/.pw-artifacts/fixture-ledger-summary.json"
rm -f "$AA_LEDGER_SUMMARY"
node "${UI_DIR}/fixture-ledger-report.mjs" "$AA_FIXTURE_LEDGER" --json "$AA_LEDGER_SUMMARY"

# --- 4. corpus census, after --------------------------------------------

if [ "$census_enabled" = "yes" ]; then
    corpus_after="$(corpus_census)"
    total_after="$(corpus_total)"
    step "Corpus census (after):  assets|posts|collections|fields|users = ${corpus_after}   (raw incl. soft-deleted: ${total_after})"
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
        printf '  %-18s %10s %10s %10s %10s %10s\n' "TABLE" "BEFORE" "AFTER" "DELTA" "BUDGET" "RAW Δ"
        IFS='|' read -r b1 b2 b3 b4 b5 <<< "$corpus_before"
        IFS='|' read -r a1 a2 a3 a4 a5 <<< "$corpus_after"
        IFS='|' read -r r1 r2 r3 r4 r5 <<< "$total_before"
        IFS='|' read -r s1 s2 s3 s4 s5 <<< "$total_after"
        over=0
        i=1
        for t in assets posts collections field_definition user; do
            eval "b=\$b$i; a=\$a$i; rb=\$r$i; ra=\$s$i"
            delta=$((a - b))
            # The RAW delta counts soft-deleted and archived rows too, so
            # it is always >= DELTA and never comes down through the API.
            # Reported so the sweep's backlog stays visible; never gated.
            raw_delta=$((${ra:-0} - ${rb:-0}))
            allowed="$(grep -E "^${t} " "$budget_file" 2>/dev/null | awk '{print $2}')"
            allowed="${allowed:-0}"
            flag=""
            if [ "$delta" -gt "$allowed" ]; then flag="  <== OVER BUDGET"; over=1; fi
            printf '  %-18s %10s %10s %+10d %10s %+10d%s\n' "$t" "$b" "$a" "$delta" "$allowed" "$raw_delta" "$flag"
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
    elif [ "$no_tests" = "yes" ]; then
        # ⛔ NOT "the invariant holds". A run that executed nothing leaves
        # the database as it found it by construction, so the comparison
        # is true and means nothing — and printing the green line under a
        # NO TESTS RAN banner is exactly the mixed signal #1272 is about.
        printf '\n\033[1;33mCorpus unchanged\033[0m — but nothing ran, so this proves nothing.\n'
    else
        printf '\n\033[1;32mCorpus invariant holds\033[0m — the run left the database as it found it.\n'
        if [ "$total_after" != "$total_before" ]; then
            printf 'Raw row counts still moved (%s → %s): every spec cleaned up, and a soft\n' \
                "$total_before" "$total_after"
            printf 'delete leaves the row behind. `aa sweep-fixtures` reclaims those.\n'
        fi
    fi

    # --- 4b. do the two instruments agree? (#1351) -----------------------
    #
    # Only where the census actually ran and both tuples are readable —
    # a comparison against a census that was skipped is not a comparison.
    if [ -n "$corpus_before" ] && [ -n "$corpus_after" ]; then
        node "${UI_DIR}/fixture-ledger-reconcile.mjs" \
            "$AA_LEDGER_SUMMARY" "$corpus_before" "$corpus_after"
        reconcile_rc=$?
        if [ "$reconcile_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then rc=$reconcile_rc; fi
    fi
else
    # ⛔ SAID AGAIN, WHERE THE VERDICT IS (#1263). The skip is announced at
    # the top of the run too, but the top of the run is twenty minutes of
    # Playwright output above here — and "the ratchet is green" is read
    # off the end. A skip that only appears where nobody looks is how the
    # census came to be off in CI for its entire existence without anyone
    # noticing.
    printf '\n\033[1;33mCORPUS INVARIANT NOT CHECKED\033[0m — %s\n' "$census_skip_reason"
    printf 'This run says NOTHING about whether the suite cleaned up after itself.\n'
fi

exit $rc
