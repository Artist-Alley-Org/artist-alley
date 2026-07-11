// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.A — idempotency-key handling on Enqueue.
//
// Integration tests against the live postgres compose stack
// (matches the established cadence for this package's other DB-
// backed tests).
package jobs_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

func TestEnqueue_NoIdempotencyKey_AllowsDuplicates(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()

	cleanIdempotencyTestRows(t, pool)
	t.Cleanup(func() { cleanIdempotencyTestRows(t, pool) })

	svc := jobs.NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)

	id1, err := svc.Enqueue(context.Background(), jobs.JobType("idemp_test_no_key"), map[string]any{"x": 1}, jobs.EnqueueOpts{})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	id2, err := svc.Enqueue(context.Background(), jobs.JobType("idemp_test_no_key"), map[string]any{"x": 1}, jobs.EnqueueOpts{})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if id1 == id2 {
		t.Errorf("two enqueues with no idempotency key collapsed to one id: %s", id1)
	}
}

func TestEnqueue_SameIdempotencyKey_ReturnsExistingJob(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()

	cleanIdempotencyTestRows(t, pool)
	t.Cleanup(func() { cleanIdempotencyTestRows(t, pool) })

	svc := jobs.NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)

	key := "idemp_test_key_alpha"
	id1, err := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_same_key"),
		map[string]any{"x": 1},
		jobs.EnqueueOpts{IdempotencyKey: key},
	)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	id2, err := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_same_key"),
		map[string]any{"x": 2}, // different payload — should still dedupe
		jobs.EnqueueOpts{IdempotencyKey: key},
	)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotency hit should return same id; got %s vs %s", id1, id2)
	}
}

func TestEnqueue_DifferentIdempotencyKeys_AllowDifferentJobs(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()

	cleanIdempotencyTestRows(t, pool)
	t.Cleanup(func() { cleanIdempotencyTestRows(t, pool) })

	svc := jobs.NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)

	id1, _ := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_diff_key"),
		nil,
		jobs.EnqueueOpts{IdempotencyKey: "alpha"},
	)
	id2, _ := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_diff_key"),
		nil,
		jobs.EnqueueOpts{IdempotencyKey: "beta"},
	)
	if id1 == id2 {
		t.Errorf("different keys collapsed: %s", id1)
	}
}

func TestEnqueue_IdempotencyKey_ScopedToType(t *testing.T) {
	// Same key with a different type → no dedup (the partial UNIQUE
	// INDEX is on (type, idempotency_key)).
	pool := openTestPool(t)
	defer pool.Close()

	cleanIdempotencyTestRows(t, pool)
	t.Cleanup(func() { cleanIdempotencyTestRows(t, pool) })

	svc := jobs.NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)

	id1, _ := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_type_a"), nil,
		jobs.EnqueueOpts{IdempotencyKey: "shared"})
	id2, _ := svc.Enqueue(context.Background(),
		jobs.JobType("idemp_test_type_b"), nil,
		jobs.EnqueueOpts{IdempotencyKey: "shared"})

	if id1 == id2 {
		t.Errorf("shared key across different types collapsed: %s", id1)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func openTestPool(t *testing.T) *pgxpool.Pool {
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
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cleanIdempotencyTestRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE type LIKE 'idemp_test_%'`)
}
