// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE RUNNER'S AUTHORITY PHASES SERIALIZE THEMSELVES (#1173, #1119).
//
// # Why this test exists rather than an inspection
//
// Twice now the defect has been "the primitive exists but a production
// caller is missing or misclassified": first a ninth authority writer in
// another package, then `aa seed` written off as offline. Both survived
// review because the caller side was argued rather than exercised.
//
// So this drives the ACTUAL runner phases — `applyTeams` and
// `applyFixturePrincipals`, the two that write `team_closure` and
// `user_roles` — and observes the serialization. It does not take the
// lock itself and it does not stand in a helper for the real path.
//
// # The mechanism
//
// A reader holds the SHARED authority lock exactly as a batch apply
// does, expressed as raw SQL so nothing here depends on the primitive
// under test. The phase is launched and must BLOCK. The test waits on
// `pg_stat_activity` for an observed wait — a state, not an elapsed
// duration — and FAILS OUTRIGHT if the overlap never happens, which is
// exactly what it does against a runner that takes no lock.
package seed

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

const (
	authorityLockSpaceForTest  = 1119
	authorityStructuralForTest = 0
)

func phaseLockPool(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s",
		envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
		envOr("AA_DB_USER", "artist_alley"), testdb.Name(t), pwd, appName)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// waitForBlockedPhase is the HAPPENS-BEFORE WITNESS. It fails rather
// than continuing if the overlap never materialises, because a
// serialization test that quietly ran its phase to completion is a test
// that reports green for the bug it exists to catch.
func waitForBlockedPhase(t *testing.T, observer *pgxpool.Pool, appName, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := observer.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, appName).Scan(&n); err != nil {
			t.Fatalf("observe phase: %v", err)
		}
		if n >= 1 {
			t.Logf("synchronisation seam: the runner's %s phase is observed BLOCKED on the "+
				"authority lock, before it could mutate authority", what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the runner's %s phase never blocked on the authority lock — it is free to "+
		"change effective authority underneath a batch that has already resolved its "+
		"verdict", what)
}

// runPhaseAgainstHeldReader is the shared body: hold the shared lock,
// launch the real phase, require an observed wait, release, require
// completion.
func runPhaseAgainstHeldReader(t *testing.T, what string, phase func(*Runner) error) {
	t.Helper()
	observer := phaseLockPool(t, "aa-phaselock-observer")
	appName := fmt.Sprintf("aa-phaselock-%d", time.Now().UnixNano())
	runnerPool := phaseLockPool(t, appName)
	ctx := t.Context()

	// THE READER, taking the shared authority lock as a batch apply
	// does — raw SQL, so this test is not written in terms of the thing
	// it is testing.
	readerTx, err := observer.Begin(ctx)
	if err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = readerTx.Rollback(context.Background())
		}
	}()
	if _, err := readerTx.Exec(ctx,
		`SELECT pg_advisory_xact_lock_shared($1::INT, $2::INT)`,
		authorityLockSpaceForTest, authorityStructuralForTest); err != nil {
		t.Fatalf("reader lock: %v", err)
	}

	r := &Runner{
		pool:  runnerPool,
		q:     New(runnerPool),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		teams: map[string]pgtype.UUID{},
	}

	done := make(chan error, 1)
	go func() { done <- phase(r) }()

	waitForBlockedPhase(t, observer, appName, what)

	released = true
	if err := readerTx.Rollback(context.Background()); err != nil {
		t.Fatalf("reader release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s after the reader released: %v", what, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never completed after the reader released", what)
	}
}

// applyTeams writes `team_closure`, which expands every team-scoped
// grant.
func TestRunnerApplyTeams_WaitsForAnInFlightAuthorityReader(t *testing.T) {
	runPhaseAgainstHeldReader(t, "applyTeams", func(r *Runner) error {
		// An EMPTY catalogue is deliberate: the phase must take the lock
		// because of what it IS, not because of how much it happens to
		// write. A phase that only locked when it had rows would leave
		// the window open on the run that mattered.
		return r.applyTeams(context.Background(), &catalogues{})
	})
}

// applyFixturePrincipals writes `user_roles` via SeedSetUserGlobalRole.
func TestRunnerApplyFixturePrincipals_WaitsForAnInFlightAuthorityReader(t *testing.T) {
	runPhaseAgainstHeldReader(t, "applyFixturePrincipals", func(r *Runner) error {
		return r.applyFixturePrincipals(context.Background(), nil)
	})
}
