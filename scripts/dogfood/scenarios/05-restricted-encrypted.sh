#!/usr/bin/env bash
# scripts/dogfood/scenarios/05-restricted-encrypted.sh
#
# Scenario 05 (ADR 0049 §Scenarios): Restricted content end-to-end (post-1.22.I-i).
#
#   studio-a shares restricted content with studio-b's user;
#   observe encrypted envelope; b decrypts with X25519 private
#   key; restricted content renders correctly on b's UI;
#   rotation of b's keys at +1 day still allows in-flight
#   decryption via retained key.
#
# Validates:
#   Final 1.22.I acceptance test. Lands with 1.22.I-i; this
#   script is a hard stub until then.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

cat <<EOF >&2

${YELLOW}Scenario 05 is gated on Phase 1.22.I-i (X25519 + encrypted federation).${RESET}

Until 1.22.I-i ships:
  - Scenario 04 covers the restricted-share refusal path.
  - Restricted content stays local-only by design.

When 1.22.I-i ships, this script implements:
  1. Confirm both peers advertise the 'encryption.aa-nacl-box.v1'
     capability in their /federation/peers/{id} response.
  2. Create a restricted-tier asset on studio-a.
  3. Share it with studio-b's bob.
  4. Verify the outbox envelope is NaCl-box encrypted to bob's
     X25519 public key.
  5. Verify bob decrypts cleanly + the restricted asset renders.
  6. Rotate bob's X25519 keys (admin endpoint).
  7. Verify the in-flight envelope (in studio-a's outbox between
     emit and delivery) still decrypts via bob's retained
     prior key.

EOF

exit 2
