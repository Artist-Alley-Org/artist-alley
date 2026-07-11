// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Conformance harness for the federation v1 wire format.
//
// Validates every fixture under testdata/conformance/. A change to
// any fixture is a wire-format change and demands a coordinated
// spec doc update + cross-implementation rollout. The fixtures
// are themselves the contract anyone re-implementing the spec
// validates against.
//
// Golden-file pattern: run with `go test -run Conformance -update`
// to (re)generate fixtures when the spec changes. Subsequent runs
// verify against the committed fixtures. CI runs WITHOUT -update;
// drift between code and fixtures fails the build.

package federation_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/nacl"
)

var updateFixtures = flag.Bool("update", false,
	"regenerate conformance vectors under testdata/conformance/ instead of verifying")

const conformanceDir = "testdata/conformance"

// --- RFC 8785 canonicalization vectors -----------------------------------

func TestConformance_RFC8785(t *testing.T) {
	dir := filepath.Join(conformanceDir, "rfc8785")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".input.json") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".input.json")
		t.Run(base, func(t *testing.T) {
			inputPath := filepath.Join(dir, base+".input.json")
			canonPath := filepath.Join(dir, base+".canonical.bin")
			input, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatalf("read %s: %v", inputPath, err)
			}
			got, err := federation.Canonicalize(input)
			if err != nil {
				t.Fatalf("canonicalize %s: %v", base, err)
			}
			if *updateFixtures {
				if err := os.WriteFile(canonPath, got, 0o644); err != nil {
					t.Fatalf("update %s: %v", canonPath, err)
				}
				t.Logf("wrote %s (%d bytes)", canonPath, len(got))
				return
			}
			want, err := os.ReadFile(canonPath)
			if err != nil {
				t.Fatalf("read %s: %v (run with -update to generate)", canonPath, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("canonicalize mismatch for %s\n  got:  %q\n  want: %q", base, got, want)
			}
		})
	}
}

// --- envelope sign/verify round-trip vectors -----------------------------

// envelopeVector is the on-disk JSON shape of an envelope fixture.
// hex-encoded keypair keeps the fixture readable. The unsigned
// envelope is the input to Sign; signed is the expected output.
type envelopeVector struct {
	Description       string          `json:"description"`
	ActorPrivateKeyHex string         `json:"actor_private_key_hex"` // 64-hex (32-byte seed)
	ActorPublicKeyHex  string         `json:"actor_public_key_hex"`  // 64-hex (32-byte public)
	PublishedFixed     string          `json:"published_fixed"`        // RFC3339Nano timestamp for determinism
	EnvelopeUnsigned   json.RawMessage `json:"envelope_unsigned"`
	CanonicalBytesB64  string          `json:"canonical_bytes_b64"`
	SignatureValueB64  string          `json:"signature_value_b64"`
	ExpectedVerify     bool            `json:"expected_verify"`
}

func TestConformance_Envelope(t *testing.T) {
	dir := filepath.Join(conformanceDir, "envelope")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var v envelopeVector
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			// Reconstruct the keypair from the hex seed.
			seed, err := hex.DecodeString(v.ActorPrivateKeyHex)
			if err != nil || len(seed) != ed25519.SeedSize {
				t.Fatalf("bad actor_private_key_hex: %v / len=%d", err, len(seed))
			}
			priv := ed25519.NewKeyFromSeed(seed)
			pub := priv.Public().(ed25519.PublicKey)
			pubHex := hex.EncodeToString(pub)
			if pubHex != v.ActorPublicKeyHex {
				t.Fatalf("fixture pubkey drift: %s vs %s", pubHex, v.ActorPublicKeyHex)
			}

			// Decode the unsigned envelope into our typed shape so
			// Sign sees exactly what the fixture intended.
			env, err := federation.Unmarshal(v.EnvelopeUnsigned)
			if err != nil {
				t.Fatalf("unmarshal unsigned: %v", err)
			}
			// Pin published to the fixture timestamp so the canonical
			// bytes are reproducible.
			if v.PublishedFixed != "" {
				t0, err := time.Parse(time.RFC3339Nano, v.PublishedFixed)
				if err != nil {
					t.Fatalf("parse fixed timestamp: %v", err)
				}
				env.Published = t0
			}

			// Canonical bytes of the unsigned envelope MUST match
			// the fixture's recorded canonical_bytes_b64.
			gotCanon, err := env.CanonicalSigningBytes()
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			gotCanonB64 := base64.StdEncoding.EncodeToString(gotCanon)

			// Sign deterministically: Ed25519 is deterministic given
			// (private key, message), so signing the same envelope
			// twice always produces the same signature.
			privPEM, err := federation.PrivateKeyToPEM(priv)
			if err != nil {
				t.Fatalf("priv to PEM: %v", err)
			}
			if err := env.Sign(privPEM, "https://test.example/users/test#main-key"); err != nil {
				t.Fatalf("sign: %v", err)
			}
			gotSig := env.Signature.Value

			// Verify always: the signature we just produced MUST
			// pass Verify under the same actor pubkey.
			pubPEM, err := federation.PublicKeyToPEM(pub)
			if err != nil {
				t.Fatalf("pub to PEM: %v", err)
			}
			verifyErr := env.Verify(pubPEM)
			actualVerify := verifyErr == nil

			if *updateFixtures {
				v.CanonicalBytesB64 = gotCanonB64
				v.SignatureValueB64 = gotSig
				v.ExpectedVerify = actualVerify
				out, _ := json.MarshalIndent(v, "", "  ")
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatalf("update fixture: %v", err)
				}
				t.Logf("updated %s", path)
				return
			}

			if gotCanonB64 != v.CanonicalBytesB64 {
				t.Errorf("canonical bytes mismatch\n  got:  %s\n  want: %s",
					gotCanonB64, v.CanonicalBytesB64)
			}
			if gotSig != v.SignatureValueB64 {
				t.Errorf("signature mismatch\n  got:  %s\n  want: %s",
					gotSig, v.SignatureValueB64)
			}
			if actualVerify != v.ExpectedVerify {
				t.Errorf("verify result mismatch: got=%v want=%v (err=%v)",
					actualVerify, v.ExpectedVerify, verifyErr)
			}
		})
	}
}

