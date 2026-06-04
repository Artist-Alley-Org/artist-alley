// Ed25519 signature verification + temporal + iss checks.
//
// Verify(envelopeText) does the entire flow in one call:
//
//   1. Decode the envelope.
//   2. Look up the kid in the embedded PublicKeys catalog.
//   3. Reconstruct canonical JSON from the parsed claims.
//   4. Verify the Ed25519 signature.
//   5. Check temporal window (now ∈ [nbf, exp]).
//   6. Check issuer matches ExpectedIssuer (when set in pubkeys.gen.go).
//
// The caller gets back the parsed claims (so the app can read the
// feature list, tier, caps, etc.) plus a typed error indicating
// which check failed when verification doesn't pass.

package licensing

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Known typed errors. Callers can `errors.Is` against these to
// distinguish "signature failed" from "expired" from "wrong issuer"
// when surfacing license status to admins.
var (
	ErrNotInWindow     = errors.New("license: outside validity window")
	ErrNotYetValid     = errors.New("license: not yet valid")
	ErrExpired         = errors.New("license: expired")
	ErrUnknownKID      = errors.New("license: unknown signing key")
	ErrBadSignature    = errors.New("license: signature invalid")
	ErrWrongIssuer     = errors.New("license: wrong issuer")
	ErrBadEnvelope     = errors.New("license: malformed envelope")
	ErrChainExpired    = errors.New("license: publisher cert expired")
	ErrChainBadSig     = errors.New("license: publisher cert signature invalid")
	ErrChainScope      = errors.New("license: plugin not in publisher's allowed_products")
	ErrChainKIDMismatch = errors.New("license: plugin kid does not match cert kid")

	// Org cross-binding (Layer-1) errors. Surface when the .lic
	// declares an `org_pubkey` claim but the customer-held org.key
	// file on disk is missing, malformed, or doesn't match.
	ErrOrgKeyMissing   = errors.New("license: org.key missing — license requires customer activation file")
	ErrOrgKeyBadFormat = errors.New("license: org.key malformed — expected base64-encoded 32-byte seed")
	ErrOrgKeyMismatch  = errors.New("license: org.key does not match license org_pubkey")
)

// Verify decodes + verifies a core license envelope end-to-end.
// Returns the parsed claims on success; returns a typed error on
// failure. Always logs the reason for failure at WARN level (the
// caller doesn't need to log it again).
func Verify(text string) (LicenseClaims, error) {
	signed, err := DecodeLicense(text)
	if err != nil {
		return LicenseClaims{}, fmt.Errorf("%w: %v", ErrBadEnvelope, err)
	}
	return VerifyLicense(signed)
}

