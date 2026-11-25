// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Durable dispatch admission (#1577, migration 00101): regressions for the
// production Dispatcher.RunOnce over real Postgres, with activities recorded
// by the production activities.Writer inside explicit transactions (so
// created_at is the column DEFAULT, each transaction's start).
//
// The schedules are explicit transaction barriers and synchronous RunOnce
// calls; the one concurrency case is decided by row locks, not timing. No
// cursor is rewound, no outbox row is written by hand, no timestamp is set.
// Every query's error is checked. Each test first drains whatever the
// shared test database left pending, through the dispatcher itself.

package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// workFixture is one user, one connected peer and a dispatcher wired as the
// dispatcher happy-path test wires it (production resolver and received
// grant check, public tier).
type workFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	reg   *cache.Registry
	ref   int64
	actor string
	peer  uuid.UUID
	pURL  string
	audit *auditSpy
	w     *activities.Writer
}

func newWorkFixture(t *testing.T) *workFixture {
	t.Helper()
	pool := openTestPool(t)
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)
	f := &workFixture{t: t, pool: pool, reg: reg, audit: &auditSpy{}, w: activities.NewWriter(pool, logger, nil)}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Dispatch Work', 1) RETURNING ref`,
		"dispatch-work-"+randHex(4)).Scan(&f.ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	f.pURL = "https://dispatch-work-" + randHex(4) + ".example"
	if err := pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Dispatch Work Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2) RETURNING id`, f.pURL, f.ref).Scan(&f.peer); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM federation_outbox WHERE peer_id = $1`,
			`DELETE FROM federation_inbound_grants WHERE peer_id = $1`,
		} {
			if _, err := pool.Exec(c, q, f.peer); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if _, err := pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, f.ref); err != nil {
			t.Errorf("cleanup activities: %v", err)
		}
		if _, err := pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, f.peer); err != nil {
			t.Errorf("cleanup peer: %v", err)
		}
		if _, err := pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, f.ref); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	f.actor = localActorURI(t, pool, f.ref)
	f.drain(f.dispatcher(100))
	return f
}

// dispatcher is a new production Dispatcher over the fixture's pool.
func (f *workFixture) dispatcher(batch int32) *outbox.Dispatcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := outbox.NewResolver(f.pool, f.reg, func(context.Context) bool { return true })
	withReceivedGrants(resolver, f.pool)
	d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: batch}, f.pool, resolver, logger)
	d.SetSkippedAudit(f.audit.record)
	d.SetSensitivityLookup(fixedSensitivity(outbox.SensitivityPublic))
	return d
}

// drain runs d until a scan handles nothing, so a test starts with no
// pending work but its own.
func (f *workFixture) drain(d *outbox.Dispatcher) {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		if p, _, _ := d.RunOnce(f.t.Context()); p == 0 {
			if n := f.count(`SELECT count(*) FROM federation_dispatch_work`); n != 0 {
				f.t.Fatalf("drain: %d work rows still pending after a scan handled none", n)
			}
			return
		}
	}
	f.t.Fatal("drain: still handling work after 200 scans")
}

func (f *workFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (f *workFixture) outbox(id uuid.UUID) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`, id, f.peer)
}

func (f *workFixture) pending(id uuid.UUID) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM federation_dispatch_work WHERE activity_id = $1`, id)
}

func (f *workFixture) cursor() *uuid.UUID {
	f.t.Helper()
	var id *uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(), `SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`).Scan(&id); err != nil {
		f.t.Fatalf("read the cursor: %v", err)
	}
	return id
}

// like is a Like of a fresh post at the peer, with (granted) or without
// a live received grant for it: the Input emit.Like records.
func (f *workFixture) like(granted bool) activities.Input {
	f.t.Helper()
	target := f.pURL + "/posts/" + uuid.NewString()
	if granted {
		seedReceivedGrant(f.t, f.pool, f.peer, target)
	}
	ref := f.ref
	return activities.Input{
		Type:         federation.ActivityLike,
		ActivityURI:  dispatcherTestBase + "/activities/work-" + uuid.NewString(),
		ActorUserRef: &ref,
		ActorURI:     f.actor,
		Object:       &activities.ObjectRef{URI: target, Kind: activities.ObjectKindPost},
		Payload:      map[string]any{"target_uri": target, "target_peer_id": f.peer.String()},
	}
}

