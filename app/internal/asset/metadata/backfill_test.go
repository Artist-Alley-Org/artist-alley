// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// fakeEnqueuer records each EnqueueExtract call. Optional err is
// returned for the first N calls to exercise the failure path.
type fakeEnqueuer struct {
	mu         sync.Mutex
	calls      []uuid.UUID
	failFirstN int
	failErr    error
}

func (f *fakeEnqueuer) EnqueueExtract(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFirstN > 0 {
		f.failFirstN--
		return f.failErr
	}
	f.calls = append(f.calls, id)
	return nil
}

func seedBackfillRun(t *testing.T, pool *pgxpool.Pool, scope metadata.BackfillScope) uuid.UUID {
	t.Helper()
	scopeJSON, _ := json.Marshal(scope)
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO metadata_backfill_run (scope) VALUES ($1) RETURNING id
	`, scopeJSON).Scan(&id)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM metadata_backfill_run WHERE id = $1`, id)
	})
	return id
}

func runState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) metadata.BackfillRunRow {
	t.Helper()
	h := metadata.NewAdminHandler(pool)
	row, err := h.GetBackfill(context.Background(), id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return row
}

func TestBackfillJob_WalksEligibleAssetsAndEnqueues(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Three eligible assets in this case's window.
	a1 := seedAsset(t, pool)
	a2 := seedAsset(t, pool)
	a3 := seedAsset(t, pool)

	enq := &fakeEnqueuer{}
	h := metadata.NewBackfillJobHandler(pool, enq, nil).WithBatchSize(2)

	runID := seedBackfillRun(t, pool, metadata.BackfillScope{})
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	_, err := h.Handle(ctx, &jobs.Claim{Payload: payload})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	// At least our 3 should appear in the enqueue list — other test
	// cases in this DB may add to the pool, but the case's three
	// must each appear.
	seen := map[uuid.UUID]bool{}
	for _, id := range enq.calls {
		seen[id] = true
	}
	for _, id := range []uuid.UUID{a1, a2, a3} {
		if !seen[id] {
			t.Errorf("expected asset %v in enqueue list, missing", id)
		}
	}

	state := runState(t, pool, runID)
	if state.CompletedAt == nil {
		t.Errorf("run should be completed_at set after Handle, got nil")
	}
	if state.Succeeded < 3 {
		t.Errorf("Succeeded = %d, want >= 3", state.Succeeded)
	}
	if state.Failed != 0 {
		t.Errorf("Failed = %d, want 0", state.Failed)
	}
}

func TestBackfillJob_CountsEnqueueFailuresAsFailedNotSucceeded(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	_ = seedAsset(t, pool)
	_ = seedAsset(t, pool)

	enq := &fakeEnqueuer{failFirstN: 1, failErr: context.Canceled}
	h := metadata.NewBackfillJobHandler(pool, enq, nil).WithBatchSize(2)

	runID := seedBackfillRun(t, pool, metadata.BackfillScope{})
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(ctx, &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	state := runState(t, pool, runID)
	if state.Failed < 1 {
		t.Errorf("Failed = %d, want >= 1 (one EnqueueExtract was forced to fail)", state.Failed)
	}
	if state.Processed != state.Succeeded+state.Failed {
		t.Errorf("Processed (%d) should equal Succeeded (%d) + Failed (%d)",
			state.Processed, state.Succeeded, state.Failed)
	}
}

func TestBackfillJob_StopsOnCancel(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Seed enough rows to force at least one batch-boundary check.
	for i := 0; i < 4; i++ {
		_ = seedAsset(t, pool)
	}

	runID := seedBackfillRun(t, pool, metadata.BackfillScope{})

	// Flip cancelled_at BEFORE the job starts — the first batch-
	// boundary cancel-check should fire immediately.
	_, err := pool.Exec(ctx,
		`UPDATE metadata_backfill_run SET cancelled_at = NOW() WHERE id = $1`,
		runID,
	)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	enq := &fakeEnqueuer{}
	h := metadata.NewBackfillJobHandler(pool, enq, nil).WithBatchSize(2)
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(ctx, &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(enq.calls) != 0 {
		t.Errorf("cancelled run enqueued %d items, want 0", len(enq.calls))
	}
	state := runState(t, pool, runID)
	if state.CancelledAt == nil {
		t.Errorf("cancelled_at should remain set")
	}
	if state.CompletedAt != nil {
		t.Errorf("cancelled run should NOT be marked completed_at")
	}
}

func TestBackfillJob_AssetTypeScopeNarrowsPopulation(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// All test assets are inserted with asset_type=1 (Image). Scope
	// to a non-existent asset_type — the walk should enqueue
	// nothing from THIS case's seeded rows. Other suites' rows
	// can't accidentally bleed into this scope either.
	bogusType := int64(999_999)
	_ = seedAsset(t, pool)
	_ = seedAsset(t, pool)

	enq := &fakeEnqueuer{}
	h := metadata.NewBackfillJobHandler(pool, enq, nil)

	runID := seedBackfillRun(t, pool, metadata.BackfillScope{
		AssetTypeRef: &bogusType,
	})
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(ctx, &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(enq.calls) != 0 {
		t.Errorf("bogus-type scope enqueued %d items, want 0", len(enq.calls))
	}

	state := runState(t, pool, runID)
	if state.Processed != 0 {
		t.Errorf("Processed = %d, want 0", state.Processed)
	}
	if state.CompletedAt == nil {
		t.Errorf("CompletedAt should be set after successful empty walk")
	}
}

func TestBackfillJob_FileExtensionScopeFilters(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// seedAsset writes file_extension='jpg'. Re-stamp two of the
	// assets to .cr2 so we can verify the extension filter narrows
	// the walk.
	a1 := seedAsset(t, pool)
	a2 := seedAsset(t, pool)
	a3 := seedAsset(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET file_extension = 'cr2' WHERE id = ANY($1)`,
		[]uuid.UUID{a1, a3},
	); err != nil {
		t.Fatalf("re-stamp extensions: %v", err)
	}

	enq := &fakeEnqueuer{}
	h := metadata.NewBackfillJobHandler(pool, enq, nil)
	runID := seedBackfillRun(t, pool, metadata.BackfillScope{
		FileExtensions: []string{"cr2"},
	})
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(ctx, &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got := map[uuid.UUID]bool{}
	for _, id := range enq.calls {
		got[id] = true
	}
	if !got[a1] || !got[a3] {
		t.Errorf("extension filter dropped expected raws: a1=%v a3=%v", got[a1], got[a3])
	}
	if got[a2] {
		t.Errorf("extension filter let .jpg through: a2 enqueued")
	}
}

func TestBackfillJob_AssetTypeRefsMultiSelect(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	a1 := seedAsset(t, pool)
	// Re-stamp asset_type=2 + 3 + 4 so each row is its own type.
	a2 := seedAsset(t, pool)
	a3 := seedAsset(t, pool)
	a4 := seedAsset(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET asset_type = CASE id WHEN $1 THEN 2 WHEN $2 THEN 3 WHEN $3 THEN 4 ELSE asset_type END WHERE id = ANY($4)`,
		a2, a3, a4, []uuid.UUID{a2, a3, a4},
	); err != nil {
		t.Fatalf("re-stamp asset_type: %v", err)
	}

	enq := &fakeEnqueuer{}
	h := metadata.NewBackfillJobHandler(pool, enq, nil)
	// Multi-select for types 2 + 4 only — should pick up a2 + a4,
	// skip a1 (type 1) and a3 (type 3).
	runID := seedBackfillRun(t, pool, metadata.BackfillScope{
		AssetTypeRefs: []int64{2, 4},
	})
	payload, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runID})
	if _, err := h.Handle(ctx, &jobs.Claim{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, id := range enq.calls {
		got[id] = true
	}
	if got[a1] || got[a3] {
		t.Errorf("multi-select leaked: a1=%v a3=%v", got[a1], got[a3])
	}
	if !got[a2] || !got[a4] {
		t.Errorf("multi-select missed in-scope rows: a2=%v a4=%v", got[a2], got[a4])
	}
}

func TestBackfillJob_IncludeNonImageOpensPDFs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Seed two assets, flip one to has_image=false (representing a
	// PDF or other paginated non-image type).
	a1 := seedAsset(t, pool) // stays has_image=true
	a2 := seedAsset(t, pool) // becomes has_image=false
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET has_image = false, file_extension = 'pdf' WHERE id = $1`,
		a2,
	); err != nil {
		t.Fatalf("flip has_image: %v", err)
	}

	// Default scope (IncludeNonImage=false) excludes a2.
	enqA := &fakeEnqueuer{}
	hA := metadata.NewBackfillJobHandler(pool, enqA, nil)
	runA := seedBackfillRun(t, pool, metadata.BackfillScope{})
	pA, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runA})
	if _, err := hA.Handle(ctx, &jobs.Claim{Payload: pA}); err != nil {
		t.Fatalf("handle (default scope): %v", err)
	}
	if didEnqueue(enqA.calls, a2) {
		t.Errorf("default scope shouldn't enqueue non-image asset")
	}
	if !didEnqueue(enqA.calls, a1) {
		t.Errorf("default scope should still enqueue image asset")
	}

	// IncludeNonImage=true opens up to PDFs.
	enqB := &fakeEnqueuer{}
	hB := metadata.NewBackfillJobHandler(pool, enqB, nil)
	runB := seedBackfillRun(t, pool, metadata.BackfillScope{IncludeNonImage: true})
	pB, _ := json.Marshal(metadata.BackfillJobPayload{RunID: runB})
	if _, err := hB.Handle(ctx, &jobs.Claim{Payload: pB}); err != nil {
		t.Fatalf("handle (open scope): %v", err)
	}
	if !didEnqueue(enqB.calls, a2) {
		t.Errorf("IncludeNonImage scope should enqueue non-image asset a2")
	}
}

func didEnqueue(calls []uuid.UUID, want uuid.UUID) bool {
	for _, id := range calls {
		if id == want {
			return true
		}
	}
	return false
}

func TestAdminHandler_BackfillLifecycle(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	h := metadata.NewAdminHandler(pool)

	// Get a backfill that doesn't exist.
	missing, err := h.GetBackfill(ctx, uuid.New())
	if err == nil {
		t.Errorf("GetBackfill(unknown) = %v, want ErrBackfillNotFound", missing)
	}

	if err := h.CancelBackfill(ctx, uuid.New()); err == nil {
		t.Errorf("CancelBackfill(unknown) should return ErrBackfillNotFound")
	}

	// Listing is best-effort — just runs without error.
	if _, err := h.ListRecentBackfills(ctx, 5); err != nil {
		t.Errorf("ListRecentBackfills: %v", err)
	}
}
