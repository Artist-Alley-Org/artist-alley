// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit tests for the Phase 1.22.I-e encryption primitives + the
// EncryptionBlock JSON round-trip. No DB dependency.

package federation

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// freshKeypair mints an X25519 keypair via crypto/ecdh (same path
// federation_user_keys uses). Returns the raw 32-byte private
// scalar + 32-byte public key.
func freshKeypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("X25519 GenerateKey: %v", err)
	}
	return k.Bytes(), k.PublicKey().Bytes()
}

// --- primitive tests ---------------------------------------------

func TestEncryptActivityPayload_ProducesCorrectShapes(t *testing.T) {
	senderPriv, _ := freshKeypair(t)
	_, recipientPub := freshKeypair(t)
	plaintext := []byte(`{"type":"Like","object":"https://b.local/posts/x"}`)

	nonce, ciphertext, err := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(nonce) != NaClBoxNonceLen {
		t.Errorf("nonce len = %d, want %d", len(nonce), NaClBoxNonceLen)
	}
	// box.Seal output = ciphertext + 16-byte auth tag.
	wantMinCiphertext := len(plaintext) + 16
	if len(ciphertext) < wantMinCiphertext {
		t.Errorf("ciphertext len = %d, want >= %d (plaintext + overhead)", len(ciphertext), wantMinCiphertext)
	}
}

func TestEncryptActivityPayload_RoundtripDecrypts(t *testing.T) {
	senderPriv, senderPub := freshKeypair(t)
	recipientPriv, recipientPub := freshKeypair(t)
	plaintext := []byte(`{"type":"Comment","content":"hi"}`)

	nonce, ciphertext, err := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := DecryptActivityPayload(ciphertext, nonce, recipientPriv, senderPub)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt got %q, want %q", got, plaintext)
	}
}

func TestEncryptActivityPayload_NoncesAreUniqueAcrossCalls(t *testing.T) {
	// NaCl-box nonce reuse with the same keypair is catastrophic
	// (XSalsa20 reuse leaks plaintext); the primitive's only
	// defence is fresh crypto/rand per call. Run a small batch
	// + assert no collisions. 32 calls × 24 bytes random has
	// negligible birthday probability (~10^-49); a regression
	// where the nonce becomes static would surface immediately.
	senderPriv, _ := freshKeypair(t)
	_, recipientPub := freshKeypair(t)
	plaintext := []byte("payload")

	seen := make(map[string]struct{}, 32)
	for i := range 32 {
		nonce, _, err := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
		if err != nil {
			t.Fatalf("Encrypt #%d: %v", i, err)
		}
		key := string(nonce)
		if _, dup := seen[key]; dup {
			t.Fatalf("nonce collision on call #%d", i)
		}
		seen[key] = struct{}{}
	}
}

func TestEncryptActivityPayload_SamePlaintextProducesDifferentCiphertext(t *testing.T) {
	// Symmetric to the nonce-uniqueness test: even with the same
	// plaintext + same keypair, the ciphertext must differ
	// because the nonce differs. Direct guard against a regression
	// that hardcoded the nonce or recycled the previous call's.
	senderPriv, _ := freshKeypair(t)
	_, recipientPub := freshKeypair(t)
	plaintext := []byte(`{"type":"Like"}`)

	_, c1, _ := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	_, c2, _ := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	if bytes.Equal(c1, c2) {
		t.Errorf("identical ciphertext from two Encrypts with same input — nonce reuse?")
	}
}

func TestEncryptActivityPayload_WrongRecipientKey_DecryptFails(t *testing.T) {
	senderPriv, senderPub := freshKeypair(t)
	_, recipientPub := freshKeypair(t)
	wrongPriv, _ := freshKeypair(t)
	plaintext := []byte("secret payload")

	nonce, ciphertext, err := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, err = DecryptActivityPayload(ciphertext, nonce, wrongPriv, senderPub)
	if !errors.Is(err, ErrEncryptionDecryptFailed) {
		t.Errorf("expected ErrEncryptionDecryptFailed with wrong recipient priv; got %v", err)
	}
}

func TestEncryptActivityPayload_TamperedCiphertext_DecryptFails(t *testing.T) {
	// The Poly1305 tag in NaCl-box is what catches tampering.
	// Flipping a byte mid-ciphertext must surface as
	// ErrEncryptionDecryptFailed rather than producing garbage
	// plaintext or panicking.
	senderPriv, senderPub := freshKeypair(t)
	recipientPriv, recipientPub := freshKeypair(t)
	plaintext := []byte(`{"type":"Like"}`)

	nonce, ciphertext, err := EncryptActivityPayload(plaintext, senderPriv, recipientPub)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext[len(ciphertext)/2] ^= 0xFF
	_, err = DecryptActivityPayload(ciphertext, nonce, recipientPriv, senderPub)
	if !errors.Is(err, ErrEncryptionDecryptFailed) {
		t.Errorf("expected ErrEncryptionDecryptFailed on tampered ciphertext; got %v", err)
	}
}

func TestEncryptActivityPayload_MalformedKeys_ReturnTypedError(t *testing.T) {
	good, _ := freshKeypair(t)
	for _, badLen := range []int{0, 1, 31, 33, 64} {
		short := make([]byte, badLen)
		_, _, err := EncryptActivityPayload([]byte("x"), short, good)
		if !errors.Is(err, ErrEncryptionMalformedKey) {
			t.Errorf("senderPriv len=%d: err = %v, want ErrEncryptionMalformedKey", badLen, err)
		}
		_, _, err = EncryptActivityPayload([]byte("x"), good, short)
		if !errors.Is(err, ErrEncryptionMalformedKey) {
			t.Errorf("recipientPub len=%d: err = %v, want ErrEncryptionMalformedKey", badLen, err)
		}
	}
}

