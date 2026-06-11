// Unit tests for the Phase 1.22.I-d capability vocabulary.
// Pure data + JSON tests; no DB dependency, no skip on
// AA_DB_PASSWORD.

package peer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCapabilitySet_Has_True(t *testing.T) {
	s := CapabilitySet{CapE2EEncrypted, CapNaClBox, CapX25519}
	for _, want := range []PeerCapability{CapE2EEncrypted, CapNaClBox, CapX25519} {
		if !s.Has(want) {
			t.Errorf("Has(%q) = false, want true", want)
		}
	}
}

func TestCapabilitySet_Has_False(t *testing.T) {
	s := CapabilitySet{CapE2EEncrypted}
	for _, want := range []PeerCapability{CapNaClBox, CapX25519, CapEd25519EnvelopeSig, "unknown-cap"} {
		if s.Has(want) {
			t.Errorf("Has(%q) = true, want false", want)
		}
	}
}

func TestIntersect_OverlapOnly(t *testing.T) {
	a := CapabilitySet{CapE2EEncrypted, CapNaClBox, CapX25519, CapHTTP2BatchedInbox}
	b := CapabilitySet{CapNaClBox, CapX25519, CapEd25519EnvelopeSig}
	got := Intersect(a, b)
	want := CapabilitySet{CapNaClBox, CapX25519}
	if !slices.Equal(got, want) {
		t.Errorf("Intersect = %v, want %v", got, want)
	}
}

func TestIntersect_PreservesOrderFromFirstArg(t *testing.T) {
	a := CapabilitySet{CapX25519, CapE2EEncrypted, CapNaClBox}
	b := CapabilitySet{CapNaClBox, CapE2EEncrypted, CapX25519}
	got := Intersect(a, b)
	want := CapabilitySet{CapX25519, CapE2EEncrypted, CapNaClBox} // a's order
	if !slices.Equal(got, want) {
		t.Errorf("Intersect = %v, want %v (a's order)", got, want)
	}
}

func TestIntersect_EmptyOneSide_ReturnsEmpty(t *testing.T) {
	got := Intersect(CapabilitySet{CapE2EEncrypted, CapNaClBox}, CapabilitySet{})
	if len(got) != 0 {
		t.Errorf("Intersect(non-empty, []) = %v, want []", got)
	}
	got = Intersect(CapabilitySet{}, CapabilitySet{CapE2EEncrypted, CapNaClBox})
	if len(got) != 0 {
		t.Errorf("Intersect([], non-empty) = %v, want []", got)
	}
	// Intersect must never return nil — JSON marshal of a nil
	// CapabilitySet emits "[]" per the override, so this is
	// belt-and-braces: callers can use the result as-is in the
	// SetPeerCapabilities sqlc arg without a nil check.
	if got == nil {
		t.Errorf("Intersect returned nil; want empty non-nil slice")
	}
}

func TestIntersect_NoOverlap_ReturnsEmpty(t *testing.T) {
	a := CapabilitySet{CapE2EEncrypted, CapNaClBox}
	b := CapabilitySet{CapEd25519EnvelopeSig, CapHTTP2BatchedInbox}
	got := Intersect(a, b)
	if len(got) != 0 {
		t.Errorf("Intersect (no overlap) = %v, want []", got)
	}
}

func TestSupportsE2E_AllThree_True(t *testing.T) {
	s := CapabilitySet{CapE2EEncrypted, CapNaClBox, CapX25519}
	if !s.SupportsE2E() {
		t.Errorf("SupportsE2E = false, want true with all three caps present")
	}
	// Order doesn't matter.
	s = CapabilitySet{CapX25519, CapE2EEncrypted, CapNaClBox}
	if !s.SupportsE2E() {
		t.Errorf("SupportsE2E = false on reordered set; want true")
	}
	// Extra caps in the set don't change the answer.
	s = CapabilitySet{CapE2EEncrypted, CapNaClBox, CapX25519, CapHTTP2BatchedInbox, "future-cap"}
	if !s.SupportsE2E() {
		t.Errorf("SupportsE2E = false with extra caps present; want true")
	}
}

func TestSupportsE2E_MissingE2E_False(t *testing.T) {
	s := CapabilitySet{CapNaClBox, CapX25519}
	if s.SupportsE2E() {
		t.Errorf("SupportsE2E = true without CapE2EEncrypted; want false")
	}
}

func TestSupportsE2E_MissingNaClBox_False(t *testing.T) {
	s := CapabilitySet{CapE2EEncrypted, CapX25519}
	if s.SupportsE2E() {
		t.Errorf("SupportsE2E = true without CapNaClBox; want false")
	}
}

