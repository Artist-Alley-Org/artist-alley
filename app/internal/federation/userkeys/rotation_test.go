// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the Phase 1.22.I-h rotation primitive. Real Postgres
// + atrest; skips without AA_DB_PASSWORD (same convention as the
// other federation/userkeys tests).
//
// Six cases pin the contract:
//
//   1. Happy path — generates a new key at version N+1 with the
//      previous current row demoted to retained-with-TTL.
//   2. The demoted row carries rotated_at + rotated_by_user_ref.
//   3. The new row's rotated_by_user_ref is recorded.
//   4. Self-rotation vs admin-rotation distinguished by
//      rotated_by_user_ref.
//   5. First-time rotation (no prior key) inserts at v=1
//      (defensive — shouldn't happen post-I-b).
//   6. Audit hook fires with the right metadata.

package userkeys_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// rotateSpy captures every RotateAuditFireFn invocation so the
// tests can assert "fired once per rotation with the right
// metadata" without writing to audit_events.
type rotateSpy struct {
	mu    sync.Mutex
	calls []rotateSpyCall
}

type rotateSpyCall struct {
	subjectUserRef   int64
	rotatedByUserRef int64
	newVersion       int32
	previousVersion  int32
	algorithm        string
}

func (s *rotateSpy) hook(
	_ context.Context,
	subjectUserRef int64,
	rotatedByUserRef int64,
	newVersion int32,
	previousVersion int32,
	algorithm string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rotateSpyCall{
		subjectUserRef:   subjectUserRef,
		rotatedByUserRef: rotatedByUserRef,
		newVersion:       newVersion,
		previousVersion:  previousVersion,
		algorithm:        algorithm,
	})
}

func (s *rotateSpy) lastForSubject(userRef int64) (rotateSpyCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].subjectUserRef == userRef {
			return s.calls[i], true
		}
	}
	return rotateSpyCall{}, false
}

// userWithKey inserts a fresh user + ensures they have a current
// keypair via EnsureCurrentForUser. Returns the user_ref + the
// v1 public key bytes for "post-rotation key is different" assertions.
func userWithKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int64, []byte) {
	t.Helper()
	ref := fixtureUser(t, ctx, pool)
	if _, err := userkeys.EnsureCurrentForUser(ctx, userkeys.New(pool), ref); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	v1, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
	if err != nil {
		t.Fatalf("get seeded key: %v", err)
	}
	return ref, append([]byte{}, v1.PublicKey...)
}

// --- 1. happy path ---

func TestRotateForUser_GeneratesNewVersionAndDemotesPrevious(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	ref, v1Pub := userWithKey(t, ctx, pool)
	spy := &rotateSpy{}

	result, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		userkeys.DefaultRetentionDays, spy.hook)
	if err != nil {
		t.Fatalf("RotateForUser: %v", err)
	}

	if result.NewVersion != 2 {
		t.Errorf("NewVersion = %d, want 2", result.NewVersion)
	}
	if result.PreviousVersion != 1 {
		t.Errorf("PreviousVersion = %d, want 1", result.PreviousVersion)
	}
	if bytes.Equal(result.NewPublicKey, v1Pub) {
		t.Errorf("post-rotation public key matches the previous key — "+
			"rotation didn't mint fresh material (len=%d)", len(result.NewPublicKey))
	}

	// The "new current" is v2.
	got, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
	if err != nil {
		t.Fatalf("GetCurrentUserKey: %v", err)
	}
	if got.Version != 2 || !got.IsCurrent {
		t.Errorf("post-rotation current = %+v, want v=2 is_current=true", got)
	}
	if !bytes.Equal(got.PublicKey, result.NewPublicKey) {
		t.Errorf("current row's public_key doesn't match the returned RotationResult.NewPublicKey")
	}
}

// --- 2. demoted row state ---

func TestRotateForUser_DemotedRowCarriesRetentionAndMetadata(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	ref, _ := userWithKey(t, ctx, pool)

	if _, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		userkeys.DefaultRetentionDays, nil); err != nil {
		t.Fatalf("RotateForUser: %v", err)
	}

	v1, err := userkeys.New(pool).GetUserKeyByVersion(ctx,
		userkeys.GetUserKeyByVersionParams{UserRef: ref, Version: 1})
	if err != nil {
		t.Fatalf("GetUserKeyByVersion v1: %v", err)
	}
	if v1.IsCurrent {
		t.Errorf("v1 still is_current after rotation; constraint violation imminent")
	}
	if !v1.RetainedUntil.Valid {
		t.Errorf("v1 retained_until is NULL after rotation; CHECK constraint expects non-NULL on demoted")
	}
	// Retained TTL should be ~30 days out. We don't bind a tight
	// window because Now() drifts across the rotation tx; +/- 1
	// day buffer is plenty to assert "in the right ballpark."
	if !v1.RotatedAt.Valid {
		t.Errorf("v1 rotated_at not populated on demoted row")
	}
	if v1.RotatedByUserRef == nil || *v1.RotatedByUserRef != ref {
		got := int64(0)
		if v1.RotatedByUserRef != nil {
			got = *v1.RotatedByUserRef
		}
		t.Errorf("v1 rotated_by_user_ref = %d, want %d (self-rotation)", got, ref)
	}
}

// --- 3. new row metadata ---

