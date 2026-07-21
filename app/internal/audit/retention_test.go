// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #467 — audit retention purge + DSAR tombstoning, end to end.
//
// Skips without AA_DB_PASSWORD.

package audit

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func retPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
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
	t.Cleanup(pool.Close)
	return pool
}

// seedEvent inserts one audit row at a specific age, returning its id.
func seedEvent(t *testing.T, pool *pgxpool.Pool, eventType string, age time.Duration, hold bool, actor *int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO audit_events (id, event_type, occurred_at, actor_user_ref, legal_hold, metadata)
		VALUES ($1,$2, NOW() - $3::interval, $4, $5, '{}'::jsonb)`,
		id, eventType, age.String(), actor, hold)
	if err != nil {
		t.Fatalf("seed %s: %v", eventType, err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_events WHERE id=$1`, id) })
	return id
}

func exists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("exists: %v", err)
	}
	return n > 0
}

func retJob(pool *pgxpool.Pool) *RetentionJob {
	return &RetentionJob{
		Pool:   pool,
		Rec:    NewRecorder(pool, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

// TestRetention_PurgesOverAgeRespectsPolicyHoldAndCategory is
// acceptance 2 in one pass: an over-age login row (90d policy) purges;
// an under-age one survives; an over-age row in a category with no
// policy (7y default) survives; and a legal-held over-age login row
// survives.
func TestRetention_PurgesOverAgeRespectsPolicyHoldAndCategory(t *testing.T) {
	pool := retPool(t)
	// login policy is 90 days (seeded by migration 00013).
	overAgeLogin := seedEvent(t, pool, "login.succeeded", 100*24*time.Hour, false, nil)
	underAgeLogin := seedEvent(t, pool, "login.succeeded", 10*24*time.Hour, false, nil)
	heldOldLogin := seedEvent(t, pool, "login.failed", 200*24*time.Hour, true, nil)
	// 'zzdemo' has no policy row → 7-year default; 100 days is well under.
	otherOld := seedEvent(t, pool, "zzdemo.event", 100*24*time.Hour, false, nil)

	total, perCat, err := retJob(pool).purgeAll(context.Background())
	if err != nil {
		t.Fatalf("purgeAll: %v", err)
	}

	if exists(t, pool, overAgeLogin) {
		t.Error("an over-age login row survived the 90-day policy purge")
	}
	if !exists(t, pool, underAgeLogin) {
		t.Error("an under-age login row was purged; only over-age rows should go")
	}
	if !exists(t, pool, heldOldLogin) {
		t.Error("a legal_hold row was purged; holds must exempt a row regardless of age")
	}
	if !exists(t, pool, otherOld) {
		t.Error("a row in a no-policy category was purged before its 7-year default")
	}
	if perCat["login"] < 1 || total < 1 {
		t.Errorf("purge counts look wrong: total=%d perCat=%v", total, perCat)
	}

	// Acceptance 6: the purge wrote its own audit event.
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE event_type=$1 AND metadata->>'category'='login'`,
		EventAuditRetentionPurged).Scan(&n)
	if n < 1 {
		t.Error("the retention purge did not write its own audit event")
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_events WHERE event_type=$1`, EventAuditRetentionPurged)
}

// TestDSAR_TombstonePreservesRowsAnonymizesActor is acceptance 3: BOTH
// halves — the rows survive AND the actor identity is anonymized.
func TestDSAR_TombstonePreservesRowsAnonymizesActor(t *testing.T) {
	pool := retPool(t)
	const userRef int64 = 46700001
	e1 := seedEvent(t, pool, "login.succeeded", time.Hour, false, ptr(userRef))
	e2 := seedEvent(t, pool, "asset.uploaded", time.Hour, false, ptr(userRef))
	// A different actor's row must be untouched.
	const other int64 = 46700002
	e3 := seedEvent(t, pool, "login.succeeded", time.Hour, false, ptr(other))

	n, err := Tombstone(context.Background(), pool, userRef)
	if err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if n != 2 {
		t.Fatalf("tombstoned %d rows, want 2", n)
	}

	// Rows PRESENT (this is not a delete).
	if !exists(t, pool, e1) || !exists(t, pool, e2) {
		t.Fatal("tombstone deleted rows; DSAR must preserve the events")
	}
	// Actor identity ANONYMIZED: ref cleared, pseudonym recorded.
	for _, id := range []uuid.UUID{e1, e2} {
		var actor *int64
		var tomb *string
		err := pool.QueryRow(context.Background(),
			`SELECT actor_user_ref, metadata->>'actor_tombstone' FROM audit_events WHERE id=$1`, id).Scan(&actor, &tomb)
		if err != nil {
			t.Fatalf("read tombstoned row: %v", err)
		}
		if actor != nil {
			t.Errorf("actor_user_ref not cleared on %s: %v", id, *actor)
		}
		if tomb == nil || *tomb != "deleted-user-46700001" {
			t.Errorf("actor tombstone on %s = %v, want deleted-user-46700001", id, tomb)
		}
	}
	// The other actor's row is untouched.
	var otherActor *int64
	_ = pool.QueryRow(context.Background(), `SELECT actor_user_ref FROM audit_events WHERE id=$1`, e3).Scan(&otherActor)
	if otherActor == nil || *otherActor != other {
		t.Error("tombstone touched a different actor's row")
	}
}

func ptr(v int64) *int64 { return &v }
