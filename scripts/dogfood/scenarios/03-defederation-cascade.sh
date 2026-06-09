#!/usr/bin/env bash
# scripts/dogfood/scenarios/03-defederation-cascade.sh
#
# Scenario 03 (ADR 0049 §Scenarios): Defederation cascade.
#
#   studio-a defederates studio-b after Scenarios 01/02 have
#   active state. Observe aa:Unshare emissions, outbox
#   cancel-by-peer, peer marked disabled, inbox refuses
#   subsequent activity from studio-b, audit log shows cascade
#   event.
#
# Validates:
#   The destructive path that most likely surfaces ordering bugs.
#
# Status: OUTLINE — run after 01 + 02 are confirmed working so
# there's real state to cascade through.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

warn "Scenario 03 is an outline."

step "Plan:"
info "  Prereq: 01 + 02 have run, leaving active Likes + Comments + Shares."
info ""
info "  1. As studio-a admin, POST /admin/federation/peers/{id}/disable"
info "     (or DELETE — pick the cleaner one with audit support)."
info ""
info "  2. Assert immediately:"
info "     - peer row 'enabled' flag = FALSE on studio-a."
info "     - federation_outbox cancel-by-peer: pending rows for this peer"
info "       transition to status='cancelled' within sub-1s."
info "     - aa:Unshare activities emitted for every active share that"
info "       targeted this peer (audit + outbox both reflect this)."
info ""
info "  3. As studio-b, attempt to POST /federation/inbox with a fresh"
info "     Like or Comment for studio-a."
info "     - studio-a's inbox MUST refuse with 403 + audit event."
info ""
info "  4. As alice@studio-a, POST a new Like targeting a studio-b post."
info "     - The outbox should NOT enqueue a delivery to studio-b."
info "     - The Like still lands locally on studio-a (we're not censoring"
info "       intent; we just stopped sharing)."
info ""
info "  5. Verify audit log on both stacks shows the cascade:"
info "     - federation.peer.disabled"
info "     - federation.outbox.cancel-by-peer with count"
info "     - federation.share.unshared with peer + count"

exit 2
