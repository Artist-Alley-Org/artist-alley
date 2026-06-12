// Capability vocabulary for Phase 1.22.I-d peer-handshake
// capability negotiation. The bilateral intersection of both
// peers' advertised sets is stored in federation_peers.capabilities
// (migration 00009). This file defines the typed constants the
// reference implementation recognises + the helpers callers use
// to check membership, intersect, gate dispatch.
//
// # Open vocabulary on the wire, closed dispatch in code
//
// Per ADR 0042 distributed catalogues: peers MAY advertise any
// string they like in their handshake envelope. We round-trip
// unknown values through the JSONB column so the peer-side truth
// is preserved (and so re-saving a row doesn't drop metadata),
// but we never DISPATCH on unknown values. SupportsE2E + future
// gate helpers only consult [KnownCapabilities].
//
// # MUST NOT advertise unimplemented capabilities
//
// A peer advertising `e2e-encrypted` MUST be able to decrypt; a
// peer advertising `nacl-box` MUST be able to encrypt with it.
// TestKnownCapabilities_AllHaveDispatchCases is the code-side
// enforcement — every entry in KnownCapabilities must map to a
// non-empty purpose via [purposeOf] (which the dispatch sites
// also consult).

package peer

import (
	"bytes"
	"encoding/json"
	"slices"
)

// Capability is the typed wrapper for the strings carried in
// federation_peers.capabilities. Open type — peer-advertised
// values that aren't in [KnownCapabilities] still round-trip
// through this type cleanly.
type PeerCapability string

const (
	// CapE2EEncrypted — peer supports end-to-end encrypted
	// activity envelopes at all. Required for any encrypted
	// emission. Distinct from the algorithm capabilities below
	// so future versions can add CapPostQuantum or similar
	// without touching this gate.
	CapE2EEncrypted PeerCapability = "e2e-encrypted"

	// CapNaClBox — NaCl-box envelope construction (X25519 +
	// XSalsa20 + Poly1305 per https://nacl.cr.yp.to/box.html).
	// Decoupled from CapE2EEncrypted so future versions can add
	// CapAESGCM, CapChaCha20Poly1305 as alternatives without
	// breaking the gate's e2e meaning.
	CapNaClBox PeerCapability = "nacl-box"

	// CapX25519 — X25519 key agreement (RFC 7748). Decoupled
	// from CapNaClBox so future versions can add CapP256, CapX448.
	CapX25519 PeerCapability = "x25519"

	// CapEd25519EnvelopeSig — Ed25519 envelope signature
	// (per spec §5). Existing 1.22.D capability; advertised for
	// completeness so a peer that doesn't advertise it is treated
	// as legacy (1.22.D dispatched against everyone unconditionally
	// — this capability lets future versions tighten the gate).
	CapEd25519EnvelopeSig PeerCapability = "ed25519-envelope-sig"

	// CapHTTP2BatchedInbox — peer's /federation/inbox/batch
	// endpoint accepts multi-envelope POSTs (shipped 1.22.D-b-5).
	// Advertised so the delivery worker can pick the right
	// endpoint (currently the delivery worker always tries the
	// batched endpoint for ≥2 envelopes; future versions can
	// gate this on the capability).
	CapHTTP2BatchedInbox PeerCapability = "http2-batched-inbox"
)

// KnownCapabilities is the closed set this reference implementation
// recognises + dispatches on. Order is the order the handshake
// envelope emits them; receivers see the same order, the
// intersection helper preserves it.
//
// Implementation MUST NOT advertise a capability that isn't in
// this list. The TestKnownCapabilities_AllHaveDispatchCases test
// enforces it — any const here that doesn't map to a non-empty
// [purposeOf] result fails the test.
//
// # CapNaClBox rollout coordination (historical: removed at I-e,
// # restored at I-f)
//
// Per ADR 0049 §Track B rollout coordination: advertising a
// capability MUST imply both sides can honor it. I-e shipped the
// sender-side encrypt path; I-f ships the receiver-side decrypt
// path. The capability was REMOVED between I-e + I-f because if
// CapNaClBox were advertised across that gap, the I-d handshake
// would land it in the intersection, the I-e dispatcher would
// encrypt, and every encrypted envelope would fail inbound at
// the receiver until I-f shipped — a transient production
// breakage the removed-cap / re-add-at-I-f pattern prevents.
//
// Phase 1.22.I-f RESTORES CapNaClBox here as the final commit
// of the encryption arc. The on-disk capability set persisted
// for already-paired peers does NOT auto-refresh; an operator
// must trigger a re-pair (or wait for the next handshake
// round-trip) for the intersection to land CapNaClBox. The
// outbox resolver's per-peer gate (I-d) still works without the
// restore — it just emission-skips with reason
// capability_missing_naclbox until both sides advertise it.
var KnownCapabilities = CapabilitySet{
	CapE2EEncrypted,
	// CapNaClBox — restored in 1.22.I-f. See § rollout
	// coordination above for the I-e ↔ I-f gap rationale +
	// re-pair note.
	CapNaClBox,
	CapX25519,
	CapEd25519EnvelopeSig,
	CapHTTP2BatchedInbox,
}

