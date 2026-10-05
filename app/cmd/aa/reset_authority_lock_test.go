// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// `aa seed --reset` IS NOT LIFECYCLE-EXEMPT (#1173, #1119).
//
// # The classification that was wrong
//
// This command was written off as "offline maintenance that also
// TRUNCATEs", and both halves of that were false.
//
// It is DESIGNED for a live instance: its migrate step is documented as
// safe "whether the server already migrated, is migrating right now, or
// was never started", and --reset broadcasts a wildcard cache flush
// over NOTIFY precisely because "the seeder is a separate process with
// no cache Registry" and a server may be serving throughout.
//
// And the TRUNCATE is the BLAST RADIUS, not a reason to dismiss it.
// `seed.Reset`'s TRUNCATE ... CASCADE and its `DELETE FROM teams` empty
// `user_roles`, `user_capability_grants` and `user_capability_revokes`
// wholesale; `bootstrap.Run` then restores the admin's role. That is the
// largest authority mutation the system performs, and it can run while a
// batch metadata edit is mid-flight holding a verdict drawn from the
// rows being emptied.
//
// # What this test asserts
//
// The same held-lock-plus-observed-wait mechanism the batch race seams
// use, from the other side. A reader takes the SHARED authority lock
// exactly as a batch apply does — as raw SQL, so this test compiles and
// runs against the pre-correction tree too — and holds it. Then
// `resetContent`, the real production composition, is launched.
//
// It must BLOCK. The test waits until `pg_stat_activity` reports the
// reset's backend waiting on a lock, which is an observation of STATE
// and not an elapsed duration, and FAILS OUTRIGHT if the overlap never
// happens. Before the correction the reset takes no authority lock at
// all, sails past the held reader, and empties the authority tables
// underneath it.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/bootstrap"
)

// The two keys the authority lock uses. Restated here rather than
// imported so that a change to the handler's keys without a change here
// stops the gate blocking anything, `waitForBlockedReset` times out, and
// the test FAILS LOUDLY rather than quietly proving nothing.
const (
	authorityLockSpace  = 1119
	authorityStructural = 0
)

// waitForBlockedReset is the HAPPENS-BEFORE WITNESS.
func waitForBlockedReset(t *testing.T, pool *pgxpool.Pool, appName string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, appName).Scan(&n); err != nil {
			t.Fatalf("observe reset: %v", err)
		}
		if n >= 1 {
			t.Log("synchronisation seam: `aa seed --reset` is observed BLOCKED on the " +
				"authority lock, before it could empty the authority tables")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the reset never blocked on the authority lock — it is free to empty " +
		"user_roles, user_capability_grants and user_capability_revokes underneath a " +
		"batch that has already resolved its verdict from them")
}

func TestSeedReset_WaitsForAnInFlightAuthorityReader(t *testing.T) {
	pool := openResetAdminPool(t)
	ctx := t.Context()

	if !atrest.Initialised() {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}
		if err := atrest.InitWithKey(key); err != nil {
			t.Fatalf("atrest: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := bootstrap.Config{
		ScrambleKey:         "reset-authority-lock-test-key",
		AdminPath:           t.TempDir(),
		DefaultAdminEnabled: true,
	}

	// THE READER. It takes the shared authority lock exactly as a batch
	// apply does, expressed as raw SQL so nothing here depends on a
	// symbol the pre-correction tree lacks.
	readerTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	readerReleased := false
	defer func() {
		if !readerReleased {
			_ = readerTx.Rollback(context.Background())
		}
	}()
	if _, err := readerTx.Exec(ctx,
		`SELECT pg_advisory_xact_lock_shared($1::INT, $2::INT)`,
		authorityLockSpace, authorityStructural); err != nil {
		t.Fatalf("reader lock: %v", err)
	}

	// The reset runs on its OWN pool with a distinctive application_name,
	// so the wait observation cannot be confused by anything else sharing
	// this database.
	appName := fmt.Sprintf("aa-resetlock-%d", time.Now().UnixNano())
	resetPool := openResetAdminPoolNamed(t, appName)

	done := make(chan error, 1)
	go func() {
		done <- resetContent(context.Background(), resetPool, cfg, logger)
	}()

	waitForBlockedReset(t, pool, appName)

	// Release the reader; the reset may now proceed.
	readerReleased = true
	if err := readerTx.Rollback(context.Background()); err != nil {
		t.Fatalf("reader release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reset after the reader released: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the reset never completed after the reader released")
	}
}
