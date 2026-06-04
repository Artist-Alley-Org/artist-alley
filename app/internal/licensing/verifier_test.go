package licensing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// End-to-end test: generate a fresh keypair, plug it into the
// verifier's catalog, build claims, sign canonically, encode the
// envelope, parse it back, verify. This is the contract every
// signed license travels through; if this passes, the artist-alley
// app can verify anything the license server signs (assuming the
// real catalog has the right pubkey).

func TestVerify_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	kid := "test-kid-01"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		seats := int64(50)
		assetCap := int64(50000)
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZTEST", Product: "artist-alley:core",
			Tier: "pro", Seats: &seats, SeatWindowDays: 30, AssetCap: &assetCap,
			Owner: "admin@studio.com", Org: "studio",
			NotBefore: now - 60, Expires: now + 86400, IssuedAt: now,
			Features: []string{"core", "ai_enrichment"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)

		got, err := Verify(text)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Tier != "pro" {
			t.Errorf("tier = %q, want pro", got.Tier)
		}
		if !hasFeatureIn("ai_enrichment", got.Features) {
			t.Errorf("missing feature ai_enrichment: %v", got.Features)
		}
	})
}

func TestVerify_RejectsExpired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "expired-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "x", Product: "p", Tier: "pro",
			SeatWindowDays: 30, Owner: "o@x.com", Org: "o",
			NotBefore: now - 1000, Expires: now - 1, IssuedAt: now - 1000,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)
		if _, err := Verify(text); !errors.Is(err, ErrExpired) {
			t.Errorf("expected ErrExpired, got %v", err)
		}
	})
}

func TestVerify_RejectsWrongIssuer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "wrongiss-kid"
	withTestKey(t, kid, pub, "lic-real.artist-alley.org", func() {
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "x", Product: "p", Tier: "pro",
			SeatWindowDays: 30, Owner: "o@x.com", Org: "o",
			NotBefore: now - 60, Expires: now + 86400, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-bogus.example.com", // ≠ ExpectedIssuer
		}
		text := mustSignEnvelope(t, priv, claims)
		if _, err := Verify(text); !errors.Is(err, ErrWrongIssuer) {
			t.Errorf("expected ErrWrongIssuer, got %v", err)
		}
	})
}

func TestVerify_RejectsTamperedClaim(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "tamper-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		seats := int64(50)
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "x", Product: "p", Tier: "pro",
			Seats: &seats, SeatWindowDays: 30, Owner: "o@x.com", Org: "o",
			NotBefore: now - 60, Expires: now + 86400, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)
		// Inflate seat count post-signing.
		bumped := strings.Replace(text, "seats: 50", "seats: 9999", 1)
		if _, err := Verify(bumped); !errors.Is(err, ErrBadSignature) {
			t.Errorf("expected ErrBadSignature, got %v", err)
		}
	})
}

func TestVerify_RejectsUnknownKID(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "known-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: "totally-different-kid", LID: "x", Product: "p", Tier: "pro",
			SeatWindowDays: 30, Owner: "o@x.com", Org: "o",
			NotBefore: now - 60, Expires: now + 86400, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		// Sign with the known key — but envelope kid points to an
		// unknown one, so lookup fails before signature check.
		text := mustSignEnvelope(t, priv, claims)
		if _, err := Verify(text); !errors.Is(err, ErrUnknownKID) {
			t.Errorf("expected ErrUnknownKID, got %v", err)
		}
	})
}

// --- helpers --------------------------------------------------------------

// withTestKey injects a generated keypair into the global PublicKeys
// catalog + overrides ExpectedIssuer for the duration of the test.
// Both are restored on cleanup so tests don't bleed.
func withTestKey(t *testing.T, kid string, pub ed25519.PublicKey, iss string, fn func()) {
	t.Helper()
	origKeys := PublicKeys
	origIss := ExpectedIssuer

	PublicKeys = append(append([]PublicKey(nil), origKeys...), PublicKey{
		KID:          kid,
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Purpose:      "core",
		CreatedAt:    "2026-01-01T00:00:00Z",
		RetiredAt:    "",
	})
	ExpectedIssuer = iss
	defer func() {
		PublicKeys = origKeys
		ExpectedIssuer = origIss
	}()
	fn()
}

// mustSignEnvelope canonicalises + signs the given claims with
// `priv`, then encodes the result into the artist-alley license
// envelope (whitespace + ISO timestamps matching the TS encoder).
func mustSignEnvelope(t *testing.T, priv ed25519.PrivateKey, c LicenseClaims) string {
	t.Helper()
	bytes, err := canonicalBytes(canonicalLicenseMap(c))
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	sig := ed25519.Sign(priv, bytes)
	sigB64u := base64.RawURLEncoding.EncodeToString(sig)
	return encodeEnvelope(c, sigB64u)
}

// encodeEnvelope reproduces the TS encoder's text layout. Kept in
// the test file (not the production code) because parsing — not
// emitting — envelopes is what the artist-alley app does in
// production.
func encodeEnvelope(c LicenseClaims, sigB64u string) string {
	seats := "unlimited"
	if c.Seats != nil {
		seats = fmt.Sprintf("%d", *c.Seats)
	}
	assetCap := "unlimited"
	if c.AssetCap != nil {
		assetCap = fmt.Sprintf("%d", *c.AssetCap)
	}
	hostFp := "none"
	if c.HostFingerprint != nil {
		hostFp = *c.HostFingerprint
	}
	orgPubkey := "none"
	if c.OrgPubkey != nil {
		orgPubkey = *c.OrgPubkey
	}
	boundDomains := "none"
	if c.BoundDomains != nil {
		boundDomains = strings.Join(c.BoundDomains, ",")
	}
	lines := []string{
		"-----BEGIN ARTIST-ALLEY LICENSE-----",
		fmt.Sprintf("v: %d", c.V),
		fmt.Sprintf("kid: %s", c.KID),
		fmt.Sprintf("lid: %s", c.LID),
		fmt.Sprintf("prod: %s", c.Product),
		fmt.Sprintf("tier: %s", c.Tier),
		fmt.Sprintf("seats: %s", seats),
		fmt.Sprintf("seat_window_days: %d", c.SeatWindowDays),
		fmt.Sprintf("asset_cap: %s", assetCap),
		fmt.Sprintf("owner: %s", c.Owner),
		fmt.Sprintf("org: %s", c.Org),
		fmt.Sprintf("nbf: %s", epochToISO(c.NotBefore)),
		fmt.Sprintf("exp: %s", epochToISO(c.Expires)),
		fmt.Sprintf("iat: %s", epochToISO(c.IssuedAt)),
		fmt.Sprintf("features: %s", strings.Join(c.Features, ",")),
		fmt.Sprintf("host_fp: %s", hostFp),
		fmt.Sprintf("org_pubkey: %s", orgPubkey),
		fmt.Sprintf("bound_domains: %s", boundDomains),
		fmt.Sprintf("iss: %s", c.Issuer),
		"",
		fmt.Sprintf("sig: %s", sigB64u),
		"-----END ARTIST-ALLEY LICENSE-----",
		"",
	}
	return strings.Join(lines, "\n")
}
