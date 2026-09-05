// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE RUNNER'S AUTHORITY PHASES SERIALIZE THEIR ACTUAL WRITES
// (#1173, #1119).
//
// # The vacuity this file replaces, named so it is not rebuilt
//
// The first version of these tests drove the real phases with an EMPTY
// catalogue and a nil principal slice, on the reasoning that "the phase
// must lock because of what it IS, not because of how much it writes".
//
// ⛔ That reasoning is appealing and it was wrong. `SeedInsertTeamClosureSelf`
// sits INSIDE `for _, t := range cat.Teams`, and `SeedSetUserGlobalRole`
// sits AFTER `if len(ps) == 0 { return nil }`. With an empty fixture
// NEITHER WRITE EXECUTES AT ALL, so both tests proved only that the
// wrapper acquires a lock — the one thing that was never in doubt —
// while bypassing the exact mutation they existed to protect.
//
// It is the third instance of one pattern in this arc: the primitive or
// the wrapper is proven and the production write is not. So these tests
// are built the other way round, from the row outwards.
//
// # What each test now establishes, in order
//
//  1. a reader holds the SHARED authority lock, exactly as a batch apply
//     does — raw SQL, so nothing here is written in terms of the thing
//     under test;
//  2. the REAL phase runs with a POPULATED fixture, so its authority
//     write is genuinely reached;
//  3. the phase is observed BLOCKED via pg_stat_activity — a state, not
//     an elapsed duration — and the test fails outright if that overlap
//     never happens;
//  4. ⭐ THE AUTHORITY ROW IS ASSERTED ABSENT while the reader holds.
//     This is what makes the block meaningful rather than decorative:
//     the phase is stopped BEFORE its mutation can commit;
//  5. the reader releases;
//  6. ⭐ THE AUTHORITY ROW IS ASSERTED PRESENT. Not that the phase
//     returned nil — a phase that locked, did nothing and succeeded
//     would satisfy that, which is the same trap wearing a populated
//     catalogue.
package seed

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
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
				"authority lock, before its authority write could commit", what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the runner's %s phase never blocked on the authority lock — it is free to "+
		"change effective authority underneath a batch that has already resolved its "+
		"verdict", what)
}

// phaseSerializationCase is the shared shape. `count` reads the ACTUAL
// authority row the phase is supposed to write, so absence and presence
// are both observations of the database rather than of control flow.
type phaseSerializationCase struct {
	what  string
	run   func(*Runner) error
	count func(*pgxpool.Pool) int
}

