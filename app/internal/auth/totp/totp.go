// Package totp implements RFC 6238 Time-Based One-Time Passwords +
// the surrounding self-service primitives the artist-alley auth
// surface needs:
//
//   - Generate a random shared secret (20 bytes = 160 bits, per
//     RFC 4226 §4 "Recommended Length") + emit the otpauth:// URI
//     the user's authenticator app consumes via QR scan.
//
//   - Compute the 6-digit code for a given Unix timestamp + 30 s
//     step. Verify with a ±1-step window to absorb device clock
//     drift (≤±30 s — same window every reference impl uses).
//
//   - Generate user-friendly base32 recovery codes for the "phone
//     lost" path + hash them with sha256 for at-rest storage.
//
// Zero non-stdlib deps — RFC 6238 is HMAC-SHA1 of an 8-byte counter,
// truncated per RFC 4226 §5.3. ~30 lines.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// SecretBytes is the shared-secret length per RFC 4226 §4
	// "Recommended Length" — 20 bytes (160 bits, matching SHA-1's
	// block size).
	SecretBytes = 20

	// Period is the TOTP time-step in seconds. 30 s is the
	// universal default every authenticator app expects.
	Period = 30

	// Digits is the truncation width. 6 is the de-facto standard;
	// changing this requires both ends to agree (otpauth has a
	// `digits=` param but Google Authenticator etc. ignore it).
	Digits = 6

	// recoveryCodeCount is how many backup codes we mint per
	// enroll / regenerate call. 10 is the industry norm (Google,
	// GitHub, etc.).
	recoveryCodeCount = 10

	// recoveryCodeBytes is the raw entropy per recovery code
	// BEFORE base32 encoding. 6 bytes → 10 base32 chars (no
	// padding once we trim the trailing `==`). 48 bits of entropy
	// is well above the brute-force ceiling given the per-code
	// rate limit + single-use semantics.
	recoveryCodeBytes = 6
)

// GenerateSecret returns SecretBytes of cryptographically random
// bytes — the shared HMAC key the authenticator app + server
// both compute against.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("totp: random secret: %w", err)
	}
	return b, nil
}

// EncodeSecret returns the RFC 4648 base32 (no padding) encoding
// authenticator apps expect — that's the form the otpauth URI
// carries + that some apps let the user paste manually.
func EncodeSecret(secret []byte) string {
	return strings.TrimRight(base32.StdEncoding.EncodeToString(secret), "=")
}

// DecodeSecret reverses EncodeSecret. Accepts standard base32 with
// or without trailing `=` padding so users who paste from
// different sources don't see "Invalid secret".
func DecodeSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	// Strip any existing trailing padding so we can canonicalise
	// then re-pad to a multiple of 8 — accepts both forms.
	s = strings.TrimRight(s, "=")
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	return base32.StdEncoding.DecodeString(s)
}

// OtpauthURL builds the otpauth://totp/<issuer>:<account>?... URI
// authenticator apps read via QR. issuer is the site name + the
// account is typically the user's username or email; both are
// displayed in the app so users can tell which credential is
// which when they have many.
func OtpauthURL(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", Digits))
	q.Set("period", fmt.Sprintf("%d", Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Code returns the 6-digit TOTP value for the given secret + Unix
// timestamp. Exposed so tests can pin a deterministic time and so
// the verifier can compute the ±1-step neighbours.
func Code(secret []byte, unixSec int64) string {
	counter := uint64(unixSec / Period)
	return hotp(secret, counter)
}

// Verify checks whether candidate matches the TOTP value for
// secret at `at` (or the immediate neighbours, ±1 step). Returns
// true on match. Constant-time on the digit comparison to defuse
// timing-side-channel attempts.
func Verify(secret []byte, candidate string, at time.Time) bool {
	candidate = strings.TrimSpace(candidate)
	if len(candidate) != Digits {
		return false
	}
	unix := at.Unix()
	for step := int64(-1); step <= 1; step++ {
		expected := Code(secret, unix+step*Period)
		if hmacEq(expected, candidate) {
			return true
		}
	}
	return false
}

// hotp is the RFC 4226 §5 truncate-then-mod-10^Digits primitive.
// Lower-level than Code so tests can pin a counter directly.
func hotp(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation per RFC 4226 §5.3.
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, bin%mod)
}

func hmacEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

// GenerateRecoveryCodes returns recoveryCodeCount fresh plaintext
// codes formatted as `XXXX-XXXXX` for human readability. The
// caller hashes via HashRecoveryCode + persists; the plaintext
// is shown to the user once and never reachable again.
func GenerateRecoveryCodes() ([]string, error) {
	out := make([]string, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		b := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("totp: random recovery code: %w", err)
		}
		raw := strings.TrimRight(base32.StdEncoding.EncodeToString(b), "=")
		// Insert one hyphen at the midpoint for readability.
		mid := len(raw) / 2
		out = append(out, strings.ToUpper(raw[:mid]+"-"+raw[mid:]))
	}
	return out, nil
}

// NormalizeRecoveryCode strips whitespace + hyphens + uppercases
// so the verifier doesn't care how the user typed the backup code.
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

// HashRecoveryCode returns the sha256 of a normalized recovery
// code — what we persist + compare against on redemption.
func HashRecoveryCode(s string) []byte {
	h := sha256.Sum256([]byte(NormalizeRecoveryCode(s)))
	return h[:]
}
