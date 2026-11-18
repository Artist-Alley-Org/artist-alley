// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// #1605: database capacity beside the long-lived LISTEN connections.

// lazyPool is a pool that has never connected: pgxpool dials only on
// first use, so New's capacity check can be exercised without a
// database, and a refusal proves New touched nothing.
func lazyPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("host=capacity-check.invalid port=5432 user=x password=x dbname=x sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCheckDBCapacity(t *testing.T) {
	if MinDBMaxConns != 8 {
		t.Fatalf("MinDBMaxConns = %d; the rule is 3 pool-held listeners + 5 query slots", MinDBMaxConns)
	}
	for _, n := range []int32{1, 4, 7} {
		err := checkDBCapacity(lazyPool(t, n))
		if err == nil || !strings.Contains(err.Error(), "AA_DB_MAX_CONNS") || !strings.Contains(err.Error(), "at least 8") {
			t.Errorf("max %d: %v, want a refusal naming AA_DB_MAX_CONNS and the minimum 8", n, err)
		}
	}
	for _, n := range []int32{8, 20} {
		if err := checkDBCapacity(lazyPool(t, n)); err != nil {
			t.Errorf("max %d refused: %v", n, err)
		}
	}
}

// An undersized pool fails New at once, before any connection is made:
// no listener acquired, nothing served, no hang.
func TestNew_UndersizedPoolFailsPromptlyBeforeAnyConnection(t *testing.T) {
	pool := lazyPool(t, 4)
	cfg := config.Config{StorageBackend: "fs", StorageFSRoot: t.TempDir(), HTTPAddr: "127.0.0.1:0"}
	start := time.Now()
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pool, "test")
	if err == nil {
		s.Close()
		t.Fatal("New accepted a 4-connection pool")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refusal took %s", took)
	}
	if !strings.Contains(err.Error(), "AA_DB_MAX_CONNS") {
		t.Fatalf("error does not name AA_DB_MAX_CONNS: %v", err)
	}
	if st := pool.Stat(); st.TotalConns() != 0 || st.AcquireCount() != 0 {
		t.Fatalf("New used the pool before refusing it: total %d, acquires %d", st.TotalConns(), st.AcquireCount())
	}
}

// openServerPool opens a pool as production does: db.Open's settings
// sized by config.Load (AA_DB_MAX_CONNS, default 20), with a unique
// application_name so a test can find this pool's backends.
func openServerPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	prod, _ := config.Load() // pool settings only; Load's secret checks do not apply
	cfg, err := pgxpool.ParseConfig("host=" + envOrSens("AA_DB_HOST", "postgres") + " port=" + envOrSens("AA_DB_PORT", "5432") +
		" user=" + envOrSens("AA_DB_USER", "artist_alley") + " password=" + pwd + " dbname=" + testdb.Name(t) + " sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns, cfg.MinConns, cfg.MaxConnLifetime = prod.DBMaxConns, prod.DBMinConns, prod.DBConnMaxLifetime
	app := "aa-capacity-" + uuid.NewString()[:8]
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool, app
}

