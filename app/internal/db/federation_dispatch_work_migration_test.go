// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// dispatchFixture is a fresh database migrated to 100 with helpers to
// record activities as the ledger's direct-SQL producers do.
type dispatchFixture struct {
	t     *testing.T
	sqlDB *sql.DB
}

func (f dispatchFixture) exec(q string, args ...any) error {
	_, err := f.sqlDB.ExecContext(f.t.Context(), q, args...)
	return err
}

func (f dispatchFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.sqlDB.QueryRowContext(f.t.Context(), q, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// activity records one activity (source 'local' or a peer URL) in its own
// transaction and returns its id. Each statement is its own transaction, so
// created_at (DEFAULT now()) increases from one call to the next.
func (f dispatchFixture) activity(source string) string {
	f.t.Helper()
	var id string
	if err := f.sqlDB.QueryRowContext(f.t.Context(), `INSERT INTO activities (activity_uri, activity_type, actor_uri, payload, source)
		VALUES ($1, 'Like', 'https://migrate-test.example/users/a', '{}'::jsonb, $2) RETURNING id`,
		"https://migrate-test.example/activities/"+uuid.NewString(), source).Scan(&id); err != nil {
		f.t.Fatalf("record a %s activity: %v", source, err)
	}
	return id
}

func (f dispatchFixture) work() []string {
	f.t.Helper()
	rows, err := f.sqlDB.QueryContext(f.t.Context(), `SELECT activity_id::text FROM federation_dispatch_work ORDER BY activity_created_at, activity_id`)
	if err != nil {
		f.t.Fatalf("read work: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f dispatchFixture) setCursor(id any) {
	f.t.Helper()
	if err := f.exec(`UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, id); err != nil {
		f.t.Fatalf("set the legacy cursor: %v", err)
	}
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMigration00101_FederationDispatchWork_Boundary (#1577): Up makes pending
// only the local activities after the legacy cursor (no replay of history at
// or before it, no remote activity); afterwards the trigger schedules exactly
// the committed local inserts; a deleted activity takes its work; Down
// refuses while work is pending, changing nothing; once none is left Down
// removes the table, trigger and function and leaves 00100 alone; Up again
// restores them.
func TestMigration00101_FederationDispatchWork_Boundary(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 100); err != nil {
		t.Fatalf("up to 100: %v", err)
	}
	f := dispatchFixture{t: t, sqlDB: sqlDB}
	before1, before2 := f.activity("local"), f.activity("local") // considered by the legacy dispatcher
	f.setCursor(before2)
	after := f.activity("local") // after the cursor: still to dispatch
	remote := f.activity("https://peer.example")
	if _, err := p.UpTo(ctx, 101); err != nil {
		t.Fatalf("up to 101: %v", err)
	}
	if got := f.work(); !sameIDs(got, []string{after}) {
		t.Fatalf("pending after Up: %v, want only %s (history %s %s and remote %s excluded)", got, after, before1, before2, remote)
	}
	if n := f.count(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.federation_dispatch_work'::regclass AND contype = 'f'
		AND confrelid = 'public.activities'::regclass AND confdeltype = 'c'`); n != 1 {
		t.Fatalf("%d cascading foreign keys to activities, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM pg_indexes WHERE indexname = 'federation_dispatch_work_order_idx'`); n != 1 {
		t.Fatalf("the ordering index is missing (%d)", n)
	}
	if n := f.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'federation_dispatch_work'`); n != 2 {
		t.Fatalf("the work table has %d columns, want 2 (an id and its order time)", n)
	}
	if n := f.count(`SELECT count(*) FROM federation_dispatch_work w JOIN activities a ON a.id = w.activity_id
		WHERE w.activity_created_at <> a.created_at`); n != 0 {
		t.Fatalf("%d work rows carry another time than their activity", n)
	}

	// After Up: a committed local insert is scheduled, a remote one is not,
	// a rolled-back one leaves nothing.
	local := f.activity("local")
	f.activity("https://peer.example")
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rolledBack string
	if err := tx.QueryRowContext(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, payload)
		VALUES ($1, 'Like', 'https://migrate-test.example/users/a', '{}'::jsonb) RETURNING id`,
		"https://migrate-test.example/activities/"+uuid.NewString()).Scan(&rolledBack); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := f.work(); !sameIDs(got, []string{after, local}) {
		t.Fatalf("pending after the inserts: %v, want %s then %s", got, after, local)
	}
	if n := f.count(`SELECT count(*) FROM activities WHERE id = $1`, rolledBack); n != 0 {
		t.Fatal("the rolled-back activity is visible")
	}
	// A deleted activity takes its work with it.
	if err := f.exec(`DELETE FROM activities WHERE id = $1`, local); err != nil {
		t.Fatal(err)
	}
	if got := f.work(); !sameIDs(got, []string{after}) {
		t.Fatalf("pending after the delete: %v, want %s", got, after)
	}

	// Down refuses while work is pending, and changes nothing.
	if _, err := p.DownTo(ctx, 100); err == nil || !strings.Contains(err.Error(), "still to be dispatched") {
		t.Fatalf("down to 100 with work: %v", err)
	}
	if got := f.work(); !sameIDs(got, []string{after}) {
		t.Fatalf("the refused Down changed the work: %v", got)
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 101 {
		t.Fatalf("after the refused Down the version is %d (%v), want 101", v, err)
	}
	if err := f.exec(`DELETE FROM federation_dispatch_work`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 100); err != nil {
		t.Fatalf("down to 100: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_dispatch_work'`); n != 0 {
		t.Fatalf("after Down the table remains (%d)", n)
	}
	if n := f.count(`SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_dispatch_work_schedule_trg'`) +
		f.count(`SELECT count(*) FROM pg_proc WHERE proname = 'federation_dispatch_work_schedule'`); n != 0 {
		t.Fatalf("after Down the trigger or function remains (%d)", n)
	}
	// 00100's producers, 00099's queue and 00001's dispatch NOTIFY are untouched.
	if n := f.count(`SELECT count(*) FROM pg_trigger WHERE tgname IN ('federation_metadata_asset_changed', 'federation_dispatch_notify_trg')`); n != 2 {
		t.Fatalf("Down 101 touched an earlier trigger (%d of 2 remain)", n)
	}
	if n := f.count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_metadata_work'`); n != 1 {
		t.Fatalf("Down 101 touched 00099 (%d)", n)
	}
	// Up again restores the table, its constraints and the trigger.
	if _, err := p.UpTo(ctx, 101); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.federation_dispatch_work'::regclass`); n != 2 {
		t.Fatalf("after Up again %d constraints, want 2", n)
	}
	again := f.activity("local")
	if got := f.work(); len(got) == 0 || got[len(got)-1] != again {
		t.Fatalf("after Up again the trigger did not schedule %s: %v", again, got)
	}
}

// TestMigration00101_NullCursorAndRefusals (#1577): with a NULL legacy cursor
// every local activity becomes pending (remote ones never); with no cursor
// row, or a cursor naming a deleted activity, Up refuses and changes nothing.
func TestMigration00101_NullCursorAndRefusals(t *testing.T) {
	t.Run("a NULL cursor makes every local activity pending", func(t *testing.T) {
		cfg := freshDatabase(t)
		sqlDB := openCfg(t, cfg)
		p := afvhProvider(t, sqlDB)
		if _, err := p.UpTo(t.Context(), 100); err != nil {
			t.Fatalf("up to 100: %v", err)
		}
		f := dispatchFixture{t: t, sqlDB: sqlDB}
		a, _, b := f.activity("local"), f.activity("https://peer.example"), f.activity("local")
		f.setCursor(nil)
		if _, err := p.UpTo(t.Context(), 101); err != nil {
			t.Fatalf("up to 101: %v", err)
		}
		if got := f.work(); !sameIDs(got, []string{a, b}) {
			t.Fatalf("pending: %v, want %s %s", got, a, b)
		}
	})
	for _, c := range []struct {
		name, prepare, want string
	}{
		{"no cursor row", `DELETE FROM federation_dispatch_state WHERE id = 1`, "no cursor row"},
		{"a cursor naming a deleted activity", `UPDATE federation_dispatch_state SET last_dispatched_activity_id = gen_random_uuid() WHERE id = 1`, "no longer exists"},
	} {
		t.Run(c.name+" refuses", func(t *testing.T) {
			cfg := freshDatabase(t)
			sqlDB := openCfg(t, cfg)
			p := afvhProvider(t, sqlDB)
			if _, err := p.UpTo(t.Context(), 100); err != nil {
				t.Fatalf("up to 100: %v", err)
			}
			f := dispatchFixture{t: t, sqlDB: sqlDB}
			f.activity("local")
			if err := f.exec(c.prepare); err != nil {
				t.Fatal(err)
			}
			if _, err := p.UpTo(t.Context(), 101); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("up to 101: %v, want a refusal naming %q", err, c.want)
			}
			if v, err := p.GetDBVersion(t.Context()); err != nil || v != 100 {
				t.Fatalf("after the refusal the version is %d (%v), want 100", v, err)
			}
			if n := f.count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_dispatch_work'`) +
				f.count(`SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_dispatch_work_schedule_trg'`); n != 0 {
				t.Fatalf("the refused Up left %d objects", n)
			}
		})
	}
}

// TestMigration00101_DownDoesNotDiscardConcurrentWork (#1577): an activity
// insert still uncommitted (its trigger has added work) blocks Down, which
// is observed waiting behind it in pg_locks (not by timing); once the insert
// commits, Down refuses, leaving the table, the work and version 101.
func TestMigration00101_DownDoesNotDiscardConcurrentWork(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 101); err != nil {
		t.Fatalf("up to 101: %v", err)
	}
	insConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer insConn.Close()
	tx, err := insConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	var insPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&insPID); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRowContext(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, payload)
		VALUES ($1, 'Like', 'https://migrate-test.example/users/a', '{}'::jsonb) RETURNING id`,
		"https://migrate-test.example/activities/"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	downDB := openCfg(t, cfg)
	pd := afvhProvider(t, downDB)
	downErr := make(chan error, 1)
	go func() {
		_, err := pd.DownTo(ctx, 100)
		downErr <- err
	}()
	blocked := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-downErr:
			t.Fatalf("Down returned while the insert was uncommitted (err %v): it did not wait for it", err)
		default:
		}
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l
			WHERE l.locktype = 'relation' AND NOT l.granted
			  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND l.relation = 'public.activities'::regclass
			  AND $1 = ANY (pg_blocking_pids(l.pid))`, insPID).Scan(&n); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if n == 1 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("Down never blocked on activities behind the uncommitted insert; the overlap this test asserts never happened")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the insert: %v", err)
	}
	select {
	case err = <-downErr:
	case <-ctx.Done():
		t.Fatal("Down never returned after the insert committed")
	}
	if err == nil || !strings.Contains(err.Error(), "still to be dispatched") {
		t.Errorf("Down with concurrently committed work: %v, want a refusal", err)
	}
	f := dispatchFixture{t: t, sqlDB: sqlDB}
	if got := f.work(); !sameIDs(got, []string{id}) {
		t.Fatalf("after the refused Down the work is %v, want %s", got, id)
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 101 {
		t.Fatalf("after the refused Down the version is %d (%v), want 101", v, err)
	}
}
