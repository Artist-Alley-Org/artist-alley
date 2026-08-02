// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Integration tests for operator string overrides (#794, ADR 0081 §1).
//
// Real Postgres (skipped without AA_DB_PASSWORD). Covers:
//
//   - the anonymous read, and that it returns `{}` rather than null
//   - the write cap gate (anonymous / wrong cap / config.write / admin)
//   - an unknown key → 422 WHOSE MESSAGE NAMES THE KEY
//   - locale scoping: an `es` override does not touch `en`
//   - cache invalidation with no restart: write then immediately read
//     through the SAME handler and see the new value
//   - the cross-instance pg_notify path: a write on one registry drops
//     the peer registry's copy

package sitetext_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sitetext"
)

// A key that certainly exists in the shipped catalogue; asserted by
// catalogue_test.go so a rename shows up there first.
const realKey = "nav.upload"

func TestGetSiteText_AnonymousReadsEmptyObject(t *testing.T) {
	fx := setup(t)
	resp, err := fx.http.GetSiteText(context.Background(), openapi.GetSiteTextRequestObject{})
	if err != nil {
		t.Fatalf("GetSiteText: %v", err)
	}
	got, ok := resp.(openapi.GetSiteText200JSONResponse)
	if !ok {
		t.Fatalf("anonymous read: got %T, want a 200 — this endpoint is deliberately ungated", resp)
	}
	// Non-nil, so the JSON is `{}` and a client never has to
	// special-case "no overrides yet" — the state of every fresh
	// install.
	if got.Overrides == nil {
		t.Errorf("Overrides is nil; want an empty map so the response marshals as {}")
	}
}

func TestSetSiteText_RejectsAnonymous(t *testing.T) {
	fx := setup(t)
	resp, err := fx.http.SetSiteText(context.Background(), setReq(realKey, "en", "Add"))
	if err != nil {
		t.Fatalf("SetSiteText: %v", err)
	}
	if _, ok := resp.(openapi.SetSiteText401JSONResponse); !ok {
		t.Errorf("anonymous write: got %T, want 401", resp)
	}
}

func TestSetSiteText_RejectsWithoutConfigWrite(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.config.read")
	resp, err := fx.http.SetSiteText(ctx, setReq(realKey, "en", "Add"))
	if err != nil {
		t.Fatalf("SetSiteText: %v", err)
	}
	if _, ok := resp.(openapi.SetSiteText403JSONResponse); !ok {
		t.Errorf("config.read holder: got %T, want 403 — the read cap must not open the write", resp)
	}
}

func TestSetSiteText_SystemAdminIsSufficient(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.admin")
	resp, err := fx.http.SetSiteText(ctx, setReq(realKey, "en", "Send us a file"))
	if err != nil {
		t.Fatalf("SetSiteText: %v", err)
	}
	if _, ok := resp.(openapi.SetSiteText200JSONResponse); !ok {
		t.Errorf("system.admin: got %T, want 200 — admin wildcards every cap", resp)
	}
}

// TestSetSiteText_UnknownKeyIs422NamingTheKey is the fail-loud rule.
//
// The assertion is deliberately on the MESSAGE, not just the status: an
// operator who mistypes a key has to be able to tell that it was the
// key that was wrong. A bare 422 would leave them unable to distinguish
// "I typed it wrong" from "this feature is broken", which is the exact
// ambiguity ADR 0081 cites #774 for.
func TestSetSiteText_UnknownKeyIs422NamingTheKey(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.config.write")
	const bogus = "nav.uploadd"

	resp, err := fx.http.SetSiteText(ctx, setReq(bogus, "en", "whatever"))
	if err != nil {
		t.Fatalf("SetSiteText: %v", err)
	}
	got, ok := resp.(openapi.SetSiteText422JSONResponse)
	if !ok {
		t.Fatalf("unknown key: got %T, want 422", resp)
	}
	if !strings.Contains(got.Error, bogus) {
		t.Errorf("422 message %q does not name the refused key %q", got.Error, bogus)
	}

	// And nothing was stored — a refusal that still wrote a row would
	// be the worst of both.
	all, err := fx.domain.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, present := all["en"][bogus]; present {
		t.Errorf("a refused key was stored anyway")
	}
}

