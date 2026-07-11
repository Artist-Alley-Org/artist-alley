// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.A-2 follow-up A — dedup race test.
//
// Proves the load-bearing concurrency invariant: N concurrent
// uploads of the same file by the same user produce EXACTLY ONE
// asset row, with the losers getting a dedup-warning response
// (default behavior). The partial unique index on
// (owner_user_ref, file_hash) from migration 00016 + the
// 23505-classification race-loser path in CreateAsset together
// provide the guarantee.

package assets_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

func TestUpload_ConcurrentSameFile_ExactlyOneAssetRowCreated(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(func() { pool.Close() })

	// Unique per-run hash so concurrent runs of this test against
	// the same shared dev DB don't collide on each other's
	// fixtures. Storage's CHECK constraint requires 64 hex chars
	// (sha256 shape), so strip the UUID dashes + pad with hex
	// nibbles to length.
	rawA := uuid.New().String() // 36 chars incl. dashes
	rawB := uuid.New().String()
	cleaned := ""
	for _, r := range rawA + rawB {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			cleaned += string(r)
		}
	}
	for len(cleaned) < 64 {
		cleaned += "0"
	}
	hash := cleaned[:64]
	const ownerRef int64 = 9_142_999

	// Seed a parent storage_objects row so the asset's file_hash
	// FK resolves. Hard-delete any prior assets for this (owner,
	// hash) so we have a clean slate.
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
		ownerRef, hash,
	); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/png', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash); err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}

	// Real storage Service for the AddPin call path inside
	// CreateAsset. fs backend over a t.TempDir — no real bytes
	// involved, just pin bookkeeping in the DB.
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	h := assets.NewHandler(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)

	const N = 5
	type result struct {
		resp openapi.CreateAssetResponseObject
		err  error
	}
	var wg sync.WaitGroup
	results := make([]result, N)
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all goroutines at once
			ctx := auth.WithIdentity(context.Background(),
				&auth.Identity{UserRef: ownerRef, Capabilities: []string{}})
			ext := "png"
			req := openapi.CreateAssetRequestObject{
				Body: &openapi.AssetCreate{
					Title:         "race-test",
					AssetType:     1,
					FileHash:      &hash,
					FileExtension: &ext,
				},
			}
			resp, err := h.CreateAsset(ctx, req)
			results[idx] = result{resp: resp, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// Tally outcomes. The CONCURRENT-RACE INVARIANT is:
	//   - exactly one goroutine creates an asset row
	//   - the other N-1 see a dedup response (either 200 + warn
	//     or 201 — both shapes mean "you uploaded the same file
	//     someone else just did")
	//   - DB ends with exactly one live row for (owner, hash)
	var creates201, warns200, errsHard int
	var createdID, dedupID openapi_types.UUID
	for _, r := range results {
		if r.err != nil {
			errsHard++
			t.Errorf("hard error: %v", r.err)
			continue
		}
		switch v := r.resp.(type) {
		case openapi.CreateAsset201JSONResponse:
			creates201++
			createdID = openapi.Asset(v).Id
		case openapi.CreateAsset200JSONResponse:
			warns200++
			dedupID = v.DuplicateWarning.ExistingAssetId
		default:
			t.Errorf("unexpected response type %T", r.resp)
		}
	}
	if errsHard > 0 {
		t.Fatalf("got %d hard errors", errsHard)
	}
	if creates201 != 1 {
		t.Errorf("creates201 = %d, want exactly 1 (race winner)", creates201)
	}
	if warns200 != N-1 {
		t.Errorf("warns200 = %d, want exactly %d (race losers)", warns200, N-1)
	}
	// Dedup ID should match the created ID.
	if dedupID != createdID {
		t.Errorf("dedup ID %v doesn't match created ID %v", dedupID, createdID)
	}

	// DB-level invariant: exactly one live row for (owner, hash).
	var liveCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM assets WHERE owner_user_ref = $1 AND file_hash = $2 AND deleted_at IS NULL`,
		ownerRef, hash,
	).Scan(&liveCount)
	if liveCount != 1 {
		t.Errorf("live row count = %d, want 1 (the partial unique index from migration 00016 is the guarantee)", liveCount)
	}

	// Cleanup so the next test run starts clean.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
			ownerRef, hash,
		)
	})
}
