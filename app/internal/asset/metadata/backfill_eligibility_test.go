// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #579 — the EXIF backfill selected ZERO assets on its default path.
//
// It gated on `has_image = TRUE`, a column that is DEFAULT false NOT
// NULL with no writer anywhere in the tree (live: 1007/1007 false). The
// only way to make the query return a row was scope.IncludeNonImage — a
// flag whose purpose is to WIDEN the population to PDFs, not to make the
// default population non-empty. So the backfill "succeeded" on every run
// and enqueued nothing, and pixel_width / pixel_height had no producer.
//
// The whole package's tests passed throughout, because the shared
// seedAsset helper stamped `has_image = true` — a value no production
// code path writes. The fixtures asserted a database state that could
// not occur. That is the specific failure this file exists to prevent
// recurring, so it asserts the property in terms of what production
// actually produces: an active jpg with a file hash, and has_image left
// at its default.

package metadata_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// TestBackfillEligibility_ImageAssetIsEligibleWithHasImageAtDefault is
// THE invariant that was never true.
//
// If this fails, the backfill is selecting nothing again and every
// downstream consumer of extracted metadata — pixel dimensions, and so
// IIIF info.json (#618) — is starved with no error anywhere.
func TestBackfillEligibility_ImageAssetIsEligibleWithHasImageAtDefault(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	asset := seedAsset(t, pool) // active, jpg, file_hash set, has_image DEFAULT

	// Confirm the premise, so a future change that starts writing the
	// column cannot make this pass for the wrong reason.
	var hasImage bool
	if err := pool.QueryRow(ctx,
		`SELECT has_image FROM assets WHERE id = $1`, asset).Scan(&hasImage); err != nil {
		t.Fatalf("read has_image: %v", err)
	}
	if hasImage {
		t.Fatal("fixture asset has has_image = true; something now writes that " +
			"column and this test no longer reproduces the #579 conditions")
	}

	enq := &fakeEnqueuer{}
	runBackfill(t, pool, enq, metadata.BackfillScope{})

	if !containsID(enq.calls, asset) {
		t.Fatalf("an active jpg with a file_hash was NOT enqueued (%d enqueued "+
			"in total). The backfill is gating on something production never "+
			"sets again — this is #579", len(enq.calls))
	}
	if len(enq.calls) == 0 {
		t.Fatal("backfill enqueued nothing; a successful run that does no work " +
			"is the exact shape of the original bug")
	}
}

// TestBackfillEligibility_NonImageStaysExcludedUntilWidened pins the
// other half: the fix must not become permissive.
//
// IncludeNonImage has to keep meaning "widen", which requires the
// default population to already be non-empty AND to exclude non-images.
// A gate that admitted everything would satisfy the test above while
// enqueuing extract jobs for every markdown file in the library.
func TestBackfillEligibility_NonImageStaysExcludedUntilWidened(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	img := seedAsset(t, pool)
	doc := seedAsset(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET file_extension = 'md' WHERE id = $1`, doc); err != nil {
		t.Fatalf("make markdown: %v", err)
	}

	t.Run("default scope excludes the non-image", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		runBackfill(t, pool, enq, metadata.BackfillScope{})
		if !containsID(enq.calls, img) {
			t.Error("the jpg was not enqueued on the default scope")
		}
		if containsID(enq.calls, doc) {
			t.Error("a markdown file was enqueued for EXIF extraction — the gate " +
				"has become permissive, which swaps 'extracts nothing' for " +
				"'fails on everything'")
		}
	})

	t.Run("IncludeNonImage widens rather than enables", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		runBackfill(t, pool, enq, metadata.BackfillScope{IncludeNonImage: true})
		// Both, not just the widened one — "widen" means the default
		// population is a SUBSET of the widened one. When the gate was
		// has_image, the default population was empty and this flag was
		// the only thing that returned anything, which made it an
		// on/off switch wearing the name of a widener.
		if !containsID(enq.calls, img) {
			t.Error("the jpg dropped out under IncludeNonImage — the flag is " +
				"replacing the population rather than widening it")
		}
		if !containsID(enq.calls, doc) {
			t.Error("IncludeNonImage did not admit the non-image asset")
		}
	})
}

// runBackfill drives one full backfill through the real job entrypoint,
// the same way the sibling tests do.
func runBackfill(t *testing.T, pool *pgxpool.Pool, enq *fakeEnqueuer, scope metadata.BackfillScope) {
	t.Helper()
	h := metadata.NewBackfillJobHandler(pool, enq, nil)
	runID := seedBackfillRun(t, pool, scope)
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("backfill handle: %v", err)
	}
}

func containsID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
