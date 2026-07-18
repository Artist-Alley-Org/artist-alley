// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-g — sender-refusal policy.
//
// Pure decision function. Given a share's sensitivity tier + the
// recipient peer's encryption capability + whether the recipient's
// public key is available, decide which of three emission paths
// the delivery Worker should take:
//
//   - EmissionEncrypted   — encrypt + dispatch (the I-e path).
//   - EmissionPlaintext   — plaintext + dispatch (the legacy 1.22.D
//                            path, kept for backwards compat against
//                            pre-I-f peers that haven't re-paired).
//   - EmissionRefused     — do NOT dispatch; mark the row refused +
//                            audit; the activity reaches no peer
//                            until policy or peer state changes.
//
// # Why this is a separate package-level pure function
//
// Three reasons:
//
//  1. **Testability.** A pure function over (Sensitivity, bool, bool)
//     has 16 input combinations; a unit-test sweep covers the entire
//     decision matrix in milliseconds without standing up a Worker.
//
//  2. **Auditability.** The decision matrix in
//     [docs/protocol/archivepub.md] §3.6 is the wire-side contract;
//     ChoosePathFor is the code-side mirror. Both can be read +
//     reviewed side by side because the table layout matches the
//     switch shape exactly.
//
//  3. **Single source of truth.** The Worker calls this function;
//     future surfaces (admin "would-this-be-refused?" preflight
//     checks, scenario dogfood probes) call it too. Keeping the
//     decision in one function means there's exactly one place to
//     update if the matrix changes.
//
// # Why refusal is per-recipient, not per-envelope
//
// One activity may fan out to multiple recipient peers; each gets
// its own federation_outbox row. Capability state varies per peer
// (one may have negotiated nacl-box, another may not). Refusing
// the whole activity because ONE recipient can't decrypt would
// drop unrelated recipients that COULD have received the activity
// encrypted. Per-recipient refusal lets the Worker do a partial
// emit: encrypt for the capable peers, refuse for the rest, log
// both outcomes in the audit feed.
//
// # Why "unknown" sensitivity tier defaults to requires-encryption
//
// Conservative default. A future sensitivity tier we don't yet
// recognize (e.g., the next ADR's "embargo+legal-hold" or whatever)
// SHOULD refuse rather than silently leak through the plaintext
// branch. If the operator added a new tier without updating this
// function, the failure mode is "refused + visible in the audit
// log" rather than "leaked + invisible." Refusing forces the
// operator to update the matrix.

package outbox

import "errors"

// EmissionPath is the typed result of [ChoosePathFor].
type EmissionPath int

const (
	// EmissionEncrypted — Worker encrypts env.Extra against the
	// recipient pubkey + dispatches. The shape that flows for
	// any share when both sides advertise nacl-box AND the
	// recipient's key is cached.
	EmissionEncrypted EmissionPath = iota

	// EmissionPlaintext — Worker skips encryption + dispatches
	// the legacy 1.22.D shape. Reachable only when the share's
	// sensitivity tier is public OR team (the lower-sensitivity
	// half) AND encryption isn't available (capability missing
	// OR key unfetchable). Higher-sensitivity tiers go to
	// EmissionRefused instead of falling back to plaintext.
	EmissionPlaintext

	// EmissionRefused — Worker marks the row refused with reason
	// encryption_required_but_unavailable + audits the decision
	// + does NOT POST. Terminal: the row's status flips to
	// 'refused' (per migration 00012's expanded CHECK), which
	// the partial-index on status='queued' filters out so
	// ListDueOutbox never picks it up again. Operator action
	// (re-pair → caps refresh OR move the share to a lower tier)
	// is required to unblock; the protocol does not auto-retry
	// after capability changes.
	EmissionRefused
)

// String surfaces a human label for log + audit output. The
// audit feed pivots on the string form so an operator can grep
// directly.
func (p EmissionPath) String() string {
	switch p {
	case EmissionEncrypted:
		return "encrypted"
	case EmissionPlaintext:
		return "plaintext"
	case EmissionRefused:
		return "refused"
	default:
		return "unknown"
	}
}

