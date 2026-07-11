// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 handler integration tests. Real Postgres;
// skips without AA_DB_PASSWORD.
//
// Covers the load-bearing contracts:
//
//   * Upsert idempotency (same lang twice → row updated)
//   * Cache hit-then-write-then-miss cycle
//   * RequiresAudioVideo policy gate on Upsert + Delete
//   * Asset hard-delete → CASCADE wipes tracks + cache invalidates
//   * SELECT count(*) FROM assets excludes subtitle tracks (the
//     load-bearing operator constraint)
//   * Orphan-track INSERT rejected by FK
//   * Track row order (ASC by lang)

package subtitles_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
)

// --- fixture helpers -------------------------------------------------

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

// fixtureAsset inserts an asset of the given asset_type ref + returns
// its UUID. assetTypeRef: 3=Video, 4=Audio, 1=Image. Cleanup deletes
// the asset (CASCADE removes any tracks the test inserted).
func fixtureAsset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, assetTypeRef int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	title := "subtitle-test-" + randHex(t, 4)
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, status)
		 VALUES ($1, $2, $3, 'active')`,
		id, title, assetTypeRef,
	); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func newHandler(t *testing.T, pool *pgxpool.Pool) *subtitles.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := cache.NewRegistry(pool, logger)
	return subtitles.NewHandler(pool, reg, logger)
}

func mustVideo(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	return fixtureAsset(t, ctx, pool, 3) // 3 = Video
}

func mustImage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	return fixtureAsset(t, ctx, pool, 1) // 1 = Image
}

// --- tests -----------------------------------------------------------

func TestUpsert_NewTrack_Inserts(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	got, err := h.Upsert(ctx, subtitles.Track{
		AssetID:      assetID,
		Lang:         "en",
		Label:        "English",
		FileHash:     "abc123",
		SourceFormat: "vtt",
		Confidence:   1.0,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got.AssetID != assetID || got.Lang != "en" {
		t.Errorf("inserted shape mismatch: %+v", got)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence=%v, want 1.0", got.Confidence)
	}
}

func TestUpsert_ExistingLang_Replaces(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	first := subtitles.Track{AssetID: assetID, Lang: "en", Label: "v1", FileHash: "h1", SourceFormat: "srt", Confidence: 1.0}
	second := subtitles.Track{AssetID: assetID, Lang: "en", Label: "v2", FileHash: "h2", SourceFormat: "vtt", Confidence: 0.85}
	if _, err := h.Upsert(ctx, first); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if _, err := h.Upsert(ctx, second); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got, err := h.Get(ctx, assetID, "en")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Label != "v2" || got.FileHash != "h2" {
		t.Errorf("second upsert didn't replace; got %+v", got)
	}
	if got.SourceFormat != "vtt" {
		t.Errorf("source didn't update: %+v", got)
	}
	// float32 round-trip introduces ULP-level imprecision; tolerate.
	if got.Confidence < 0.84 || got.Confidence > 0.86 {
		t.Errorf("confidence didn't update: got %v want ~0.85", got.Confidence)
	}
}

func TestUpsert_OnImageAsset_NotApplicable(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustImage(t, ctx, pool)
	_, err := h.Upsert(ctx, subtitles.Track{
		AssetID:      assetID,
		Lang:         "en",
		FileHash:     "h",
		SourceFormat: "vtt",
		Confidence:   1.0,
	})
	if !errors.Is(err, subtitles.ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable", err)
	}
	// Confirm nothing was inserted.
	n, _ := h.Count(ctx, assetID)
	if n != 0 {
		t.Errorf("track count=%d after failed Upsert; want 0", n)
	}
}

func TestUpsert_InvalidLang_Rejected(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	_, err := h.Upsert(ctx, subtitles.Track{
		AssetID:      assetID,
		Lang:         "1234",
		FileHash:     "h",
		SourceFormat: "vtt",
		Confidence:   1.0,
	})
	if !errors.Is(err, subtitles.ErrInvalidLang) {
		t.Errorf("err=%v, want ErrInvalidLang", err)
	}
}

func TestGetForAsset_EmptyAsset_ReturnsEmptySlice(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	got, err := h.GetForAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetForAsset: %v", err)
	}
	if got == nil {
		t.Errorf("got nil, want []subtitles.Track{} (non-nil empty slice)")
	}
	if len(got) != 0 {
		t.Errorf("len=%d, want 0", len(got))
	}
}

func TestGetForAsset_OrderByLang(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	// Insert in non-alphabetical order.
	for _, lang := range []string{"zh", "en", "ja"} {
		if _, err := h.Upsert(ctx, subtitles.Track{
			AssetID:      assetID,
			Lang:         lang,
			FileHash:     "h-" + lang,
			SourceFormat: "vtt",
			Confidence:   1.0,
		}); err != nil {
			t.Fatalf("Upsert(%s): %v", lang, err)
		}
	}
	got, err := h.GetForAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetForAsset: %v", err)
	}
	want := []string{"en", "ja", "zh"}
	if len(got) != 3 {
		t.Fatalf("count=%d, want 3", len(got))
	}
	for i, lang := range want {
		if got[i].Lang != lang {
			t.Errorf("order mismatch at %d: got %q want %q", i, got[i].Lang, lang)
		}
	}
}

func TestDelete_RemovesRow(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	if _, err := h.Upsert(ctx, subtitles.Track{
		AssetID: assetID, Lang: "en", FileHash: "h", SourceFormat: "vtt", Confidence: 1.0,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.Delete(ctx, assetID, "en"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := h.Get(ctx, assetID, "en")
	if !errors.Is(err, subtitles.ErrTrackNotFound) {
		t.Errorf("post-delete Get err=%v, want ErrTrackNotFound", err)
	}
}

func TestDelete_UnknownLang_ReturnsNotFound(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	err := h.Delete(ctx, assetID, "fr")
	if !errors.Is(err, subtitles.ErrTrackNotFound) {
		t.Errorf("err=%v, want ErrTrackNotFound", err)
	}
}

func TestDelete_OnImageAsset_NotApplicable_GateFiresFirst(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustImage(t, ctx, pool)
	err := h.Delete(ctx, assetID, "en")
	if !errors.Is(err, subtitles.ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable (the gate must fire BEFORE the row lookup; 422 not 404)", err)
	}
}

func TestAssetHardDelete_CascadesViaFK(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	if _, err := h.Upsert(ctx, subtitles.Track{
		AssetID: assetID, Lang: "en", FileHash: "h", SourceFormat: "vtt", Confidence: 1.0,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Now hard-delete the asset directly.
	if _, err := pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID); err != nil {
		t.Fatalf("delete asset: %v", err)
	}
	// FK CASCADE should have wiped the track row.
	n, _ := h.Count(ctx, assetID)
	if n != 0 {
		t.Errorf("track count=%d after asset delete; want 0 (CASCADE failed?)", n)
	}
}

// TestAssetCount_ExcludesSubtitleTracks LOCKS the load-bearing
// operator constraint: subtitles MUST NOT count toward asset
// totals. The schema separation makes this true automatically,
// but the test pins it so a future "let's unify into asset_attachments"
// refactor surfaces in CI.
func TestAssetCount_ExcludesSubtitleTracks(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	// Snapshot the pre-test asset count.
	var before int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE deleted_at IS NULL`).Scan(&before); err != nil {
		t.Fatalf("pre-test count: %v", err)
	}

	assetID := mustVideo(t, ctx, pool)
	// Add 5 tracks.
	for _, lang := range []string{"en", "ja", "fr", "de", "es"} {
		if _, err := h.Upsert(ctx, subtitles.Track{
			AssetID: assetID, Lang: lang, FileHash: "h", SourceFormat: "vtt", Confidence: 1.0,
		}); err != nil {
			t.Fatalf("Upsert(%s): %v", lang, err)
		}
	}

	// Post-test asset count should be before + 1 (the asset),
	// NOT before + 6 (asset + 5 tracks).
	var after int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE deleted_at IS NULL`).Scan(&after); err != nil {
		t.Fatalf("post-test count: %v", err)
	}
	if after != before+1 {
		t.Errorf("asset count delta = %d, want 1 (subtitle tracks should NOT count); before=%d after=%d", after-before, before, after)
	}

	// Sanity: the tracks DID land in the right table.
	n, _ := h.Count(ctx, assetID)
	if n != 5 {
		t.Errorf("subtitle count=%d, want 5", n)
	}
}

func TestSubtitleTrack_OrphanInsert_FKViolation(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Direct INSERT with a non-existent asset_id. Must fail at
	// the FK constraint layer (the gate is bypassed in this raw
	// SQL).
	bogus := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO asset_subtitle_tracks (asset_id, lang, file_hash, source_format)
		 VALUES ($1, 'en', 'h', 'vtt')`,
		bogus,
	)
	if err == nil {
		t.Errorf("orphan INSERT accepted; want FK violation")
	}
}

func TestUpsert_InvalidatesCache(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()
	h := newHandler(t, pool)

	assetID := mustVideo(t, ctx, pool)
	// First read populates cache (empty result).
	got, err := h.GetForAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetForAsset 1: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("initial got %d tracks, want 0", len(got))
	}
	// Upsert should bust the cache.
	if _, err := h.Upsert(ctx, subtitles.Track{
		AssetID: assetID, Lang: "en", FileHash: "h", SourceFormat: "vtt", Confidence: 1.0,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Re-read should reflect the insert.
	got, err = h.GetForAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetForAsset 2: %v", err)
	}
	if len(got) != 1 || got[0].Lang != "en" {
		t.Errorf("post-upsert read = %+v, want one en track", got)
	}
}
