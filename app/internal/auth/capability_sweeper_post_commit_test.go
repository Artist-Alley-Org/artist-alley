// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE SWEEP COMMITS BEFORE IT ANNOUNCES ITSELF (#1173, #1119).
//
// Every effect the sweeper fires — the reaped-grant audit, the reaped-
// revoke audit, the request cascade, the capability-cache drop — is a
// BEST-EFFORT CONSEQUENCE of a reap that has already happened. None may
// run before the reap is durable.
//
// # Why this became possible to get wrong
//
// The sweep did not used to have a transaction at all: each statement
// committed on its own, so a callback firing after the DELETE returned
// was firing after a durable change by construction. Bringing the sweep
// under one transaction — needed so it can hold the STRUCTURAL authority
// lock across the whole reap — put every callback inside that
// transaction and silently made them pre-commit. A rollback would then
// have left the system having announced reaps for rows that were still
// present.
//
// # What these tests assert, and why it is an ORDERING and not a count
//
// A test that checked the callbacks merely RAN would have passed both
// before and after. So the callback here QUERIES THE DATABASE ON ITS OWN
// CONNECTION and asserts its own grant is ALREADY GONE. Before the fix
// that read sees the row still present, because the deleting transaction
// has not committed.
package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// grantRowExists reads on its OWN pool connection, outside whatever
// transaction the sweeper is running. That is the whole instrument: an
// uncommitted DELETE is invisible here, so "still present" means "not
// yet committed".
func grantRowExists(t *testing.T, pool *pgxpool.Pool, userRef int64, code string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_capability_grants
		  WHERE user_ref = $1 AND capability_code = $2`, userRef, code).Scan(&n); err != nil {
		t.Fatalf("read grant row: %v", err)
	}
	return n > 0
}

func revokeRowExists(t *testing.T, pool *pgxpool.Pool, userRef int64, code string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_capability_revokes
		  WHERE user_ref = $1 AND capability_code = $2`, userRef, code).Scan(&n); err != nil {
		t.Fatalf("read revoke row: %v", err)
	}
	return n > 0
}

// A reaped-GRANT audit callback observes its grant already committed
// gone.
func TestCapabilitySweeper_GrantAuditFiresAfterCommit(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	seedGrant(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour))

	var observedPresent bool
	var fired int
	audit := func(ctx context.Context, userRef int64, code, teamID string, expiredAt time.Time) {
		fired++
		observedPresent = grantRowExists(t, pool, userRef, code)
	}

	sw := auth.NewCapabilitySweeper(pool, nil, audit, nil, nil, 1*time.Hour)
	if g, _ := sw.SweepOnce(context.Background()); g != 1 {
		t.Fatalf("grants reaped = %d, want 1", g)
	}
	if fired != 1 {
		t.Fatalf("audit callback fired %d times, want 1", fired)
	}
	if observedPresent {
		t.Fatal("THE AUDIT FIRED BEFORE THE COMMIT: the callback announced a reaped grant " +
			"while the row was still present, so a rollback would have left the system " +
			"asserting a change to durable state that never happened")
	}
}

// The same for a reaped REVOKE.
func TestCapabilitySweeper_RevokeAuditFiresAfterCommit(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	seedRevoke(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour))

	var observedPresent bool
	var fired int
	audit := func(ctx context.Context, userRef int64, code, teamID string, expiredAt time.Time) {
		fired++
		observedPresent = revokeRowExists(t, pool, userRef, code)
	}

	sw := auth.NewCapabilitySweeper(pool, nil, nil, audit, nil, 1*time.Hour)
	if _, r := sw.SweepOnce(context.Background()); r != 1 {
		t.Fatalf("revokes reaped = %d, want 1", r)
	}
	if fired != 1 {
		t.Fatalf("audit callback fired %d times, want 1", fired)
	}
	if observedPresent {
		t.Fatal("THE REVOKE AUDIT FIRED BEFORE THE COMMIT")
	}
}

// And the REQUEST CASCADE, which reaches into another package on its own
// connection and is the effect with the most visible consequences.
func TestCapabilitySweeper_RequestCascadeFiresAfterCommit(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	requestID := uuid.New()
	seedFakeRequestRow(t, pool, requestID, u)
	seedGrantWithRequestRef(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour), requestID)

	var observedPresent bool
	var fired int
	sw := auth.NewCapabilitySweeper(pool, nil, nil, nil, nil, 1*time.Hour)
	sw.SetRequestCascade(func(ctx context.Context, rid uuid.UUID, expiredAt time.Time) error {
		fired++
		observedPresent = grantRowExists(t, pool, u, "posts.publish")
		return nil
	})

	if g, _ := sw.SweepOnce(context.Background()); g != 1 {
		t.Fatalf("grants reaped = %d, want 1", g)
	}
	if fired != 1 {
		t.Fatalf("cascade fired %d times, want 1", fired)
	}
	if observedPresent {
		t.Fatal("THE REQUEST CASCADE FIRED BEFORE THE COMMIT: another package was told a " +
			"request's grant had expired while that grant was still present")
	}
}

// ⚠️ THE CONTRACT THAT MUST SURVIVE THE FIX. Post-commit means the reap
// STANDS even when a callback fails — the callback is a consequence, not
// a participant. This is the pre-existing best-effort contract, asserted
// here against the new ordering so moving the callbacks cannot quietly
// make a failing audit undo a reap.
func TestCapabilitySweeper_CallbackFailureDoesNotUndoTheReap(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	u := seedSubjectUser(t, pool, "")
	requestID := uuid.New()
	seedFakeRequestRow(t, pool, requestID, u)
	seedGrantWithRequestRef(t, pool, u, "posts.publish", pastTime(t, 1*time.Hour), requestID)

	sw := auth.NewCapabilitySweeper(pool, nil,
		func(ctx context.Context, userRef int64, code, teamID string, expiredAt time.Time) {
			panic("an audit sink that is down must not un-reap anything")
		}, nil, nil, 1*time.Hour)
	sw.SetRequestCascade(func(ctx context.Context, rid uuid.UUID, expiredAt time.Time) error {
		return errors.New("cascade target unreachable")
	})

	func() {
		defer func() { _ = recover() }()
		_, _ = sw.SweepOnce(context.Background())
	}()

	if grantRowExists(t, pool, u, "posts.publish") {
		t.Fatal("THE REAP MUST STAND: a failing callback is a consequence of the reap, " +
			"not a participant in it")
	}
}
