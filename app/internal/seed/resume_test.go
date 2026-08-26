// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestSeedInsertAsset_ConflictTellsResumeFromDedup pins the distinction
// that made an incremental re-seed lose post members (#1290).
//
// `SeedInsertAsset` ends in `ON CONFLICT DO NOTHING`, and TWO unrelated
// constraints can trigger it:
//
//	id pkey            a resumed run — the row IS this manifest entry
//	owner+file_hash    a byte-identical sibling, no row under THIS id
//
// Both surface as `pgx.ErrNoRows`, so the caller cannot tell them apart
// from the insert alone. It used to treat both as "skip", which meant an
// asset seeded by an earlier run never entered the runner's id map — and
// `applyPosts` resolves members out of that map. A post added to the
// catalogue afterwards therefore seeded with only its NEW members, and a
// post whose members all pre-existed was dropped as a no-member post.
//
// The symptom was silent: the post existed, the wall looked fine, and
// only its membership was wrong.
func TestSeedInsertAsset_ConflictTellsResumeFromDedup(t *testing.T) {
	pool := openAIProvenancePool(t)
	ctx := context.Background()
	q := New(pool)

	at := pgtype.Timestamptz{
		Time:  time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Valid: true,
	}
	ext, status := "png", "active"
	var size int64 = 1

	id := uuid.New()
	params := SeedInsertAssetParams{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		Title:         "resume-fixture",
		AssetType:     1,
		Status:        status,
		FileExtension: &ext,
		FileSizeBytes: &size,
		Metadata:      []byte(`{"acquisition_source":"test-fixture"}`),
		Sensitivity:   "public",
		CreatedAt:     at,
		UpdatedAt:     at,
	}
	if _, err := q.SeedInsertAsset(ctx, params); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})

	// The second attempt is what a resumed run does on every row it
	// already seeded.
	if _, err := q.SeedInsertAsset(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a repeat insert must conflict into ErrNoRows; got %v", err)
	}

	// …and the recovery must hand the id back, or the asset is invisible
	// to every later phase even though it is sitting in the table.
	got, err := q.SeedGetAssetIDByID(ctx, params.ID)
	if err != nil {
		t.Fatalf("resumed row must be recoverable by id: %v", err)
	}
	if uuid.UUID(got.Bytes) != id {
		t.Fatalf("recovered the wrong row: want %s got %s", id, uuid.UUID(got.Bytes))
	}

	// The OTHER conflict: an id the manifest names that has no row of its
	// own, because the same owner already holds byte-identical content.
	// Recovery must report nothing rather than inventing a link — that is
	// what keeps the hash collapse a collapse.
	if _, err := q.SeedGetAssetIDByID(ctx, pgtype.UUID{Bytes: uuid.New(), Valid: true}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an id with no row must stay unrecoverable; got %v", err)
	}
}
