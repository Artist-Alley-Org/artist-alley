// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func TestGenerateActorKeyPairShapes(t *testing.T) {
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public key size: got %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("private key size: got %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	// Public key derived from private key MUST match.
	derived, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("private key Public() did not return ed25519.PublicKey")
	}
	if string(derived) != string(pub) {
		t.Error("public key drift between returned pub and priv.Public()")
	}
}

func TestPublicKeyPEMRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatalf("to PEM: %v", err)
	}
	if !strings.Contains(string(pem), "-----BEGIN PUBLIC KEY-----") {
		t.Errorf("PEM blob doesn't look right: %q", string(pem)[:60])
	}
	got, err := federation.PublicKeyFromPEM(pem)
	if err != nil {
		t.Fatalf("from PEM: %v", err)
	}
	if string(got) != string(pub) {
		t.Error("public key drift through PEM round-trip")
	}
}

func TestPrivateKeyPEMRoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := federation.PrivateKeyToPEM(priv)
	if err != nil {
		t.Fatalf("to PEM: %v", err)
	}
	if !strings.Contains(string(pem), "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("private PEM blob doesn't look right: %q", string(pem)[:60])
	}
	got, err := federation.PrivateKeyFromPEM(pem)
	if err != nil {
		t.Fatalf("from PEM: %v", err)
	}
	if string(got) != string(priv) {
		t.Error("private key drift through PEM round-trip")
	}
}

func TestPublicKeyFromPEMRejectsMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("not pem at all"),
		[]byte("-----BEGIN PUBLIC KEY-----\nbm90LXZhbGlkLWJhc2U2NA==\n-----END PUBLIC KEY-----\n"),
	}
	for i, c := range cases {
		_, err := federation.PublicKeyFromPEM(c)
		if err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestPublicKeyFromPEMRejectsWrongAlgorithm(t *testing.T) {
	// A valid PKIX PEM but for a non-Ed25519 key. Build by
	// taking a private PKCS#8 ECDSA-shaped blob? Easier: pass a
	// PRIVATE-KEY-typed PEM where PUBLIC-KEY is expected.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pem, _ := federation.PrivateKeyToPEM(priv)
	_, err := federation.PublicKeyFromPEM(pem)
	if !errors.Is(err, federation.ErrPEMMalformed) {
		t.Errorf("wrong-block-type: expected ErrPEMMalformed, got %v", err)
	}
}

func TestPublicKeyFingerprintDifferentKeysDiffer(t *testing.T) {
	a, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _ := ed25519.GenerateKey(rand.Reader)
	fpA := federation.PublicKeyFingerprint(a)
	fpB := federation.PublicKeyFingerprint(b)
	if fpA == fpB {
		t.Error("two distinct keys produced the same fingerprint")
	}
	if federation.PublicKeyFingerprint(ed25519.PublicKey{}) != "" {
		t.Error("empty key should produce empty fingerprint")
	}
}

func TestVerifyRejectsBadLength(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	err := federation.Verify(pub, []byte("msg"), []byte{0xab, 0xcd})
	if !errors.Is(err, federation.ErrSigMalformed) {
		t.Errorf("expected ErrSigMalformed, got %v", err)
	}
}

func TestVerifyRejectsWrongSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	bogus := make([]byte, ed25519.SignatureSize) // all zeros
	err := federation.Verify(pub, []byte("msg"), bogus)
	if !errors.Is(err, federation.ErrSigInvalid) {
		t.Errorf("expected ErrSigInvalid, got %v", err)
	}
}

func TestSignProducesVerifiableSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := []byte("the message")
	sig := federation.Sign(priv, msg)
	if err := federation.Verify(pub, msg, sig); err != nil {
		t.Errorf("round trip Sign→Verify failed: %v", err)
	}
}