// begin records in inside a new transaction and leaves it open.
func (f *workFixture) begin(in activities.Input) (pgx.Tx, *activities.Record) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.t.Context())
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	rec, err := f.w.RecordActivity(f.t.Context(), tx, in)
	if err != nil {
		f.t.Fatalf("record: %v", err)
	}
	return tx, rec
}

func (f *workFixture) commit(tx pgx.Tx) {
	f.t.Helper()
	if err := tx.Commit(f.t.Context()); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
}

// record records and commits in.
func (f *workFixture) record(in activities.Input) *activities.Record {
	f.t.Helper()
	tx, rec := f.begin(in)
	f.commit(tx)
	return rec
}

// TestDispatchWork_LateCommitIsEnqueued: an activity whose transaction began
// first but commits after a later activity was dispatched is still pending,
// and the next scan enqueues it exactly once (#1577; the rev794 schedule,
// which lost it under cursor admission). The cursor never moves back.
func TestDispatchWork_LateCommitIsEnqueued(t *testing.T) {
	f := newWorkFixture(t)
	d := f.dispatcher(100)

	t.Run("commit order: both enqueued once", func(t *testing.T) {
		txA, a := f.begin(f.like(true))
		txB, b := f.begin(f.like(true))
		if !a.CreatedAt.Before(b.CreatedAt) {
			t.Fatalf("A created_at %s is not before B %s", a.CreatedAt, b.CreatedAt)
		}
		f.commit(txA)
		f.commit(txB)
		if p, s, e := d.RunOnce(t.Context()); p != 2 || s != 0 || e != 2 {
			t.Fatalf("RunOnce = %d/%d/%d, want 2/0/2", p, s, e)
		}
		if f.outbox(a.ID) != 1 || f.outbox(b.ID) != 1 || f.pending(a.ID) != 0 || f.pending(b.ID) != 0 {
			t.Fatalf("A outbox %d pending %d, B outbox %d pending %d", f.outbox(a.ID), f.pending(a.ID), f.outbox(b.ID), f.pending(b.ID))
		}
	})

	t.Run("the earlier activity commits after the later one was dispatched", func(t *testing.T) {
		txA, a := f.begin(f.like(true))
		txB, b := f.begin(f.like(true))
		if !a.CreatedAt.Before(b.CreatedAt) {
			t.Fatalf("A created_at %s is not before B %s", a.CreatedAt, b.CreatedAt)
		}
		f.commit(txB)
		if p, _, e := d.RunOnce(t.Context()); p != 1 || e != 1 {
			t.Fatalf("RunOnce with A open = %d processed, %d enqueued, want 1/1", p, e)
		}
		if f.outbox(b.ID) != 1 || f.count(`SELECT count(*) FROM activities WHERE id = $1`, a.ID) != 0 {
			t.Fatalf("B outbox %d; A visible before its commit", f.outbox(b.ID))
		}
		if c := f.cursor(); c == nil || *c != b.ID {
			t.Fatalf("the cursor is %v, want B %s", c, b.ID)
		}
		f.commit(txA)
		if f.pending(a.ID) != 1 {
			t.Fatalf("A has %d work rows after its commit, want 1", f.pending(a.ID))
		}
		if p, s, e := d.RunOnce(t.Context()); p != 1 || s != 0 || e != 1 {
			t.Fatalf("RunOnce after A committed = %d/%d/%d, want 1/0/1", p, s, e)
		}
		if f.outbox(a.ID) != 1 || f.pending(a.ID) != 0 || f.outbox(b.ID) != 1 {
			t.Fatalf("A outbox %d pending %d, B outbox %d", f.outbox(a.ID), f.pending(a.ID), f.outbox(b.ID))
		}
		if c := f.cursor(); c == nil || *c != b.ID {
			t.Fatalf("the cursor is %v after A, want it kept at the newer B %s", c, b.ID)
		}
		if p, _, _ := d.RunOnce(t.Context()); p != 0 || f.outbox(a.ID) != 1 {
			t.Fatalf("a further scan handled %d rows, A outbox %d", p, f.outbox(a.ID))
		}
	})

	t.Run("a rolled-back activity leaves no work, a remote one gets none", func(t *testing.T) {
		tx, a := f.begin(f.like(true))
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.pending(a.ID) != 0 || f.count(`SELECT count(*) FROM activities WHERE id = $1`, a.ID) != 0 {
			t.Fatal("a rolled-back activity left its row or its work")
		}
		in := f.like(true)
		in.Source = f.pURL
		in.ActorUserRef = nil
		in.ActorURI = f.pURL + "/users/remote"
		r := f.record(in)
		if f.count(`SELECT count(*) FROM activities WHERE id = $1 AND source = $2`, r.ID, f.pURL) != 1 || f.pending(r.ID) != 0 {
			t.Fatal("a remote-source activity was not recorded, or was scheduled for local dispatch")
		}
	})
}