// purposeOf maps a known Capability to a short human-readable
// purpose string. The map IS the dispatch catalogue —
// TestKnownCapabilities_AllHaveDispatchCases iterates
// KnownCapabilities + asserts each one returns a non-empty
// purpose, so it's impossible to add a const here without
// touching this switch.
//
// Returns "" for unknown / peer-advertised capabilities we don't
// understand. Callers use the empty-string check to decide
// whether to log + skip.
func purposeOf(c PeerCapability) string {
	switch c {
	case CapE2EEncrypted:
		return "envelope encryption supported (gates SupportsE2E)"
	case CapNaClBox:
		return "NaCl-box envelope construction (X25519 + XSalsa20 + Poly1305)"
	case CapX25519:
		return "X25519 key agreement for NaCl-box recipient encryption"
	case CapEd25519EnvelopeSig:
		return "Ed25519 envelope signature (existing 1.22.D wire)"
	case CapHTTP2BatchedInbox:
		return "/federation/inbox/batch endpoint accepts multi-envelope POSTs"
	default:
		return ""
	}
}

// CapabilitySet is the typed array carried in
// federation_peers.capabilities + exchanged on the wire as the
// supported_capabilities field of the handshake envelope.
//
// nil + empty slice are semantically equivalent everywhere
// (CapabilitySet(nil).Has(x) == false, Intersect(nil, anything)
// is empty, etc.) — MarshalJSON normalises nil to "[]" so the
// JSONB column never stores `null`.
type CapabilitySet []PeerCapability

// Has reports whether the set contains c. O(n) scan; sets are
// small (single-digit entries in practice), so a linear scan
// avoids the overhead of building a map for one-shot checks.
func (s CapabilitySet) Has(c PeerCapability) bool {
	return slices.Contains(s, c)
}

// Intersect returns the capabilities present in both a and b,
// preserving the order from a. Used at handshake completion on
// both sides to record what BOTH peers support.
//
// Returns an empty CapabilitySet (not nil) so JSON marshal
// produces "[]" consistently regardless of intersection size.
func Intersect(a, b CapabilitySet) CapabilitySet {
	out := make(CapabilitySet, 0, len(a))
	for _, c := range a {
		if b.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

// SupportsE2E is the gate I-e + I-g consume: returns true iff
// the set includes ALL of (CapE2EEncrypted, CapNaClBox,
// CapX25519). Triple-AND because the three can shift
// independently — a future peer might advertise e2e + aes-gcm
// + x25519 but not nacl-box; this gate stays specific to the
// nacl-box path. Future algorithm gates (SupportsE2E_AESGCM,
// SupportsPQ_KEM, etc.) sit alongside this one without changing it.
func (s CapabilitySet) SupportsE2E() bool {
	return s.Has(CapE2EEncrypted) && s.Has(CapNaClBox) && s.Has(CapX25519)
}

// MarshalJSON normalises the nil case to "[]" so the JSONB
// column never stores `null`. Without this override, json.Marshal
// of a nil []Capability emits the JSON literal `null` which
// fails the federation_peers.capabilities NOT NULL constraint.
func (s CapabilitySet) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	// Round-trip via []string so we don't recurse into our
	// own MarshalJSON.
	out := make([]string, len(s))
	for i, c := range s {
		out[i] = string(c)
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses a JSON array of strings into the typed
// CapabilitySet. Unknown values are PRESERVED — re-saving a row
// loaded from the DB doesn't drop peer-side metadata we don't
// understand.
func (s *CapabilitySet) UnmarshalJSON(b []byte) error {
	// Tolerate JSON null + empty input as empty set rather than
	// erroring; sqlc's RawMessage path can produce either form.
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*s = CapabilitySet{}
		return nil
	}
	var raw []string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(CapabilitySet, len(raw))
	for i, c := range raw {
		out[i] = PeerCapability(c)
	}
	*s = out
	return nil
}
