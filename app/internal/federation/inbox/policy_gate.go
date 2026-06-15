// Phase 1.22.I-h receiver-side encryption policy gate.
//
// Counterpart to outbox/policy.go's sender-side refusal flip
// (Phase 1.22.I-g). When an inbound envelope arrives PLAINTEXT
// but its target object's sensitivity tier mandates encryption,
// the dispatcher rejects the envelope with reject_reason
// "encryption_required" + fires an audit event. Defense in
// depth: I-g's sender-side gate is the primary enforcement, but
// a peer running pre-I-g code (or a malicious peer ignoring the
// policy) shouldn't be able to deposit a plaintext envelope on
// a restricted share.
//
// # Why local Sensitivity + SensitivityLookup types (not imported)
//
// outbox already imports inbox (admin.go cross-package handle).
// Importing outbox from inbox would create a cycle. The
// sensitivity vocabulary is small (4 values) + the policy
// function is one switch; mirroring the types here costs ~10
// LOC + keeps the package graph acyclic. The values MUST stay
// in lockstep with outbox.Sensitivity — both packages document
// the mirror so a future edit on one side trips the test that
// pins the values on the other.
//
// # Why a callback + setter, not a direct dependency
//
// The receiver-side gate needs to look up the sensitivity tier
// for an arbitrary local object (post, asset, collection, …).
// Each object kind lives in a different package; importing all
// of them from the federation dispatcher would create a
// reverse-dependency thicket. The SensitivityLookup callback
// pattern lets the boot-wiring code build a single dispatch
// function over every kind without the dispatcher needing to
// know about any of them. Same shape as
// outbox.SensitivityLookup so a future refactor can collapse
// both to one shared callback if a shared place lands.
//
// # When the gate is a no-op
//
//   - SensitivityLookup unwired (test fixtures, defense): pass
//     through. Production wires it at boot; tests opt in.
//   - row.ObjectKind == nil OR "": no target object →
//     sensitivity isn't a meaningful question. Passes through.
//     Activities like Follow (target is a remote actor) hit
//     this branch.
//   - Lookup returns the SensitivityNotFound sentinel: object
//     was deleted between the sender's emit and the receiver's
//     dispatch. Passes through — the activity will fail
//     downstream when the handler can't resolve the object,
//     with a more appropriate reject reason.
//   - Lookup returns a tier that doesn't require encryption
//     (public / team): pass through. The 1.22.D plaintext path
//     remains valid for those tiers.

package inbox

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Sensitivity mirrors outbox.Sensitivity. Tests pin the values
// against the outbox package; do not change one without the
// other.
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityTeam       Sensitivity = "team"
	SensitivityRestricted Sensitivity = "restricted"
	SensitivityEmbargo    Sensitivity = "embargo"

	// SensitivityNotFound is the lookup callback's signal that
	// the referenced object doesn't exist locally. Distinct from
	// "" (which would also pass the require-encryption test
	// silently) so the callback can communicate "I looked, found
	// nothing" without the gate firing.
	SensitivityNotFound Sensitivity = "not_found"
)

// SensitivityLookup resolves an object's sensitivity tier for
// the receiver-side gate. objectKind is the activity's
// row.ObjectKind dereferenced (e.g., "post", "asset", "comment");
// objectID is the local UUID dereferenced from row.ObjectID.
//
// Returns SensitivityNotFound when the lookup didn't find a
// matching local object — the gate treats this as pass-through
// (the activity's downstream handler will reject with a more
// specific reason if the object is genuinely required).
//
// Implementation lives in boot-wiring (http/server.go); a
// production lookup dispatches per objectKind to the post /
// asset / comment store and returns its sensitivity column.
type SensitivityLookup func(
	ctx context.Context,
	objectKind string,
	objectID uuid.UUID,
) (Sensitivity, error)

// ErrEncryptionRequired is the sentinel the policy gate returns
// when the receiver-side rule fires. The dispatcher catches it
// + marks the row rejected with reject_reason "encryption_required"
// + fires federation.inbox.encryption_required_rejected.
//
// Tests assert errors.Is(err, ErrEncryptionRequired) instead of
// pattern-matching the message so the wording can change without
// breaking the consumer contract.
var ErrEncryptionRequired = errors.New("inbox: plaintext envelope but target requires encryption")

// SetSensitivityLookup wires the per-object sensitivity callback.
// nil-safe: passing nil leaves the dispatcher in pass-through
// mode (the gate becomes a no-op for every row). Test fixtures
// that exercise non-gate code paths can skip the call.
func (d *Dispatcher) SetSensitivityLookup(fn SensitivityLookup) {
	d.sensitivityLookup = fn
}

// requiresEncryption is the pure check (mirrors
// outbox.RequiresEncryption). Public + Team don't require
// encryption (1.22.D plaintext path remains valid); Restricted +
// Embargo do; SensitivityNotFound passes through; any other
// (future-tier / unknown) value defaults to requiring encryption
// — conservative, matches the sender-side default.
func requiresEncryption(tier Sensitivity) bool {
	switch tier {
	case SensitivityPublic, SensitivityTeam, SensitivityNotFound:
		return false
	case SensitivityRestricted, SensitivityEmbargo:
		return true
	default:
		return true
	}
}

// checkInboundEncryptionPolicy runs the receiver-side gate.
// Called from dispatchOne BEFORE tryDecryptInbound (which only
// runs for envelopes that arrived encrypted).
//
// Returns nil on pass-through (gate disabled, no target object,
// non-sensitive tier, lookup miss); returns ErrEncryptionRequired
// when the gate fires. Other errors (DB hiccup in the lookup
// callback) propagate so the caller marks the row failed (not
// rejected — failure can retry on the next tick).
func (d *Dispatcher) checkInboundEncryptionPolicy(
	ctx context.Context,
	row FederationInbox,
) error {
	if d.sensitivityLookup == nil {
		return nil
	}
	if row.ObjectKind == nil || *row.ObjectKind == "" {
		return nil
	}
	objectID := uuid.UUID(row.ObjectID.Bytes)
	if objectID == (uuid.UUID{}) {
		return nil
	}

	tier, err := d.sensitivityLookup(ctx, *row.ObjectKind, objectID)
	if err != nil {
		return err
	}
	if !requiresEncryption(tier) {
		return nil
	}
	return ErrEncryptionRequired
}
