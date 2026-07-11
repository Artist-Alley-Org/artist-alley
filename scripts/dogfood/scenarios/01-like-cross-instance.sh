#!/usr/bin/env bash
# scripts/dogfood/scenarios/01-like-cross-instance.sh
#
# Scenario 01 (ADR 0049 §Scenarios): Basic pairing + Like.
#
# What this script ships TODAY (1.22.I-a):
#   Phase A — pairing health + instance endpoint round-trip.
#   Confirms the wire is alive in both directions, both peers
#   appear in each other's federation_peers, and the public
#   instance endpoint returns the right shape on both ends.
#
# What needs aa:Share + post-mirror wiring (1.22.C-tail):
#   Phase B — the actual cross-instance Like.
#   For alice on studio-a to Like bob's studio-b post she
#   needs a LOCAL referent — a mirrored post stub on studio-a
#   carrying origin_server_id pointing at studio-b. The
#   mirror is created by accepting an aa:Share from studio-b,
#   which lives in scenario 02. The like dispatcher then
#   detects the mirror's origin_server_id and emits the
#   aa:Like outbox activity per the federation outbox path
#   shipped in 1.22.D.
#
# The dogfood week will exercise Phase B by running scenario
# 02 (share) first then a like-by-mirror follow-up. Keeping
# this script Phase-A-only avoids baking the share dependency
# into the wire test.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

# --- Phase A: pairing + instance round-trip --------------------------------

step "Logging in as admin on both stacks"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"
login_admin "$B_HOST" "$B_COOKIES"; pass "studio-b admin in"

step "Fetching /federation/instance on both stacks"
a_inst=$(api_get "$A_HOST" "$A_COOKIES" "/federation/instance")
b_inst=$(api_get "$B_HOST" "$B_COOKIES" "/federation/instance")
a_fp=$(echo "$a_inst"  | python3 -c 'import sys, json; print(json.load(sys.stdin)["fingerprint"])')
b_fp=$(echo "$b_inst"  | python3 -c 'import sys, json; print(json.load(sys.stdin)["fingerprint"])')
[ -n "$a_fp" ] || fail "studio-a instance endpoint missing fingerprint"
[ -n "$b_fp" ] || fail "studio-b instance endpoint missing fingerprint"
pass "studio-a fingerprint ${a_fp:0:16}…"
pass "studio-b fingerprint ${b_fp:0:16}…"

step "Verifying each peer appears in the other's federation_peers"
a_sees_b=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM federation_peers
     WHERE instance_url = 'https://studio-b.local:9443' AND enabled = TRUE")
b_sees_a=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM federation_peers
     WHERE instance_url = 'http://nginx' AND enabled = TRUE")
[ "$a_sees_b" -ge 1 ] || fail "studio-a doesn't have studio-b as a peer"
[ "$b_sees_a" -ge 1 ] || fail "studio-b doesn't have studio-a as a peer"
pass "studio-a sees studio-b as peer (enabled)"
pass "studio-b sees studio-a as peer (enabled)"

# --- Phase A done ----------------------------------------------------------

printf '\n%sScenario 01 Phase A PASS%s — pairing + instance endpoints healthy.\n' \
    "$GREEN" "$RESET"
printf '\n%sPhase B (cross-instance Like) requires:%s\n' "$YELLOW" "$RESET"
info "  - studio-b shares bob's post with studio-a via aa:Share (scenario 02)"
info "  - studio-a receives the share + creates a local mirror with origin_server_id"
info "  - alice Likes the mirror via POST /posts/{id}/like"
info "  - studio-a's like dispatcher emits aa:Like to studio-b's inbox"
info ""
info "Scaffold scenario 02 first; the like-by-mirror follow-up is a 5-line"
info "addition once a mirror exists."
