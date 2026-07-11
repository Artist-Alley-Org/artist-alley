// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package userkeys

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
)

// initAtrest seeds atrest with a fresh random master key.
// Tests call this in t.Cleanup or up front; it isolates them
// from each other so a Reset in one doesn't leak.
func initAtrest(t *testing.T) {
	t.Helper()
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("seed master key: %v", err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

func TestGenerate_ProducesCorrectShapes(t *testing.T) {
	initAtrest(t)

	pub, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got, want := len(pub), PublicKeyLen; got != want {
		t.Fatalf("public key length = %d, want %d", got, want)
	}
	// atrest wraps with a 1-byte version + 12-byte nonce + tag.
	// Plaintext is 32 bytes (X25519 private), so wrap is at least
	// 1+12+32+16 = 61 bytes. Lock the lower bound.
	if got := len(wrapped); got < 1+12+32+16 {
		t.Fatalf("wrapped private key length = %d, want >= 61", got)
	}
}

func TestGenerate_KeysAreFresh(t *testing.T) {
	initAtrest(t)

	pubA, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate #1: %v", err)
	}
	pubB, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate #2: %v", err)
	}
	if bytes.Equal(pubA, pubB) {
		t.Fatalf("two consecutive Generate calls produced the same public key — entropy bug")
	}
}

func TestRoundtrip_UnwrapMatchesPublic(t *testing.T) {
	initAtrest(t)

	pub, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	priv, err := Unwrap(wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if got, want := priv.PublicKey().Bytes(), pub; !bytes.Equal(got, want) {
		t.Fatalf("derived public != stored public\n got: %x\nwant: %x", got, want)
	}
}

func TestGenerate_FailsWhenAtrestNotInitialised(t *testing.T) {
	atrest.Reset()
	// Restore for sibling tests in case the suite runs in-process.
	defer initAtrest(t)

	_, _, err := Generate()
	if !errors.Is(err, ErrAtrestUnavailable) {
		t.Fatalf("Generate without atrest init: err = %v, want ErrAtrestUnavailable", err)
	}
}

func TestUnwrap_FailsWhenAtrestNotInitialised(t *testing.T) {
	initAtrest(t)
	_, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	atrest.Reset()
	defer initAtrest(t)

	_, err = Unwrap(wrapped)
	if !errors.Is(err, ErrAtrestUnavailable) {
		t.Fatalf("Unwrap without atrest init: err = %v, want ErrAtrestUnavailable", err)
	}
}

func TestUnwrap_FailsOnTruncatedCiphertext(t *testing.T) {
	initAtrest(t)
	_, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Drop the last few bytes — the AES-GCM tag at the tail won't
	// verify any more.
	tampered := wrapped[:len(wrapped)-4]
	if _, err := Unwrap(tampered); err == nil {
		t.Fatalf("Unwrap of truncated wrap returned nil err")
	}
}

func TestUnwrap_FailsOnFlippedByte(t *testing.T) {
	initAtrest(t)
	_, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Flip the middle byte of the ciphertext — the GCM tag won't
	// verify. This is the canonical "stored ciphertext got
	// modified out of band" detection.
	tampered := append([]byte{}, wrapped...)
	tampered[len(tampered)/2] ^= 0xFF
	if _, err := Unwrap(tampered); err == nil {
		t.Fatalf("Unwrap of bit-flipped wrap returned nil err")
	}
}

func TestUnwrap_FailsWhenMasterKeyChanges(t *testing.T) {
	initAtrest(t)
	_, wrapped, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Rotate the master key and re-init. The old wrap should no
	// longer authenticate — mirrors the "AA_MASTER_KEY rotated
	// between deploys" production scenario that federation/identity
	// surfaces as ErrBadCiphertext.
	newKey := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(newKey); err != nil {
		t.Fatalf("rotate master key: %v", err)
	}
	if err := atrest.InitWithKey(newKey); err != nil {
		t.Fatalf("re-init atrest: %v", err)
	}

	if _, err := Unwrap(wrapped); err == nil {
		t.Fatalf("Unwrap with rotated master key returned nil err — expected failure")
	}
}

func TestParsePublicKey_AcceptsCorrectLength(t *testing.T) {
	initAtrest(t)
	pub, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parsed, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !bytes.Equal(parsed.Bytes(), pub) {
		t.Fatalf("ParsePublicKey roundtrip mismatch")
	}
}

func TestParsePublicKey_RejectsWrongLength(t *testing.T) {
	// Locks the boundary error path. The migration's CHECK
	// constraint makes a wrong-length row unreachable for inserts
	// going through this package, but a sibling tool could violate
	// it — surface a clear error rather than panicking inside
	// crypto/ecdh's parser.
	for _, badLen := range []int{0, 31, 33, 64} {
		buf := make([]byte, badLen)
		if _, err := ParsePublicKey(buf); !errors.Is(err, ErrPublicKeyWrongSize) {
			t.Fatalf("ParsePublicKey(%d bytes): err = %v, want ErrPublicKeyWrongSize", badLen, err)
		}
	}
}
