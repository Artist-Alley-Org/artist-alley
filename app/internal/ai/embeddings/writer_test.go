// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package embeddings_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/embeddings"
)

// Compile-time satisfaction so a future signature drift surfaces here.
var _ ai.EmbeddingWriter = (*embeddings.Writer)(nil)

func TestWriter_HappyPath_PersistsAndIdempotentReplaces(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_140_001
	assetID := seedAsset(t, pool, ownerRef, "embed happy",
		"deadbeefcafebabe1111111111abcdef0123456789abcdef0123456789abcdef")
	cleanupAsset(t, pool, assetID)

	w, err := embeddings.NewWriter(context.Background(), pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	vec := randomVector(768)
	err = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:     assetID,
		Provider:    "clip_local",
		Model:       "nomic-embed-text",
		Modality:    "text",
		Vector:      vec,
		ContentHash: "deadbeefcontent",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if count := countEmbeddings(t, pool, assetID); count != 1 {
		t.Errorf("after first upsert count = %d, want 1", count)
	}

	// Re-upsert with a different vector — the row is REPLACED, not
	// duplicated; same composite key.
	vec2 := randomVector(768)
	err = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:     assetID,
		Provider:    "clip_local",
		Model:       "nomic-embed-text",
		Modality:    "text",
		Vector:      vec2,
		ContentHash: "deadbeefcontent_v2",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if count := countEmbeddings(t, pool, assetID); count != 1 {
		t.Errorf("after re-upsert count = %d, want 1 (idempotent replace)", count)
	}
}

func TestWriter_UnsupportedModel_ReturnsTypedError(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	w, err := embeddings.NewWriter(context.Background(), pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	err = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:  uuid.New(),
		Provider: "openai",
		Model:    "never-registered-model",
		Modality: "text",
		Vector:   randomVector(768),
	})
	if !errors.Is(err, embeddings.ErrUnsupportedModel) {
		t.Errorf("got %v, want ErrUnsupportedModel", err)
	}
}

func TestWriter_DimensionMismatch_ReturnsTypedError(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	w, err := embeddings.NewWriter(context.Background(), pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// nomic-embed-text is registered at dim=768; pass dim=512.
	err = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:  uuid.New(),
		Provider: "clip_local",
		Model:    "nomic-embed-text",
		Modality: "text",
		Vector:   randomVector(512),
	})
	if !errors.Is(err, embeddings.ErrDimensionMismatch) {
		t.Errorf("got %v, want ErrDimensionMismatch", err)
	}
}

func TestWriter_AssetGone_ReturnsAssetNotFoundSentinel(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	w, err := embeddings.NewWriter(context.Background(), pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Random asset_id with no parent row → FK violation maps to
	// ai.ErrAssetNotFound so job handlers can classify as terminal.
	err = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:  uuid.New(),
		Provider: "clip_local",
		Model:    "nomic-embed-text",
		Modality: "text",
		Vector:   randomVector(768),
	})
	if !errors.Is(err, ai.ErrAssetNotFound) {
		t.Errorf("got %v, want ai.ErrAssetNotFound", err)
	}
}

func TestDimRegistry_DimForModel_HitsAndMisses(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	r := embeddings.NewDimRegistry(pool)
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Seeded by migration 00011.
	if dim, ok := r.DimForModel("nomic-embed-text"); !ok || dim != 768 {
		t.Errorf("nomic-embed-text → (%d, %t), want (768, true)", dim, ok)
	}
	if _, ok := r.DimForModel("never-registered"); ok {
		t.Errorf("never-registered should miss")
	}

	snap := r.Snapshot()
	if snap["nomic-embed-text"] != 768 {
		t.Errorf("Snapshot[nomic-embed-text] = %d, want 768", snap["nomic-embed-text"])
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
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

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func seedAsset(t *testing.T, pool *pgxpool.Pool, ownerRef int64, title, hash string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash)
	if err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	// Phase 1.18.A-2 follow-up A added a partial unique index on
	// (owner_user_ref, file_hash) WHERE deleted_at IS NULL.
	// Sweep any leftover live row before insert so prior-run
	// fixtures in the shared dev DB don't block this seed.
	if _, err := pool.Exec(ctx,
		`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
		ownerRef, hash,
	); err != nil {
		t.Fatalf("sweep prior asset: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO assets (
			id, title, asset_type, owner_user_ref, status,
			file_hash, file_extension, file_size_bytes, sensitivity
		) VALUES ($1, $2, 1, $3, 'active', $4, 'png', 1024, 'public')
	`, id, title, ownerRef, hash)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	return id
}

func cleanupAsset(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_embedding_d768 WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})
}

func countEmbeddings(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM asset_embedding_d768 WHERE asset_id = $1`, assetID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// randomVector returns a deterministic-shaped non-zero float32 slice
// — the writer's only check is the length, not the values.
func randomVector(dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(i+1) * 0.001
	}
	return v
}
