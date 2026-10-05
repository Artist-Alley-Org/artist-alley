// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #40 sprint 1 — the scheduled-action engine, end to end.
//
// These drive the real reaper execution path (processOne) against real
// rows: schedule an action, run the reaper, assert the DB state, the
// domain change, and the audit trail. In-package (package
// scheduledactions) so the test can call the unexported reaper loop
// directly rather than standing up the whole jobs queue.
//
// Skips without AA_DB_PASSWORD.

package scheduledactions

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func saPool(t *testing.T) *pgxpool.Pool {
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
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

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

// fakeNotifier records the notifications it was asked to send.
type fakeNotifier struct {
	calls []int64
	fail  bool
}

func (f *fakeNotifier) Notify(_ context.Context, recipient int64, _ *int64, _, _, _ string, _ map[string]any) error {
	if f.fail {
		return context.DeadlineExceeded // any error
	}
	f.calls = append(f.calls, recipient)
	return nil
}

func newReaper(t *testing.T, pool *pgxpool.Pool, n Notifier) *ReaperJob {
	t.Helper()
	return &ReaperJob{
		Pool:     pool,
		Rec:      audit.NewRecorder(pool, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
		Notifier: n,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

// drain runs processOne until no action is claimable, mimicking one
// Handle tick without the re-enqueue. Returns (doneCount, failedCount).
func drain(t *testing.T, h *ReaperJob) (int, int) {
	t.Helper()
	exec := h.newExecutor()
	done, failed := 0, 0
	for {
		outcome, claimed, err := h.processOne(context.Background(), exec)
		if err != nil {
			t.Fatalf("processOne: %v", err)
		}
		if !claimed {
			break
		}
		switch outcome {
		case StateDone:
			done++
		case StateFailed:
			failed++
		}
	}
	return done, failed
}

func seedAsset(t *testing.T, pool *pgxpool.Pool, sensitivity string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,$2,4400001,(SELECT MIN(ref) FROM asset_types),'active',$3,'ready')`,
		id, "sa-"+sensitivity, sensitivity)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

func at(offset time.Duration) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(offset), Valid: true}
}

// schedule inserts one action and registers its cleanup.
func schedule(t *testing.T, s *Store, in ScheduleInput) ScheduledAction {
	t.Helper()
	row, err := s.Schedule(context.Background(), in)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM scheduled_actions WHERE id=$1`, row.ID)
	})
	return row
}

func stateOf(t *testing.T, s *Store, id pgtype.UUID) ScheduledAction {
	t.Helper()
	row, err := s.q.GetScheduledAction(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return row
}

// auditCount returns how many executed/failed audit rows reference an
// action id.
func auditCount(t *testing.T, pool *pgxpool.Pool, eventType, actionID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events
		  WHERE event_type=$1 AND metadata->>'scheduled_action_id'=$2`,
		eventType, actionID).Scan(&n)
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM audit_events WHERE metadata->>'scheduled_action_id'=$1`, actionID)
	})
	return n
}

// TestReaper_PastActionExecutes: a due action runs, lands in done, and
// writes an audit row (acceptance 2).
func TestReaper_PastActionExecutes(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "embargo")
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: asset.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(-time.Minute),
	})

	done, failed := drain(t, newReaper(t, pool, &fakeNotifier{}))
	if done != 1 || failed != 0 {
		t.Fatalf("drain done=%d failed=%d, want 1/0", done, failed)
	}
	if got := stateOf(t, store, a.ID).State; got != StateDone {
		t.Errorf("state=%q, want done", got)
	}
	if n := auditCount(t, pool, audit.EventScheduledActionExecuted, uuid.UUID(a.ID.Bytes).String()); n != 1 {
		t.Errorf("executed audit rows=%d, want 1", n)
	}
}

// TestReaper_FutureActionStaysPending: acceptance 3.
func TestReaper_FutureActionStaysPending(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "embargo")
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: asset.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(time.Hour),
	})

	done, _ := drain(t, newReaper(t, pool, &fakeNotifier{}))
	if done != 0 {
		t.Fatalf("a future action executed (done=%d); it must stay pending", done)
	}
	if got := stateOf(t, store, a.ID).State; got != StatePending {
		t.Errorf("state=%q, want pending", got)
	}
}

// TestReaper_CancelledNeverExecutes: acceptance 4 — cancel, then let its
// time pass, and confirm it never runs.
func TestReaper_CancelledNeverExecutes(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "embargo")
	// Scheduled in the PAST so it would fire immediately if not cancelled.
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: asset.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(-time.Minute),
	})
	ok, err := store.Cancel(context.Background(), uuid.UUID(a.ID.Bytes))
	if err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}

	done, failed := drain(t, newReaper(t, pool, &fakeNotifier{}))
	if done != 0 || failed != 0 {
		t.Fatalf("a cancelled action was executed (done=%d failed=%d)", done, failed)
	}
	if got := stateOf(t, store, a.ID).State; got != StateCancelled {
		t.Errorf("state=%q, want cancelled", got)
	}
	// And the asset's sensitivity is untouched.
	var sens string
	_ = pool.QueryRow(context.Background(), `SELECT sensitivity FROM assets WHERE id=$1`, asset).Scan(&sens)
	if sens != "embargo" {
		t.Errorf("cancelled action still mutated the asset: sensitivity=%q, want embargo", sens)
	}
}

