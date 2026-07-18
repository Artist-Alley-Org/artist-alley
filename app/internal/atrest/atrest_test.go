// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package atrest_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
)

func mustInit(t *testing.T) {
	t.Helper()
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(atrest.Reset)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	mustInit(t)
	cases := [][]byte{
		[]byte("hello world"),
		[]byte(""),
		bytes.Repeat([]byte{0xaa}, 10_000), // 10 KiB
	}
	for _, plain := range cases {
		blob, err := atrest.Encrypt(plain)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if len(blob) < 1+12 {
			t.Errorf("blob too short: %d", len(blob))
		}
		if blob[0] != 0x01 {
			t.Errorf("expected version byte 0x01, got 0x%02x", blob[0])
		}
		out, err := atrest.Decrypt(blob)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(plain, out) {
			t.Errorf("round-trip mismatch: got %q want %q", out, plain)
		}
	}
}

func TestEncryptIsNondeterministic(t *testing.T) {
	mustInit(t)
	plain := []byte("same payload")
	a, _ := atrest.Encrypt(plain)
	b, _ := atrest.Encrypt(plain)
	// Different nonces → different ciphertexts. Critical property.
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertexts; nonce reuse")
	}
}

func TestDecryptTamperedReturnsBadCiphertext(t *testing.T) {
	mustInit(t)
	blob, _ := atrest.Encrypt([]byte("sensitive"))
	// Flip a byte in the ciphertext body.
	blob[len(blob)-5] ^= 0xff
	if _, err := atrest.Decrypt(blob); !errors.Is(err, atrest.ErrBadCiphertext) {
		t.Errorf("tampered decrypt: expected ErrBadCiphertext, got %v", err)
	}
}

func TestDecryptWrongVersionReturnsBadCiphertext(t *testing.T) {
	mustInit(t)
	blob, _ := atrest.Encrypt([]byte("sensitive"))
	blob[0] = 0x02 // unknown version
	if _, err := atrest.Decrypt(blob); !errors.Is(err, atrest.ErrBadCiphertext) {
		t.Errorf("wrong-version decrypt: expected ErrBadCiphertext, got %v", err)
	}
}

func TestDecryptTruncatedReturnsBadCiphertext(t *testing.T) {
	mustInit(t)
	blob, _ := atrest.Encrypt([]byte("sensitive"))
	if _, err := atrest.Decrypt(blob[:5]); !errors.Is(err, atrest.ErrBadCiphertext) {
		t.Errorf("truncated decrypt: expected ErrBadCiphertext, got %v", err)
	}
}

func TestUninitialisedRefuses(t *testing.T) {
	atrest.Reset()
	if _, err := atrest.Encrypt([]byte("x")); !errors.Is(err, atrest.ErrNotInitialised) {
		t.Errorf("encrypt before init: expected ErrNotInitialised, got %v", err)
	}
	if _, err := atrest.Decrypt([]byte{0x01, 0, 0, 0}); !errors.Is(err, atrest.ErrNotInitialised) {
		t.Errorf("decrypt before init: expected ErrNotInitialised, got %v", err)
	}
}

func TestInitFromEnvVar(t *testing.T) {
	t.Setenv(atrest.EnvVar, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, 32)))
	t.Cleanup(atrest.Reset)
	if err := atrest.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !atrest.Initialised() {
		t.Fatal("Initialised reports false after Init")
	}
	// Round-trip works with env-sourced key.
	blob, _ := atrest.Encrypt([]byte("payload"))
	out, _ := atrest.Decrypt(blob)
	if !bytes.Equal(out, []byte("payload")) {
		t.Errorf("env-key round-trip mismatch")
	}
}

func TestInitMissingEnv(t *testing.T) {
	t.Setenv(atrest.EnvVar, "")
	if err := atrest.Init(); !errors.Is(err, atrest.ErrEnvMissing) {
		t.Errorf("missing env: expected ErrEnvMissing, got %v", err)
	}
}

func TestInitMalformedEnv(t *testing.T) {
	cases := []string{
		"not-base64-at-all!@#",
		base64.StdEncoding.EncodeToString([]byte("too-short")),         // <32 bytes
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, 64)), // >32 bytes
	}
	for _, v := range cases {
		t.Setenv(atrest.EnvVar, v)
		if err := atrest.Init(); !errors.Is(err, atrest.ErrEnvMalformed) {
			t.Errorf("malformed env %q: expected ErrEnvMalformed, got %v", short(v), err)
		}
	}
}

func TestInitAcceptsRawURLBase64(t *testing.T) {
	// Operators may pipe base64 without padding (Kubernetes secret
	// idioms sometimes do this).
	t.Setenv(atrest.EnvVar, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	t.Cleanup(atrest.Reset)
	if err := atrest.Init(); err != nil {
		t.Errorf("raw-url-base64: expected accepted, got %v", err)
	}
}

func short(s string) string {
	if len(s) > 20 {
		return s[:20] + "…"
	}
	return s
}

func TestDecryptByOtherKeyReturnsBadCiphertext(t *testing.T) {
	// Encrypt under one master key, swap to a different one,
	// decrypt should fail with ErrBadCiphertext (not a panic, not
	// silent garbage).
	keyA := bytes.Repeat([]byte{0x11}, 32)
	keyB := bytes.Repeat([]byte{0x22}, 32)

	if err := atrest.InitWithKey(keyA); err != nil {
		t.Fatal(err)
	}
	blob, err := atrest.Encrypt([]byte("encrypted-under-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := atrest.InitWithKey(keyB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(atrest.Reset)
	if _, err := atrest.Decrypt(blob); !errors.Is(err, atrest.ErrBadCiphertext) {
		t.Errorf("cross-key decrypt: expected ErrBadCiphertext, got %v", err)
	}
}

func TestEnvVarConstantStable(t *testing.T) {
	// Pin the env-var name. Renaming it is a breaking operational
	// change and should require a deliberate doc update.
	if atrest.EnvVar != "AA_MASTER_KEY" {
		t.Errorf("EnvVar drift: got %q, want AA_MASTER_KEY", atrest.EnvVar)
	}
	if !strings.HasPrefix(atrest.EnvVar, "AA_") {
		t.Errorf("EnvVar must be in the AA_* namespace")
	}
}
