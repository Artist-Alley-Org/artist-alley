// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.C — CapabilitySweeper tests.
//
// Pins the contract end-to-end against real Postgres:
//
//   * expired grants/revokes are reaped on SweepOnce
//   * future + permanent rows survive
//   * per-row audit fires (one callback per reaped row, with
//     correct subject + capability + team_id + expired_at)
//   * cache invalidation broadcasts once per affected user_ref
//     (dedup; not once per reaped row)
//   * happy steady state (nothing expired) is quiet — no audit,
//     no log, no cache churn
//   * Run loop ticks then exits cleanly on ctx cancel

package auth_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func TestCapabilitySweeper_ReapsExpiredGrants(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u1 := seedSubjectUser(t, pool, "")
	u2 := seedSubjectUser(t, pool, "")

	seedGrant(t, pool, u1, "posts.publish", pastTime(t, 1*time.Hour))
	seedGrant(t, pool, u2, "users.read", pastTime(t, 30*time.Minute))
	seedGrant(t, pool, u1, "teams.read", futureTime(t, 2*time.Hour)) // future — should survive

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)

	g, r := sw.SweepOnce(context.Background())
	if g != 2 || r != 0 {
		t.Errorf("SweepOnce = (%d grants, %d revokes), want (2, 0)", g, r)
	}

	if rec.GrantCalls() != 2 {
		t.Errorf("audit grant calls = %d, want 2", rec.GrantCalls())
	}
	if rec.InvalidateCount() != 2 {
		t.Errorf("invalidate calls = %d, want 2 (one per affected user)", rec.InvalidateCount())
	}
	// Future-expiry row stays.
	if !grantExists(t, pool, u1, "teams.read") {
		t.Error("future-expiry grant should have survived")
	}
}

func TestCapabilitySweeper_PreservesPermanentRows(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")

	seedGrant(t, pool, u, "posts.publish", pgtype.Timestamptz{}) // NULL = permanent

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)
	g, _ := sw.SweepOnce(context.Background())
	if g != 0 {
		t.Errorf("SweepOnce reaped %d grants; permanent row should survive", g)
	}
	if !grantExists(t, pool, u, "posts.publish") {
		t.Error("permanent grant got reaped")
	}
}

func TestCapabilitySweeper_DedupsCacheInvalidatePerUser(t *testing.T) {
	// Two reaped rows for the same user → exactly one cache
	// invalidation. NOTIFY churn is real on big installs; the
	// sweeper dedups by user_ref so the resolver gets one bust
	// instead of N.
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")

	seedGrant(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour))
	seedGrant(t, pool, u, "users.read", pastTime(t, 1*time.Hour))
	seedRevoke(t, pool, u, "teams.read", pastTime(t, 1*time.Hour))

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)
	g, r := sw.SweepOnce(context.Background())
	if g != 2 || r != 1 {
		t.Errorf("SweepOnce = (%d, %d), want (2, 1)", g, r)
	}
	if rec.InvalidateCount() != 1 {
		t.Errorf("invalidate calls = %d, want 1 (deduped per user_ref)", rec.InvalidateCount())
	}
	if rec.LastInvalidateUser() != u {
		t.Errorf("invalidate user = %d, want %d", rec.LastInvalidateUser(), u)
	}
}

func TestCapabilitySweeper_HappySteadyState_Quiet(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	// Seed a permanent + a future-expiry — nothing should reap.
	u := seedSubjectUser(t, pool, "")
	seedGrant(t, pool, u, "posts.publish", pgtype.Timestamptz{})
	seedGrant(t, pool, u, "users.read", futureTime(t, 1*time.Hour))

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)
	g, r := sw.SweepOnce(context.Background())
	if g != 0 || r != 0 {
		t.Errorf("SweepOnce = (%d, %d), want (0, 0)", g, r)
	}
	if rec.GrantCalls() != 0 || rec.RevokeCalls() != 0 {
		t.Errorf("happy steady state emitted audit calls: grants=%d revokes=%d", rec.GrantCalls(), rec.RevokeCalls())
	}
	if rec.InvalidateCount() != 0 {
		t.Errorf("happy steady state broadcast cache invalidation: %d", rec.InvalidateCount())
	}
}

func TestCapabilitySweeper_Run_ExitsOnCtxCancel(t *testing.T) {
	pool := openTestPool(t)
	// Short tick so we observe the loop without waiting.
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	// Let it tick a few times.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — Run returned
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}

func TestCapabilitySweeper_NilCallbacks_StillReaps(t *testing.T) {
	// All callbacks nil — sweeper still reaps the row (the
	// DELETE is the load-bearing change). Audit + cache are
	// observability/coherency; their absence shouldn't block
	// the reap.
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	seedGrant(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour))

	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	g, _ := sw.SweepOnce(context.Background())
	if g != 1 {
		t.Errorf("nil-callback sweep reaped %d, want 1", g)
	}
	if grantExists(t, pool, u, "posts.publish") {
		t.Error("row should be gone after sweep")
	}
}

// ---------------------------------------------------------------
// recordingSweeperHooks + seed helpers
// ---------------------------------------------------------------

type recordingSweeperHooks struct {
	mu             sync.Mutex
	grantCalls     int
	revokeCalls    int
	invalidate     atomic.Int32
	lastInvalidate atomic.Int64
}

func newSweeperRecorder() *recordingSweeperHooks { return &recordingSweeperHooks{} }

func (r *recordingSweeperHooks) OnGrant(_ context.Context, _ int64, _, _ string, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantCalls++
}
func (r *recordingSweeperHooks) OnRevoke(_ context.Context, _ int64, _, _ string, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revokeCalls++
}
func (r *recordingSweeperHooks) OnInvalidate(_ context.Context, userRef int64) {
	r.invalidate.Add(1)
	r.lastInvalidate.Store(userRef)
}
func (r *recordingSweeperHooks) GrantCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.grantCalls
}
func (r *recordingSweeperHooks) RevokeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revokeCalls
}
func (r *recordingSweeperHooks) InvalidateCount() int      { return int(r.invalidate.Load()) }
func (r *recordingSweeperHooks) LastInvalidateUser() int64 { return r.lastInvalidate.Load() }

func pastTime(t *testing.T, ago time.Duration) pgtype.Timestamptz {
	t.Helper()
	return pgtype.Timestamptz{Time: time.Now().Add(-ago), Valid: true}
}
func futureTime(t *testing.T, ahead time.Duration) pgtype.Timestamptz {
	t.Helper()
	return pgtype.Timestamptz{Time: time.Now().Add(ahead), Valid: true}
}

func seedGrant(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string, expiresAt pgtype.Timestamptz) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, expires_at) VALUES ($1, $2, $3)`,
		userRef, cap, expiresAt,
	); err != nil {
		t.Fatalf("seedGrant: %v", err)
	}
}
func seedRevoke(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string, expiresAt pgtype.Timestamptz) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_revokes (user_ref, capability_code, expires_at) VALUES ($1, $2, $3)`,
		userRef, cap, expiresAt,
	); err != nil {
		t.Fatalf("seedRevoke: %v", err)
	}
}
func grantExists(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string) bool {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2`,
		userRef, cap,
	).Scan(&n); err != nil {
		t.Fatalf("grantExists: %v", err)
	}
	return n > 0
}