func TestDecryptActivityPayload_MalformedNonce_ReturnsTypedError(t *testing.T) {
	senderPriv, senderPub := freshKeypair(t)
	recipientPriv, recipientPub := freshKeypair(t)
	_, ciphertext, _ := EncryptActivityPayload([]byte("x"), senderPriv, recipientPub)
	_, err := DecryptActivityPayload(ciphertext, []byte("short"), recipientPriv, senderPub)
	if !errors.Is(err, ErrEncryptionMalformedKey) {
		t.Errorf("expected ErrEncryptionMalformedKey on short nonce; got %v", err)
	}
}

// --- EncryptionBlock JSON round-trip -------------------------------

func TestEncryptionBlock_JSONRoundtrip(t *testing.T) {
	in := EncryptionBlock{
		Algorithm:           EncryptionAlgNaClBoxV1,
		SenderKeyID:         "https://studio-a.local/users/alice#encryption-key",
		SenderKeyVersion:    1,
		RecipientKeyID:      "https://studio-b.local/users/bob#encryption-key",
		RecipientKeyVersion: 2,
		Nonce:               Base64Bytes("twenty-four-bytes-of-non"),
		Ciphertext:          Base64Bytes("sealed-payload-bytes"),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Sanity: byte fields encode as base64-std strings.
	if !strings.Contains(string(raw), `"nonce":"`) {
		t.Errorf("expected nonce as JSON string in output: %s", raw)
	}

	var out EncryptionBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Algorithm != in.Algorithm {
		t.Errorf("Algorithm drift: got %q want %q", out.Algorithm, in.Algorithm)
	}
	if out.SenderKeyVersion != in.SenderKeyVersion {
		t.Errorf("SenderKeyVersion drift: got %d want %d", out.SenderKeyVersion, in.SenderKeyVersion)
	}
	if !bytes.Equal(out.Nonce, in.Nonce) {
		t.Errorf("Nonce drift: got %q want %q", out.Nonce, in.Nonce)
	}
	if !bytes.Equal(out.Ciphertext, in.Ciphertext) {
		t.Errorf("Ciphertext drift: got %q want %q", out.Ciphertext, in.Ciphertext)
	}
}

// --- Envelope-level encryption presence/absence -------------------

func TestEnvelope_WithEncryptionBlock_RoundtripsThroughMarshal(t *testing.T) {
	// The Envelope.Encryption field is omitempty + the
	// Marshal/Unmarshal pair handles the named field. Verify it
	// round-trips through the strict-parse path that catches
	// unknown top-level fields.
	env := &Envelope{
		Context: ContextV1,
		Type:    ActivityLike,
		ID:      "https://a.local/activities/uuid",
		Actor:   "https://a.local/users/alice",
		To:      []string{"https://b.local/users/bob"},
		Encryption: &EncryptionBlock{
			Algorithm:           EncryptionAlgNaClBoxV1,
			SenderKeyID:         "https://a.local/users/alice#encryption-key",
			SenderKeyVersion:    1,
			RecipientKeyID:      "https://b.local/users/bob#encryption-key",
			RecipientKeyVersion: 1,
			Nonce:               Base64Bytes(make([]byte, NaClBoxNonceLen)),
			Ciphertext:          Base64Bytes("sealed"),
		},
		Signature: &Signature{Type: SignatureAlgEd25519, PublicKey: "https://a.local/users/alice#main-key", Value: "AAAAAAAAAAAA"},
	}
	raw, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"encryption":{`) {
		t.Errorf("expected encryption block in wire body; body=%s", raw)
	}
	if !strings.Contains(string(raw), `"sender_key_id":`) {
		t.Errorf("expected sender_key_id in wire body")
	}

	parsed, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.Encryption == nil {
		t.Fatalf("encryption block lost on roundtrip")
	}
	if parsed.Encryption.Algorithm != EncryptionAlgNaClBoxV1 {
		t.Errorf("algorithm drift: got %q want %q", parsed.Encryption.Algorithm, EncryptionAlgNaClBoxV1)
	}
	if parsed.Encryption.SenderKeyVersion != 1 {
		t.Errorf("sender_key_version drift: got %d want 1", parsed.Encryption.SenderKeyVersion)
	}
}

func TestEnvelope_WithoutEncryption_OmitsField(t *testing.T) {
	// omitempty regression: a plaintext envelope MUST NOT carry
	// `"encryption":null` on the wire. Wire-format extensions
	// from later phases (v0.6 + v0.7) get parsed by v0.5 receivers
	// only when the field is genuinely absent.
	env := &Envelope{
		Context:   ContextV1,
		Type:      ActivityLike,
		ID:        "https://a.local/activities/uuid",
		Actor:     "https://a.local/users/alice",
		To:        []string{"https://b.local/users/bob"},
		Signature: &Signature{Type: SignatureAlgEd25519, PublicKey: "https://a.local/users/alice#main-key", Value: "AAAAAAAAAAAA"},
	}
	raw, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"encryption"`) {
		t.Errorf("plaintext envelope unexpectedly carries encryption field: %s", raw)
	}
}