// TestReaper_ChangeSensitivity_FlipsAndAuditsOldNew: acceptance 5 — the
// reveal-on-date recipe, proven end-to-end with the old->new trail.
func TestReaper_ChangeSensitivity_FlipsAndAuditsOldNew(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "embargo")
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: asset.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(-time.Second),
	})

	drain(t, newReaper(t, pool, &fakeNotifier{}))

	var sens string
	if err := pool.QueryRow(context.Background(), `SELECT sensitivity FROM assets WHERE id=$1`, asset).Scan(&sens); err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if sens != "public" {
		t.Errorf("sensitivity=%q, want public — the executor must flip the real column", sens)
	}
	// The audit row must carry old=embargo, new=public.
	var old, nw string
	err := pool.QueryRow(context.Background(),
		`SELECT metadata->>'old', metadata->>'new' FROM audit_events
		  WHERE event_type=$1 AND metadata->>'scheduled_action_id'=$2`,
		audit.EventScheduledActionExecuted, uuid.UUID(a.ID.Bytes).String()).Scan(&old, &nw)
	if err != nil {
		t.Fatalf("read audit changeset: %v", err)
	}
	if old != "embargo" || nw != "public" {
		t.Errorf("audit old->new = %q->%q, want embargo->public", old, nw)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_events WHERE metadata->>'scheduled_action_id'=$1`, uuid.UUID(a.ID.Bytes).String())
}

// TestReaper_ExecutorWritesAudit is the sabotage target (acceptance 6):
// removing recordExecuted from the change_sensitivity executor makes
// this go red, because no executed audit row is written.
func TestReaper_ExecutorWritesAudit(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "team")
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: asset.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(-time.Second),
	})

	drain(t, newReaper(t, pool, &fakeNotifier{}))

	if n := auditCount(t, pool, audit.EventScheduledActionExecuted, uuid.UUID(a.ID.Bytes).String()); n != 1 {
		t.Errorf("executor did not write its audit row (executed rows=%d, want 1) — "+
			"every execution must be audited", n)
	}
}

// TestReaper_FailedExecutorCapturesError: acceptance 7 — a failing
// executor lands in state=failed with the error, no silent swallow, no
// retry loop, and the domain change rolled back.
func TestReaper_FailedExecutorCapturesError(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	// Target a NON-EXISTENT asset so the sensitivity update finds no
	// row and the executor returns an error.
	missing := uuid.New()
	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeSensitivity, TargetKind: TargetAsset, TargetID: missing.String(),
		Params: map[string]any{"to": "public"}, ScheduledFor: at(-time.Second),
	})

	done, failed := drain(t, newReaper(t, pool, &fakeNotifier{}))
	if done != 0 || failed != 1 {
		t.Fatalf("drain done=%d failed=%d, want 0/1", done, failed)
	}
	row := stateOf(t, store, a.ID)
	if row.State != StateFailed {
		t.Errorf("state=%q, want failed", row.State)
	}
	if row.Error == nil || *row.Error == "" {
		t.Error("failed action captured no error message")
	}
	if !row.ExecutedAt.Valid {
		t.Error("failed action has no executed_at; the attempt must be timestamped")
	}
	// A second drain must NOT re-run it (no infinite retry): failed is
	// terminal.
	done2, failed2 := drain(t, newReaper(t, pool, &fakeNotifier{}))
	if done2 != 0 || failed2 != 0 {
		t.Errorf("a terminal failed action was re-processed (done=%d failed=%d)", done2, failed2)
	}
	if n := auditCount(t, pool, audit.EventScheduledActionFailed, uuid.UUID(a.ID.Bytes).String()); n != 1 {
		t.Errorf("failed audit rows=%d, want 1", n)
	}
}

// TestReaper_NotifyAction sends through the injected notifier.
func TestReaper_NotifyAction(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	fn := &fakeNotifier{}
	a := schedule(t, store, ScheduleInput{
		Action: ActionNotify, TargetKind: TargetUser, TargetID: "4400009",
		Params: map[string]any{"verb": "test_scheduled"}, ScheduledFor: at(-time.Second),
	})

	drain(t, newReaper(t, pool, fn))

	if len(fn.calls) != 1 || fn.calls[0] != 4400009 {
		t.Errorf("notifier calls=%v, want [4400009]", fn.calls)
	}
	if got := stateOf(t, store, a.ID).State; got != StateDone {
		t.Errorf("notify state=%q, want done", got)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_events WHERE metadata->>'scheduled_action_id'=$1`, uuid.UUID(a.ID.Bytes).String())
}
