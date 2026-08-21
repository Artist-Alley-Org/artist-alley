package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the subset of *pgxpool.Pool that teardown needs. Taking an
// interface keeps this package free of a pool dependency and lets a
// test pass a pool, a conn or a tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Purge runs teardown statements and REPORTS every failure instead of
// discarding it.
//
// WHY (#870)
// ----------
// The convention these tests used was:
//
//	_, _ = pool.Exec(cleanCtx, `DELETE FROM ... WHERE user_ref = $1`, ref)
//
// with a comment calling it "best-effort". The `_, _ =` is the defect.
// Every one of those deletes had been failing — the helper closed the
// pool on a function `defer` while the deletes ran from a `t.Cleanup`,
// which fires later — and because the error went to `_`, the suite
// stayed green for as long as the leak existed. A teardown that cannot
// fail out loud is indistinguishable from a teardown that works, and
// the database is the only place the difference shows up.
//
// So Purge is deliberately NOT best-effort. It uses tb.Errorf: the
// statements still all run (one bad table should not strand the rest),
// but a failure marks the test. If teardown cannot do its job the run
// is not clean, and saying so is the entire point.
//
// It runs on context.Background() rather than the test's context,
// because by cleanup time t.Context() is already cancelled.
func Purge(tb testing.TB, db Execer, arg any, stmts ...string) {
	tb.Helper()
	ctx := context.Background()
	for _, sql := range stmts {
		if _, err := db.Exec(ctx, sql, arg); err != nil {
			tb.Errorf("teardown failed, the run has leaked rows: %s: %v", sql, err)
		}
	}
}
