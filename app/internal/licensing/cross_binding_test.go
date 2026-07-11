// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package licensing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// VerifyOrgCrossBinding green path: write a fresh seed to disk,
// derive its public key, pass that as orgPubkeyB64 — must succeed.
// Mirrors what the customer-activation script + license server do
// in production end-to-end.
func TestVerifyOrgCrossBinding_Happy(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	seed := priv.Seed()
	seedB64 := base64.StdEncoding.EncodeToString(seed)

	dir := t.TempDir()
	path := filepath.Join(dir, "org.key")
	if err := os.WriteFile(path, []byte(seedB64+"\n"), 0o600); err != nil {
		t.Fatalf("write org.key: %v", err)
	}

	pub := priv.Public().(ed25519.PublicKey)
	orgPubkeyB64 := base64.StdEncoding.EncodeToString(pub)

	if err := VerifyOrgCrossBinding(orgPubkeyB64, path); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// Empty orgPubkeyB64 is the community-mode opt-out — must return nil
// even when no key path is configured.
func TestVerifyOrgCrossBinding_NoBindingSkips(t *testing.T) {
	if err := VerifyOrgCrossBinding("", ""); err != nil {
		t.Errorf("expected nil for empty pubkey, got %v", err)
	}
	if err := VerifyOrgCrossBinding("", "/nonexistent/org.key"); err != nil {
		t.Errorf("expected nil for empty pubkey, got %v", err)
	}
}

// Missing file → ErrOrgKeyMissing.
func TestVerifyOrgCrossBinding_MissingFile(t *testing.T) {
	err := VerifyOrgCrossBinding("abc", filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, ErrOrgKeyMissing) {
		t.Errorf("expected ErrOrgKeyMissing, got %v", err)
	}
}

// Empty configured path with bound license → ErrOrgKeyMissing.
func TestVerifyOrgCrossBinding_EmptyPath(t *testing.T) {
	err := VerifyOrgCrossBinding("abc", "")
	if !errors.Is(err, ErrOrgKeyMissing) {
		t.Errorf("expected ErrOrgKeyMissing, got %v", err)
	}
}

// Garbage in the file → ErrOrgKeyBadFormat.
func TestVerifyOrgCrossBinding_BadFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "org.key")
	if err := os.WriteFile(path, []byte("not base64 at all !!!"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := VerifyOrgCrossBinding("abc", path)
	if !errors.Is(err, ErrOrgKeyBadFormat) {
		t.Errorf("expected ErrOrgKeyBadFormat, got %v", err)
	}
}

// Wrong public key → ErrOrgKeyMismatch.
func TestVerifyOrgCrossBinding_Mismatch(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())

	dir := t.TempDir()
	path := filepath.Join(dir, "org.key")
	if err := os.WriteFile(path, []byte(seedB64), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Use a different freshly-generated pubkey on the "license" side.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	wrongPubkeyB64 := base64.StdEncoding.EncodeToString(otherPub)

	err := VerifyOrgCrossBinding(wrongPubkeyB64, path)
	if !errors.Is(err, ErrOrgKeyMismatch) {
		t.Errorf("expected ErrOrgKeyMismatch, got %v", err)
	}
}

// URL-safe base64 in the file is tolerated (paste mangling) — must
// still produce a valid derivation that matches the standard-alphabet
// public key embedded in the license.
func TestVerifyOrgCrossBinding_URLSafeSeedAccepted(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	seedB64 := base64.URLEncoding.EncodeToString(priv.Seed())

	dir := t.TempDir()
	path := filepath.Join(dir, "org.key")
	if err := os.WriteFile(path, []byte(seedB64), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	pub := priv.Public().(ed25519.PublicKey)
	orgPubkeyB64 := base64.StdEncoding.EncodeToString(pub)

	if err := VerifyOrgCrossBinding(orgPubkeyB64, path); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// A 64-byte expanded private key on disk is also accepted (some
// keystores export that form).
func TestVerifyOrgCrossBinding_ExpandedPrivateKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	privB64 := base64.StdEncoding.EncodeToString(priv)

	dir := t.TempDir()
	path := filepath.Join(dir, "org.key")
	if err := os.WriteFile(path, []byte(privB64), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	pub := priv.Public().(ed25519.PublicKey)
	orgPubkeyB64 := base64.StdEncoding.EncodeToString(pub)

	if err := VerifyOrgCrossBinding(orgPubkeyB64, path); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}
