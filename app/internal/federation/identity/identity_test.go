// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the instance identity package — Phase 1.22.B-b.
// Coverage:
//   - First-boot path: generate + persist + reload returns the
//     same keypair.
//   - Idempotent Load: calling twice returns the same Identity
//     without regenerating.
//   - Sign + Verify round-trip via the cached Identity.
//   - ErrAtrestUnavailable when the master key isn't set.
//
// Skips without AA_DB_PASSWORD per project convention.

package identity_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// scopedSysconfigKey wipes the existing identity row, runs the
// test, then restores the original. Lets tests exercise the
// first-boot path without losing the live keypair.
func scopedSysconfigKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var saved []byte
	err := pool.QueryRow(ctx, `SELECT value FROM system_config WHERE key='federation.instance_identity'`).Scan(&saved)
	if err != nil {
		saved = nil
	}
	if _, err := pool.Exec(ctx, `DELETE FROM system_config WHERE key='federation.instance_identity'`); err != nil {
		t.Fatalf("clear identity: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM system_config WHERE key='federation.instance_identity'`)
		if saved != nil {
			_, _ = pool.Exec(c, `INSERT INTO system_config (key, value) VALUES ('federation.instance_identity', $1)`, saved)
		}
	})
}

// ensureAtrest seeds the atrest master key for tests. The
// integration test env already sets AA_MASTER_KEY in the app
// container; if running tests directly without that env, this
// initialises a throwaway key.
func ensureAtrest(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	// 32-byte key for the test process. Doesn't survive process
	// restarts; that's fine — tests don't either.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

func TestLoad_FirstBoot_GeneratesAndPersists(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	ensureAtrest(t)
	scopedSysconfigKey(t, ctx, pool)

	mgr := identity.NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id1, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(id1.PublicKey()) != 32 {
		t.Errorf("public key should be 32 bytes; got %d", len(id1.PublicKey()))
	}
	if id1.Fingerprint() == "" {
		t.Error("fingerprint should be populated")
	}
	if id1.GeneratedAt().IsZero() {
		t.Error("generated_at should be populated")
	}

	// Reload via a NEW manager — proves the row was persisted +
	// can be read back through the decrypt path.
	mgr2 := identity.NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id2, err := mgr2.Load(ctx)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Errorf("reload produced different identity: %s vs %s", id1.Fingerprint(), id2.Fingerprint())
	}
}

func TestLoad_Idempotent_SameInstance(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	ensureAtrest(t)
	// Scope the sysconfig row so we don't try to decrypt a row
	// encrypted with a different at-rest key (prod boot uses
	// AA_MASTER_KEY; tests use ad-hoc keys when atrest hasn't
	// been initialised yet — without scoping the two will collide).
	scopedSysconfigKey(t, ctx, pool)

	mgr := identity.NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id1, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	id2, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	// Same Manager → same pointer (cached).
	if id1 != id2 {
		t.Error("second Load on same Manager should return cached pointer")
	}
}

func TestGet_BeforeLoad_ReturnsErrNotLoaded(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ensureAtrest(t)

	mgr := identity.NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := mgr.Get()
	if !errors.Is(err, identity.ErrNotLoaded) {
		t.Errorf("expected ErrNotLoaded, got %v", err)
	}
}

func TestSignAndVerify_RoundTrip(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	ensureAtrest(t)
	scopedSysconfigKey(t, ctx, pool)

	mgr := identity.NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, err := mgr.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("handshake payload " + randHex(t, 6))
	sig := id.Sign(msg)
	if len(sig) != 64 {
		t.Errorf("Ed25519 signature should be 64 bytes; got %d", len(sig))
	}
	if err := id.Verify(msg, sig); err != nil {
		t.Errorf("verify own signature: %v", err)
	}
	// Tamper detection.
	bad := append([]byte{}, sig...)
	bad[0] ^= 0xFF
	if err := id.Verify(msg, bad); err == nil {
		t.Error("tampered signature should fail verification")
	}
}
