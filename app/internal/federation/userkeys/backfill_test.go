// Tests for the Phase 1.22.I-b boot-time keypair backfill safety
// net. Real Postgres + atrest (skips without AA_DB_PASSWORD).
//
// Three cases pin the contract:
//
//   1. Happy steady state — no users without keys → sweep
//      returns zero stats, no audit fires, no log noise.
//   2. The real production case — N approved users exist with
//      no federation_user_keys row → sweep mints one keypair
//      per user, returns (N, N, 0), audit fires N times.
//   3. Approval gating — non-approved users (approved=0 or 2)
//      are EXCLUDED from the sweep so the master key + DB
//      cycles don't get spent on accounts that may never
//      federate.

package userkeys_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// keylessUserAtState INSERTs a fresh user with the given approval
// state (no federation_user_keys row) so the sweep has something
// to find / skip. Returns the user_ref + registers cleanup.
func keylessUserAtState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, approved int) int64 {
	t.Helper()
	username := "backfill-test-" + randHex(t, 4)
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved)
		 VALUES ($1, 'Backfill Test', $2)
		 RETURNING ref`,
		username, approved,
	).Scan(&ref); err != nil {
		t.Fatalf("insert keyless user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_user_keys WHERE user_id = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// auditSpy captures every AuditFireFn call so the tests can
// assert "fired once per mint" without writing to audit_events.
// Concurrent-safe in case BackfillMissingKeys grows parallel
// minting in a later version.
type auditSpy struct {
	mu    sync.Mutex
	calls []auditSpyCall
}

type auditSpyCall struct {
	subjectUserRef int64
	version        int32
	algorithm      string
}

func (s *auditSpy) hook(_ context.Context, subjectUserRef int64, version int32, algorithm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, auditSpyCall{subjectUserRef, version, algorithm})
}

func (s *auditSpy) calledFor(userRef int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.subjectUserRef == userRef {
			n++
		}
	}
	return n
}

// --- 1. happy steady state ---

func TestBackfillMissingKeys_HappyPath_NoKeylessUsers_NoOpAndQuiet(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Seed a user who DOES have a current key — sweep should
	// skip them.
	ref := fixtureUser(t, ctx, pool)
	if _, err := userkeys.EnsureCurrentForUser(ctx, userkeys.New(pool), ref); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	spy := &auditSpy{}

	stats, err := userkeys.BackfillMissingKeys(ctx, pool, nil, spy.hook)
	if err != nil {
		t.Fatalf("BackfillMissingKeys: %v", err)
	}

	if spy.calledFor(ref) != 0 {
		t.Errorf("user with existing key should NOT trigger audit; spy fired %d times for ref=%d",
			spy.calledFor(ref), ref)
	}
	// stats.KeysGenerated for OUR user should be 0; other test
	// fixtures' keyless users may bump it, so we don't assert
	// on the absolute value — only on the audit-spy invariant
	// scoped to OUR user_ref.
	_ = stats
}

// --- 2. real production case ---

func TestBackfillMissingKeys_ApprovedKeylessUser_MintsAndAudits(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	ref := keylessUserAtState(t, ctx, pool, 1) // approved=1
	spy := &auditSpy{}

	stats, err := userkeys.BackfillMissingKeys(ctx, pool, nil, spy.hook)
	if err != nil {
		t.Fatalf("BackfillMissingKeys: %v", err)
	}

	// Verify the user now has a current key.
	got, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
	if err != nil {
		t.Fatalf("GetCurrentUserKey after backfill: %v", err)
	}
	if got.Version != 1 || !got.IsCurrent {
		t.Errorf("post-backfill key shape: %+v want version=1 is_current=true", got)
	}
	if got.Algorithm != userkeys.Algorithm {
		t.Errorf("algorithm = %q, want %q", got.Algorithm, userkeys.Algorithm)
	}

	if spy.calledFor(ref) != 1 {
		t.Errorf("audit spy should have fired EXACTLY once for ref=%d; got %d calls",
			ref, spy.calledFor(ref))
	}

	// stats should reflect at least 1 mint (other concurrent
	// test fixtures may bump it higher).
	if stats.KeysGenerated < 1 {
		t.Errorf("stats.KeysGenerated = %d, want >= 1 (our mint should count)", stats.KeysGenerated)
	}
	if stats.Errors != 0 {
		t.Errorf("stats.Errors = %d, want 0 (no per-user failures expected)", stats.Errors)
	}
}

// --- 3. approval gating ---

func TestBackfillMissingKeys_UnapprovedUser_Skipped(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Two unapproved users — approved=0 (pending) and approved=2
	// (disabled). Both should be SKIPPED by the sweep.
	pendingRef := keylessUserAtState(t, ctx, pool, 0)
	disabledRef := keylessUserAtState(t, ctx, pool, 2)
	spy := &auditSpy{}

	_, err := userkeys.BackfillMissingKeys(ctx, pool, nil, spy.hook)
	if err != nil {
		t.Fatalf("BackfillMissingKeys: %v", err)
	}

	// Neither user should have been minted for.
	if spy.calledFor(pendingRef) != 0 {
		t.Errorf("pending user (approved=0) should be SKIPPED; spy fired for ref=%d", pendingRef)
	}
	if spy.calledFor(disabledRef) != 0 {
		t.Errorf("disabled user (approved=2) should be SKIPPED; spy fired for ref=%d", disabledRef)
	}

	// Confirm the table state — neither has a federation_user_keys row.
	for _, ref := range []int64{pendingRef, disabledRef} {
		_, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
		if err == nil {
			t.Errorf("ref=%d has a current key but should have been skipped", ref)
		}
	}
}

// --- 4. idempotency ---

// TestBackfillMissingKeys_Idempotent_SecondCallIsNoOp proves the
// safety net is safe to invoke on every boot — repeated calls
// after a successful mint don't double-mint or double-audit.
func TestBackfillMissingKeys_Idempotent_SecondCallIsNoOp(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	ref := keylessUserAtState(t, ctx, pool, 1)
	spy := &auditSpy{}

	// First call mints.
	if _, err := userkeys.BackfillMissingKeys(ctx, pool, nil, spy.hook); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstCalls := spy.calledFor(ref)

	// Second call should be a complete no-op for our user.
	if _, err := userkeys.BackfillMissingKeys(ctx, pool, nil, spy.hook); err != nil {
		t.Fatalf("second call: %v", err)
	}
	secondCalls := spy.calledFor(ref)

	if firstCalls != 1 {
		t.Errorf("first call fired audit %d times for ref=%d; want 1", firstCalls, ref)
	}
	if secondCalls != firstCalls {
		t.Errorf("second call should NOT re-fire audit; before=%d after=%d (delta=%d)",
			firstCalls, secondCalls, secondCalls-firstCalls)
	}
}