func TestSupportsE2E_MissingX25519_False(t *testing.T) {
	s := CapabilitySet{CapE2EEncrypted, CapNaClBox}
	if s.SupportsE2E() {
		t.Errorf("SupportsE2E = true without CapX25519; want false")
	}
}

func TestSupportsE2E_EmptySet_False(t *testing.T) {
	if (CapabilitySet{}).SupportsE2E() {
		t.Errorf("SupportsE2E = true on empty set; want false")
	}
	if CapabilitySet(nil).SupportsE2E() {
		t.Errorf("SupportsE2E = true on nil set; want false")
	}
}

// TestKnownCapabilities_AllHaveDispatchCases is the code-side
// enforcement of "MUST NOT advertise unimplemented capabilities."
// Every entry in KnownCapabilities must map to a non-empty
// purposeOf result — purposeOf IS the dispatch catalogue, so a
// const added to KnownCapabilities without a switch case here
// fails the test.
func TestKnownCapabilities_AllHaveDispatchCases(t *testing.T) {
	for _, c := range KnownCapabilities {
		if got := purposeOf(c); got == "" {
			t.Errorf("KnownCapabilities entry %q has no dispatch case in purposeOf; "+
				"either add a case or remove the const from KnownCapabilities",
				c)
		}
	}
}

func TestPurposeOf_ReturnsEmptyForUnknown(t *testing.T) {
	// Symmetric coverage: a value NOT in KnownCapabilities MUST
	// produce empty purposeOf. Guards against the default case
	// silently mapping unknowns to a non-empty string.
	for _, c := range []PeerCapability{"unknown-cap", "future-pq-kem", ""} {
		if got := purposeOf(c); got != "" {
			t.Errorf("purposeOf(%q) = %q, want empty (unknown cap)", c, got)
		}
	}
}

func TestCapabilitySet_JSON_RoundtripKnownValues(t *testing.T) {
	in := CapabilitySet{CapE2EEncrypted, CapNaClBox, CapX25519}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Sanity: known caps serialise as their string values.
	want := `["e2e-encrypted","nacl-box","x25519"]`
	if string(raw) != want {
		t.Errorf("Marshal = %s, want %s", raw, want)
	}
	var out CapabilitySet
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !slices.Equal(in, out) {
		t.Errorf("Roundtrip: got %v want %v", out, in)
	}
}

func TestCapabilitySet_JSON_NilMarshalsAsEmptyArray(t *testing.T) {
	// Without the override, Go's reflection-based json.Marshal
	// of a nil []PeerCapability emits the literal `null`, which
	// fails the federation_peers.capabilities NOT NULL constraint
	// at INSERT/UPDATE time. The override prevents that.
	raw, err := json.Marshal(CapabilitySet(nil))
	if err != nil {
		t.Fatalf("Marshal nil: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("nil Marshal = %s, want []", raw)
	}
}

func TestCapabilitySet_JSON_UnmarshalNullProducesEmpty(t *testing.T) {
	// Symmetric: a stored row that somehow ended up with the JSON
	// literal `null` (legacy data, manual DB edit, peer with a
	// buggy implementation) must unmarshal to an empty set, not
	// error. The DB constraint should prevent this state but the
	// in-process parser stays defensive.
	var s CapabilitySet
	if err := json.Unmarshal([]byte("null"), &s); err != nil {
		t.Fatalf("Unmarshal null: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("null unmarshalled to %v, want empty", s)
	}
}

func TestCapabilitySet_UnmarshalUnknownCapability_Preserves(t *testing.T) {
	// Per ADR 0042 distributed catalogues: peers MAY advertise
	// capabilities we don't recognise. Loading a row that contains
	// them MUST round-trip — re-saving the row doesn't drop the
	// peer-side metadata. Helps the operator surface "peer advertises
	// X but we don't know what that means" diagnostics rather than
	// silently dropping the field.
	in := []byte(`["e2e-encrypted","future-pq-kem","compression-brotli"]`)
	var s CapabilitySet
	if err := json.Unmarshal(in, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(s) != 3 {
		t.Fatalf("len = %d, want 3", len(s))
	}
	if s[0] != CapE2EEncrypted ||
		s[1] != "future-pq-kem" ||
		s[2] != "compression-brotli" {
		t.Errorf("preserved values = %v", s)
	}
	// Has on unknown values works without special-casing.
	if !s.Has("future-pq-kem") {
		t.Errorf("Has(unknown) should return true when the value is present")
	}
	// SupportsE2E still answers correctly even with unknowns in
	// the set (the gate only consults known caps).
	if s.SupportsE2E() {
		t.Errorf("SupportsE2E should be false — only CapE2EEncrypted present, not the triple")
	}
	// Round-trip — unknown values survive a Marshal-Unmarshal pass.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Re-Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "future-pq-kem") {
		t.Errorf("re-marshalled output dropped unknown cap: %s", raw)
	}
}
