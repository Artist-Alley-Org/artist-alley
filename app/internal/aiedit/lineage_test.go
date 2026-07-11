// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Integration tests for the aiedit lineage store. Same live-DB
// cadence as the other AI-subsystem tests (skips without
// AA_DB_PASSWORD). Each test creates throwaway assets via direct
// SQL INSERT so the lineage FKs have something to point at, then
// cleans up via t.Cleanup AFTER pool.Close (see
// feedback_userkeys_race_loser_classification for the cleanup-vs-
// defer ordering reason).

package aiedit_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/aiedit"
)

func TestLineage_Insert_HappyPath_RoundTripsViaGetByDerivative(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	source := fixtureAsset(t, pool, "lineage-source")
	deriv := fixtureAsset(t, pool, "lineage-deriv")

	s := aiedit.NewLineageStore(pool)
	out, err := s.Insert(context.Background(), aiedit.InsertParams{
		DerivativeAssetID: deriv,
		SourceAssetID:     source,
		GenerationMetadata: map[string]any{
			"provider":        "comfyuimcp",
			"mcp_server_name": "comfyui-lan",
			"mcp_tool":        "img2img",
			"prompt":          "watercolour sketch",
			"seed":            float64(42),
			"steps":           float64(20),
			"model":           "sdxl",
		},
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if out.DerivativeAssetID != deriv || out.SourceAssetID != source {
		t.Errorf("returned IDs mismatch: derivative=%v source=%v", out.DerivativeAssetID, out.SourceAssetID)
	}
	if out.GenerationMetadata["prompt"] != "watercolour sketch" {
		t.Errorf("metadata not round-tripped: %+v", out.GenerationMetadata)
	}
	if out.CreatedAt.IsZero() {
		t.Errorf("created_at not populated")
	}

	got, err := s.GetByDerivative(context.Background(), deriv)
	if err != nil {
		t.Fatalf("GetByDerivative: %v", err)
	}
	if got.SourceAssetID != source {
		t.Errorf("read-back source = %v, want %v", got.SourceAssetID, source)
	}
	if got.GenerationMetadata["model"] != "sdxl" {
		t.Errorf("read-back model = %v, want sdxl", got.GenerationMetadata["model"])
	}
}

func TestLineage_Insert_DuplicateDerivative_Conflicts(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	source := fixtureAsset(t, pool, "lineage-dup-source")
	deriv := fixtureAsset(t, pool, "lineage-dup-deriv")

	s := aiedit.NewLineageStore(pool)
	params := aiedit.InsertParams{
		DerivativeAssetID:  deriv,
		SourceAssetID:      source,
		GenerationMetadata: map[string]any{"prompt": "first"},
	}
	if _, err := s.Insert(context.Background(), params); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := s.Insert(context.Background(), params); err == nil {
		t.Errorf("second insert should fail on PK conflict, got nil")
	}
}

func TestLineage_GetByDerivative_NoRows_ReturnsErrLineageNotFound(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	s := aiedit.NewLineageStore(pool)
	_, err := s.GetByDerivative(context.Background(), uuid.New())
	if !errors.Is(err, aiedit.ErrLineageNotFound) {
		t.Errorf("got %v, want ErrLineageNotFound", err)
	}
}

func TestLineage_ListBySource_ReturnsChain_NewestFirst(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	source := fixtureAsset(t, pool, "lineage-chain-source")
	deriv1 := fixtureAsset(t, pool, "lineage-chain-deriv1")
	deriv2 := fixtureAsset(t, pool, "lineage-chain-deriv2")
	deriv3 := fixtureAsset(t, pool, "lineage-chain-deriv3")

	s := aiedit.NewLineageStore(pool)
	for _, d := range []uuid.UUID{deriv1, deriv2, deriv3} {
		if _, err := s.Insert(context.Background(), aiedit.InsertParams{
			DerivativeAssetID:  d,
			SourceAssetID:      source,
			GenerationMetadata: map[string]any{"prompt": d.String()},
		}); err != nil {
			t.Fatalf("Insert(%v): %v", d, err)
		}
		// Sleep a millisecond between inserts so DESC ordering by
		// created_at is unambiguous — without it, the rows may share
		// a timestamp and the ORDER BY tiebreak is undefined.
		time.Sleep(time.Millisecond)
	}

	got, err := s.ListBySource(context.Background(), source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	// Newest first — deriv3 should lead.
	if got[0].DerivativeAssetID != deriv3 {
		t.Errorf("newest-first: got[0] = %v, want deriv3=%v", got[0].DerivativeAssetID, deriv3)
	}
	if got[2].DerivativeAssetID != deriv1 {
		t.Errorf("oldest-last: got[2] = %v, want deriv1=%v", got[2].DerivativeAssetID, deriv1)
	}
}

func TestLineage_ListBySource_NoRows_ReturnsEmpty(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	s := aiedit.NewLineageStore(pool)
	got, err := s.ListBySource(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d rows", len(got))
	}
}

// ---------------------------------------------------------------------------
// Test scaffold
// ---------------------------------------------------------------------------

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
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// fixtureAsset inserts a throwaway asset row so the lineage FKs
// have something to point at. The label only matters in DB
// inspection — the test ignores it. Cleanup deletes the asset and
// (via ON DELETE CASCADE) any creative_lineage rows pointing at it.
func fixtureAsset(t *testing.T, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	// asset_type=1 (photo) + minimum required columns. status='active'
	// and title is non-empty. No file_hash so we don't poke the
	// storage layer.
	_, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, asset_type, status, processing_status, metadata)
		VALUES ($1, $2, 1, 'active', 'ready', '{}'::jsonb)
	`, id, "aiedit-test:"+label)
	if err != nil {
		t.Fatalf("fixture asset %q: %v", label, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}
