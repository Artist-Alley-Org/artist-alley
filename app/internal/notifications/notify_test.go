// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package notifications

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// The Writer cadence fork (Phase 1.55.Y): immediate → enqueue the email
// job now; hourly/daily/weekly → insert a digest_queue row instead;
// email-off → no email at all. The in-app notification is written
// independently and always fires when in_app is on.

func digestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") + " port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") + " dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	uniq := time.Now().UnixNano()
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		"cadence"+itoa(uniq), "Cadence Test",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM digest_queue WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// fakePrefs returns a fixed channel list + cadence for every verb.
type fakePrefs struct {
	channels []string
	cadence  string
}

func (f fakePrefs) ChannelsFor(_ context.Context, _ int64, _ string) ([]string, error) {
	return f.channels, nil
}
func (f fakePrefs) CadenceFor(_ context.Context, _ int64, _ string) (string, error) {
	return f.cadence, nil
}

// fakeJobs captures enqueue calls.
type fakeJobs struct{ enqueued []string }

func (f *fakeJobs) Enqueue(_ context.Context, kind string, _ []byte) error {
	f.enqueued = append(f.enqueued, kind)
	return nil
}

func countDigestRows(t *testing.T, pool *pgxpool.Pool, ref int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM digest_queue WHERE user_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatalf("count digest_queue: %v", err)
	}
	return n
}

func countNotifs(t *testing.T, pool *pgxpool.Pool, ref int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE recipient_user_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

func newWriterWith(pool *pgxpool.Pool, prefs prefsResolver, jobs *fakeJobs) *Writer {
	return NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, prefs, jobs, nil)
}

func TestWriter_ImmediateCadence_EnqueuesNow(t *testing.T) {
	pool := digestTestPool(t)
	ref := seedUser(t, pool)
	jobs := &fakeJobs{}
	w := newWriterWith(pool, fakePrefs{channels: []string{"in_app", "email"}, cadence: "immediate"}, jobs)

	if err := w.Notify(context.Background(), Input{RecipientUserRef: ref, Verb: "mention_of_me"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(jobs.enqueued) != 1 || jobs.enqueued[0] != "notification.email" {
		t.Fatalf("expected one notification.email enqueue, got %v", jobs.enqueued)
	}
	if got := countDigestRows(t, pool, ref); got != 0 {
		t.Fatalf("immediate cadence should NOT queue a digest row, got %d", got)
	}
	if got := countNotifs(t, pool, ref); got != 1 {
		t.Fatalf("in-app notification should be written, got %d", got)
	}
}

func TestWriter_DailyCadence_QueuesRow_NoImmediateEnqueue(t *testing.T) {
	pool := digestTestPool(t)
	ref := seedUser(t, pool)
	jobs := &fakeJobs{}
	w := newWriterWith(pool, fakePrefs{channels: []string{"in_app", "email"}, cadence: "daily"}, jobs)

	if err := w.Notify(context.Background(), Input{RecipientUserRef: ref, Verb: "mention_of_me"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(jobs.enqueued) != 0 {
		t.Fatalf("daily cadence must NOT enqueue an immediate email, got %v", jobs.enqueued)
	}
	if got := countDigestRows(t, pool, ref); got != 1 {
		t.Fatalf("daily cadence should queue exactly one digest row, got %d", got)
	}
	if got := countNotifs(t, pool, ref); got != 1 {
		t.Fatalf("in-app notification should still be written, got %d", got)
	}
}

func TestWriter_EmailOff_NoEmail_InAppStillWritten(t *testing.T) {
	pool := digestTestPool(t)
	ref := seedUser(t, pool)
	jobs := &fakeJobs{}
	// "email" not in the channel list = email off for this topic.
	w := newWriterWith(pool, fakePrefs{channels: []string{"in_app"}, cadence: "daily"}, jobs)

	if err := w.Notify(context.Background(), Input{RecipientUserRef: ref, Verb: "mention_of_me"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(jobs.enqueued) != 0 {
		t.Fatalf("email-off must not enqueue, got %v", jobs.enqueued)
	}
	if got := countDigestRows(t, pool, ref); got != 0 {
		t.Fatalf("email-off must not queue a digest, got %d", got)
	}
	if got := countNotifs(t, pool, ref); got != 1 {
		t.Fatalf("in-app notification must still fire regardless of email pref, got %d", got)
	}
}

// itoa keeps usernames short + unique without pulling strconv.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
