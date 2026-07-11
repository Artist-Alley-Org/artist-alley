// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// seedAssetForEditSafety inserts a minimal asset directly + returns
// its id + updated_at. Bypasses the upload pipeline since the
// edit-safety check is orthogonal to file bytes.
func seedAssetForEditSafety(t *testing.T) (uuid.UUID, time.Time, *assets.Handler) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	h := assets.NewHandler(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)

	const userRef int64 = 525252
	id := uuid.New()
	// Random file_hash to avoid colliding with the per-user dedup
	// unique index from migration 00016.
	hb := make([]byte, 16)
	_, _ = rand.Read(hb)
	hashHex := hex.EncodeToString(sha256.New().Sum(hb))[:64]

	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING
	`, hashHex)
	if err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, status, file_hash, file_extension, file_size_bytes, sensitivity, has_image)
		VALUES ($1, 'edit-safety-test', 1, $2, 'active', $3, 'png', 1024, 'public', true)
	`, id, userRef, hashHex)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM assets WHERE id = $1`, id,
	).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id, updatedAt, h
}

func authedCtx(userRef int64) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef: userRef, AuthMethod: "session",
	})
}

func TestUpdateAsset_EditSafety_AbsentTimestampIsLegacyBehavior(t *testing.T) {
	id, _, h := seedAssetForEditSafety(t)
	newTitle := "no-check"
	resp, err := h.UpdateAsset(authedCtx(525252),
		openapi.UpdateAssetRequestObject{
			Id:   openapi_types.UUID(id),
			Body: &openapi.AssetUpdate{Title: &newTitle},
		})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		t.Errorf("absent if_unchanged_since should pass through (legacy); got %T", resp)
	}
}

func TestUpdateAsset_EditSafety_MatchingTimestampPasses(t *testing.T) {
	id, ts, h := seedAssetForEditSafety(t)
	newTitle := "matched"
	resp, err := h.UpdateAsset(authedCtx(525252),
		openapi.UpdateAssetRequestObject{
			Id: openapi_types.UUID(id),
			Body: &openapi.AssetUpdate{
				Title:            &newTitle,
				IfUnchangedSince: &ts,
			},
		})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		t.Errorf("matching if_unchanged_since should 200; got %T", resp)
	}
}

func TestUpdateAsset_EditSafety_StaleTimestampReturns409(t *testing.T) {
	id, ts, h := seedAssetForEditSafety(t)
	ctx := authedCtx(525252)

	// First update bumps updated_at — simulates the "other admin
	// just saved" race.
	first := "first-write"
	if _, err := h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(id),
		Body: &openapi.AssetUpdate{Title: &first},
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Second update uses the ORIGINAL ts — should 409 because the
	// row's updated_at has advanced.
	second := "second-write"
	resp, err := h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id: openapi_types.UUID(id),
		Body: &openapi.AssetUpdate{
			Title:            &second,
			IfUnchangedSince: &ts,
		},
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	conflict, ok := resp.(openapi.UpdateAsset409JSONResponse)
	if !ok {
		t.Fatalf("stale if_unchanged_since should 409; got %T", resp)
	}
	if conflict.UpdatedAt.Before(ts) || conflict.UpdatedAt.Equal(ts) {
		t.Errorf("409 should carry the advanced updated_at; got %v vs original %v", conflict.UpdatedAt, ts)
	}
	if conflict.Error == "" {
		t.Errorf("409 should carry a human-readable error message")
	}
}

func TestUpdateAsset_EditSafety_MicrosecondPrecisionTolerated(t *testing.T) {
	// Postgres stores timestamptz at microsecond precision, but
	// Go's JSON marshal round-trip preserves nanoseconds. The
	// handler must truncate both sides before comparing or every
	// real-world client will 409 on the FIRST PATCH after a GET.
	id, ts, h := seedAssetForEditSafety(t)
	// Add a sub-microsecond delta to simulate the JSON round-trip
	// (Go's time.Now() generally returns ns-precision; the µs
	// boundary is below the stored value).
	with500ns := ts.Add(500 * time.Nanosecond)
	newTitle := "ns-tolerance"
	resp, err := h.UpdateAsset(authedCtx(525252),
		openapi.UpdateAssetRequestObject{
			Id: openapi_types.UUID(id),
			Body: &openapi.AssetUpdate{
				Title:            &newTitle,
				IfUnchangedSince: &with500ns,
			},
		})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		t.Errorf("sub-µs delta should not trigger 409 (truncate both sides); got %T", resp)
	}
}