func runPhaseSerialization(t *testing.T, newRunner func(*pgxpool.Pool, *slog.Logger) *Runner, c phaseSerializationCase) {
	t.Helper()
	observer := phaseLockPool(t, "aa-phaselock-observer")
	appName := fmt.Sprintf("aa-phaselock-%d", time.Now().UnixNano())
	runnerPool := phaseLockPool(t, appName)
	ctx := t.Context()

	if n := c.count(observer); n != 0 {
		t.Fatalf("%s: the authority row already exists before the phase runs (%d); "+
			"the fixture must be one this run actually creates, or presence proves nothing", c.what, n)
	}

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

	r := newRunner(runnerPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- c.run(r) }()

	waitForBlockedPhase(t, observer, appName, c.what)

	// ⭐ THE BLOCK IS BEFORE THE MUTATION. Read on the observer's own
	// connection: an uncommitted write would be invisible here anyway,
	// and a committed one would prove the phase had already got past the
	// thing the lock exists to stop.
	if n := c.count(observer); n != 0 {
		t.Fatalf("%s: the authority write COMMITTED while a reader held the shared "+
			"authority lock (%d rows) — the phase blocked somewhere, but not before its "+
			"mutation", c.what, n)
	}

	released = true
	if err := readerTx.Rollback(context.Background()); err != nil {
		t.Fatalf("reader release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s after the reader released: %v", c.what, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never completed after the reader released", c.what)
	}

	// ⭐ AND THE REAL MUTATION LANDED. Asserted on the ROW, never on the
	// phase returning nil: a phase that locked, wrote nothing and
	// succeeded would pass that weaker check, which is exactly the trap
	// the empty-catalogue version fell into.
	after := c.count(observer)
	if after == 0 {
		t.Fatalf("%s: the phase completed but its authority row was never written — "+
			"the test would have proved only that a wrapper takes a lock", c.what)
	}
	t.Logf("NON-VACUITY: %s's authority row went 0 -> %d, and was still 0 while the "+
		"reader held the shared lock — the real write was reached AND was serialized",
		c.what, after)
}

// applyTeams writes `team_closure`, which expands every team-scoped
// grant. The catalogue carries ONE REAL TEAM so the write executes.
func TestRunnerApplyTeams_SerializesItsTeamClosureWrite(t *testing.T) {
	teamID := uuid.New()
	slug := "phaselock-" + teamID.String()[:8]
	pgTeam := pgtype.UUID{Bytes: teamID, Valid: true}

	t.Cleanup(func() {
		p := phaseLockPool(t, "aa-phaselock-cleanup")
		c := context.Background()
		_, _ = p.Exec(c, `DELETE FROM team_closure WHERE ancestor_id = $1 OR descendant_id = $1`, pgTeam)
		_, _ = p.Exec(c, `DELETE FROM teams WHERE id = $1`, pgTeam)
	})

	runPhaseSerialization(t,
		func(pool *pgxpool.Pool, log *slog.Logger) *Runner {
			return &Runner{pool: pool, q: New(pool), log: log, teams: map[string]pgtype.UUID{}}
		},
		phaseSerializationCase{
			what: "applyTeams",
			run: func(r *Runner) error {
				return r.applyTeams(context.Background(), &catalogues{
					Teams: []catTeam{{ID: teamID.String(), Name: slug}},
				})
			},
			// The SELF-CLOSURE row, which is the authority-bearing
			// write — not the `teams` row, which is not.
			count: func(p *pgxpool.Pool) int {
				var n int
				if err := p.QueryRow(context.Background(),
					`SELECT count(*) FROM team_closure
					  WHERE ancestor_id = $1 AND descendant_id = $1`, pgTeam).Scan(&n); err != nil {
					t.Fatalf("count team_closure: %v", err)
				}
				return n
			},
		})
}

// applyFixturePrincipals writes `user_roles` via SeedSetUserGlobalRole.
// The principal list carries ONE REAL ACCOUNT so the write executes.
func TestRunnerApplyFixturePrincipals_SerializesItsUserRoleWrite(t *testing.T) {
	username := "phaselock-" + uuid.NewString()[:8]

	userRef := func(p *pgxpool.Pool) (int64, bool) {
		var ref int64
		if err := p.QueryRow(context.Background(),
			`SELECT ref FROM "user" WHERE username = $1`, username).Scan(&ref); err != nil {
			return 0, false
		}
		return ref, true
	}

	t.Cleanup(func() {
		p := phaseLockPool(t, "aa-phaselock-cleanup")
		c := context.Background()
		if ref, ok := userRef(p); ok {
			_, _ = p.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, ref)
			_, _ = p.Exec(c, `DELETE FROM federation_user_keys WHERE user_ref = $1`, ref)
			_, _ = p.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
		}
	})

	runPhaseSerialization(t,
		func(pool *pgxpool.Pool, log *slog.Logger) *Runner {
			admin := NewAdminHandler(pool, nil, nil, nil,
				// A real hash is not the point here; the phase must
				// only be able to create the account it then assigns a
				// role to.
				func(plaintext string) (string, error) { return "x" + plaintext, nil },
				nil)
			return &Runner{
				pool: pool, q: New(pool), log: log,
				admin: admin,
				users: map[string]int64{},
			}
		},
		phaseSerializationCase{
			what: "applyFixturePrincipals",
			run: func(r *Runner) error {
				return r.applyFixturePrincipals(context.Background(), []catFixturePrincipal{{
					Username: username,
					Password: "phase-lock-fixture-password",
					FullName: "Phase Lock Fixture",
				}})
			},
			// The ROLE ASSIGNMENT, which is the authority-bearing write.
			count: func(p *pgxpool.Pool) int {
				ref, ok := userRef(p)
				if !ok {
					return 0
				}
				var n int
				if err := p.QueryRow(context.Background(),
					`SELECT count(*) FROM user_roles WHERE user_ref = $1`, ref).Scan(&n); err != nil {
					t.Fatalf("count user_roles: %v", err)
				}
				return n
			},
		})
}
