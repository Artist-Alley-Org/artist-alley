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
	Description        string          `json:"description"`
	ActorPrivateKeyHex string          `json:"actor_private_key_hex"` // 64-hex (32-byte seed)
	ActorPublicKeyHex  string          `json:"actor_public_key_hex"`  // 64-hex (32-byte public)
	PublishedFixed     string          `json:"published_fixed"`       // RFC3339Nano timestamp for determinism
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
	Description      string   `json:"description"`
	PlaintextHex     string   `json:"plaintext_hex"`
	EphemeralPrivHex string   `json:"ephemeral_priv_hex"`
	EphemeralPubHex  string   `json:"ephemeral_pub_hex"`
	RecipientPubHex  []string `json:"recipient_pub_hex"`
	RecipientPrivHex []string `json:"recipient_priv_hex"`
	NonceHex         []string `json:"nonce_hex"`
	CiphertextHex    []string `json:"ciphertext_hex"`
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

// --- archival-1 profile vectors (#1513, v1.md §16-§17) --------------------

// archivalVector is the on-disk shape of an archival-1 vector. Each
// vector names the check that runs NOW (enforcement.now) and the
// issue that wires it into the running inbox / ingest path
// (enforcement.pending); a vector passing here proves the contract's
// rule is executable, not that a receiver enforces it at runtime.
type archivalVector struct {
	Description string          `json:"description"`
	Spec        string          `json:"spec"`
	Stage       string          `json:"stage"`
	Negotiated  bool            `json:"negotiated"`
	Envelope    json.RawMessage `json:"envelope"`
	Expected    struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	} `json:"expected"`
	Enforcement struct {
		Now     string `json:"now"`
		Pending string `json:"pending"`
	} `json:"enforcement"`
	Received *struct {
		File     int    `json:"file"`
		BytesB64 string `json:"bytes_b64"`
	} `json:"received,omitempty"`
}

// archivalPendingOwners are the open issues that own runtime
// enforcement of archival-1 (#1513 stage): parser / inbox gating and
// sender refusal (#1580), CAS fetch (#72), ingest (#1574), lifecycle
// (#1575), metadata and provenance mapping (#1577).
var archivalPendingOwners = []string{"#1580", "#72", "#1574", "#1575", "#1577"}

func archivalReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errorsIs(err, federation.ErrProfileNotNegotiated):
		return string(federation.InboxStatusProfileNotNegotiated)
	case errorsIs(err, federation.ErrFixityMismatch):
		return string(federation.InboxStatusFixityMismatch)
	case errorsIs(err, federation.ErrArchivalInvalid):
		return string(federation.InboxStatusArchivalInvalid)
	default:
		return "unexpected error: " + err.Error()
	}
}

func TestConformance_Archival1(t *testing.T) {
	dir := filepath.Join(conformanceDir, "archival-1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var v archivalVector
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("%s: fixture shape: %v", name, err)
		}
		counts[v.Stage+"/"+v.Expected.Outcome]++
		t.Run(name, func(t *testing.T) {
			if v.Description == "" || !strings.Contains(v.Spec, "§1") || v.Enforcement.Now == "" {
				t.Fatalf("description, spec section and enforcement.now are required")
			}
			owned := false
			for _, o := range archivalPendingOwners {
				owned = owned || strings.HasPrefix(v.Enforcement.Pending, o+":") || strings.HasPrefix(v.Enforcement.Pending, o+" ")
			}
			if !owned {
				t.Fatalf("enforcement.pending %q must name the owning issue (%v)", v.Enforcement.Pending, archivalPendingOwners)
			}
			switch v.Expected.Outcome {
			case "accept":
				if v.Expected.Reason != "" {
					t.Fatalf("an accepted vector has no reason")
				}
			case "reject":
				if _, ok := federation.KnownInboxStatuses[federation.InboxStatus(v.Expected.Reason)]; !ok {
					t.Fatalf("reason %q is not a §12.1 inbox status", v.Expected.Reason)
				}
			default:
				t.Fatalf("outcome %q", v.Expected.Outcome)
			}
			// Every vector is an otherwise valid v1 envelope, so the
			// only thing under test is the archival-1 part.
			env, err := federation.Unmarshal(v.Envelope)
			if err != nil {
				t.Fatalf("not a valid v1 envelope: %v", err)
			}
			var got string
			switch v.Stage {
			case "schema", "negotiation":
				got = archivalReason(federation.ValidateArchivalMembers(env, v.Negotiated))
			case "fixity":
				if err := federation.ValidateArchivalMembers(env, true); err != nil {
					t.Fatalf("fixity vector's asset is not schema-valid: %v", err)
				}
				asset, err := federation.ParseArchivalAsset(env.Extra[federation.MemberArchivalAsset])
				if err != nil || v.Received == nil || v.Received.File >= len(asset.Files) {
					t.Fatalf("fixity vector needs an asset and received.file: %v", err)
				}
				b, err := base64.StdEncoding.DecodeString(v.Received.BytesB64)
				if err != nil {
					t.Fatal(err)
				}
				got = archivalReason(federation.VerifyFileFixity(asset.Files[v.Received.File], bytes.NewReader(b)))
			case "round-trip":
				got = archivalReason(federation.ValidateArchivalMembers(env, true))
				out, err := env.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				in, err := federation.Canonicalize(v.Envelope)
				if err != nil {
					t.Fatal(err)
				}
				back, err := federation.Canonicalize(out)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(in, back) {
					t.Errorf("round trip changed the envelope\n in:  %s\n out: %s", in, back)
				}
			default:
				t.Fatalf("stage %q", v.Stage)
			}
			if got != v.Expected.Reason {
				t.Errorf("got %q want %q (%s)", got, v.Expected.Reason, v.Description)
			}
		})
	}
	// Deleting vectors must not make this pass: every stage keeps its
	// accept and reject cases.
	for _, need := range []string{"schema/accept", "schema/reject", "negotiation/reject", "fixity/accept", "fixity/reject", "round-trip/accept"} {
		if counts[need] == 0 {
			t.Errorf("no %s vector", need)
		}
	}
}
