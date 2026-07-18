// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package atrest is the symmetric at-rest encryption primitive
// for sensitive blobs that live in the database — federation actor
// private keys (Phase 1.22.A, the first real consumer), license
// signing keys, SMTP credentials (planned), OAuth client secrets,
// any other material we will not ship to disk plaintext.
//
// # Design
//
// AES-256-GCM with a 32-byte host master key sourced from the
// AA_MASTER_KEY environment variable (base64-encoded). The master
// key is operator-managed — mounted secret, KMS-fetched at boot,
// sealed-secret, whatever the deployment uses. It is never stored
// in the database. Loss of AA_MASTER_KEY means loss of access to
// every column wrapped by this package; rotation is operator-
// driven and intentionally out of scope for v1 (a future
// migration walks every wrapped row).
//
// The wire format of an at-rest ciphertext is:
//
//	| version_byte (0x01) | nonce_12_bytes | ciphertext_||_tag |
//
// The version byte allows future algorithm migration (AES-256-SIV,
// ChaCha20-Poly1305, etc.) without touching call sites — Decrypt
// dispatches on the version byte.
//
// Threat model
//
//   - Defends against: database dumps, backup-tape theft, naive
//     replication snapshots, postgres-row exfiltration.
//   - Does NOT defend against: process memory inspection (the
//     master key lives in process memory), root-on-host with debugger
//     attach (same), or any attack vector that compromises the
//     running process. Federation private keys, once decrypted,
//     exist in memory for the duration of the signing operation;
//     callers SHOULD zero the byte slice in deferred cleanup.
//
// # Spec reference
//
// See docs/spec/federation/v1.md §13 for the wire format + threat
// model in the protocol context. The package itself is generic
// and predates per-feature use.
package atrest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"
)

const (
	// EnvVar names the operator-supplied environment variable
	// carrying the base64-encoded 32-byte master key.
	EnvVar = "AA_MASTER_KEY"

	// MasterKeyLen is the required raw length of the master key
	// after base64 decoding. AES-256 requires 32 bytes.
	MasterKeyLen = 32

	// nonceLen is the AES-GCM standard nonce length per NIST SP
	// 800-38D §8.2. 12 bytes is the only length the Go stdlib's
	// GCM mode accepts without an explicit override; we follow.
	nonceLen = 12

	// versionV1 is the first (and currently only) wire-format
	// version byte. Increment if the algorithm ever changes; old
	// values stay decodable.
	versionV1 byte = 0x01
)

// Errors callers may distinguish on.
var (
	// ErrNotInitialised is returned when Encrypt or Decrypt is
	// called before Init has succeeded. Application boot must
	// initialise the package before any feature that wraps
	// secrets is reachable.
	ErrNotInitialised = errors.New("atrest: master key not initialised; call Init")

	// ErrBadCiphertext covers every failure decoding or
	// authenticating a wrapped ciphertext — wrong version byte,
	// truncated input, tag mismatch. The collapsed error helps
	// callers avoid leaking which specific malformation occurred
	// (a useful side-channel resistance).
	ErrBadCiphertext = errors.New("atrest: ciphertext could not be authenticated")

	// ErrEnvMissing is the Init error when AA_MASTER_KEY is unset.
	ErrEnvMissing = errors.New("atrest: AA_MASTER_KEY env var is required")

	// ErrEnvMalformed is the Init error when AA_MASTER_KEY is set
	// but not a base64-encoded 32-byte key.
	ErrEnvMalformed = errors.New("atrest: AA_MASTER_KEY must be base64-encoded 32 bytes")
)

var (
	mu      sync.RWMutex
	cipher_ cipher.AEAD // initialised by Init, read by Encrypt/Decrypt
)

// Init reads AA_MASTER_KEY from the environment, decodes, validates
// length, and constructs the AES-GCM AEAD held by the package. Call
// once at process boot. Subsequent calls replace the in-process key
// (used by tests).
//
// Returns ErrEnvMissing if the variable is unset, ErrEnvMalformed if
// it's set but not a valid 32-byte base64 payload.
func Init() error {
	raw := os.Getenv(EnvVar)
	if raw == "" {
		return ErrEnvMissing
	}
	key, err := decodeMasterKey(raw)
	if err != nil {
		return err
	}
	return initWithKey(key)
}

// InitWithKey lets tests bypass the env var path. Production code
// should call Init.
func InitWithKey(key []byte) error {
	if len(key) != MasterKeyLen {
		return ErrEnvMalformed
	}
	cp := make([]byte, MasterKeyLen)
	copy(cp, key)
	return initWithKey(cp)
}

func initWithKey(key []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("atrest: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("atrest: cipher.NewGCM: %w", err)
	}
	mu.Lock()
	cipher_ = gcm
	mu.Unlock()
	return nil
}

// Reset clears the in-process master key. Used by tests; not a
// production rotation path (production rotation re-encrypts every
// wrapped row and is operator-driven).
func Reset() {
	mu.Lock()
	cipher_ = nil
	mu.Unlock()
}

// Initialised reports whether Init or InitWithKey has succeeded.
func Initialised() bool {
	mu.RLock()
	defer mu.RUnlock()
	return cipher_ != nil
}

// Encrypt wraps plaintext as a versioned at-rest ciphertext.
// Returns ErrNotInitialised if Init has not run.
//
// The ciphertext layout is documented in the package doc + at
// docs/spec/federation/v1.md §13.2.
func Encrypt(plaintext []byte) ([]byte, error) {
	mu.RLock()
	g := cipher_
	mu.RUnlock()
	if g == nil {
		return nil, ErrNotInitialised
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("atrest: random nonce: %w", err)
	}
	// Layout: version || nonce || sealed.
	// Seal appends to dst, so we pre-allocate the prefix.
	out := make([]byte, 1+nonceLen, 1+nonceLen+len(plaintext)+g.Overhead())
	out[0] = versionV1
	copy(out[1:], nonce)
	out = g.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt unwraps a versioned at-rest ciphertext. Returns
// ErrBadCiphertext on any failure: wrong version byte, truncated
// input, tag mismatch.
func Decrypt(blob []byte) ([]byte, error) {
	mu.RLock()
	g := cipher_
	mu.RUnlock()
	if g == nil {
		return nil, ErrNotInitialised
	}
	if len(blob) < 1+nonceLen+g.Overhead() {
		return nil, ErrBadCiphertext
	}
	if blob[0] != versionV1 {
		return nil, ErrBadCiphertext
	}
	nonce := blob[1 : 1+nonceLen]
	sealed := blob[1+nonceLen:]
	plaintext, err := g.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrBadCiphertext
	}
	return plaintext, nil
}

// PackageEncrypter is an instance-shaped adapter over the
// package-level Encrypt/Decrypt functions. Some consumers (e.g.
// sysconfig.Store) accept an interface for at-rest wrapping so
// tests can substitute a fake; production boot passes this type's
// zero value to satisfy that interface.
type PackageEncrypter struct{}

// Encrypt delegates to the package-level [Encrypt].
func (PackageEncrypter) Encrypt(plaintext []byte) ([]byte, error) {
	return Encrypt(plaintext)
}

// Decrypt delegates to the package-level [Decrypt].
func (PackageEncrypter) Decrypt(ciphertext []byte) ([]byte, error) {
	return Decrypt(ciphertext)
}

// decodeMasterKey accepts either standard or URL-safe base64,
// with or without padding — accommodates whatever the operator
// pipes in.
func decodeMasterKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == MasterKeyLen {
			return b, nil
		}
	}
	return nil, ErrEnvMalformed
}
