// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package users_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// initAtrestForTest installs a one-shot master key. Tests that
// touch EnsureActorKeyMaterial need atrest live; clean up after
// themselves via t.Cleanup.
func initAtrestForTest(t *testing.T) {
	t.Helper()
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
	t.Cleanup(atrest.Reset)
}

// newTestUser inserts a throwaway row in "user" and returns its
// ref. Cleanup deletes the row + any actor_key columns set on it.
func newTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int64, string) {
	t.Helper()
	username := "actorkey-test-" + randHex(t, 6)
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Actor Key Test",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref, username
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return strings.ToLower(hexEncode(b))
}

func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = hexDigits[x>>4]
		out[i*2+1] = hexDigits[x&0x0f]
	}
	return string(out)
}

func TestActorKeys_GetWhenAbsent(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, _ := newTestUser(t, ctx, pool)

	_, err := h.GetActorKeyMaterial(ctx, ref)
	if !errors.Is(err, users.ErrNoActorKeyMaterial) {
		t.Errorf("expected ErrNoActorKeyMaterial for fresh user, got %v", err)
	}
}

func TestActorKeys_EnsureGeneratesAndPersists(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()
	initAtrestForTest(t)

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, username := newTestUser(t, ctx, pool)
	const baseURL = "https://test.example"

	mat, err := h.EnsureActorKeyMaterial(ctx, ref, baseURL, username)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Actor URI composed correctly.
	wantURI := baseURL + "/users/" + username
	if mat.ActorURI != wantURI {
		t.Errorf("actor URI: got %q want %q", mat.ActorURI, wantURI)
	}

	// Public-key PEM round-trips back to an Ed25519 key.
	pub, err := federation.PublicKeyFromPEM(mat.SigningPublicKeyPEM)
	if err != nil {
		t.Errorf("public key PEM didn't round-trip: %v", err)
	}
	if len(pub) != 32 {
		t.Errorf("Ed25519 public key wrong length: %d", len(pub))
	}

	// Encrypted private signing key decrypts to a valid PKCS#8
	// PEM that round-trips to the matching private key.
	signPrivPEM, err := mat.DecryptSigningPrivateKey()
	if err != nil {
		t.Fatalf("decrypt signing priv: %v", err)
	}
	signPriv, err := federation.PrivateKeyFromPEM(signPrivPEM)
	if err != nil {
		t.Fatalf("priv PEM round trip: %v", err)
	}
	// Sign a probe + verify under the public key — confirms the
	// keypair is internally consistent.
	probe := []byte("federation actor key consistency probe")
	sig := federation.Sign(signPriv, probe)
	if err := federation.Verify(pub, probe, sig); err != nil {
		t.Errorf("keypair consistency check failed: %v", err)
	}

	// Encryption keypair: 32-byte X25519 public + 32-byte
	// decrypted private scalar.
	if len(mat.EncryptionPublicKey) != 32 {
		t.Errorf("X25519 public key length: %d", len(mat.EncryptionPublicKey))
	}
	encPriv, err := mat.DecryptEncryptionPrivateKey()
	if err != nil {
		t.Fatalf("decrypt encryption priv: %v", err)
	}
	if len(encPriv) != 32 {
		t.Errorf("X25519 private scalar length: %d", len(encPriv))
	}
}

func TestActorKeys_EnsureIsIdempotent(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()
	initAtrestForTest(t)

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, username := newTestUser(t, ctx, pool)

	first, err := h.EnsureActorKeyMaterial(ctx, ref, "https://test.example", username)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := h.EnsureActorKeyMaterial(ctx, ref, "https://test.example", username)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// Same public key — second call MUST NOT have rotated.
	if string(first.SigningPublicKeyPEM) != string(second.SigningPublicKeyPEM) {
		t.Error("Ensure not idempotent — second call rotated the signing key")
	}
	if string(first.EncryptionPublicKey) != string(second.EncryptionPublicKey) {
		t.Error("Ensure not idempotent — second call rotated the X25519 key")
	}
}

func TestActorKeys_EnsureRefusesWithoutMasterKey(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()
	// Deliberately do NOT init atrest. Confirm Ensure refuses.
	atrest.Reset()

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, username := newTestUser(t, ctx, pool)

	_, err := h.EnsureActorKeyMaterial(ctx, ref, "https://test.example", username)
	if err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("expected master-key refusal, got %v", err)
	}
}

func TestActorKeys_CacheHitAfterEnsure(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()
	initAtrestForTest(t)

	reg := cache.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer reg.Stop()

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	ref, username := newTestUser(t, ctx, pool)

	if _, err := h.EnsureActorKeyMaterial(ctx, ref, "https://test.example", username); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Subsequent GetActorKeyMaterial should serve from cache —
	// observable side effect: same public-key bytes returned.
	// (We can't easily observe "no DB hit" without instrumenting
	// the pool; the deterministic value equality is the
	// black-box proxy here.)
	a, err := h.GetActorKeyMaterial(ctx, ref)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	b, err := h.GetActorKeyMaterial(ctx, ref)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if string(a.SigningPublicKeyPEM) != string(b.SigningPublicKeyPEM) {
		t.Error("two Get calls returned different signing keys (cache + DB drift?)")
	}
	if a.ActorURI != b.ActorURI {
		t.Error("two Get calls returned different actor URIs")
	}
}