// --- reject vectors ------------------------------------------------------

// rejectVector describes a fixture envelope that MUST be rejected
// at the parser layer. expected_error names the sentinel error
// the implementation MUST return.
type rejectVector struct {
	Description   string          `json:"description"`
	Envelope      json.RawMessage `json:"envelope"`
	ExpectedError string          `json:"expected_error"` // name of the package sentinel
}

func TestConformance_Reject(t *testing.T) {
	dir := filepath.Join(conformanceDir, "reject")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	// Sentinel-name → error map. The fixture spells the sentinel
	// by name; the harness looks it up so the wire format stays
	// implementation-agnostic.
	sentinels := map[string]error{
		"ErrInvalidContext":   federation.ErrInvalidContext,
		"ErrMissingField":     federation.ErrMissingField,
		"ErrInvalidType":      federation.ErrInvalidType,
		"ErrInvalidPublished": federation.ErrInvalidPublished,
		"ErrUnsigned":         federation.ErrUnsigned,
		"ErrUnsupportedAlg":   federation.ErrUnsupportedAlg,
		"ErrSigInvalid":       federation.ErrSigInvalid,
		"ErrSigMalformed":     federation.ErrSigMalformed,
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var v rejectVector
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			expected, ok := sentinels[v.ExpectedError]
			if !ok {
				t.Fatalf("unknown sentinel name %q (add to sentinels map)", v.ExpectedError)
			}
			_, err = federation.Unmarshal(v.Envelope)
			if err == nil {
				t.Fatalf("expected rejection (%s), got nil", v.ExpectedError)
			}
			if !errorsIs(err, expected) {
				t.Errorf("rejection reason mismatch\n  got:  %v\n  want: %v",
					err, expected)
			}
		})
	}
}

// errorsIs is a local helper; can't import errors package only to
// alias errors.Is in this one file. Inlined to keep dependencies
// out of the conformance harness.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- NaCl-box vectors ----------------------------------------------------

// naclVector documents one multi-recipient NaCl-box envelope with
// known ephemeral keys + per-recipient nonces for full
// determinism. Implementations encrypt with the supplied
// ephemeral/nonces and confirm ciphertexts match; decrypt with
// each recipient key and confirm plaintext matches.
type naclVector struct {
	Description       string   `json:"description"`
	PlaintextHex      string   `json:"plaintext_hex"`
	EphemeralPrivHex  string   `json:"ephemeral_priv_hex"`
	EphemeralPubHex   string   `json:"ephemeral_pub_hex"`
	RecipientPubHex   []string `json:"recipient_pub_hex"`
	RecipientPrivHex  []string `json:"recipient_priv_hex"`
	NonceHex          []string `json:"nonce_hex"`
	CiphertextHex     []string `json:"ciphertext_hex"`
}

func TestConformance_NaCl(t *testing.T) {
	dir := filepath.Join(conformanceDir, "nacl")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var v naclVector
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			plain, _ := hex.DecodeString(v.PlaintextHex)
			ephPrivBytes, _ := hex.DecodeString(v.EphemeralPrivHex)
			ephPubBytes, _ := hex.DecodeString(v.EphemeralPubHex)
			var ephPriv, ephPub [32]byte
			copy(ephPriv[:], ephPrivBytes)
			copy(ephPub[:], ephPubBytes)

			rxPubs := make([][]byte, len(v.RecipientPubHex))
			for i, h := range v.RecipientPubHex {
				b, _ := hex.DecodeString(h)
				rxPubs[i] = b
			}
			nonces := make([][]byte, len(v.NonceHex))
			for i, h := range v.NonceHex {
				b, _ := hex.DecodeString(h)
				nonces[i] = b
			}

			sealed, err := nacl.SealDet(plain, rxPubs, &ephPub, &ephPriv, nonces)
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			if !bytes.Equal(sealed.EphemeralPublicKey, ephPubBytes) {
				t.Errorf("ephemeral public key mismatch")
			}
			if *updateFixtures {
				v.CiphertextHex = make([]string, len(sealed.Recipients))
				for i, r := range sealed.Recipients {
					v.CiphertextHex[i] = hex.EncodeToString(r.Ciphertext)
				}
				out, _ := json.MarshalIndent(v, "", "  ")
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatalf("update: %v", err)
				}
				t.Logf("updated %s", path)
				return
			}

			if len(sealed.Recipients) != len(v.CiphertextHex) {
				t.Fatalf("recipient count mismatch: got=%d want=%d",
					len(sealed.Recipients), len(v.CiphertextHex))
			}
			for i, r := range sealed.Recipients {
				wantCT, _ := hex.DecodeString(v.CiphertextHex[i])
				if !bytes.Equal(r.Ciphertext, wantCT) {
					t.Errorf("recipient %d ciphertext mismatch", i)
				}
				// Decrypt with the recipient's private key + confirm
				// plaintext round-trip.
				rxPriv, _ := hex.DecodeString(v.RecipientPrivHex[i])
				got, err := nacl.Open(r.Ciphertext, r.Nonce, sealed.EphemeralPublicKey, rxPriv)
				if err != nil {
					t.Errorf("recipient %d decrypt: %v", i, err)
					continue
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("recipient %d plaintext round-trip mismatch", i)
				}
			}
		})
	}
}
