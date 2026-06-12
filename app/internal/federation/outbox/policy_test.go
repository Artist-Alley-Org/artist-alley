// Phase 1.22.I-g — pure-function tests for the sender-refusal
// policy. No DB, no fixtures: every test exercises the decision
// matrix from docs/protocol/archivepub.md §3.6 against a
// (sensitivity, peer-cap, key-availability) triple.

package outbox

import "testing"

// --- RequiresEncryption — the tier-only half ---

func TestRequiresEncryption_Restricted_True(t *testing.T) {
	if !RequiresEncryption(SensitivityRestricted) {
		t.Error("restricted tier MUST require encryption")
	}
}

func TestRequiresEncryption_Embargo_True(t *testing.T) {
	if !RequiresEncryption(SensitivityEmbargo) {
		t.Error("embargo tier MUST require encryption")
	}
}

func TestRequiresEncryption_Public_False(t *testing.T) {
	if RequiresEncryption(SensitivityPublic) {
		t.Error("public tier MUST NOT require encryption (opportunistic only)")
	}
}

func TestRequiresEncryption_Team_False(t *testing.T) {
	if RequiresEncryption(SensitivityTeam) {
		t.Error("team tier MUST NOT require encryption (opportunistic only)")
	}
}

// TestRequiresEncryption_Unknown_True asserts the conservative
// default: any tier not in the closed catalogue is treated as
// require-encryption. This is the load-bearing invariant that
// protects against silently-leaking-plaintext-after-adding-a-
// new-tier — the failure mode of an unrecognized tier MUST be
// "refuse" (visible in the audit log) rather than "fall back to
// plaintext" (invisible).
func TestRequiresEncryption_Unknown_True(t *testing.T) {
	if !RequiresEncryption(Sensitivity("future-tier-not-yet-added")) {
		t.Error("unknown sensitivity tier MUST default to require-encryption (conservative)")
	}
	if !RequiresEncryption(Sensitivity("")) {
		t.Error("empty sensitivity MUST default to require-encryption (conservative)")
	}
}

// --- ChoosePathFor — the full decision matrix ---

func TestChoosePathFor_RestrictedPeerSupportsE2E_KeyAvailable_Encrypt(t *testing.T) {
	got := ChoosePathFor(SensitivityRestricted, true, true)
	if got != EmissionEncrypted {
		t.Errorf("got %v, want EmissionEncrypted", got)
	}
}

func TestChoosePathFor_RestrictedPeerNoE2E_Refuse(t *testing.T) {
	// Peer doesn't advertise nacl-box (legacy / pre-I-f peer that
	// hasn't re-paired). Restricted share → refuse rather than
	// fall back to plaintext.
	got := ChoosePathFor(SensitivityRestricted, false, true)
	if got != EmissionRefused {
		t.Errorf("got %v, want EmissionRefused", got)
	}
}

func TestChoosePathFor_RestrictedPeerSupportsE2E_KeyMissing_Refuse(t *testing.T) {
	// Peer CAN encrypt (has the capability) but we don't have
	// their recipient pubkey cached (cold-miss + no
	// federation_remote_actors row). Same refusal as no-cap.
	got := ChoosePathFor(SensitivityRestricted, true, false)
	if got != EmissionRefused {
		t.Errorf("got %v, want EmissionRefused", got)
	}
}

func TestChoosePathFor_PublicPeerNoE2E_Plaintext(t *testing.T) {
	// Backwards-compat path: public share + legacy peer →
	// plaintext envelope per the existing 1.22.D wire. Refusing
	// here would break public-tier federation against any
	// pre-I-f peer.
	got := ChoosePathFor(SensitivityPublic, false, true)
	if got != EmissionPlaintext {
		t.Errorf("got %v, want EmissionPlaintext", got)
	}
}

func TestChoosePathFor_PublicPeerSupportsE2E_Encrypt(t *testing.T) {
	// Opportunistic encryption — public-tier share but both
	// sides CAN encrypt, so we do. Better transit privacy +
	// builds positive signal that encryption works end-to-end.
	got := ChoosePathFor(SensitivityPublic, true, true)
	if got != EmissionEncrypted {
		t.Errorf("got %v, want EmissionEncrypted (opportunistic)", got)
	}
}

// --- Cross-tier sweep — sanity checks the matrix invariants ---

func TestChoosePathFor_MatrixSweep_NoPlaintextLeakOnRequiredTiers(t *testing.T) {
	// Sweep every restricted/embargo cell that should refuse.
	// The invariant: no required-encryption tier should ever
	// produce EmissionPlaintext. Plaintext is reachable ONLY
	// from non-required tiers (public / team).
	cases := []struct {
		tier Sensitivity
		e2e  bool
		key  bool
	}{
		{SensitivityRestricted, false, false},
		{SensitivityRestricted, false, true},
		{SensitivityRestricted, true, false},
		{SensitivityEmbargo, false, false},
		{SensitivityEmbargo, false, true},
		{SensitivityEmbargo, true, false},
	}
	for _, c := range cases {
		got := ChoosePathFor(c.tier, c.e2e, c.key)
		if got == EmissionPlaintext {
			t.Errorf("tier=%v e2e=%v key=%v: got EmissionPlaintext — REQUIRED tier leaked to plaintext",
				c.tier, c.e2e, c.key)
		}
	}
}

func TestChoosePathFor_EmbargoPeerNoE2E_Refuse(t *testing.T) {
	// Embargo is symmetric to restricted — both required-
	// encryption tiers behave identically in the matrix.
	got := ChoosePathFor(SensitivityEmbargo, false, false)
	if got != EmissionRefused {
		t.Errorf("got %v, want EmissionRefused", got)
	}
}

// --- EmissionPath.String — audit feed pivots on the strings ---

func TestEmissionPath_StringStability(t *testing.T) {
	// These strings appear in audit metadata + log lines + the
	// scenario 11 dogfood probe. They MUST stay stable; the
	// test exists so a rename surfaces as a CI failure rather
	// than a silent operator-facing regression.
	cases := []struct {
		path EmissionPath
		want string
	}{
		{EmissionEncrypted, "encrypted"},
		{EmissionPlaintext, "plaintext"},
		{EmissionRefused, "refused"},
	}
	for _, c := range cases {
		if got := c.path.String(); got != c.want {
			t.Errorf("path=%d: got %q, want %q", c.path, got, c.want)
		}
	}
}
