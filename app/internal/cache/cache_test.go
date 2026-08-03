// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package cache_test

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// TestCacheBasicGetAdd covers the LRU surface: hit, miss, add, len,
// remove. No Registry needed for these.
func TestCacheBasicGetAdd(t *testing.T) {
	t.Parallel()
	r := cache.NewRegistry(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := cache.Register[string](r, "test.basic", 4)

	if _, ok := c.Get("missing"); ok {
		t.Errorf("expected miss")
	}
	c.Add("a", "alpha")
	c.Add("b", "beta")
	if v, ok := c.Get("a"); !ok || v != "alpha" {
		t.Errorf("got (%q,%v) want (alpha,true)", v, ok)
	}
	if c.Len() != 2 {
		t.Errorf("len=%d want 2", c.Len())
	}
}

// TestCacheLRUEviction confirms the bounded size — adding past
// capacity evicts the LRU entry.
func TestCacheLRUEviction(t *testing.T) {
	t.Parallel()
	r := cache.NewRegistry(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := cache.Register[int](r, "test.evict", 2)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3) // evicts "a"
	if _, ok := c.Get("a"); ok {
		t.Errorf("a should have been evicted")
	}
	if v, _ := c.Get("c"); v != 3 {
		t.Errorf("c=%d want 3", v)
	}
}

// TestNotifyRoundTrip verifies that Invalidate on one Registry
// broadcasts via Postgres NOTIFY and is dispatched on a second
// Registry subscribing to the same DB. This is the cross-instance
// invariant the package exists to guarantee.
func TestNotifyRoundTrip(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; NOTIFY integration test skipped")
	}
	ctx := t.Context()

	poolA := openPool(t, pwd)
	defer poolA.Close()
	poolB := openPool(t, pwd)
	defer poolB.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regA := cache.NewRegistry(poolA, logger)
	regB := cache.NewRegistry(poolB, logger)

	cA := cache.Register[string](regA, "test.roundtrip", 8)
	cB := cache.Register[string](regB, "test.roundtrip", 8)

	if err := regA.Start(ctx); err != nil {
		t.Fatalf("regA Start: %v", err)
	}
	defer regA.Stop()
	if err := regB.Start(ctx); err != nil {
		t.Fatalf("regB Start: %v", err)
	}
	defer regB.Stop()

	// Both LISTENs share the same Postgres NOTIFY pipe; give it a
	// beat to subscribe before we emit.
	time.Sleep(100 * time.Millisecond)

	// Seed both caches with the same key. After the cross-instance
	// invalidate, both should drop their entries.
	cA.Add("k1", "v1")
	cB.Add("k1", "v1")

	if err := cA.Invalidate(ctx, "k1"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, ok := cA.Get("k1"); ok {
		t.Errorf("regA: local invalidate didn't drop entry")
	}

	// Wait for the NOTIFY to round-trip into regB.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cB.Get("k1"); !ok {
			break // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := cB.Get("k1"); ok {
		t.Errorf("regB: NOTIFY didn't propagate; entry still cached")
	}
}

// TestNotifyPurgeAll verifies the empty-key payload purges the
// whole domain on peer instances.
func TestNotifyPurgeAll(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	ctx := t.Context()

	poolA := openPool(t, pwd)
	defer poolA.Close()
	poolB := openPool(t, pwd)
	defer poolB.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	regA := cache.NewRegistry(poolA, logger)
	regB := cache.NewRegistry(poolB, logger)
	cA := cache.Register[int](regA, "test.purgeall", 16)
	cB := cache.Register[int](regB, "test.purgeall", 16)
	if err := regA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer regA.Stop()
	if err := regB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer regB.Stop()
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 5; i++ {
		cA.Add(string(rune('a'+i)), i)
		cB.Add(string(rune('a'+i)), i)
	}
	if err := cA.InvalidateAll(ctx); err != nil {
		t.Fatalf("invalidate all: %v", err)
	}
	if cA.Len() != 0 {
		t.Errorf("local purge failed: len=%d", cA.Len())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cB.Len() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if cB.Len() != 0 {
		t.Errorf("peer purge failed: len=%d", cB.Len())
	}
}

// TestNotifyFlushAll verifies the wildcard flush (#845): EmitFlushAll —
// which owns no Registry, mirroring the seeder — purges EVERY registered
// domain on a peer instance, not just one. This is the invariant that
// lets `aa seed --reset` clear a running instance's caches without a
// restart.
func TestNotifyFlushAll(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	ctx := t.Context()

	poolA := openPool(t, pwd)
	defer poolA.Close()
	poolB := openPool(t, pwd)
	defer poolB.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Only regB registers caches + LISTENs — it stands in for a running
	// serving instance. poolA stands in for the seeder: it broadcasts the
	// flush with no Registry of its own.
	regB := cache.NewRegistry(poolB, logger)
	c1 := cache.Register[int](regB, "test.flush.one", 16)
	c2 := cache.Register[string](regB, "test.flush.two", 16)
	if err := regB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer regB.Stop()
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 3; i++ {
		c1.Add(string(rune('a'+i)), i)
		c2.Add(string(rune('a'+i)), "v")
	}
	if c1.Len() == 0 || c2.Len() == 0 {
		t.Fatalf("precondition: caches empty before flush (%d, %d)", c1.Len(), c2.Len())
	}

	// The seeder-side broadcast: no Registry, just a pool.
	if err := cache.EmitFlushAll(ctx, poolA); err != nil {
		t.Fatalf("EmitFlushAll: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (c1.Len() > 0 || c2.Len() > 0) {
		time.Sleep(20 * time.Millisecond)
	}
	if c1.Len() != 0 || c2.Len() != 0 {
		t.Errorf("wildcard flush did not purge all domains: one=%d two=%d", c1.Len(), c2.Len())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
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

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
