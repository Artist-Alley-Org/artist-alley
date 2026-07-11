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

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/embeddings"
)

func TestReader_FindSimilarByAnchor_HappyPath_RanksByDistance(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_140_010
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := embeddings.NewWriter(context.Background(), pool, logger)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	r := embeddings.NewReader(pool, w.DimRegistry())

	// Seed three assets with hand-crafted vectors so the cosine
	// distances have a known ranking. Anchor at [1,0,0,...]; near at
	// [0.9, 0.1, 0, ...] (cos≈0.012); far at [-1,0,0,...] (cos=2.0).
	anchorID := seedAsset(t, pool, ownerRef, "anchor",
		"cafebabe11111111111111111111111111111111111111111111111111111111")
	nearID := seedAsset(t, pool, ownerRef, "near",
		"cafebabe22222222222222222222222222222222222222222222222222222222")
	farID := seedAsset(t, pool, ownerRef, "far",
		"cafebabe33333333333333333333333333333333333333333333333333333333")
	cleanupAsset(t, pool, anchorID)
	cleanupAsset(t, pool, nearID)
	cleanupAsset(t, pool, farID)

	anchorVec := unitVector(0, 768)
	nearVec := tiltedFirstAxis(0.9, 0.1, 768)
	farVec := negUnitVector(0, 768)

	// Unique provider so the kNN scan can't pick up stale rows from
	// previous test runs that share the (router, nomic-embed-text)
	// space. Each test gets its own namespace + a pre-clean sweeps
	// previous failed-run rows (asset_id changes per run; cleanup-
	// by-asset-id can't catch them).
	const provider, model, modality = "test_reader_happy_path", "nomic-embed-text", "text"
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM asset_embedding_d768 WHERE provider = $1`, provider)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_embedding_d768 WHERE provider = $1`, provider)
	})
	for _, ww := range []struct {
		id  uuid.UUID
		vec []float32
	}{
		{anchorID, anchorVec},
		{nearID, nearVec},
		{farID, farVec},
	} {
		if err := w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
			AssetID:  ww.id,
			Provider: provider,
			Model:    model,
			Modality: modality,
			Vector:   ww.vec,
		}); err != nil {
			t.Fatalf("upsert %s: %v", ww.id, err)
		}
	}

	got, err := r.FindSimilarByAnchor(context.Background(), anchorID, provider, model, modality, 10)
	if err != nil {
		t.Fatalf("FindSimilarByAnchor: %v", err)
	}

	// Anchor must be excluded; near + far must be ordered by distance.
	for _, n := range got {
		if n.AssetID == anchorID {
			t.Errorf("anchor leaked into result set")
		}
	}
	if len(got) < 2 {
		t.Fatalf("expected at least 2 neighbours, got %d", len(got))
	}
	if got[0].AssetID != nearID {
		t.Errorf("nearest neighbour = %s, want near=%s", got[0].AssetID, nearID)
	}
	if got[len(got)-1].AssetID != farID {
		t.Errorf("farthest neighbour = %s, want far=%s", got[len(got)-1].AssetID, farID)
	}
	if got[0].Distance >= got[len(got)-1].Distance {
		t.Errorf("distances not ascending: %v vs %v", got[0].Distance, got[len(got)-1].Distance)
	}
}

func TestReader_FindSimilarByAnchor_NoAnchorEmbedding_ReturnsSentinel(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_140_011
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, _ := embeddings.NewWriter(context.Background(), pool, logger)
	r := embeddings.NewReader(pool, w.DimRegistry())

	// Asset exists, but no embedding row.
	lonelyID := seedAsset(t, pool, ownerRef, "lonely",
		"cafebabe44444444444444444444444444444444444444444444444444444444")
	cleanupAsset(t, pool, lonelyID)

	_, err := r.FindSimilarByAnchor(context.Background(), lonelyID,
		"test_reader_no_embedding", "nomic-embed-text", "text", 10)
	if !errors.Is(err, embeddings.ErrAnchorHasNoEmbedding) {
		t.Errorf("got %v, want ErrAnchorHasNoEmbedding", err)
	}
}

func TestReader_HasEmbedding_HitsAndMisses(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	const ownerRef int64 = 9_140_012
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, _ := embeddings.NewWriter(context.Background(), pool, logger)
	r := embeddings.NewReader(pool, w.DimRegistry())

	withEmbed := seedAsset(t, pool, ownerRef, "with",
		"cafebabe55555555555555555555555555555555555555555555555555555555")
	noEmbed := seedAsset(t, pool, ownerRef, "without",
		"cafebabe66666666666666666666666666666666666666666666666666666666")
	cleanupAsset(t, pool, withEmbed)
	cleanupAsset(t, pool, noEmbed)

	const provider = "test_reader_has_embedding"
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM asset_embedding_d768 WHERE provider = $1`, provider)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_embedding_d768 WHERE provider = $1`, provider)
	})
	_ = w.UpsertAssetEmbedding(context.Background(), ai.EmbeddingInput{
		AssetID:  withEmbed,
		Provider: provider,
		Model:    "nomic-embed-text",
		Modality: "text",
		Vector:   unitVector(0, 768),
	})

	yes, err := r.HasEmbedding(context.Background(), withEmbed, provider, "nomic-embed-text", "text")
	if err != nil {
		t.Fatalf("HasEmbedding: %v", err)
	}
	if !yes {
		t.Error("HasEmbedding should be true for the embedded asset")
	}

	no, err := r.HasEmbedding(context.Background(), noEmbed, provider, "nomic-embed-text", "text")
	if err != nil {
		t.Fatalf("HasEmbedding: %v", err)
	}
	if no {
		t.Error("HasEmbedding should be false for the un-embedded asset")
	}
}

// ---------------------------------------------------------------------------
// Vector constructors — deterministic, no math/rand
// ---------------------------------------------------------------------------

func unitVector(axis, dim int) []float32 {
	v := make([]float32, dim)
	v[axis] = 1.0
	return v
}

func negUnitVector(axis, dim int) []float32 {
	v := make([]float32, dim)
	v[axis] = -1.0
	return v
}

func tiltedFirstAxis(major, minor float32, dim int) []float32 {
	v := make([]float32, dim)
	v[0] = major
	v[1] = minor
	return v
}

