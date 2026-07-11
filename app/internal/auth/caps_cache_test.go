// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// pgUUID is a tiny wrapper for [16]byte → pgtype.UUID so the test
// can pass roleID into SetUserGlobalRoleParams cleanly.
func pgUUID(b [16]byte) pgtype.UUID { return pgtype.UUID{Bytes: b, Valid: true} }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestCapsCache_PopulatesAndHits verifies the basic two-tier path:
// the first loadCapabilities call populates the cache; the second
// reads through it. We verify cache state — after one load the
// cache has one entry, after Invalidate the entry's gone, and after
// a second load the entry is back.
//
// Uses withFixture so the test seeds its own user + role and runs
// hermetically on a clean DB.
func TestCapsCache_PopulatesAndHits(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// Seed: a role with one capability, assign to the fixture user.
		seedCap(t, ctx, fx.pool, "test.caps.cached")
		roleID := seedRole(t, ctx, fx.pool, "test_CapsCache", nil, "test.caps.cached")
		q := New(fx.pool)
		if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
			UserRef: fx.userRef,
			RoleID:   pgUUID(roleID),
		}); err != nil {
			t.Fatalf("assign role: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		reg := cache.NewRegistry(fx.pool, logger)
		r := NewResolver(fx.pool, logger, nil, reg)

		// Start the LISTEN goroutine — this is what dispatches incoming
		// NOTIFYs to per-domain caches. Without it, InvalidateUserCaps
		// broadcasts into the void same-process.
		if err := reg.Start(ctx); err != nil {
			t.Fatalf("reg.Start: %v", err)
		}
		t.Cleanup(reg.Stop)

		if r.caps == nil {
			t.Fatal("expected caps cache to be wired when registry is supplied")
		}
		if got := r.caps.Len(); got != 0 {
			t.Errorf("baseline cache size = %d, want 0", got)
		}

		// First load — cold cache, populates from DB.
		id := &Identity{UserRef: fx.userRef}
		r.loadCapabilities(ctx, q, id)
		if got := r.caps.Len(); got != 1 {
			t.Errorf("after first load: cache size = %d, want 1", got)
		}
		key := strconv.FormatInt(fx.userRef, 10)
		v, ok := r.caps.Get(key)
		if !ok {
			t.Fatalf("expected cache hit for key %q", key)
		}
		if !contains(v.Global, "test.caps.cached") {
			t.Errorf("cached caps missing test.caps.cached: got %v", v.Global)
		}

		// Second load — warm cache. Cache size stays at 1; the same
		// Identity fields populate.
		id2 := &Identity{UserRef: fx.userRef}
		r.loadCapabilities(ctx, q, id2)
		if got := r.caps.Len(); got != 1 {
			t.Errorf("after second load: cache size = %d, want 1", got)
		}
		if !contains(id2.Capabilities, "test.caps.cached") {
			t.Errorf("warm-cache load missing test.caps.cached: %v", id2.Capabilities)
		}

		// Invalidate via the public helper (the same path
		// SetUserGlobalRole uses). The broadcast goes via NOTIFY →
		// LISTEN goroutine → dispatch → cache.invalidate. We poll-wait
		// for the LISTEN side to catch up; deadline is generous so
		// CI hiccups don't flake this.
		InvalidateUserCaps(ctx, reg, fx.userRef)
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, ok := r.caps.Get(key); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("cache entry still present 2s after InvalidateUserCaps")
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Third load re-populates.
		id3 := &Identity{UserRef: fx.userRef}
		r.loadCapabilities(ctx, q, id3)
		if got := r.caps.Len(); got != 1 {
			t.Errorf("after re-load post-invalidation: cache size = %d, want 1", got)
		}
	})
}

// TestCapsCache_NilRegistryFallsBackToDB verifies that a Resolver
// without a registry (the test-only legacy construction) still works
// — it just hits the DB every call.
func TestCapsCache_NilRegistryFallsBackToDB(t *testing.T) {
	pwd := envOr("AA_DB_PASSWORD", "")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	r := NewResolver(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if r.caps != nil {
		t.Errorf("expected caps cache to be nil with no registry")
	}

	// loadCapabilities with nil cache should still set Identity.Capabilities
	// (or leave it empty for unknown users) without panicking.
	id := &Identity{UserRef: 1} // probably unknown — that's fine
	r.loadCapabilities(context.Background(), New(pool), id)
	// No assertion on the cap set; we're testing that nil-cache works.
}