// RefuseReason is the catalogue of audit + DB-column values that
// explain WHY the Worker refused a row. v1 ships one value; the
// type exists so future reasons land without a column-type
// change + so grep/filter queries are typo-resistant.
//
// The strings must stay stable — they appear in audit metadata +
// in [federation_outbox.refused_reason], both queried directly by
// the admin federation page.
type RefuseReason string

const (
	// RefuseReasonEncryptionRequiredButUnavailable — the share
	// sensitivity tier mandates encrypted transmission (per
	// [RequiresEncryption]) AND the per-recipient encryption
	// inputs aren't both present (peer doesn't advertise the
	// nacl-box capability OR the recipient's pubkey isn't
	// cached locally). The two failure modes collapse to one
	// reason at the catalogue level; the audit row's free-text
	// detail field carries the specific diagnosis when needed.
	RefuseReasonEncryptionRequiredButUnavailable RefuseReason = "encryption_required_but_unavailable"
)

// ErrEmissionRefused is the sentinel the Worker's delivery path
// returns through the encrypt-attempt flow when policy says
// refuse. Plumbed so the per-row caller can short-circuit to the
// mark-refused + audit path without inspecting state in two
// places.
//
// Never propagates outside the Worker — refusal is a normal
// terminal state, not an error from the dispatcher loop's
// perspective.
var ErrEmissionRefused = errors.New("outbox: emission refused by sensitivity policy")

// RequiresEncryption is the share-tier-side question: does this
// share's sensitivity tier MANDATE encrypted transmission?
//
//   - public + team   → false (best-effort encryption; plaintext OK
//     when capabilities are missing)
//   - restricted +
//     embargo         → true (encryption is required; refuse if
//     not available)
//   - other (unknown,
//     future tiers)   → true (conservative default; see file-level
//     comment for the rationale)
//
// Pure function over the tier alone — no I/O, no policy
// dispatch. Used by [ChoosePathFor] + by tests that need to
// answer "would this tier refuse?" in isolation.
func RequiresEncryption(tier Sensitivity) bool {
	switch tier {
	case SensitivityPublic, SensitivityTeam:
		return false
	case SensitivityRestricted, SensitivityEmbargo:
		return true
	default:
		return true
	}
}

// ChoosePathFor computes the emission path for one (share,
// peer, recipient-key) tuple. Pure function; no side effects.
// The Worker consumes the result + acts on it (encrypt + dispatch
// / plaintext + dispatch / mark-refused + audit).
//
// Decision matrix (mirrors docs/protocol/archivepub.md §3.6):
//
//	tier             | peer e2e | key | path
//	-----------------+----------+-----+-------------
//	public           |   any    | any | encrypted-if-both / plaintext
//	team             |   any    | any | encrypted-if-both / plaintext
//	restricted       |   yes    | yes | encrypted
//	restricted       |   no     | any | REFUSED
//	restricted       |   yes    | no  | REFUSED
//	embargo          |   yes    | yes | encrypted
//	embargo          |   no     | any | REFUSED
//	embargo          |   yes    | no  | REFUSED
//	unknown / future |   yes    | yes | encrypted
//	unknown / future |   no     | any | REFUSED
//	unknown / future |   yes    | no  | REFUSED
//
// The "encrypted-if-both" cell for public + team is the
// opportunistic-encryption preserved from 1.22.I-e: when both
// sides CAN encrypt, we do. Falls back to plaintext only when at
// least one side can't.
func ChoosePathFor(
	tier Sensitivity,
	peerSupportsE2E bool,
	recipientKeyAvailable bool,
) EmissionPath {
	canEncrypt := peerSupportsE2E && recipientKeyAvailable
	if canEncrypt {
		return EmissionEncrypted
	}
	if RequiresEncryption(tier) {
		return EmissionRefused
	}
	return EmissionPlaintext
}
