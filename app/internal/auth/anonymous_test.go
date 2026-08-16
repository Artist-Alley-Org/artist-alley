// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLoadAnonymousIdentity_SeededHasNoCaps verifies the Anonymous
// role from migration 00001 exists and starts with zero caps, so
// id.Can("anything") returns false out of the box.
func TestLoadAnonymousIdentity_SeededHasNoCaps(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openAnonPool(t, pwd)
	defer pool.Close()

	id := LoadAnonymousIdentity(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if id == nil {
		t.Fatal("LoadAnonymousIdentity returned nil")
	}
	if id.UserRef != 0 {
		t.Errorf("UserRef=%d, want 0 (anonymous sentinel)", id.UserRef)
	}
	if !id.IsAnonymous() {
		t.Errorf("IsAnonymous() = false, want true")
	}
	if id.Can("posts.read.public") {
		t.Errorf("anonymous identity should not have posts.read.public by default")
	}
	if id.Can("system.admin") {
		t.Errorf("anonymous identity must not have system.admin")
	}
}

// TestLoadAnonymousIdentity_ReflectsRoleCaps confirms that granting a
// capability to the Anonymous role makes it visible to subsequent
// LoadAnonymousIdentity calls — i.e. the helper actually walks the
// role chain at request time.
func TestLoadAnonymousIdentity_ReflectsRoleCaps(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openAnonPool(t, pwd)
	defer pool.Close()

	// Seed a throwaway capability and grant it to Anonymous.
	const cap = "test.anon.allowed"
	if _, err := pool.Exec(ctx,
		`INSERT INTO capabilities (code) VALUES ($1) ON CONFLICT DO NOTHING`, cap,
	); err != nil {
		t.Fatalf("seed cap: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO role_capabilities (role_id, capability_code)
		SELECT id, $1 FROM roles WHERE name = 'Anonymous'
		ON CONFLICT DO NOTHING
	`, cap); err != nil {
		t.Fatalf("grant cap to Anonymous: %v", err)
	}
	t.Cleanup(func() {
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `
			DELETE FROM role_capabilities
			WHERE capability_code = $1
			  AND role_id = (SELECT id FROM roles WHERE name = 'Anonymous')
		`, cap)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM capabilities WHERE code = $1`, cap)
	})

	id := LoadAnonymousIdentity(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !id.Can(cap) {
		t.Errorf("anonymous identity should have %q after grant, got caps=%v", cap, id.Capabilities)
	}
}

// openAnonPool is a tiny test pool opener, separate from the one in
// handler_test.go to avoid coupling these focused tests to that test
// fixture's larger lifecycle.
func openAnonPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := os.Getenv("AA_DB_HOST")
	if host == "" {
		host = "postgres"
	}
	port := os.Getenv("AA_DB_PORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("AA_DB_USER")
	if user == "" {
		user = "artist_alley"
	}
	name := os.Getenv("AA_DB_NAME")
	if name == "" {
		name = "artist_alley"
	}
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
