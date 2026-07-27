// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/assets"
)

// Compile-time satisfaction — duplicates the runtime asserts in
// ai_bridge.go so a future signature change to either interface
// surfaces here as a build break in the test binary too.
var (
	_ ai.AssetLookup = (*assets.Handler)(nil)
	_ ai.TagWriter   = (*assets.Handler)(nil)
)

// newBridgeHandler returns a Handler wired with the minimal deps the
// bridge methods need (pool + logger). The storage / jobs / cache
// fields aren't touched by GetAssetForAI or SetAITagsForAsset.
func newBridgeHandler(t *testing.T, pool *pgxpool.Pool) *assets.Handler {
	t.Helper()
	return assets.NewHandler(pool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
}

// seedBridgeAsset inserts a row directly via SQL — avoids dragging in
// the whole storage + upload flow for a read/write-side bridge test.
// Returns the new asset id. Caller schedules its own cleanup.
//
// Phase 1.18.A-2 follow-up A added a partial unique index on
// (owner_user_ref, file_hash) WHERE deleted_at IS NULL. To stay
// independent of prior-run leftovers in the shared dev DB, this
// helper hard-deletes any live row matching (ownerRef, hash)
// before inserting. The tests using this helper pin specific
// owner+hash pairs for narrative clarity, not because the values
// are load-bearing.
func seedBridgeAsset(t *testing.T, pool *pgxpool.Pool, ownerRef int64, title, hash string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	// Sweep any leftover live row for this (owner, hash) so the
	// unique index doesn't block our fresh insert.
	if _, err := pool.Exec(ctx,
		`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
		ownerRef, hash,
	); err != nil {
		t.Fatalf("sweep prior asset: %v", err)
	}
	// file_hash FK → storage_objects(hash); the check constraint
	// demands 64-char hex. Seed a parent storage row so the FK
	// resolves. ON CONFLICT DO NOTHING in case two test cases
	// happen to share a hash.
	_, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash)
	if err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	// asset_type is a FK to asset_types(ref); ref=1 is the seeded
	// "Image" type. file_extension='png' is what drives the bridge's
	// MimeType derivation to "image/png" (#621) — it used to stamp
	// has_image=true, a column no production path writes, which is
	// exactly the fixture pattern that let #614/#618 hide.
	_, err = pool.Exec(ctx, `
		INSERT INTO assets (
			id, title, asset_type, owner_user_ref, status,
			file_hash, file_extension, file_size_bytes,
			sensitivity
		) VALUES ($1, $2, 1, $3, 'active', $4, 'png', 1024, 'public')
	`, id, title, ownerRef, hash)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	return id
}

func seedTag(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID, tag, source string, conf *float32) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO asset_tag (asset_id, tag, source, confidence) VALUES ($1,$2,$3,$4)`,
		pgtype.UUID{Bytes: assetID, Valid: true}, tag, source, conf,
	)
	if err != nil {
		t.Fatalf("seed tag(%s/%s): %v", tag, source, err)
	}
}

// readTagsBySource returns the asset's tag values grouped by source.
// Used to assert merge semantics after SetAITagsForAsset.
func readTagsBySource(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) map[string][]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT tag, source FROM asset_tag WHERE asset_id = $1 ORDER BY source, tag`,
		pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var tag, src string
		if err := rows.Scan(&tag, &src); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[src] = append(out[src], tag)
	}
	return out
}

func cleanupBridgeAsset(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_tag WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})
}

// ---------------------------------------------------------------------------
// GetAssetForAI
// ---------------------------------------------------------------------------

func TestGetAssetForAI_HappyPath(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_142_001
	const hash = "deadbeefcafebabe1234567890abcdef0123456789abcdef0123456789abcdef"
	assetID := seedBridgeAsset(t, pool, ownerRef, "bridge happy", hash)
	cleanupBridgeAsset(t, pool, assetID)
	seedTag(t, pool, assetID, "operator-set", "manual", nil)
	seedTag(t, pool, assetID, "imported", "import", nil)

	h := newBridgeHandler(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := h.GetAssetForAI(ctx, assetID)
	if err != nil {
		t.Fatalf("GetAssetForAI: %v", err)
	}
	if got.ID != assetID {
		t.Errorf("id = %s, want %s", got.ID, assetID)
	}
	if got.Title != "bridge happy" {
		t.Errorf("title = %q, want %q", got.Title, "bridge happy")
	}
	if got.Sensitivity != ai.SensitivityPublic {
		t.Errorf("sensitivity = %q, want public", got.Sensitivity)
	}
	if got.ContentHash != hash {
		t.Errorf("content hash = %q, want %q", got.ContentHash, hash)
	}
	if got.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", got.MimeType)
	}

	// Two tags total; both manual + import — no AI tags yet.
	if len(got.ExistingTags) != 2 {
		t.Fatalf("existing tags = %d, want 2 (%+v)", len(got.ExistingTags), got.ExistingTags)
	}
	bySource := map[ai.TagSource]string{}
	for _, tg := range got.ExistingTags {
		bySource[tg.Source] = tg.Value
	}
	if bySource[ai.TagSourceManual] != "operator-set" {
		t.Errorf("manual tag = %q, want operator-set", bySource[ai.TagSourceManual])
	}
	if bySource[ai.TagSourceImport] != "imported" {
		t.Errorf("import tag = %q, want imported", bySource[ai.TagSourceImport])
	}
}

func TestGetAssetForAI_NotFoundReturnsSentinel(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()
	h := newBridgeHandler(t, pool)

	_, err := h.GetAssetForAI(context.Background(), uuid.New())
	if !errors.Is(err, ai.ErrAssetNotFound) {
		t.Fatalf("got %v, want ai.ErrAssetNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// SetAITagsForAsset — merge semantics
// ---------------------------------------------------------------------------

func TestSetAITagsForAsset_PreservesManualAndImport(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_142_002
	assetID := seedBridgeAsset(t, pool, ownerRef, "merge test",
		"deadbeefcafebabe2222222222abcdef0123456789abcdef0123456789abcdef")
	cleanupBridgeAsset(t, pool, assetID)

	// Pre-seed three sources. The AI re-run should replace AI rows
	// only — manual + import survive untouched.
	seedTag(t, pool, assetID, "operator-portrait", "manual", nil)
	seedTag(t, pool, assetID, "csv-batch-2025", "import", nil)
	stale := float32(0.4)
	seedTag(t, pool, assetID, "old-ai-tag", "ai", &stale)

	h := newBridgeHandler(t, pool)
	ctx := context.Background()

	err := h.SetAITagsForAsset(ctx, assetID, []ai.TagOutput{
		{Value: "fresh-ai-1", Confidence: 0.91},
		{Value: "fresh-ai-2", Confidence: 0.72},
		{Value: "", Confidence: 0.5}, // defensive: empty tag is skipped
	}, ai.AIProvenance{
		Provider:      "openai",
		Model:         "gpt-4o",
		PromptVersion: "v1",
	})
	if err != nil {
		t.Fatalf("SetAITagsForAsset: %v", err)
	}

	got := readTagsBySource(t, pool, assetID)
	if want := []string{"operator-portrait"}; !equalSorted(got["manual"], want) {
		t.Errorf("manual tags = %v, want %v", got["manual"], want)
	}
	if want := []string{"csv-batch-2025"}; !equalSorted(got["import"], want) {
		t.Errorf("import tags = %v, want %v", got["import"], want)
	}
	if want := []string{"fresh-ai-1", "fresh-ai-2"}; !equalSorted(got["ai"], want) {
		t.Errorf("ai tags = %v, want %v (stale tag must be deleted; empty skipped)", got["ai"], want)
	}
}

func TestSetAITagsForAsset_EmptyInputClearsAIOnly(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_142_003
	assetID := seedBridgeAsset(t, pool, ownerRef, "empty clears",
		"deadbeefcafebabe3333333333abcdef0123456789abcdef0123456789abcdef")
	cleanupBridgeAsset(t, pool, assetID)
	seedTag(t, pool, assetID, "keep-me", "manual", nil)
	conf := float32(0.6)
	seedTag(t, pool, assetID, "drop-me", "ai", &conf)

	h := newBridgeHandler(t, pool)
	err := h.SetAITagsForAsset(context.Background(), assetID, nil, ai.AIProvenance{
		Provider: "anthropic", Model: "claude-3-5", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatalf("SetAITagsForAsset: %v", err)
	}

	got := readTagsBySource(t, pool, assetID)
	if want := []string{"keep-me"}; !equalSorted(got["manual"], want) {
		t.Errorf("manual = %v, want %v", got["manual"], want)
	}
	if len(got["ai"]) != 0 {
		t.Errorf("ai tags = %v, want none (empty input still deletes stale)", got["ai"])
	}
}

func TestSetAITagsForAsset_NotFoundReturnsSentinel(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	h := newBridgeHandler(t, pool)
	err := h.SetAITagsForAsset(context.Background(), uuid.New(),
		[]ai.TagOutput{{Value: "x", Confidence: 0.9}},
		ai.AIProvenance{Provider: "openai", Model: "gpt-4o"})
	if !errors.Is(err, ai.ErrAssetNotFound) {
		t.Fatalf("got %v, want ai.ErrAssetNotFound", err)
	}
}

// equalSorted reports whether two string slices contain the same
// values (order-independent). Both inputs are sorted as a side
// effect — fine for short test slices.
func equalSorted(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, v := range got {
		seen[v]++
	}
	for _, v := range want {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}
