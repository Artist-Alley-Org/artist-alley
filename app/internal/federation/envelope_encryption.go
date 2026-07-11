// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-e — per-recipient NaCl-box envelope encryption.
//
// This file defines the wire-shape struct ([EncryptionBlock]) +
// the encrypt/decrypt primitives. Consumers:
//
//   - federation/outbox.Worker.buildEnvelope (Phase 1.22.I-e-2)
//     calls [EncryptActivityPayload] when the recipient peer has
//     negotiated e2e capability + we can resolve a recipient
//     public key from federation_remote_actors.
//
//   - federation/inbox (Phase 1.22.I-f) will call [DecryptActivityPayload]
//     to recover the original Extra map before dispatch.
//
// # Wire shape
//
// The block is the value of the envelope's "encryption" field
// (see [Envelope.Encryption]). Per ADR 0049 §Track B Decision 2:
// per-recipient, sender's permanent X25519 keypair (not ephemeral),
// 24-byte fresh-random nonce per emission, base64-std for byte
// fields.
//
// # Algorithm
//
// v0.5 supports `nacl-box` only — that's X25519 + XSalsa20 + Poly1305
// per https://nacl.cr.yp.to/box.html. The Algorithm field is
// explicit so a future addition (Hybrid PQ KEM, ChaCha20-Poly1305,
// etc.) lands as a new typed constant + a reader dispatch case
// without a wire-format change.
//
// # Key rotation safety
//
// SenderKeyVersion + RecipientKeyVersion travel inside the block
// so the receiver knows which retained version to try for decrypt
// during the 7-day rotation grace window (Phase 1.22.I-h). The
// sender records the version it used; the receiver dispatches
// on its own retained-key set.
//
// # Per-emission unwrap
//
// The sender's private key is master-key-wrapped at rest (Phase
// 1.22.I-b). Callers MUST unwrap per emission + zero the bytes
// after [box.Seal] returns. [EncryptActivityPayload] takes the
// already-unwrapped 32-byte slice so the unwrap+zero discipline
// lives at the dispatcher boundary, not inside this primitive.

package federation

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// EncryptionAlgNaClBoxV1 is the canonical algorithm token for
// the v0.5 per-recipient NaCl-box construction. Distinct value
// from [EncryptionAlgNaClBox] (the older 1.22.A multi-recipient
// scaffolding constant) so the dispatch code can branch
// unambiguously on the wire format.
const EncryptionAlgNaClBoxV1 = "nacl-box-v1"

// NaClBoxKeyLen is the size of an X25519 key (private or public).
// RFC 7748. Used to length-check inputs at the primitive boundary
// so a misconfigured caller fails with [ErrEncryptionMalformedKey]
// instead of a panic inside nacl/box.
const NaClBoxKeyLen = 32

// NaClBoxNonceLen is the NaCl-box nonce length per the libsodium
// reference. Tests assert the generated nonce length matches.
const NaClBoxNonceLen = 24

// EncryptionBlock is the typed wire shape carried in the envelope's
// "encryption" field. JSON marshaling uses the standard reflection
// path; the byte fields encode as base64-std strings (the typed
// alias on the field handles conversion transparently).
type EncryptionBlock struct {
	// Algorithm is the construction token. v0.5 emits
	// [EncryptionAlgNaClBoxV1] only; future algorithms land as
	// additional constants.
	Algorithm string `json:"algorithm"`

	// SenderKeyID is the URL fragment of the sender's actor + the
	// "#encryption-key" anchor that the receiver uses to look up
	// the sender's published public key (cached via the I-c
	// remote-actor cache).
	SenderKeyID      string `json:"sender_key_id"`
	SenderKeyVersion int32  `json:"sender_key_version"`

	// RecipientKeyID mirrors SenderKeyID for the recipient. The
	// receiver uses RecipientKeyVersion to pick the right private
	// key from federation_user_keys during the 7-day rotation
	// grace window (Phase 1.22.I-h).
	RecipientKeyID      string `json:"recipient_key_id"`
	RecipientKeyVersion int32  `json:"recipient_key_version"`

	// Nonce + Ciphertext are base64-std byte fields. Nonce is
	// always 24 bytes (NaCl-box convention); Ciphertext is the
	// sealed payload (plaintext_len + box.Overhead bytes).
	Nonce      Base64Bytes `json:"nonce"`
	Ciphertext Base64Bytes `json:"ciphertext"`
}

// Base64Bytes wraps a []byte with MarshalJSON / UnmarshalJSON that
// round-trip through base64-std. Keeps the field declarations on
// [EncryptionBlock] readable as []byte at the Go level while
// emitting strings on the wire.
type Base64Bytes []byte

// MarshalJSON emits the JSON string `"<base64>"`. nil → `""`.
func (b Base64Bytes) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte(`""`), nil
	}
	encoded := base64.StdEncoding.EncodeToString(b)
	out := make([]byte, 0, len(encoded)+2)
	out = append(out, '"')
	out = append(out, encoded...)
	out = append(out, '"')
	return out, nil
}

// UnmarshalJSON accepts the JSON string `"<base64>"`. Empty string
// → empty non-nil slice (callers can check len() == 0 either way).
func (b *Base64Bytes) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return errors.New("federation: Base64Bytes: expected JSON string")
	}
	s := string(data[1 : len(data)-1])
	if s == "" {
		*b = Base64Bytes{}
		return nil
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("federation: Base64Bytes: %w", err)
	}
	*b = out
	return nil
}

