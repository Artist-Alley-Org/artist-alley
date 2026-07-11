#!/usr/bin/env bash
# scripts/dogfood/scenarios/02-share-collection-comment.sh
#
# Scenario 02 (ADR 0049 §Scenarios): Share collection + cross-instance comment.
#
#   studio-a's admin shares a public-asset collection with
#   studio-b's bob; bob comments on a shared asset; alice
#   sees the comment.
#
# Validates:
#   aa:Share + access gate + Create(Note) handler + reply
#   resolution.
#
# Status: OUTLINE — fill in once Scenario 01 confirms the wire
# is healthy. The step sequence below is correct; the API
# bodies need to be matched to the current /admin/federation/
# shares + /comments handlers (which may have evolved since
# this was sketched).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

warn "Scenario 02 is an outline. Run scenario 01 first to confirm
       the wire is healthy, then implement the steps below."

step "Plan:"
info "  1. Login admin on both stacks."
info "  2. Create alice@studio-a, bob@studio-b fixtures."
info "  3. Create a collection on studio-a with 2-3 public assets."
info "  4. POST /admin/federation/shares  { collection_id, peer_id, scope: 'view+comment' }"
info "     → emits aa:Share to studio-b."
info "  5. Poll studio-b for the shared collection to appear in bob's view."
info "  6. As bob, POST /comments on one of the shared assets."
info "     → studio-b emits Create(Note) back to studio-a."
info "  7. Poll studio-a as alice for the comment to appear on the asset."
info "  8. Assert: alice sees the comment with peer attribution."
info "  9. (Optional) verify federation_inbox + federation_outbox rows on both ends."

exit 2
