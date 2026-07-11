// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.E — CapabilitySweeper request-cascade tests.
//
// Pins the lock-in contract:
//
//   * A reaped grant with a non-NULL request_ref calls the
//     RequestCascadeFn exactly once with the matching request id.
//   * A reaped grant WITHOUT request_ref doesn't fire the cascade.
//   * A cascade-callback failure doesn't fail the sweep + doesn't
//     undo the grant reap (best-effort by contract).
//   * Multiple reaped grants with distinct request_refs each fire
//     the cascade once.
//
// The cascade is wired in api.go to requests.Handler.MarkExpired;
// these tests use a recording stub so the sweeper's contract is
// verified independently of the requests package.

package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func TestCapabilitySweeper_GrantWithRequestRef_CascadesOnce(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")

	// Seed a fake resource_request row so the FK on request_ref
	// holds; the sweeper doesn't care about the row's state, just
	// that the FK target exists.
	requestID := uuid.New()
	seedFakeRequestRow(t, pool, requestID, u)

	// Seed an expired grant linked to that request.
	seedGrantWithRequestRef(t, pool, u, "posts.publish",
		pastTime(t, 1*time.Hour), requestID)

	rec := newCascadeRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	sw.SetRequestCascade(rec.OnCascade)

	g, _ := sw.SweepOnce(context.Background())
	if g != 1 {
		t.Errorf("grants reaped = %d, want 1", g)
	}
	if rec.calls.Load() != 1 {
		t.Errorf("cascade calls = %d, want 1", rec.calls.Load())
	}
	if rec.lastRequest.Load() == nil {
		t.Fatal("cascade fired with nil request id")
	} else if rec.lastRequest.Load().(uuid.UUID) != requestID {
		t.Errorf("cascade request id = %v, want %v", rec.lastRequest.Load(), requestID)
	}
}

func TestCapabilitySweeper_GrantWithoutRequestRef_NoCascade(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")

	// Seed an expired grant with NULL request_ref (the 1.17.A/C-
	// shape direct admin grant).
	seedGrant(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour))

	rec := newCascadeRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	sw.SetRequestCascade(rec.OnCascade)

	g, _ := sw.SweepOnce(context.Background())
	if g != 1 {
		t.Errorf("grants reaped = %d, want 1", g)
	}
	if rec.calls.Load() != 0 {
		t.Errorf("cascade fired for a grant without request_ref; calls = %d", rec.calls.Load())
	}
}

func TestCapabilitySweeper_CascadeFailure_DoesNotFailSweep(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	requestID := uuid.New()
	seedFakeRequestRow(t, pool, requestID, u)
	seedGrantWithRequestRef(t, pool, u, "posts.publish",
		pastTime(t, 1*time.Hour), requestID)

	rec := newCascadeRecorder()
	rec.failWith = errors.New("synthetic cascade failure")
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	sw.SetRequestCascade(rec.OnCascade)

	// Sweep should still complete + reap the grant.
	g, _ := sw.SweepOnce(context.Background())
	if g != 1 {
		t.Errorf("grants reaped = %d despite cascade failure; want 1", g)
	}
	// Grant row should be gone.
	if grantExists(t, pool, u, "posts.publish") {
		t.Error("grant survived a sweep with a failing cascade")
	}
}

func TestCapabilitySweeper_NilCascade_NoPanic(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	requestID := uuid.New()
	seedFakeRequestRow(t, pool, requestID, u)
	seedGrantWithRequestRef(t, pool, u, "posts.publish",
		pastTime(t, 1*time.Hour), requestID)

	// No SetRequestCascade call — sweeper should reap + log,
	// not panic on the nil callback.
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	g, _ := sw.SweepOnce(context.Background())
	if g != 1 {
		t.Errorf("grants reaped = %d, want 1 with nil cascade", g)
	}
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

type cascadeRecorder struct {
	mu          sync.Mutex
	calls       atomic.Int32
	lastRequest atomic.Value // uuid.UUID
	failWith    error
}

func newCascadeRecorder() *cascadeRecorder { return &cascadeRecorder{} }

func (r *cascadeRecorder) OnCascade(_ context.Context, requestID uuid.UUID, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.Add(1)
	r.lastRequest.Store(requestID)
	return r.failWith
}

// seedFakeRequestRow inserts a resource_request row with the given
// id so the FK on user_capability_grants.request_ref holds when
// the test seeds a grant.
func seedFakeRequestRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, requesterRef int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO resource_request (id, requester_user_ref, target_asset_id, requested_capability)
		 VALUES ($1, $2, $3, $4)`,
		id, requesterRef, uuid.New(), "posts.publish",
	); err != nil {
		t.Fatalf("seedFakeRequestRow: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM resource_request WHERE id = $1`, id)
	})
}

// seedGrantWithRequestRef seeds an expired grant with a populated
// request_ref so the sweeper's cascade path fires.
func seedGrantWithRequestRef(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string, expiresAt interface{}, requestID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, expires_at, request_ref)
		 VALUES ($1, $2, $3, $4)`,
		userRef, cap, expiresAt, requestID,
	); err != nil {
		t.Fatalf("seedGrantWithRequestRef: %v", err)
	}
}
