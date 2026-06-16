// Tests for the Phase 1.22.I-h retained-key sweeper. Real Postgres
// + atrest; skips without AA_DB_PASSWORD.
//
// Four cases pin the contract:
//
//   1. Reaps rows whose retained_until is in the past.
//   2. Preserves rows whose retained_until is in the future
//      (rotation grace window still open).
//   3. Preserves rows where is_current=TRUE (current keys are
//      never reapable — defense in depth against a row that
//      somehow has both is_current=TRUE and retained_until set,
//      which the CHECK constraint prevents but the sweeper
//      query enforces independently).
//   4. Zero-count sweep doesn't fire the audit hook (steady-state
//      silence).

package userkeys_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// seedExpiredRetained inserts a fresh user + a retained row with
// the requested retained_until offset from NOW(). offsetSec < 0
// means "already expired"; > 0 means "still in grace window."
//
// Uses raw SQL to bypass the [DemoteCurrentKey] path so the test
// can set arbitrary retained_until values without time-warping
// the transaction. Cleanup deletes the user (CASCADE drops the
// retained row).
func seedExpiredRetained(t *testing.T, ctx context.Context, pool *pgxpool.Pool, offset time.Duration) int64 {
	t.Helper()
	ref := fixtureUser(t, ctx, pool)
	// Insert a retained row directly. is_current=false +
	// retained_until=NOW()+offset; the CHECK constraint allows
	// this exact combination.
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i) // dummy; sweeper doesn't validate the bytes
	}
	priv := []byte("nonempty>=13bytes-padding") // CHECK >= 13 bytes
	if _, err := pool.Exec(ctx,
		`INSERT INTO federation_user_keys (
			user_ref, version, algorithm, public_key, private_key_enc,
			is_current, retained_until
		) VALUES ($1, 1, 'naclbox-x25519-v1', $2, $3, FALSE, NOW() + $4::interval)`,
		ref, pub, priv, offset.String(),
	); err != nil {
		t.Fatalf("seed retained row: %v", err)
	}
	return ref
}

// --- 1. reaps expired ---

func TestSweeper_ReapsExpiredRetainedRows(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Three expired retained rows from three different users so
	// the sweeper has a non-trivial set to reap.
	refs := []int64{
		seedExpiredRetained(t, ctx, pool, -1*time.Hour),
		seedExpiredRetained(t, ctx, pool, -24*time.Hour),
		seedExpiredRetained(t, ctx, pool, -30*24*time.Hour),
	}

	sw := userkeys.NewSweeper(pool, nil, nil, 0)
	count, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if count < int64(len(refs)) {
		t.Errorf("sweeper reaped %d rows; expected at least %d (our three expired)",
			count, len(refs))
	}

	// Confirm every fixture row is gone.
	for _, ref := range refs {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM federation_user_keys WHERE user_ref = $1`,
			ref,
		).Scan(&n); err != nil {
			t.Fatalf("count rows for ref=%d: %v", ref, err)
		}
		if n != 0 {
			t.Errorf("ref=%d still has %d federation_user_keys rows after sweep", ref, n)
		}
	}
}

// --- 2. preserves non-expired ---

func TestSweeper_PreservesNonExpiredRetainedRows(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// A retained row with retained_until 30 days OUT — must survive
	// the sweep.
	ref := seedExpiredRetained(t, ctx, pool, 30*24*time.Hour)

	sw := userkeys.NewSweeper(pool, nil, nil, 0)
	if _, err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM federation_user_keys WHERE user_ref = $1`,
		ref,
	).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Errorf("ref=%d had %d rows after sweep; want 1 (non-expired retained preserved)", ref, n)
	}
}

// --- 3. preserves current ---

func TestSweeper_PreservesCurrentKey(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Standard backfill mints a v1 current row with NO retained_until.
	// The sweeper query has WHERE is_current = FALSE so the current
	// row can't even enter the DELETE filter — but the test pins the
	// behavior end to end.
	ref, _ := userWithKey(t, ctx, pool)

	sw := userkeys.NewSweeper(pool, nil, nil, 0)
	if _, err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := userkeys.New(pool).GetCurrentUserKey(ctx, ref)
	if err != nil {
		t.Fatalf("current key gone after sweep — sweeper reaped a current row: %v", err)
	}
	if !got.IsCurrent || got.Version != 1 {
		t.Errorf("post-sweep current = %+v, want v=1 is_current=true", got)
	}
}

// --- 4. zero-count sweep doesn't audit ---

func TestSweeper_ZeroReapsDoesNotFireAudit(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	// No expired retained rows for our fixture user — sweep
	// against our user_ref only via a pre-clean step.
	var auditCount atomic.Int64
	hook := func(_ context.Context, count int64) {
		auditCount.Add(count)
	}

	// First, reap any leftover expired rows from concurrent test
	// runs so our zero-state assertion isn't poisoned.
	prep := userkeys.NewSweeper(pool, nil, nil, 0)
	_, _ = prep.SweepOnce(ctx)
	auditCount.Store(0)

	sw := userkeys.NewSweeper(pool, nil, hook, 0)
	count, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if count != 0 {
		t.Errorf("post-prep sweep reaped %d rows; expected 0 (steady state)", count)
	}
	if auditCount.Load() != 0 {
		t.Errorf("zero-reap sweep fired audit hook with count=%d; expected NO audit on quiet steady state",
			auditCount.Load())
	}
}
