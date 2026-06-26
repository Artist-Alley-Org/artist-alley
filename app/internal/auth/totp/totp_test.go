package totp_test

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth/totp"
)

// TestRFC6238_KnownVectors pins three of the RFC 6238 Appendix B
// test vectors (SHA-1, 8-digit). We use the published 20-byte ASCII
// secret + verify the dynamic-truncation primitive against the
// vector counters. We only check 6-digit truncation since our impl
// hard-codes Digits=6; the truncation logic is identical, the
// final mod just differs.
func TestRFC6238_KnownVectors(t *testing.T) {
	secret := []byte("12345678901234567890") // RFC 6238 reference
	// (time, expected 8-digit, expected 6-digit derived by % 1e6)
	cases := []struct {
		t       int64
		want6   string
	}{
		{59, "287082"},          // 94287082 % 1e6
		{1111111109, "081804"},  // 07081804 % 1e6
		{1111111111, "050471"},  // 14050471 % 1e6
	}
	for _, c := range cases {
		got := totp.Code(secret, c.t)
		if got != c.want6 {
			t.Errorf("Code(t=%d) = %q, want %q", c.t, got, c.want6)
		}
	}
}

func TestGenerateSecret_LengthAndDistinct(t *testing.T) {
	a, err := totp.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	b, err := totp.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(a) != totp.SecretBytes || len(b) != totp.SecretBytes {
		t.Errorf("len(a)=%d len(b)=%d want %d", len(a), len(b), totp.SecretBytes)
	}
	if string(a) == string(b) {
		t.Errorf("two GenerateSecret calls returned the same bytes — RNG is busted")
	}
}

func TestEncodeDecodeSecret_RoundTrip(t *testing.T) {
	secret, _ := totp.GenerateSecret()
	encoded := totp.EncodeSecret(secret)
	if strings.HasSuffix(encoded, "=") {
		t.Errorf("EncodeSecret should strip trailing padding; got %q", encoded)
	}
	// authenticator apps accept both with-padding and without.
	for _, in := range []string{encoded, encoded + "===", strings.ToLower(encoded), encoded + " "} {
		got, err := totp.DecodeSecret(in)
		if err != nil {
			t.Errorf("DecodeSecret(%q): %v", in, err)
			continue
		}
		if string(got) != string(secret) {
			t.Errorf("round-trip mismatch for %q", in)
		}
	}
}

func TestVerify_AcceptsCurrentAndNeighbours(t *testing.T) {
	secret, _ := totp.GenerateSecret()
	now := time.Unix(1_700_000_000, 0)
	codeNow := totp.Code(secret, now.Unix())
	codePrev := totp.Code(secret, now.Add(-30*time.Second).Unix())
	codeNext := totp.Code(secret, now.Add(30*time.Second).Unix())

	for _, c := range []string{codeNow, codePrev, codeNext} {
		if !totp.Verify(secret, c, now) {
			t.Errorf("Verify failed for in-window code %q at %v", c, now)
		}
	}
}

func TestVerify_RejectsOutsideWindow(t *testing.T) {
	secret, _ := totp.GenerateSecret()
	now := time.Unix(1_700_000_000, 0)
	// Two steps away (60 s) is outside the ±1-step window.
	codeFar := totp.Code(secret, now.Add(60*time.Second).Unix())
	if totp.Verify(secret, codeFar, now) {
		t.Errorf("Verify should reject ±2-step code %q at %v", codeFar, now)
	}
}

func TestVerify_RejectsMalformed(t *testing.T) {
	secret, _ := totp.GenerateSecret()
	now := time.Now()
	for _, bad := range []string{"", "12345" /* 5 digits */, "1234567" /* 7 */, "abcdef", "      "} {
		if totp.Verify(secret, bad, now) {
			t.Errorf("Verify should reject malformed %q", bad)
		}
	}
}

func TestVerify_RejectsWrongSecret(t *testing.T) {
	a, _ := totp.GenerateSecret()
	b, _ := totp.GenerateSecret()
	now := time.Now()
	code := totp.Code(a, now.Unix())
	if totp.Verify(b, code, now) {
		t.Errorf("Verify should reject code computed under a different secret")
	}
}

func TestOtpauthURL_StructureAndParams(t *testing.T) {
	secret, _ := totp.GenerateSecret()
	url := totp.OtpauthURL("ArtistAlley", "ada@example.com", secret)
	if !strings.HasPrefix(url, "otpauth://totp/") {
		t.Errorf("missing otpauth://totp/ prefix: %q", url)
	}
	if !strings.Contains(url, "secret=") {
		t.Errorf("missing secret= param: %q", url)
	}
	if !strings.Contains(url, "issuer=ArtistAlley") {
		t.Errorf("missing issuer param: %q", url)
	}
	if !strings.Contains(url, "algorithm=SHA1") {
		t.Errorf("missing algorithm=SHA1 (RFC 6238 default the apps assume): %q", url)
	}
}

func TestGenerateRecoveryCodes_FormatAndUniqueness(t *testing.T) {
	codes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Errorf("got %d codes, want 10", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !strings.Contains(c, "-") {
			t.Errorf("recovery code missing hyphen separator: %q", c)
		}
		// Sanity-check the underlying bytes decode as base32 once
		// the hyphen is stripped + padding restored.
		raw := strings.ReplaceAll(c, "-", "")
		if pad := len(raw) % 8; pad != 0 {
			raw += strings.Repeat("=", 8-pad)
		}
		if _, err := base32.StdEncoding.DecodeString(raw); err != nil {
			t.Errorf("recovery code %q does not decode as base32: %v", c, err)
		}
		if seen[c] {
			t.Errorf("duplicate recovery code in same batch: %q", c)
		}
		seen[c] = true
	}
}

func TestNormalizeRecoveryCode_StripsAndUppercases(t *testing.T) {
	cases := map[string]string{
		"abcd-efghi":    "ABCDEFGHI",
		"  ABCD-EFGHI ": "ABCDEFGHI",
		"abcdefghi":     "ABCDEFGHI",
		"ABCD EFGHI":    "ABCDEFGHI",
	}
	for in, want := range cases {
		if got := totp.NormalizeRecoveryCode(in); got != want {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHashRecoveryCode_DeterministicAndDistinct(t *testing.T) {
	h1 := totp.HashRecoveryCode("ABCD-EFGHI")
	h2 := totp.HashRecoveryCode("abcdefghi")
	h3 := totp.HashRecoveryCode("ABCD-EFGHJ") // last char different
	if string(h1) != string(h2) {
		t.Errorf("hash should be normalisation-invariant; h1 != h2")
	}
	if string(h1) == string(h3) {
		t.Errorf("different inputs produced the same hash — that's bad")
	}
	if len(h1) != 32 {
		t.Errorf("sha256 hash should be 32 bytes, got %d", len(h1))
	}
}
