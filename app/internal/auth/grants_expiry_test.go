// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.C — time-bound grant/revoke expiry tests.
//
// Covers the column-side persistence + the handler-side validation:
//
//   * AddAdminUserGrant with a future expires_at persists the column
//   * Same without expires_at leaves the column NULL (permanent)
//   * Past expires_at rejected with 400 before any write
//   * Same three cases for AddAdminUserRevoke
//
// Sweeper-time behavior + reaping lives in capability_sweeper_test.go
// (commit 2).

package auth_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestAddAdminUserGrant_FutureExpiry_Persists(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	subjectRef := seedSubjectUser(t, pool, "expiry-grant-future")
	caller := seedAdminCaller(t, pool, "expiry-grant-admin")

	h := &auth.Handler{Pool: pool}
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: caller, Capabilities: []string{"users.write"}})

	resp, err := h.AddAdminUserGrant(ctx, openapi.AddAdminUserGrantRequestObject{
		Ref: subjectRef,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "posts.publish",
			ExpiresAt:  &future,
		},
	})
	if err != nil {
		t.Fatalf("AddAdminUserGrant: %v", err)
	}
	if _, ok := resp.(openapi.AddAdminUserGrant204Response); !ok {
		t.Fatalf("expected 204, got %T", resp)
	}

	got := readGrantExpiry(t, pool, subjectRef, "posts.publish")
	if !got.Valid {
		t.Fatalf("expires_at not persisted; want %v", future)
	}
	if delta := got.Time.Sub(future); delta < -time.Second || delta > time.Second {
		t.Errorf("expires_at = %v, want ~%v (delta %v)", got.Time, future, delta)
	}
}

func TestAddAdminUserGrant_NoExpiry_IsPermanent(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	subjectRef := seedSubjectUser(t, pool, "expiry-grant-perm")
	caller := seedAdminCaller(t, pool, "expiry-grant-perm-admin")

	h := &auth.Handler{Pool: pool}
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: caller, Capabilities: []string{"users.write"}})

	if _, err := h.AddAdminUserGrant(ctx, openapi.AddAdminUserGrantRequestObject{
		Ref: subjectRef,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "posts.publish",
			// no ExpiresAt
		},
	}); err != nil {
		t.Fatalf("AddAdminUserGrant: %v", err)
	}

	got := readGrantExpiry(t, pool, subjectRef, "posts.publish")
	if got.Valid {
		t.Errorf("expires_at = %v, want NULL (permanent)", got.Time)
	}
}

func TestAddAdminUserGrant_PastExpiry_Rejects400(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	subjectRef := seedSubjectUser(t, pool, "expiry-grant-past")
	caller := seedAdminCaller(t, pool, "expiry-grant-past-admin")

	h := &auth.Handler{Pool: pool}
	past := time.Now().Add(-1 * time.Hour)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: caller, Capabilities: []string{"users.write"}})

	resp, err := h.AddAdminUserGrant(ctx, openapi.AddAdminUserGrantRequestObject{
		Ref: subjectRef,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "posts.publish",
			ExpiresAt:  &past,
		},
	})
	if err != nil {
		t.Fatalf("AddAdminUserGrant: %v", err)
	}
	r400, ok := resp.(openapi.AddAdminUserGrant400JSONResponse)
	if !ok {
		t.Fatalf("expected 400, got %T", resp)
	}
	if !strings.Contains(r400.Error, "future") {
		t.Errorf("400 message should mention 'future'; got %q", r400.Error)
	}

	// No row should have landed.
	if _, err := readGrantRow(pool, subjectRef, "posts.publish"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("rejected grant still wrote a row; got err=%v", err)
	}
}

