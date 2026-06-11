// Package userkeys holds the per-user X25519 keypair surface for
// Phase 1.22.I encrypted federation. The database schema lives in
// migration 00007_federation_user_keys.sql; sqlc-generated query
// types live alongside this file.
//
// # Scope (I-b)
//
// This commit ships the STORAGE LAYER ONLY: the sqlc queries +
// crypto primitives (Generate / Unwrap) + their tests. There is
// no HTTP surface and no integration with the federation wire.
// Consumer wiring into the three user-create paths (bootstrap,
// /setup, /admin/seed/users) lands in the I-b-3 commit; future
// phases consume these primitives in turn:
//
//   - I-c — actor profile inline `publicKeys` block. Consumes
//     [Queries.ListPublicKeysByUser].
//   - I-e — outbox encryption. Consumes [Queries.GetCurrentUserKey]
//     to fetch the recipient's encryption target.
//   - I-f — inbox decryption. Consumes [Queries.GetUserKeyByVersion]
//     for the fallback path when an envelope cites a previous
//     version still inside its retention window.
//   - I-h — rotation. Adds rotation-specific queries (not in
//     this commit so I-h owns the atomicity story for its UPDATE +
//     INSERT pair).
//
// # Algorithm
//
// Single algorithm at this phase: NaCl-box (X25519 + XSalsa20 +
// Poly1305 per https://nacl.cr.yp.to/box.html). The database
// column `algorithm` is explicit + carries the canonical token
// [Algorithm] so a future migration to a new envelope construction
// — Hybrid PQ KEM, Curve448, etc. — lands as a new constant + new
// reader dispatch without a schema change.
//
// # At-rest
//
// Private keys are wrapped via [app/internal/atrest] (AES-256-GCM
// with the host master key per ADR 0017). The package mirrors the
// [federation/identity] pattern exactly — same wire format, same
// best-effort zeroisation of plaintext on error paths — but stores
// directly into a bytea column rather than a base64-string-in-JSONB
// (which is what federation_instance_identity does because it
// shares system_config's JSONB schema).
//
// # Caching
//
// No cache wired in this commit because there are no readers yet.
// When the I-c actor profile fetch lands, the right place for a
// userkey lookup cache is next to the existing actor profile cache —
// invalidation already fires on profile mutations, and the key
// versions move on the same cadence (rotation bumps the profile
// version, peers re-fetch).
package userkeys

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
)

// Algorithm is the only algorithm token this package currently
// emits. Stored verbatim in the `algorithm` column of new key rows;
// callers (I-e encrypt, I-f decrypt) dispatch on the value when
// future tokens land.
const Algorithm = "naclbox-x25519-v1"

// PublicKeyLen is the on-wire size of an X25519 public key
// — 32 bytes per RFC 7748 §5. Migration 00007's CHECK constraint
// enforces the same length at the database boundary.
const PublicKeyLen = 32

// Errors callers may distinguish on.
var (
	// ErrAtrestUnavailable means the host master key isn't loaded.
	// User-create paths surface this at the wiring layer as a fatal
	// boot-time error — federation can't generate user keys
	// without a master key, and we won't ship plaintext private
	// keys to disk to work around it.
	ErrAtrestUnavailable = errors.New("userkeys: atrest master key not initialised (AA_MASTER_KEY required)")

	// ErrPublicKeyWrongSize is returned by [Unwrap]-adjacent paths
	// when a stored row's public key doesn't have the expected
	// 32-byte length. Migration 00007 enforces this at insert time,
	// so this only fires for rows tampered with out-of-band — a
	// signal worth surfacing rather than silently truncating.
	ErrPublicKeyWrongSize = errors.New("userkeys: stored public key is not 32 bytes")
)

// Generate mints a fresh X25519 keypair using crypto/ecdh +
// crypto/rand, wraps the private key bytes via atrest, and returns
// the raw public bytes + the wrapped ciphertext ready to insert
// into federation_user_keys.
//
// The plaintext private key never escapes this function — it's
// zeroised after wrap. Callers should treat the returned slices
// as opaque blobs destined for [Queries.InsertUserKey].
//
// Returns [ErrAtrestUnavailable] if atrest hasn't been initialised
// (boot order bug); any other error is wrapped with context.
func Generate() (pub []byte, wrappedPriv []byte, err error) {
	if !atrest.Initialised() {
		return nil, nil, ErrAtrestUnavailable
	}

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("userkeys: generate x25519: %w", err)
	}

	// crypto/ecdh's PrivateKey.Bytes() returns a defensive copy
	// per the stdlib contract, so we own this byte slice and zero
	// it after wrap.
	privBytes := priv.Bytes()
	wrappedPriv, err = atrest.Encrypt(privBytes)
	for i := range privBytes {
		privBytes[i] = 0
	}
	if err != nil {
		return nil, nil, fmt.Errorf("userkeys: wrap private key: %w", err)
	}

	pub = priv.PublicKey().Bytes()
	if len(pub) != PublicKeyLen {
		// Defensive — crypto/ecdh's X25519 contract is 32 bytes.
		// If the stdlib ever changes, the migration's CHECK
		// constraint catches it again at the DB; this is the
		// in-process early-fail.
		return nil, nil, fmt.Errorf("userkeys: x25519 produced %d-byte public key (want %d)", len(pub), PublicKeyLen)
	}

	return pub, wrappedPriv, nil
}

// Unwrap takes a wrapped private-key blob (the bytes that came out
// of [Generate] or were read back from federation_user_keys
// .private_key_enc) and returns the live X25519 key. Best-effort
// zeroes the plaintext on any error path so a half-decoded key
// doesn't linger in caller memory.
//
// Returns [ErrAtrestUnavailable] if atrest hasn't been initialised.
// Any decrypt or parse failure bubbles up with context — callers
// at the federation wire (I-f) treat a decrypt failure on the
// receiver's CURRENT key as a signal to try retained versions.
func Unwrap(wrapped []byte) (*ecdh.PrivateKey, error) {
	if !atrest.Initialised() {
		return nil, ErrAtrestUnavailable
	}
	privBytes, err := atrest.Decrypt(wrapped)
	if err != nil {
		return nil, fmt.Errorf("userkeys: unwrap private key: %w", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(privBytes)
	for i := range privBytes {
		privBytes[i] = 0
	}
	if err != nil {
		return nil, fmt.Errorf("userkeys: parse private key: %w", err)
	}
	return priv, nil
}

// ParsePublicKey turns a raw 32-byte public-key slice (the form
// stored in federation_user_keys.public_key) into a usable
// crypto/ecdh.PublicKey. Returns [ErrPublicKeyWrongSize] if the
// slice is the wrong length — migration 00007's CHECK constraint
// makes this unreachable for normal reads, but a mutating DB-side
// tool could violate it.
func ParsePublicKey(raw []byte) (*ecdh.PublicKey, error) {
	if len(raw) != PublicKeyLen {
		return nil, ErrPublicKeyWrongSize
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("userkeys: parse public key: %w", err)
	}
	return pub, nil
}
