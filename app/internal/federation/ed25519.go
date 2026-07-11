// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Thin wrappers around crypto/ed25519 (stdlib) for the
// federation envelope's signature scheme + PEM serialization
// for actor public-key publication.
//
// Spec reference: docs/spec/federation/v1.md §5.

package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// Public-key PEM block type per RFC 8410 §10 — the same wrapper
// `openssl pkey -text` produces for an Ed25519 public key.
const pemPublicKeyType = "PUBLIC KEY"

// Private-key PEM block type per RFC 8410 §10 (PKCS#8). We hold
// PEM-wrapped private keys at the user-handling layer ONLY when
// generating; at-rest storage is the raw bytes wrapped by
// app/internal/atrest. Never expose this PEM on the wire.
const pemPrivateKeyType = "PRIVATE KEY"

// Errors callers may distinguish on.
var (
	// ErrSigInvalid indicates the signature did not verify under
	// the supplied public key. Collapsed (not split into
	// "wrong-length" / "tag-mismatch" / "wrong-key") to avoid
	// leaking which specific failure happened — useful for
	// side-channel resistance.
	ErrSigInvalid = errors.New("federation: signature does not verify")

	// ErrSigMalformed indicates the signature bytes themselves
	// are the wrong length or otherwise structurally invalid.
	// Distinct from ErrSigInvalid because the caller usually
	// surfaces this as InboxStatusSigMalformed (a parser error)
	// rather than InboxStatusSigInvalid (a cryptographic verify
	// failure).
	ErrSigMalformed = errors.New("federation: signature bytes malformed")

	// ErrPEMMalformed indicates a PEM blob couldn't be parsed.
	ErrPEMMalformed = errors.New("federation: PEM blob malformed")

	// ErrWrongKeyAlgorithm indicates a PEM blob decoded into a
	// key but the key wasn't Ed25519. v1 supports only Ed25519
	// per docs/spec/federation/v1.md §5.5.
	ErrWrongKeyAlgorithm = errors.New("federation: key is not Ed25519")
)

// GenerateActorKeyPair returns a fresh Ed25519 keypair for an
// actor (a user). Wraps ed25519.GenerateKey(crypto/rand) — the
// thin wrapper is here so the package's only entry point for new
// keys lives in one place (easier to swap in a hardware-backed
// generator if we ever want one).
func GenerateActorKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("federation: generate keypair: %w", err)
	}
	return pub, priv, nil
}

// Sign produces the 64-byte Ed25519 signature over msg with priv.
// The caller is responsible for canonicalizing msg first — Sign
// does not.
func Sign(priv ed25519.PrivateKey, msg []byte) []byte {
	return ed25519.Sign(priv, msg)
}

// Verify checks sig under pub for msg. Returns nil on success,
// ErrSigMalformed if the signature is the wrong length, or
// ErrSigInvalid if the signature is the right shape but doesn't
// authenticate.
//
// Callers that want to map errors to the InboxStatus catalogue
// inspect (errors.Is) which return they got.
func Verify(pub ed25519.PublicKey, msg, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return ErrSigMalformed
	}
	if !ed25519.Verify(pub, msg, sig) {
		return ErrSigInvalid
	}
	return nil
}

// PublicKeyToPEM serializes pub in PKIX/PEM form (RFC 8410). The
// resulting bytes are what gets published at the actor's
// `#main-key` URL fragment + what goes into the
// user.public_key_pem database column.
func PublicKeyToPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("federation: marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemPublicKeyType, Bytes: der}), nil
}

// PublicKeyFromPEM decodes a PEM-wrapped Ed25519 public key.
// Returns ErrPEMMalformed when the PEM itself can't be parsed,
// ErrWrongKeyAlgorithm when the PEM contains a non-Ed25519 key.
func PublicKeyFromPEM(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemPublicKeyType {
		return nil, ErrPEMMalformed
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPEMMalformed, err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, ErrWrongKeyAlgorithm
	}
	return pub, nil
}

// PrivateKeyToPEM serializes priv in PKCS#8/PEM form. Used ONLY
// at generation time before handing to the at-rest encryption
// wrapper. PEM-wrapped private keys MUST NOT leak out of this
// package or appear in logs.
func PrivateKeyToPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("federation: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemPrivateKeyType, Bytes: der}), nil
}

// PrivateKeyFromPEM decodes a PEM-wrapped Ed25519 private key.
// Returns ErrPEMMalformed / ErrWrongKeyAlgorithm on shape errors.
func PrivateKeyFromPEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemPrivateKeyType {
		return nil, ErrPEMMalformed
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPEMMalformed, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, ErrWrongKeyAlgorithm
	}
	return priv, nil
}

// PublicKeyFingerprint returns the lowercase hex SHA-256 of the
// 32 raw public-key bytes (NOT of the PEM wrapper). Useful in
// audit logs + admin UI where a short stable handle for a key
// is more readable than the full PEM.
func PublicKeyFingerprint(pub ed25519.PublicKey) string {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}