// TestDispatchWork_DurableAndBounded: pending work outlives the dispatcher
// that did not process it; a new instance claims it in bounded batches.
func TestDispatchWork_DurableAndBounded(t *testing.T) {
	f := newWorkFixture(t)
	var ids []uuid.UUID
	for i := 0; i < 5; i++ {
		ids = append(ids, f.record(f.like(true)).ID)
	}
	first := f.dispatcher(2)
	if p, _, e := first.RunOnce(t.Context()); p != 2 || e != 2 {
		t.Fatalf("first instance = %d/%d, want 2/2", p, e)
	}
	// A new instance (as after a restart) finds the rest still pending.
	second := f.dispatcher(2)
	var got []int
	for {
		p, _, _ := second.RunOnce(t.Context())
		if p == 0 {
			break
		}
		got = append(got, p)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("the second instance's batches were %v, want [2 1]", got)
	}
	for _, id := range ids {
		if f.outbox(id) != 1 || f.pending(id) != 0 {
			t.Fatalf("%s: outbox %d pending %d", id, f.outbox(id), f.pending(id))
		}
	}
}

// TestDispatchWork_ClaimsAreExclusive: a work row another transaction holds
// is skipped, not processed twice; two dispatchers running at once handle
// every row exactly once between them.
func TestDispatchWork_ClaimsAreExclusive(t *testing.T) {
	f := newWorkFixture(t)
	d := f.dispatcher(100)

	t.Run("a row held by another transaction is skipped until released", func(t *testing.T) {
		x, y := f.record(f.like(true)), f.record(f.like(true))
		hold, err := f.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
		var locked uuid.UUID
		if err := hold.QueryRow(t.Context(), `SELECT activity_id FROM federation_dispatch_work WHERE activity_id = $1 FOR UPDATE`, x.ID).Scan(&locked); err != nil {
			t.Fatalf("hold x: %v", err)
		}
		if p, _, _ := d.RunOnce(t.Context()); p != 1 || f.outbox(y.ID) != 1 || f.outbox(x.ID) != 0 || f.pending(x.ID) != 1 {
			t.Fatalf("with x held: processed %d, x outbox %d pending %d, y outbox %d", p, f.outbox(x.ID), f.pending(x.ID), f.outbox(y.ID))
		}
		if err := hold.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if p, _, _ := d.RunOnce(t.Context()); p != 1 || f.outbox(x.ID) != 1 || f.pending(x.ID) != 0 {
			t.Fatalf("after release: processed %d, x outbox %d pending %d", p, f.outbox(x.ID), f.pending(x.ID))
		}
	})

	t.Run("two dispatchers at once handle each row once", func(t *testing.T) {
		const n = 40
		var ids []uuid.UUID
		for i := 0; i < n; i++ {
			ids = append(ids, f.record(f.like(true)).ID)
		}
		d1, d2 := f.dispatcher(5), f.dispatcher(5)
		var mu sync.Mutex
		total := 0
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, dd := range []*outbox.Dispatcher{d1, d2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 2*n; i++ {
					p, _, _ := dd.RunOnce(context.Background())
					mu.Lock()
					total += p
					mu.Unlock()
					if p == 0 {
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		// A scan can find nothing while the other holds the last rows; one
		// more pass picks up anything it left.
		for {
			p, _, _ := d1.RunOnce(t.Context())
			total += p
			if p == 0 {
				break
			}
		}
		if total != n {
			t.Fatalf("the dispatchers handled %d rows between them, want exactly %d", total, n)
		}
		for _, id := range ids {
			if f.outbox(id) != 1 || f.pending(id) != 0 {
				t.Fatalf("%s: outbox %d pending %d", id, f.outbox(id), f.pending(id))
			}
		}
	})
}

// TestDispatchWork_SkipsAreConsumedOnce: an activity v1 does not federate,
// and one with no recipient, are decided once: their work is acknowledged
// with no outbox row, and a later scan neither handles nor audits them again.
func TestDispatchWork_SkipsAreConsumedOnce(t *testing.T) {
	f := newWorkFixture(t)
	d := f.dispatcher(100)
	ref := f.ref
	unsupported := f.record(activities.Input{
		Type: federation.ActivityCreate, ActivityURI: dispatcherTestBase + "/activities/work-" + uuid.NewString(),
		ActorUserRef: &ref, ActorURI: f.actor,
		Object: &activities.ObjectRef{URI: dispatcherTestBase + "/posts/" + uuid.NewString(), Kind: activities.ObjectKindPost},
	})
	noRecipient := f.record(f.like(false))
	p, s, e := d.RunOnce(t.Context())
	if p != 2 || s != 2 || e != 0 {
		t.Fatalf("RunOnce = %d/%d/%d, want 2 processed, 2 skipped, 0 enqueued", p, s, e)
	}
	for _, r := range []*activities.Record{unsupported, noRecipient} {
		if f.pending(r.ID) != 0 || f.outbox(r.ID) != 0 || f.audit.findByActivityID(r.ID.String()) == nil {
			t.Fatalf("%s: pending %d outbox %d audited %v", r.ID, f.pending(r.ID), f.outbox(r.ID), f.audit.findByActivityID(r.ID.String()) != nil)
		}
	}
	audits := len(f.audit.entries)
	if p, s, _ := d.RunOnce(t.Context()); p != 0 || s != 0 || len(f.audit.entries) != audits {
		t.Fatalf("a second scan handled %d, skipped %d, audits %d -> %d", p, s, audits, len(f.audit.entries))
	}
}

// TestDispatchWork_FailureRollsBackTheBatch: when the batch fails after its
// outbox INSERT (here at the cursor update, the last step, by a test-owned
// trigger), the outbox row, the work acknowledgement and the cursor all roll
// back together; once the failure is gone, the retry enqueues exactly once.
func TestDispatchWork_FailureRollsBackTheBatch(t *testing.T) {
	f := newWorkFixture(t)
	d := f.dispatcher(100)
	before := f.cursor()
	a := f.record(f.like(true))
	fn := "test_dispatch_fail_" + randHex(4)
	for _, q := range []string{
		`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test: cursor update refused'; END $$`,
		`CREATE TRIGGER ` + fn + ` BEFORE UPDATE ON federation_dispatch_state FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
	} {
		if _, err := f.pool.Exec(t.Context(), q); err != nil {
			t.Fatalf("install the failure: %v", err)
		}
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		for _, q := range []string{`DROP TRIGGER IF EXISTS ` + fn + ` ON federation_dispatch_state`, `DROP FUNCTION IF EXISTS ` + fn + `()`} {
			if _, err := f.pool.Exec(context.Background(), q); err != nil {
				t.Errorf("remove the failure: %v", err)
			}
		}
	}
	t.Cleanup(drop)
	if p, s, e := d.RunOnce(t.Context()); p != 0 || s != 0 || e != 0 {
		t.Fatalf("a failing batch reported %d/%d/%d, want 0/0/0", p, s, e)
	}
	if f.outbox(a.ID) != 0 || f.pending(a.ID) != 1 {
		t.Fatalf("after the failure: outbox %d pending %d, want 0 and 1", f.outbox(a.ID), f.pending(a.ID))
	}
	if c := f.cursor(); (c == nil) != (before == nil) || (c != nil && *c != *before) {
		t.Fatalf("the cursor moved to %v in a failed batch (was %v)", c, before)
	}
	drop()
	if p, _, e := d.RunOnce(t.Context()); p != 1 || e != 1 {
		t.Fatalf("the retry = %d processed, %d enqueued, want 1/1", p, e)
	}
	if f.outbox(a.ID) != 1 || f.pending(a.ID) != 0 {
		t.Fatalf("after the retry: outbox %d pending %d", f.outbox(a.ID), f.pending(a.ID))
	}
	if c := f.cursor(); c == nil || *c != a.ID {
		t.Fatalf("the cursor is %v after the retry, want %s", c, a.ID)
	}
}