func TestAddAdminUserRevoke_FutureExpiry_Persists(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	subjectRef := seedSubjectUser(t, pool, "expiry-revoke-future")
	caller := seedAdminCaller(t, pool, "expiry-revoke-admin")

	h := &auth.Handler{Pool: pool}
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: caller, Capabilities: []string{"users.write"}})

	if _, err := h.AddAdminUserRevoke(ctx, openapi.AddAdminUserRevokeRequestObject{
		Ref: subjectRef,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "posts.publish",
			ExpiresAt:  &future,
		},
	}); err != nil {
		t.Fatalf("AddAdminUserRevoke: %v", err)
	}

	got := readRevokeExpiry(t, pool, subjectRef, "posts.publish")
	if !got.Valid {
		t.Fatalf("expires_at not persisted on revoke")
	}
	if delta := got.Time.Sub(future); delta < -time.Second || delta > time.Second {
		t.Errorf("expires_at = %v, want ~%v", got.Time, future)
	}
}

func TestAddAdminUserRevoke_PastExpiry_Rejects400(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	subjectRef := seedSubjectUser(t, pool, "expiry-revoke-past")
	caller := seedAdminCaller(t, pool, "expiry-revoke-past-admin")

	h := &auth.Handler{Pool: pool}
	past := time.Now().Add(-1 * time.Hour)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: caller, Capabilities: []string{"users.write"}})

	resp, err := h.AddAdminUserRevoke(ctx, openapi.AddAdminUserRevokeRequestObject{
		Ref: subjectRef,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "posts.publish",
			ExpiresAt:  &past,
		},
	})
	if err != nil {
		t.Fatalf("AddAdminUserRevoke: %v", err)
	}
	if _, ok := resp.(openapi.AddAdminUserRevoke400JSONResponse); !ok {
		t.Fatalf("expected 400 on past expires_at, got %T", resp)
	}
}

// ---------------------------------------------------------------
// Helpers (shared with capability_sweeper_test.go via package_test)
// ---------------------------------------------------------------

func seedSubjectUser(t *testing.T, pool *pgxpool.Pool, _ string) int64 {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)
	// Username has a 50-char VARCHAR cap; keep the label out and
	// rely on random hex for uniqueness. The test name itself is
	// the human-readable thread back to the fixture.
	username := "cs-" + randHex(8)
	pw := "irrelevant"
	u, err := q.CreateUser(ctx, auth.CreateUserParams{Username: &username, Password: &pw})
	if err != nil {
		t.Fatalf("seed subject: %v", err)
	}
	subjectCleanup = append(subjectCleanup, u.Ref)
	return u.Ref
}

// seedAdminCaller mints an admin and returns the ref. seedAdmin
// prefixes with "lastadmin-"; pass a short label to stay under
// the 50-char user.username cap.
func seedAdminCaller(t *testing.T, pool *pgxpool.Pool, _ string) int64 {
	t.Helper()
	return seedAdmin(t, pool, "cs"+randHex(4))
}

var subjectCleanup []int64

func cleanupSubjects(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, ref := range subjectCleanup {
		_, _ = pool.Exec(ctx, `DELETE FROM user_capability_grants WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(ctx, `DELETE FROM user_capability_revokes WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE ref = $1`, ref)
	}
	subjectCleanup = nil
}

func readGrantExpiry(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string) pgtype.Timestamptz {
	t.Helper()
	var got pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM user_capability_grants
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		userRef, cap,
	).Scan(&got); err != nil {
		t.Fatalf("readGrantExpiry: %v", err)
	}
	return got
}

func readRevokeExpiry(t *testing.T, pool *pgxpool.Pool, userRef int64, cap string) pgtype.Timestamptz {
	t.Helper()
	var got pgtype.Timestamptz
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM user_capability_revokes
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		userRef, cap,
	).Scan(&got); err != nil {
		t.Fatalf("readRevokeExpiry: %v", err)
	}
	return got
}

func readGrantRow(pool *pgxpool.Pool, userRef int64, cap string) (int64, error) {
	var n int64
	err := pool.QueryRow(context.Background(),
		`SELECT user_ref FROM user_capability_grants
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		userRef, cap,
	).Scan(&n)
	return n, err
}

// httptest import retained because future tests may add an HTTP
// roundtrip flavour — wrapped to silence the unused-import lint
// without removing the dependency line.
var _ = httptest.NewRequest