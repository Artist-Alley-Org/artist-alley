// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package users_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// TestProfileCache_InvalidateProfileEvicts verifies that the public
// InvalidateProfile helper drops the cached entry. Posts-side
// invalidations (post create/delete bumping the author's post_count)
// go through this helper, and the LISTEN goroutine dispatches the
// invalidation to the users-package cache.
//
// We exercise the registry-level path end-to-end: start the LISTEN
// goroutine, write a synthetic profile-cache entry via reflection-
// free Add (the package exports the cache.Cache through the Handler,
// but we can't reach it from _test). Instead we Register a separate
// Cache on the same domain to observe the dispatch.
func TestProfileCache_InvalidateProfileEvicts(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("reg.Start: %v", err)
	}
	t.Cleanup(reg.Stop)

	// Register an observer cache under the same domain. NOTIFYs on
	// user.profile will hit this cache's invalidate(key) the same way
	// they hit the production users.Handler.byRef cache. This lets the
	// test verify the dispatch path without reaching into the
	// Handler's private field.
	obs := cache.Register[string](reg, users.CacheDomain, 16)
	obs.Add("42", "alice")
	if got := obs.Len(); got != 1 {
		t.Fatalf("baseline cache size = %d, want 1", got)
	}

	// Broadcast the invalidation via the public helper — same call
	// the posts handler makes on CreatePost / DeletePost.
	users.InvalidateProfile(ctx, reg, 42)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := obs.Get("42"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache entry still present 2s after InvalidateProfile")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProfileCache_NilRegistry verifies the no-op nil-registry path
// for InvalidateProfile and that NewHandler(nil) returns a working
// handler with no cache.
func TestProfileCache_NilRegistry(t *testing.T) {
	// No DB needed; we're testing the nil-guard.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := users.NewHandler(nil, logger, nil)
	if h == nil {
		t.Fatal("NewHandler should return a handler even with nil registry")
	}
	users.InvalidateProfile(context.Background(), nil, 99) // must not panic
}

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

var _ = pgx.ErrNoRows // silence unused import if a sub-test gets commented out
var _ = strconv.Itoa