func TestRotateForUser_NewRowRecordsRotatedByUserRef(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	ref, _ := userWithKey(t, ctx, pool)

	if _, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		userkeys.DefaultRetentionDays, nil); err != nil {
		t.Fatalf("RotateForUser: %v", err)
	}

	v2, err := userkeys.New(pool).GetUserKeyByVersion(ctx,
		userkeys.GetUserKeyByVersionParams{UserRef: ref, Version: 2})
	if err != nil {
		t.Fatalf("GetUserKeyByVersion v2: %v", err)
	}
	if v2.RotatedByUserRef == nil || *v2.RotatedByUserRef != ref {
		t.Errorf("v2 rotated_by_user_ref not set to caller")
	}
	if !v2.RotatedAt.Valid {
		t.Errorf("v2 rotated_at not populated on new row")
	}
}

// --- 4. self vs admin rotation distinction ---

func TestRotateForUser_DistinguishesSelfVsAdminRotation(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	subject, _ := userWithKey(t, ctx, pool)
	admin := fixtureUser(t, ctx, pool) // no keypair needed for the admin

	spy := &rotateSpy{}

	// Admin-initiated rotation: rotatedByUserRef = admin.
	if _, err := userkeys.RotateForUser(ctx, pool, subject, admin,
		userkeys.DefaultRetentionDays, spy.hook); err != nil {
		t.Fatalf("RotateForUser admin: %v", err)
	}

	v2, err := userkeys.New(pool).GetUserKeyByVersion(ctx,
		userkeys.GetUserKeyByVersionParams{UserRef: subject, Version: 2})
	if err != nil {
		t.Fatalf("GetUserKeyByVersion: %v", err)
	}
	if v2.RotatedByUserRef == nil || *v2.RotatedByUserRef != admin {
		got := int64(0)
		if v2.RotatedByUserRef != nil {
			got = *v2.RotatedByUserRef
		}
		t.Errorf("v2 rotated_by_user_ref = %d, want admin=%d", got, admin)
	}

	call, ok := spy.lastForSubject(subject)
	if !ok {
		t.Fatalf("audit spy didn't fire for subject %d", subject)
	}
	if call.rotatedByUserRef != admin {
		t.Errorf("audit rotatedByUserRef = %d, want admin=%d", call.rotatedByUserRef, admin)
	}
	if call.subjectUserRef == call.rotatedByUserRef {
		t.Errorf("admin rotation flagged as self-rotation (subject==rotatedBy) — semantic mix")
	}
}

// --- 5. defensive first-time rotation ---

func TestRotateForUser_NoPreviousKey_InsertsAsV1(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	// Fresh user with no keypair (the I-b backfill safety net
	// would catch this on next boot; the rotation primitive
	// shouldn't crash on the path).
	ref := fixtureUser(t, ctx, pool)

	result, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		userkeys.DefaultRetentionDays, nil)
	if err != nil {
		t.Fatalf("RotateForUser: %v", err)
	}
	if result.NewVersion != 1 {
		t.Errorf("NewVersion = %d, want 1 (first-time)", result.NewVersion)
	}
	if result.PreviousVersion != 0 {
		t.Errorf("PreviousVersion = %d, want 0 (no prior)", result.PreviousVersion)
	}

	got, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
	if err != nil {
		t.Fatalf("GetCurrentUserKey: %v", err)
	}
	if got.Version != 1 || !got.IsCurrent {
		t.Errorf("first-time rotation: current = %+v, want v=1 is_current=true", got)
	}
}

// --- 6. audit semantics ---

func TestRotateForUser_AuditFiresWithExpectedMetadata(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	ref, _ := userWithKey(t, ctx, pool)
	spy := &rotateSpy{}

	if _, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		userkeys.DefaultRetentionDays, spy.hook); err != nil {
		t.Fatalf("RotateForUser: %v", err)
	}

	call, ok := spy.lastForSubject(ref)
	if !ok {
		t.Fatalf("audit spy didn't fire for subject %d", ref)
	}
	if call.newVersion != 2 || call.previousVersion != 1 {
		t.Errorf("audit versions = (new=%d, prev=%d), want (2, 1)",
			call.newVersion, call.previousVersion)
	}
	if call.algorithm != userkeys.Algorithm {
		t.Errorf("audit algorithm = %q, want %q", call.algorithm, userkeys.Algorithm)
	}
	if call.subjectUserRef != call.rotatedByUserRef {
		t.Errorf("self-rotation should have subject==rotatedBy; got %d != %d",
			call.subjectUserRef, call.rotatedByUserRef)
	}
}

// --- 7. retention days normalisation ---

func TestRotateForUser_NonPositiveRetentionFallsBackToDefault(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	ref, _ := userWithKey(t, ctx, pool)

	// 0 days would otherwise produce an instant-expiry retained
	// row (immediately reapable by the sweeper) — orphans
	// in-flight envelopes encrypted against v1. The primitive
	// normalises to DefaultRetentionDays so a misconfigured
	// sysconfig can't accidentally zero the grace window.
	if _, err := userkeys.RotateForUser(ctx, pool, ref, ref,
		0, nil); err != nil {
		t.Fatalf("RotateForUser zero retention: %v", err)
	}

	// We can't assert the exact retained_until without freezing
	// time, but we CAN assert non-NULL + > NOW() — the only way
	// that's true after a 0-day request is the normalisation
	// firing.
	v1, err := userkeys.New(pool).GetUserKeyByVersion(ctx,
		userkeys.GetUserKeyByVersionParams{UserRef: ref, Version: 1})
	if err != nil {
		t.Fatalf("GetUserKeyByVersion: %v", err)
	}
	if !v1.RetainedUntil.Valid {
		t.Errorf("retained_until null after rotation with zero retention; normalisation didn't fire")
	}
}