// Errors callers may distinguish on.
var (
	// ErrEncryptionMalformedKey is returned when sender private
	// key bytes or recipient public key bytes are not exactly
	// [NaClBoxKeyLen] bytes. The DB CHECK constraint on
	// federation_user_keys.public_key + federation_remote_actors
	// .encryption_public_key already enforces the length at the
	// storage boundary; this is the in-process belt-and-braces
	// check so a path that bypasses the DB still fails cleanly.
	ErrEncryptionMalformedKey = errors.New("federation: encryption key must be 32 bytes")

	// ErrEncryptionDecryptFailed is the constant decrypt-side
	// error so the I-f receiver path can distinguish "wrong key
	// version" from "actual tamper or corruption" without leaking
	// the underlying nacl/box.Open failure detail (which doesn't
	// distinguish anyway, but the constant abstracts the
	// dependency).
	ErrEncryptionDecryptFailed = errors.New("federation: decrypt failed")
)

// EncryptActivityPayload seals plaintext using NaCl-box against
// the recipient's public key with the sender's private key.
// Generates a fresh 24-byte random nonce per call; tests verify
// uniqueness via the resolver's per-emission counter.
//
// The senderPriv slice MUST be exactly 32 bytes — the unwrapped
// X25519 private scalar from federation_user_keys.private_key_enc.
// The recipientPub slice MUST be exactly 32 bytes — the raw
// public key from federation_remote_actors.encryption_public_key.
// Either input being the wrong length returns
// [ErrEncryptionMalformedKey] without panicking.
//
// The returned [EncryptionBlock] is fully populated and ready to
// assign to [Envelope.Encryption]. Caller fills the Algorithm
// constant + the key-id / key-version metadata + populates the
// envelope's wire-required fields separately.
//
// Plaintext zero length is legal (encrypts to box.Overhead bytes
// of metadata + tag); callers that want to forbid zero-length
// payloads should check at their layer.
func EncryptActivityPayload(plaintext, senderPriv, recipientPub []byte) (nonce, ciphertext []byte, err error) {
	if len(senderPriv) != NaClBoxKeyLen {
		return nil, nil, fmt.Errorf("%w: sender private key is %d bytes", ErrEncryptionMalformedKey, len(senderPriv))
	}
	if len(recipientPub) != NaClBoxKeyLen {
		return nil, nil, fmt.Errorf("%w: recipient public key is %d bytes", ErrEncryptionMalformedKey, len(recipientPub))
	}

	var nonceArr [NaClBoxNonceLen]byte
	if _, err := rand.Read(nonceArr[:]); err != nil {
		return nil, nil, fmt.Errorf("federation: nonce generation: %w", err)
	}
	var senderPrivArr, recipientPubArr [NaClBoxKeyLen]byte
	copy(senderPrivArr[:], senderPriv)
	copy(recipientPubArr[:], recipientPub)

	// box.Seal appends to its first arg; we pass nil to get a
	// freshly-allocated buffer back. The sealed bytes have layout
	// `box.Overhead || ciphertext`; both nonce + sealed are
	// transmitted in the EncryptionBlock.
	sealed := box.Seal(nil, plaintext, &nonceArr, &recipientPubArr, &senderPrivArr)

	// Zero the local copies of the keypair material now that
	// nacl/box doesn't need them. The caller's senderPriv slice
	// has its own lifetime; their zero-after-use discipline is
	// what matters for the at-rest invariant.
	for i := range senderPrivArr {
		senderPrivArr[i] = 0
	}
	for i := range recipientPubArr {
		recipientPubArr[i] = 0
	}

	return nonceArr[:], sealed, nil
}

// DecryptActivityPayload opens a NaCl-box ciphertext using the
// sender's public key + the receiver's private key. Symmetric
// argument shape to [EncryptActivityPayload]: recipientPriv is
// the receiver's unwrapped 32-byte X25519 private scalar,
// senderPub is the sender's 32-byte public key (fetched from the
// I-c remote-actor cache by the receiver).
//
// Returns [ErrEncryptionDecryptFailed] on any nacl/box.Open
// failure — wrong key version, tampered ciphertext, mismatched
// keys all collapse to the same constant to avoid leaking
// information across the boundary. Returns
// [ErrEncryptionMalformedKey] for the length mismatches.
//
// Used by Phase 1.22.I-f's inbox decrypt path. Shipped here in
// I-e alongside the encrypt primitive so the round-trip tests
// can exercise both halves without cross-package mocks.
func DecryptActivityPayload(ciphertext, nonce, recipientPriv, senderPub []byte) ([]byte, error) {
	if len(recipientPriv) != NaClBoxKeyLen {
		return nil, fmt.Errorf("%w: recipient private key is %d bytes", ErrEncryptionMalformedKey, len(recipientPriv))
	}
	if len(senderPub) != NaClBoxKeyLen {
		return nil, fmt.Errorf("%w: sender public key is %d bytes", ErrEncryptionMalformedKey, len(senderPub))
	}
	if len(nonce) != NaClBoxNonceLen {
		return nil, fmt.Errorf("%w: nonce is %d bytes", ErrEncryptionMalformedKey, len(nonce))
	}

	var nonceArr [NaClBoxNonceLen]byte
	var recipientPrivArr, senderPubArr [NaClBoxKeyLen]byte
	copy(nonceArr[:], nonce)
	copy(recipientPrivArr[:], recipientPriv)
	copy(senderPubArr[:], senderPub)

	plaintext, ok := box.Open(nil, ciphertext, &nonceArr, &senderPubArr, &recipientPrivArr)

	for i := range recipientPrivArr {
		recipientPrivArr[i] = 0
	}
	for i := range senderPubArr {
		senderPubArr[i] = 0
	}

	if !ok {
		return nil, ErrEncryptionDecryptFailed
	}
	return plaintext, nil
}