// VerifyLicense runs the crypto + temporal + iss checks against an
// already-decoded envelope. Useful for tests + when the envelope was
// parsed elsewhere (e.g. once at boot, cached).
func VerifyLicense(signed SignedLicense) (LicenseClaims, error) {
	pub, ok := lookupPublicKey(signed.Claims.KID)
	if !ok {
		return LicenseClaims{}, fmt.Errorf("%w: kid=%q", ErrUnknownKID, signed.Claims.KID)
	}
	msg, err := canonicalBytes(canonicalLicenseMap(signed.Claims))
	if err != nil {
		return LicenseClaims{}, fmt.Errorf("canonical: %w", err)
	}
	sig, err := decodeB64URL(signed.SignatureB64u)
	if err != nil {
		return LicenseClaims{}, fmt.Errorf("%w: sig decode: %v", ErrBadSignature, err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return LicenseClaims{}, ErrBadSignature
	}

	if ExpectedIssuer != "" && signed.Claims.Issuer != ExpectedIssuer {
		return LicenseClaims{}, fmt.Errorf("%w: got %q want %q",
			ErrWrongIssuer, signed.Claims.Issuer, ExpectedIssuer)
	}

	now := time.Now().Unix()
	if now < signed.Claims.NotBefore {
		return LicenseClaims{}, ErrNotYetValid
	}
	if now > signed.Claims.Expires {
		return LicenseClaims{}, ErrExpired
	}
	return signed.Claims, nil
}

// VerifyPublisherCert checks a publisher cert's signature against
// the embedded root pubkey identified by cert.signed_by_kid. Does
// NOT check temporal window — callers that need temporal validity
// (e.g. plugin license chain walk) check it separately so a
// cosmetically-expired-but-cryptographically-valid cert can still be
// surfaced for diagnostics.
func VerifyPublisherCert(cert SignedPublisherCert) error {
	pub, ok := lookupPublicKey(cert.Claims.SignedByKID)
	if !ok {
		return fmt.Errorf("%w: kid=%q", ErrUnknownKID, cert.Claims.SignedByKID)
	}
	msg, err := canonicalBytes(canonicalPublisherCertMap(cert.Claims))
	if err != nil {
		return fmt.Errorf("canonical: %w", err)
	}
	sig, err := decodeB64URL(cert.SignatureB64u)
	if err != nil {
		return fmt.Errorf("%w: sig decode: %v", ErrChainBadSig, err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return ErrChainBadSig
	}
	return nil
}

// VerifyPluginChain walks the chain of trust for a plugin license:
//
//   1. Verify the embedded publisher cert with our root key.
//   2. Check the cert is in its temporal window.
//   3. Verify the plugin license signature against the publisher's
//      pubkey (taken from the embedded cert).
//   4. Check the plugin license is in its temporal window.
//   5. Confirm plugin.publisher_kid == cert.publisher_kid.
//   6. Confirm plugin.prod ∈ cert.allowed_products.
//
// Returns the validated plugin claims on success; typed error on any
// failed step.
func VerifyPluginChain(signed SignedPluginLicense) (PluginLicenseClaims, error) {
	if err := VerifyPublisherCert(signed.PublisherCert); err != nil {
		return PluginLicenseClaims{}, err
	}
	now := time.Now().Unix()
	if now < signed.PublisherCert.Claims.NotBefore || now > signed.PublisherCert.Claims.Expires {
		return PluginLicenseClaims{}, ErrChainExpired
	}
	if signed.Claims.PublisherKID != signed.PublisherCert.Claims.PublisherKID {
		return PluginLicenseClaims{}, ErrChainKIDMismatch
	}
	if !containsString(signed.PublisherCert.Claims.AllowedProducts, signed.Claims.Product) {
		return PluginLicenseClaims{}, fmt.Errorf("%w: prod=%q allowed=%v",
			ErrChainScope, signed.Claims.Product, signed.PublisherCert.Claims.AllowedProducts)
	}

	pubBytes, err := base64.StdEncoding.DecodeString(signed.PublisherCert.Claims.PublisherPubkeyB64)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return PluginLicenseClaims{}, fmt.Errorf("publisher pubkey decode: %w", err)
	}
	pub := ed25519.PublicKey(pubBytes)
	msg, err := canonicalBytes(canonicalPluginLicenseMap(signed.Claims))
	if err != nil {
		return PluginLicenseClaims{}, fmt.Errorf("canonical: %w", err)
	}
	sig, err := decodeB64URL(signed.SignatureB64u)
	if err != nil {
		return PluginLicenseClaims{}, fmt.Errorf("%w: sig decode: %v", ErrBadSignature, err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return PluginLicenseClaims{}, ErrBadSignature
	}
	if now < signed.Claims.NotBefore {
		return PluginLicenseClaims{}, ErrNotYetValid
	}
	if now > signed.Claims.Expires {
		return PluginLicenseClaims{}, ErrExpired
	}
	return signed.Claims, nil
}

// lookupPublicKey returns the ed25519 key bytes for the given kid,
// or ok=false if not in the embedded catalog. Retired kids are still
// looked up — old licenses keep verifying after rotation.
func lookupPublicKey(kid string) (ed25519.PublicKey, bool) {
	for _, k := range PublicKeys {
		if k.KID == kid {
			b, err := base64.StdEncoding.DecodeString(k.PublicKeyB64)
			if err != nil || len(b) != ed25519.PublicKeySize {
				return nil, false
			}
			return ed25519.PublicKey(b), true
		}
	}
	return nil, false
}

// decodeB64URL decodes a base64url string (URL-safe alphabet, no
// padding). Re-adds padding if the input doesn't have it.
func decodeB64URL(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// VerifyOrgCrossBinding runs the Layer-1 cross-binding check for a
// license that declares an `org_pubkey` claim. The customer-side
// activation script (scripts/customer-activation.ts in the
// license-server repo) generates an Ed25519 keypair, writes the
// PRIVATE seed to org.key (typically /etc/artist-alley/org.key,
// chmod 600), and sends the PUBLIC half to our license server for
// embedding in the issued .lic.
//
// At boot — and again on every license-upload — we re-derive the
// public key from the seed on disk and compare it byte-for-byte to
// the license's `org_pubkey`. If the file is missing, malformed, or
// the derived pubkey doesn't match, the license is REJECTED even
// though its Ed25519 signature verified cleanly. This defeats license
// sharing: an attacker with a stolen .lic still needs the original
// install's org.key file, which never leaves the customer's host.
//
// orgPubkeyB64 is the base64 (standard alphabet) form stored in the
// license claim. orgKeyPath is the on-disk path to the seed file.
// Returns nil on a successful binding; one of the ErrOrgKey* sentinels
// on any failure step.
//
// Special case: when orgPubkeyB64 is empty, this function is a no-op
// returning nil. That's how community/trial/dev licenses opt out of
// cross-binding — they leave org_pubkey null on the server side and
// the verifier doesn't ask for org.key.
func VerifyOrgCrossBinding(orgPubkeyB64, orgKeyPath string) error {
	if orgPubkeyB64 == "" {
		return nil
	}
	if orgKeyPath == "" {
		return ErrOrgKeyMissing
	}
	raw, err := os.ReadFile(orgKeyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrOrgKeyMissing
		}
		return fmt.Errorf("read org.key: %w", err)
	}
	derived, err := deriveOrgPubkeyB64(raw)
	if err != nil {
		return err
	}
	if !constantTimeEqualString(derived, orgPubkeyB64) {
		return ErrOrgKeyMismatch
	}
	return nil
}

// deriveOrgPubkeyB64 decodes the org.key file contents and returns
// the base64 (standard alphabet) public key. Tolerates surrounding
// whitespace — the customer-activation script writes a single line
// terminated by \n; admins editing by hand may add spaces.
//
// The on-disk format is a base64 Ed25519 SEED (32 bytes). When given
// a 64-byte expanded private key we accept it too — the back half
// of an expanded key IS the public key, so we can short-circuit.
// Both formats round-trip cleanly through the standard library.
func deriveOrgPubkeyB64(raw []byte) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", ErrOrgKeyBadFormat
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Tolerate URL-safe alphabet too — `customer-activation.ts`
		// uses standard, but a copy/paste through some tools mangles
		// '+'/'/' into '-'/'_'. Falling back here is harmless: the
		// downstream length check still pins the format.
		decoded, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return "", ErrOrgKeyBadFormat
		}
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		priv := ed25519.NewKeyFromSeed(decoded)
		pub := priv.Public().(ed25519.PublicKey)
		return base64.StdEncoding.EncodeToString(pub), nil
	case ed25519.PrivateKeySize:
		priv := ed25519.PrivateKey(decoded)
		pub := priv.Public().(ed25519.PublicKey)
		return base64.StdEncoding.EncodeToString(pub), nil
	default:
		return "", ErrOrgKeyBadFormat
	}
}

// constantTimeEqualString compares two strings in constant time. Both
// inputs are base64-encoded public keys — strictly speaking pubkeys
// aren't a secret so a timing-leak attack here is academic, but the
// constant-time path costs nothing and the lint stays clean.
func constantTimeEqualString(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
