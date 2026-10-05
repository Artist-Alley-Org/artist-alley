// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package digest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ---- DueCadences (pure) ----

func TestDueCadences(t *testing.T) {
	cfg := Config{DailyHourUTC: 8, WeeklyDay: time.Monday, WeeklyHourUTC: 8}
	monday := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC) // a Monday at 08:00
	tuesday9 := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		now  time.Time
		want []string
	}{
		{"monday 8am → all three", monday, []string{"hourly", "daily", "weekly"}},
		{"tuesday 9am → hourly only", tuesday9, []string{"hourly"}},
		{"tuesday 8am → hourly + daily", time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC), []string{"hourly", "daily"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DueCadences(tc.now, cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// ---- RunOnce (DB + capture sender) ----

func openTestPool(t *testing.T) *pgxpool.Pool {
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

// seedUserWithHourlyDigest inserts a user (with email) + n notification
// rows + a matching hourly digest_queue row each. Returns the user ref.
func seedUserWithHourlyDigest(t *testing.T, pool *pgxpool.Pool, n int) int64 {
	t.Helper()
	ctx := context.Background()
	var ref int64
	suffix := time.Now().UnixNano()
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, email, approved) VALUES ($1, $2, $3, 1) RETURNING ref`,
		"digesttest"+itoa(suffix), "Digest Test", "digest+"+itoa(suffix)+"@example.test",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM digest_queue WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	for i := 0; i < n; i++ {
		var nid string
		if err := pool.QueryRow(ctx,
			`INSERT INTO notifications (recipient_user_ref, verb, target_kind, target_id, payload)
			 VALUES ($1, 'mention_of_me', 'post', gen_random_uuid()::text, '{}'::jsonb) RETURNING id`,
			ref,
		).Scan(&nid); err != nil {
			t.Fatalf("insert notification: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO digest_queue (user_ref, topic, cadence, notification_id)
			 VALUES ($1, 'mention_of_me', 'hourly', $2)`,
			ref, nid,
		); err != nil {
			t.Fatalf("insert digest_queue: %v", err)
		}
	}
	return ref
}

func newCoordinator(pool *pgxpool.Pool, sender email.Sender) *Coordinator {
	return &Coordinator{
		Pool:        pool,
		Sender:      sender,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		ScrambleKey: "test-key",
		SiteFn:      func(context.Context) SiteContext { return SiteContext{Name: "Test", URL: "https://test.example"} },
		CfgFn:       func(context.Context) Config { return Config{DailyHourUTC: 8, WeeklyDay: 1, WeeklyHourUTC: 8} },
		Now:         func() time.Time { return time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC) }, // Tue 9am → hourly only
	}
}

func TestRunOnce_BatchesPerUser_SendsOneEmail(t *testing.T) {
	pool := openTestPool(t)
	uA := seedUserWithHourlyDigest(t, pool, 3)
	uB := seedUserWithHourlyDigest(t, pool, 2)

	cap := &email.Capture{}
	c := newCoordinator(pool, cap)
	now := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)

	sent, err := c.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2 (one batched email per user)", sent)
	}
	msgs := cap.All()
	if len(msgs) != 2 {
		t.Fatalf("captured %d emails, want 2", len(msgs))
	}
	// Each email carries the RFC 8058 headers.
	for _, m := range msgs {
		if m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
			t.Fatalf("missing one-click header on digest email: %+v", m.Headers)
		}
	}
	// Rows for both users are marked sent → a second run sends nothing.
	sent2, err := c.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if sent2 != 0 {
		t.Fatalf("second run sent = %d, want 0 (idempotent — rows already consumed)", sent2)
	}
	_ = uA
	_ = uB
}

func TestRunOnce_EmptyQueue_NoOp(t *testing.T) {
	pool := openTestPool(t)
	cap := &email.Capture{}
	c := newCoordinator(pool, cap)
	// A cadence set with no rows.
	sent, err := c.RunOnce(context.Background(), time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sent = %d on empty queue, want 0", sent)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