// listeners counts this application's backends whose last statement
// was a LISTEN, seen from a separate connection.
func listeners(t *testing.T, observer *pgxpool.Pool, app string) int {
	t.Helper()
	var n int
	if err := observer.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND query LIKE 'LISTEN %'`, app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitListeners(t *testing.T, observer *pgxpool.Pool, app string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := listeners(t, observer, app)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d LISTEN connections for %s, want %d", got, app, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Close releases the cache registry's listener: it is bound to the
// server's lifecycle, not to context.Background().
func TestServerClose_ReleasesTheCacheListener(t *testing.T) {
	pool, app := openServerPool(t)
	observer := openPoolForSensitivity(t)
	t.Cleanup(observer.Close)
	initAtrestForOutboxWiring(t)
	s, err := New(config.Config{StorageBackend: "fs", StorageFSRoot: t.TempDir(), HTTPAddr: "127.0.0.1:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), pool, "test")
	if err != nil {
		t.Fatal(err)
	}
	waitCacheListener(t, observer, app, 1)
	if st := pool.Stat(); st.AcquiredConns() != 0 {
		t.Fatalf("the cache listener holds a pool connection (%d acquired); it should be hijacked out of the pool", st.AcquiredConns())
	}
	s.Close()
	waitCacheListener(t, observer, app, 0)
}

// Run holds all four long-lived listeners (three pool-held, one
// hijacked) and releases every one of them on shutdown; the pool still
// has its query slots while they run.
func TestServerRun_HoldsAndReleasesEveryListener(t *testing.T) {
	pool, app := openServerPool(t)
	observer := openPoolForSensitivity(t)
	t.Cleanup(observer.Close)
	initAtrestForOutboxWiring(t)
	s, err := New(config.Config{StorageBackend: "fs", StorageFSRoot: t.TempDir(), HTTPAddr: "127.0.0.1:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), pool, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitListeners(t, observer, app, 1+poolHeldListeners)
	st := pool.Stat()
	if free := st.MaxConns() - st.AcquiredConns(); free < minDBQuerySlots {
		t.Fatalf("only %d of %d pool connections free while the listeners run", free, st.MaxConns())
	}
	// Ordinary queries still get a connection at once.
	qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
	for i := 0; i < minDBQuerySlots; i++ {
		var one int
		if err := pool.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("query %d with the listeners running: %v", i, err)
		}
	}
	qcancel()
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop")
	}
	// The three federation listeners give their connections back to the
	// pool (none is held any more) and the cache listener's own
	// connection, outside the pool, is closed.
	deadline := time.Now().Add(15 * time.Second)
	for pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d pool connections still held after shutdown", pool.Stat().AcquiredConns())
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitCacheListener(t, observer, app, 0)
}

// waitCacheListener waits for this application's cache_invalidate
// listener count (the hijacked connection, outside the pool).
func waitCacheListener(t *testing.T, observer *pgxpool.Pool, app string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := observer.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND query = 'LISTEN cache_invalidate'`, app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d cache listeners for %s, want %d", n, app, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Run's error path releases every listener too (#1605 review): when
// ListenAndServe fails (here, the address is already bound), Run
// returns, and neither the cache listener (bound to the server's
// lifecycle) nor the three pool-held federation listeners (bound to the
// context Run gives its workers) may stay connected. The caller's
// context is NOT cancelled by the test, so only Run's own exit can end
// them.
func TestServerRun_ListenErrorReleasesEveryListener(t *testing.T) {
	pool, app := openServerPool(t)
	observer := openPoolForSensitivity(t)
	t.Cleanup(observer.Close)
	initAtrestForOutboxWiring(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	s, err := New(config.Config{StorageBackend: "fs", StorageFSRoot: t.TempDir(), HTTPAddr: busy.Addr().String()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), pool, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	waitCacheListener(t, observer, app, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // only after the assertions
	var runErr error
	done := make(chan struct{})
	go func() { runErr = s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return on a bind failure")
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "address already in use") {
		t.Fatalf("Run returned %v, want the bind failure", runErr)
	}
	if ctx.Err() != nil {
		t.Fatal("Run cancelled the caller's context")
	}
	// The cache listener's connection closes, and no pool connection is
	// held, and none is taken again by a worker that outlived Run
	// (released listener connections may sit idle in the pool with LISTEN
	// as their last statement; only a HELD connection is a leak).
	waitCacheListener(t, observer, app, 0)
	// Run cancels runCtx on return but does not wait for the goroutines
	// it started, so their pool connections are released moments after
	// Run returns (hosted CI saw one still held at the first sample).
	// Allow a short bounded drain; a connection still held past it is a
	// leak, and the sustained samples below catch one taken again.
	drain := time.Now().Add(5 * time.Second)
	for pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(drain) {
			t.Fatalf("%d pool connections still held 5s after Run returned", pool.Stat().AcquiredConns())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 15; i++ {
		if n := pool.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("%d pool connections held after Run returned (sample %d)", n, i)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