// TestWriteThenReadWithoutRestart is acceptance #7: the write
// invalidates the cache, so the very next read through the same live
// handler sees the new value. Before the invalidation existed this
// would return the pre-write map until the process restarted.
func TestWriteThenReadWithoutRestart(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.config.write")

	// Warm the cache so the test proves invalidation rather than a
	// cold miss. Without this read, the assertion below would pass
	// even with Invalidate() deleted.
	if _, err := fx.domain.All(ctx); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	if _, err := fx.domain.Set(ctx, realKey, "en", "Send us a file", &fx.userRef); err != nil {
		t.Fatalf("Set: %v", err)
	}
	all, err := fx.domain.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := all["en"][realKey]; got != "Send us a file" {
		t.Errorf("after write, read gave %q; want the new value with no restart", got)
	}

	// Revert restores the shipped string by removing the row.
	if err := fx.domain.Delete(ctx, realKey, "en"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, err = fx.domain.All(ctx)
	if err != nil {
		t.Fatalf("All after delete: %v", err)
	}
	if _, present := all["en"][realKey]; present {
		t.Errorf("after revert the override is still served")
	}
}

// TestLocaleScoping pins acceptance #6: overriding a key for one
// language leaves every other language alone. `language` being part of
// the primary key is what makes this true, and it is the property that
// would silently disappear if somebody "simplified" the table.
func TestLocaleScoping(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.config.write")

	if _, err := fx.domain.Set(ctx, realKey, "es", "Subir", nil); err != nil {
		t.Fatalf("Set es: %v", err)
	}
	all, err := fx.domain.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := all["es"][realKey]; got != "Subir" {
		t.Errorf("es override = %q, want Subir", got)
	}
	if _, present := all["en"][realKey]; present {
		t.Errorf("an es override leaked into en")
	}
}

func TestDeleteSiteText_MissingIs404(t *testing.T) {
	fx := setup(t)
	ctx := fx.identity(context.Background(), "system.config.write")
	resp, err := fx.http.DeleteSiteText(ctx, openapi.DeleteSiteTextRequestObject{
		Key:    realKey,
		Params: openapi.DeleteSiteTextParams{Language: "fr"},
	})
	if err != nil {
		t.Fatalf("DeleteSiteText: %v", err)
	}
	if _, ok := resp.(openapi.DeleteSiteText404JSONResponse); !ok {
		t.Errorf("revert of a never-overridden string: got %T, want 404", resp)
	}
}

// TestCrossInstanceInvalidation exercises the pg_notify path a second
// running instance depends on: a write on registry A must drop the
// cached map on registry B, which is holding its own copy from its own
// earlier read.
func TestCrossInstanceInvalidation(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	ctx := t.Context()

	poolA := openPoolST(t)
	poolB := openPoolST(t)
	logger := discardLogger()

	regA := cache.NewRegistry(poolA, logger)
	regB := cache.NewRegistry(poolB, logger)
	cacheA := sitetext.NewCache(regA, logger)
	cacheB := sitetext.NewCache(regB, logger)
	if err := regA.Start(ctx); err != nil {
		t.Fatalf("regA Start: %v", err)
	}
	defer regA.Stop()
	if err := regB.Start(ctx); err != nil {
		t.Fatalf("regB Start: %v", err)
	}
	defer regB.Stop()
	// Both LISTENs share one NOTIFY pipe; let them subscribe.
	time.Sleep(100 * time.Millisecond)

	instanceA := sitetext.NewHandler(poolA, cacheA, logger)
	instanceB := sitetext.NewHandler(poolB, cacheB, logger)
	t.Cleanup(func() { cleanup(t, poolA) })

	// B warms its own cache — this is the copy that has to be dropped.
	if _, err := instanceB.All(ctx); err != nil {
		t.Fatalf("B warm read: %v", err)
	}
	if _, ok := cacheB.Map.Get(sitetext.CacheKeyAll); !ok {
		t.Fatalf("B did not cache its read; the rest of this test would be vacuous")
	}

	if _, err := instanceA.Set(ctx, realKey, "en", "Contribute", nil); err != nil {
		t.Fatalf("A Set: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cacheB.Map.Get(sitetext.CacheKeyAll); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := cacheB.Map.Get(sitetext.CacheKeyAll); ok {
		t.Fatalf("A's write did not invalidate B — a second instance would serve stale copy until restart")
	}

	// And B's next read picks up A's value.
	all, err := instanceB.All(ctx)
	if err != nil {
		t.Fatalf("B re-read: %v", err)
	}
	if got := all["en"][realKey]; got != "Contribute" {
		t.Errorf("B re-read gave %q, want Contribute", got)
	}
}

// --- fixture -------------------------------------------------------------

type fixture struct {
	pool    *pgxpool.Pool
	domain  *sitetext.Handler
	http    *sitetext.HTTPHandler
	userRef int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pool := openPoolST(t)
	logger := discardLogger()
	// A registry with no pool would panic on Emit, so give it the real
	// one; Start() is deliberately NOT called — these tests exercise
	// local invalidation, and the cross-instance path has its own test.
	reg := cache.NewRegistry(pool, logger)
	domain := sitetext.NewHandler(pool, sitetext.NewCache(reg, logger), logger)
	t.Cleanup(func() { cleanup(t, pool) })
	cleanup(t, pool)
	return &fixture{
		pool:    pool,
		domain:  domain,
		http:    sitetext.NewHTTPHandler(domain, logger),
		userRef: fixtureUser(t, pool),
	}
}

// fixtureUser inserts a throwaway operator.
//
// A REAL ref, not a made-up 1: site_text.updated_by_user_ref carries a
// foreign key, and a synthetic ref made the write fail with a 23503
// that had nothing to do with what the test was asserting. Attributing
// the edit to somebody who exists is also what production does.
func fixtureUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	username := "sitetext-test-" + randSuffix()
	var ref int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Site Text Test Operator",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM site_text WHERE updated_by_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func randSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM site_text"); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

func setReq(key, language, value string) openapi.SetSiteTextRequestObject {
	body := openapi.SiteTextWrite{Language: language, Value: value}
	return openapi.SetSiteTextRequestObject{Key: key, Body: &body}
}

func (f *fixture) identity(ctx context.Context, caps ...string) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{
		UserRef:      f.userRef,
		Username:     "sitetext-fixture",
		AuthMethod:   "session",
		Capabilities: caps,
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openPoolST(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sitetext integration tests skipped")
	}
	dsn := "host=" + envOrST("AA_DB_HOST", "postgres") +
		" port=" + envOrST("AA_DB_PORT", "5432") +
		" user=" + envOrST("AA_DB_USER", "artist_alley") +
		" dbname=" + envOrST("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOrST(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
