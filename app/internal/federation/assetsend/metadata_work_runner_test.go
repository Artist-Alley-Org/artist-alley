// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 against a live database: the local metadata work runner
// (metadata_work_runner.go). Waits observe database state with a bounded
// deadline; nothing here depends on a guessed duration being enough.
// Skips without AA_DB_PASSWORD.

package assetsend_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

// syncBuffer is a log sink safe for the runner's goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// started runs r until the returned stop is called; stop waits for Run
// to return, failing the test if it does not promptly.
func started(t *testing.T, r *assetsend.MetadataWorkRunner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the runner did not return after its context ended")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (f *fx) ledgerLen() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_metadata_work`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// On start the runner processes pending work at once, a bounded burst at
// a time: after a burst it yields. Cancelled, it returns and holds no
// connection; a new runner resumes the rest.
func TestMetadataWorkRunner_StartsBurstsStopsAndResumes(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	f.clearWork(t)
	var assets []asset
	for range 5 {
		a, _ := f.eligible("Title", "", "", "")
		assets = append(assets, a)
	}
	f.dropFixtureWork()
	for _, a := range assets {
		f.enqueue(a)
	}
	pool := f.poolWith(nil, nil)
	logs := &syncBuffer{}
	runner := func() *assetsend.MetadataWorkRunner {
		return &assetsend.MetadataWorkRunner{
			Work:   assetsend.MetadataWork{Pool: pool, Writer: f.writer},
			Origin: func(context.Context) string { return base },
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
			Burst:  2, Page: 2, Idle: time.Hour, Yield: time.Hour, Backoff: time.Hour,
		}
	}
	stop := started(t, runner())
	waitFor(t, "the first burst", 10*time.Second, func() bool { return f.ledgerLen() == 3 })
	// The burst is over and the runner yields: no third page is taken.
	time.Sleep(300 * time.Millisecond)
	if n := f.ledgerLen(); n != 3 {
		t.Fatalf("after one burst of 2 the ledger holds %d rows, want 3", n)
	}
	stop()
	waitFor(t, "the runner's connections released", 5*time.Second, func() bool { return pool.Stat().AcquiredConns() == 0 })

	stop = started(t, runner())
	waitFor(t, "the resumed burst", 10*time.Second, func() bool { return f.ledgerLen() == 1 })
	stop()
	stop = started(t, runner())
	waitFor(t, "the last row", 10*time.Second, func() bool { return f.ledgerLen() == 0 })
	stop()
	for _, a := range assets {
		if got := f.metadataUpdates(a); len(got) != 1 {
			t.Fatalf("asset %s: %d Updates", a.id, len(got))
		}
	}
	if s := logs.String(); s != "" {
		t.Fatalf("a healthy run logged %q", s)
	}
}

// A failing page is retried with a backoff, not in a hot loop, and is
// logged as a constant event: no error text, no value. Its work stays.
func TestMetadataWorkRunner_BacksOffAndLogsNoValues(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)
	const private = "a-distinctive-private-title-7d31"
	a, _ := f.eligible(private, private, "", "")
	f.dropFixtureWork()
	f.enqueue(a)

	// The owner read waits on a held user table and times out each time.
	short := f.poolWith(nil, map[string]string{"lock_timeout": "50ms"})
	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx) //nolint:errcheck // a no-op once released
	if _, err := hold.Exec(ctx, `LOCK TABLE "user" IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	stop := started(t, &assetsend.MetadataWorkRunner{
		Work:   assetsend.MetadataWork{Pool: short, Writer: f.writer},
		Origin: func(context.Context) string { return base },
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Burst:  8, Page: 2, Idle: time.Hour, Yield: time.Hour, Backoff: 200 * time.Millisecond,
	})
	failures := func() int { return strings.Count(logs.String(), "metadata_work.page_failed") }
	waitFor(t, "two failed pages", 10*time.Second, func() bool { return failures() >= 2 })
	start := time.Now()
	waitFor(t, "a third failed page", 10*time.Second, func() bool { return failures() >= 3 })
	// Attempts 2 and 3 are 200ms and 400ms apart: the third is at least
	// the doubled backoff after the second.
	if gap := time.Since(start); gap < 300*time.Millisecond {
		t.Fatalf("the third attempt came %v after the second: no backoff", gap)
	}
	stop()
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.metadataUpdates(a); len(got) != 0 {
		t.Fatalf("%d Updates from failed pages", len(got))
	}
	if l := f.ledger(); len(l) != 1 || l[0].peerCursor != nil {
		t.Fatalf("ledger %+v", l)
	}
	s := logs.String()
	if strings.Contains(s, private) || strings.Contains(s, "lock") || strings.Contains(s, "err") {
		t.Fatalf("the log carries more than its events: %q", s)
	}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if !strings.HasSuffix(line, "msg=metadata_work.page_failed") {
			t.Fatalf("unexpected log line %q", line)
		}
	}
}

// With no usable origin nothing is processed and the work stays; once the
// origin is set the work is recorded on it, and a later change of origin
// is used for the next page.
func TestMetadataWorkRunner_OriginIsReadForEveryPage(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	b, _ := f.eligible("Other", "", "", "")
	f.dropFixtureWork()
	f.enqueue(a)
	var origin atomic.Value
	origin.Store("")
	logs := &syncBuffer{}
	var pool *pgxpool.Pool = f.poolWith(nil, nil)
	stop := started(t, &assetsend.MetadataWorkRunner{
		Work:   assetsend.MetadataWork{Pool: pool, Writer: f.writer},
		Origin: func(context.Context) string { return origin.Load().(string) },
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Burst:  8, Page: 2, Idle: 20 * time.Millisecond, Yield: 20 * time.Millisecond, Backoff: 20 * time.Millisecond,
	})
	waitFor(t, "the unavailable origin logged", 10*time.Second, func() bool {
		return strings.Contains(logs.String(), "metadata_work.origin_unavailable")
	})
	for _, bad := range []string{"not a url", "ftp://x.example", "https://"} {
		origin.Store(bad)
		time.Sleep(100 * time.Millisecond)
		if n := f.ledgerLen(); n != 1 || len(f.metadataUpdates(a)) != 0 {
			t.Fatalf("origin %q: %d rows, %d Updates", bad, n, len(f.metadataUpdates(a)))
		}
	}
	origin.Store(base)
	waitFor(t, "the work recorded", 10*time.Second, func() bool { return len(f.metadataUpdates(a)) == 1 })
	if actor := f.actorOf(f.metadataUpdates(a)[0].id); !strings.HasPrefix(actor, base+"/users/") {
		t.Fatalf("actor %q", actor)
	}
	const moved = "https://moved.example"
	origin.Store(moved)
	f.enqueue(b)
	waitFor(t, "the next page on the new origin", 10*time.Second, func() bool { return len(f.metadataUpdates(b)) == 1 })
	if actor := f.actorOf(f.metadataUpdates(b)[0].id); !strings.HasPrefix(actor, moved+"/users/") {
		t.Fatalf("actor %q, want one on %s", actor, moved)
	}
	stop()
	if !strings.Contains(logs.String(), "metadata_work.origin_available") {
		t.Fatalf("logs %q", logs.String())
	}
	_, _ = f.pool.Exec(context.Background(), `DELETE FROM activities WHERE actor_uri LIKE $1`, moved+"/%")
}
