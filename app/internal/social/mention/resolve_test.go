// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package mention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
)

// These tests exercise the DB-backed resolver + the service fire path.
// They need a live postgres (same convention as the rest of the social
// package: skip when AA_DB_PASSWORD is unset).

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOrDef("AA_DB_HOST", "postgres")
	port := envOrDef("AA_DB_PORT", "5432")
	user := envOrDef("AA_DB_USER", "artist_alley")
	name := envOrDef("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOrDef(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// insertUser creates a throwaway user and registers cleanup.
func insertUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	ctx := context.Background()
	var ref int64
	err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, username,
	).Scan(&ref)
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func newResolver(t *testing.T, pool *pgxpool.Pool) *Resolver {
	t.Helper()
	reg := cache.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewResolver(pool, reg)
}

func TestResolveLocal_KnownUser_ReturnsRef(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	ref := insertUser(t, pool, "mentiontest_alice_"+unique())
	r := newResolver(t, pool)

	got := r.ResolveLocal(context.Background(), []Mention{{Username: uname(t, pool, ref)}})
	if len(got) != 1 || got[0] != ref {
		t.Fatalf("got %v, want [%d]", got, ref)
	}
}

func TestResolveLocal_CaseInsensitive(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	name := "MentionTest_Bob_" + unique()
	ref := insertUser(t, pool, name)
	r := newResolver(t, pool)

	// Mention with different casing still resolves.
	got := r.ResolveLocal(context.Background(), []Mention{{Username: lowerOf(name)}})
	if len(got) != 1 || got[0] != ref {
		t.Fatalf("case-insensitive resolve got %v, want [%d]", got, ref)
	}
}

func TestResolveLocal_UnknownUser_Dropped(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	r := newResolver(t, pool)

	got := r.ResolveLocal(context.Background(), []Mention{{Username: "nobody_" + unique()}})
	if len(got) != 0 {
		t.Fatalf("unknown username should drop, got %v", got)
	}
}

func TestResolveLocal_SkipsFederated(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	ref := insertUser(t, pool, "mentiontest_carol_"+unique())
	r := newResolver(t, pool)

	// A federated mention (InstanceHost set) is ignored even if the
	// username matches a local user — v0.1.0 has no cross-instance
	// resolution.
	got := r.ResolveLocal(context.Background(), []Mention{
		{Username: uname(t, pool, ref), InstanceHost: "peer.test"},
	})
	if len(got) != 0 {
		t.Fatalf("federated mention should be skipped, got %v", got)
	}
}

func TestResolveLocal_CacheHit(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	name := "mentiontest_dave_" + unique()
	ref := insertUser(t, pool, name)
	r := newResolver(t, pool)

	// First resolve populates the cache.
	if got := r.ResolveLocal(context.Background(), []Mention{{Username: name}}); len(got) != 1 || got[0] != ref {
		t.Fatalf("first resolve got %v, want [%d]", got, ref)
	}
	// Delete the user out from under the resolver — a cache HIT must
	// still return the ref (proves the second call didn't re-query).
	if _, err := pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := r.ResolveLocal(context.Background(), []Mention{{Username: name}}); len(got) != 1 || got[0] != ref {
		t.Fatalf("cached resolve got %v, want [%d] (cache miss re-queried the deleted row)", got, ref)
	}
}

// --- service fire path ---

type captureNotifier struct {
	calls []capturedNotify
}

type capturedNotify struct {
	recipient  int64
	actor      *int64
	verb       string
	targetKind string
	targetID   string
}

func (c *captureNotifier) Notify(_ context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, _ map[string]any) error {
	c.calls = append(c.calls, capturedNotify{recipient, actor, verb, targetKind, targetID})
	return nil
}

func TestService_Process_FiresPerResolvedRef(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := insertUser(t, pool, "mentiontest_svc_a_"+unique())
	b := insertUser(t, pool, "mentiontest_svc_b_"+unique())
	an := uname(t, pool, a)
	bn := uname(t, pool, b)

	r := newResolver(t, pool)
	cap := &captureNotifier{}
	svc := NewService(r, cap, slog.New(slog.NewTextHandler(io.Discard, nil)))

	actorRef := int64(999999)
	body := "hey @" + an + " and @" + bn + " plus @ghost_" + unique() + " — look"
	svc.Process(context.Background(), actorRef, body, notifications.TargetKindPost, "post-uuid-123", map[string]any{"excerpt": "x"})

	if len(cap.calls) != 2 {
		t.Fatalf("expected 2 notifications (a+b, ghost dropped), got %d: %+v", len(cap.calls), cap.calls)
	}
	got := map[int64]bool{cap.calls[0].recipient: true, cap.calls[1].recipient: true}
	if !got[a] || !got[b] {
		t.Fatalf("expected notifications for %d + %d, got %+v", a, b, cap.calls)
	}
	for _, c := range cap.calls {
		if c.verb != notifications.VerbMentionOfMe {
			t.Fatalf("verb = %q, want %q", c.verb, notifications.VerbMentionOfMe)
		}
		if c.targetKind != notifications.TargetKindPost || c.targetID != "post-uuid-123" {
			t.Fatalf("target = %s/%s, want post/post-uuid-123", c.targetKind, c.targetID)
		}
		if c.actor == nil || *c.actor != actorRef {
			t.Fatalf("actor not threaded through (Writer needs it for self-mention gating): %+v", c.actor)
		}
	}
}

func TestService_Process_NoMentions_FiresNothing(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	r := newResolver(t, pool)
	cap := &captureNotifier{}
	svc := NewService(r, cap, slog.New(slog.NewTextHandler(io.Discard, nil)))

	svc.Process(context.Background(), 1, "a post body with no at-mentions at all", notifications.TargetKindPost, "p", nil)
	if len(cap.calls) != 0 {
		t.Fatalf("expected no notifications, got %d", len(cap.calls))
	}
}

// --- small helpers ---

func uname(t *testing.T, pool *pgxpool.Pool, ref int64) string {
	t.Helper()
	var u string
	if err := pool.QueryRow(context.Background(), `SELECT username FROM "user" WHERE ref = $1`, ref).Scan(&u); err != nil {
		t.Fatalf("read username: %v", err)
	}
	return u
}

func lowerOf(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// uniqueCounter seeds from the low bits of the clock so parallel runs
// don't collide, but stays SHORT — real usernames cap at 32 chars
// (auth.registerUsernamePattern), and the mention regex enforces that
// cap, so test usernames must fit too. base36 of a 30-bit seed is ≤ 6
// chars.
var uniqueCounter = time.Now().UnixNano() & 0x3FFFFFFF

func unique() string {
	uniqueCounter++
	return base36(uniqueCounter)
}

func base36(n int64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [13]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}
