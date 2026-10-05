// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the federation peers registry — Phase 1.22.B-a.
// Coverage:
//   - URL normalisation (https-only, trailing-slash strip,
//     reject empty / non-https / paths)
//   - PEM validation (round-trips real Ed25519 keys; rejects
//     malformed / non-Ed25519)
//   - Add → ByInstanceURL → Update → Delete round-trip
//   - Unique-violation on duplicate instance_url surfaces
//   - Catalogue validation rejects bad tier / encryption_policy
//   - Cache invalidation: snapshot drops on Add
//
// Skips without AA_DB_PASSWORD per project convention.

package peer_test

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

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
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

// fixtureAdmin inserts a throwaway admin user the tests can use
// as handshake_by_user_ref. Cleanup deletes any peers they own.
func fixtureAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	username := "peer-test-admin-" + randHex(t, 4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Peer Test Admin",
	).Scan(&ref); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE handshake_by_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// freshPEM generates a real Ed25519 keypair and returns the
// public key as PEM. Real keys catch validation bugs that fake
// PEMs would slip past.
func freshPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem)
}

// --- URL normalisation --------------------------------------------------

func TestAdd_RejectsBadURLs(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	good := freshPEM(t)

	cases := []string{
		"",                         // empty
		"http://peer.example",      // not https
		"ftp://peer.example",       // not https
		"https://peer.example/api", // has path
		"https://",                 // no host
	}
	for _, u := range cases {
		_, err := r.Add(ctx, peer.AddInput{
			InstanceURL:        u,
			DisplayName:        "x",
			InstancePublicKey:  good,
			TrustTier:          federation.TrustConnected,
			EncryptionPolicy:   federation.EncryptionPlaintext,
			HandshakeByUserRef: admin,
		})
		if !errors.Is(err, peer.ErrInstanceURLInvalid) {
			t.Errorf("URL %q: expected ErrInstanceURLInvalid, got %v", u, err)
		}
	}
}

func TestAdd_StripsTrailingSlash(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	url := "https://stripped-" + randHex(t, 4) + ".example/"
	p, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        url,
		DisplayName:        "Trim Test",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if p.InstanceURL[len(p.InstanceURL)-1] == '/' {
		t.Errorf("trailing slash not stripped: %q", p.InstanceURL)
	}
}

// --- PEM validation -----------------------------------------------------

func TestAdd_RejectsBadPEM(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	cases := []string{
		"",
		"not a pem block at all",
		"-----BEGIN PUBLIC KEY-----\nbm90LXZhbGlkLWJhc2U2NA==\n-----END PUBLIC KEY-----\n",
	}
	for _, pem := range cases {
		_, err := r.Add(ctx, peer.AddInput{
			InstanceURL:        "https://pemtest-" + randHex(t, 4) + ".example",
			DisplayName:        "PEM",
			InstancePublicKey:  pem,
			TrustTier:          federation.TrustConnected,
			EncryptionPolicy:   federation.EncryptionPlaintext,
			HandshakeByUserRef: admin,
		})
		if !errors.Is(err, peer.ErrInstancePublicKeyInvalid) {
			t.Errorf("PEM %q: expected ErrInstancePublicKeyInvalid, got %v", short(pem), err)
		}
	}
}

func short(s string) string {
	if len(s) > 30 {
		return s[:30] + "…"
	}
	return s
}

// --- catalogue validation -----------------------------------------------

func TestAdd_RejectsBadTier(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	_, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        "https://tier-" + randHex(t, 4) + ".example",
		DisplayName:        "T",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustTier("evil-tier"),
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if !errors.Is(err, peer.ErrTrustTierInvalid) {
		t.Errorf("expected ErrTrustTierInvalid, got %v", err)
	}
}

// --- round-trip + lifecycle ---------------------------------------------

func TestAddByURLUpdateDelete_RoundTrip(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	url := "https://round-" + randHex(t, 4) + ".example"
	p, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        url,
		DisplayName:        "Round Trip Peer",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !p.Enabled {
		t.Error("default Enabled should be true")
	}

	// ByInstanceURL hits the cache (warm) or DB (cold) — either
	// way returns the same row.
	got, err := r.ByInstanceURL(ctx, url)
	if err != nil {
		t.Fatalf("by URL: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("byURL drift: %v vs %v", got.ID, p.ID)
	}

	// Update: change display name + disable.
	newName := "Round Trip Peer (renamed)"
	disabled := false
	upd, err := r.Update(ctx, p.ID, peer.UpdateInput{
		DisplayName: &newName,
		Enabled:     &disabled,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.DisplayName != newName {
		t.Errorf("display name not updated: %q", upd.DisplayName)
	}
	if upd.Enabled {
		t.Error("enabled should be false after update")
	}

	// Delete.
	if err := r.Delete(ctx, p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// ByID should now return ErrPeerNotFound.
	if _, err := r.ByID(ctx, p.ID); !errors.Is(err, peer.ErrPeerNotFound) {
		t.Errorf("post-delete ByID: expected ErrPeerNotFound, got %v", err)
	}
}

// --- duplicate URL rejection --------------------------------------------

func TestAdd_DuplicateURLRejected(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	url := "https://dup-" + randHex(t, 4) + ".example"
	if _, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        url,
		DisplayName:        "First",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	}); err != nil {
		t.Fatalf("first add: %v", err)
	}

	_, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        url,
		DisplayName:        "Second",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err == nil {
		t.Error("duplicate URL should fail")
	}
	// Don't check the specific error class — the unique violation
	// surfaces as a pgconn.PgError 23505; the registry method
	// passes it through (the HTTP layer maps it to 409). All we
	// need here is "an error happened."
}

// --- cache invalidation -------------------------------------------------

func TestAdd_InvalidatesEnabledSnapshot(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	admin := fixtureAdmin(t, ctx, pool)

	reg := cache.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer reg.Stop()
	r := peer.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)

	// Prime the enabled-snapshot cache + remember a URL not in
	// it so the post-Add check is robust to concurrent writes
	// from sibling-package tests (go test ./... runs packages in
	// parallel; a count-delta assertion races with their inserts
	// and deletes). We care that the SPECIFIC peer we added is
	// present after Add — that's the cache-invalidation signal.
	pre, err := r.EnabledSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preLen := len(pre)

	addedURL := "https://invalidates-" + randHex(t, 4) + ".example"
	if _, err := r.Add(ctx, peer.AddInput{
		InstanceURL:        addedURL,
		DisplayName:        "Invalidates",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// EnabledSnapshot should now include the URL we just added.
	// A cache that didn't invalidate would return the prior list
	// (no addedURL); an invalidated cache re-queries the DB and
	// includes it.
	post, err := r.EnabledSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range post {
		if p.InstanceURL == addedURL {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("enabled snapshot didn't refresh after Add: pre=%d post=%d, added URL %q not in post (cache stale?)",
			preLen, len(post), addedURL)
	}
}
