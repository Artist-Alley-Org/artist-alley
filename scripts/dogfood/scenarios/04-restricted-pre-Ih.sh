#!/usr/bin/env bash
# scripts/dogfood/scenarios/04-restricted-pre-Ih.sh
#
# Scenario 04 (ADR 0049 §Scenarios): Restricted content (pre-I-h).
#
#   studio-a tries to share restricted content with studio-b.
#   Expected before 1.22.I-h ships: unconditional sender-side
#   refusal; admin banner explains "encrypted federation requires
#   Phase 1.22.I."
#
# Validates:
#   The refusal path that protects restricted content while
#   encryption isn't shippable yet.
#
# After 1.22.I-h ships, this scenario flips to validate
# capability check, then succeeds if capability is advertised
# on both ends. See ADR 0049 §Track B for the flip plan.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

warn "Scenario 04 is an outline. Easiest of the four to land
       — the negative path is just an HTTP error code + audit row."

step "Plan:"
info "  1. Create a fixture asset on studio-a marked sensitivity_tier='restricted'."
info "  2. As studio-a admin, attempt POST /admin/federation/shares with that asset."
info "     - Expected: 400 + body containing the 'restricted' refusal reason."
info "     - Expected: NO outbox row enqueued."
info "  3. Verify the admin UI's RestrictedShareBanner.svelte trigger fired"
info "     by checking for the audit event:"
info "       SELECT * FROM audit_log WHERE event = 'federation.share.refused.restricted'"
info "  4. (Optional) read the response body and assert it contains the"
info "     human-readable banner copy that says 1.22.I-i is required."

exit 2
