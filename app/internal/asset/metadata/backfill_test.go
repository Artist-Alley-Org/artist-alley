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
