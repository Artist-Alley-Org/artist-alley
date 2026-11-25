// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 rev796: an announcement is pending while its durable dispatch work
// row (migration 00101) is there, wherever the progress cursor is. These
// tests drive the actual outbox.Dispatcher (production Resolver, claim,
// acknowledgement and cursor queries) and the actual MetadataWork against
// a live database. Schedule control is only a transaction holding the
// announcement's dispatch work row FOR UPDATE, which the dispatcher's SKIP
// LOCKED claim passes over, as it passes over any row another dispatcher
// transaction holds: no cursor write, timestamp rewrite or hand-made
// outbox row for the held announcement. Skips without AA_DB_PASSWORD.

package assetsend_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// pendingFx is fx with the actual dispatcher and metadata work processor.
type pendingFx struct {
	*fx
	ctx context.Context
	d   *outbox.Dispatcher
	m   assetsend.MetadataWork
}

// newPendingFx is newFx with the production dispatcher (fixture lookups: an
// asset's own recorded tier, public visibility for an asset, anything else
// unresolved) and the production metadata work processor (owner usernames
// read under the asset lock). Work any earlier test left is drained by the
// dispatcher first, so the cursor moves only through what the test records.
func newPendingFx(t *testing.T) *pendingFx {
	t.Helper()
	f := newFx(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(f.pool, logger)
	t.Cleanup(reg.Stop)
	pool := f.pool
	d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, pool,
		outbox.NewResolver(pool, reg, func(context.Context) bool { return true }), logger)
	d.SetSensitivityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Sensitivity, error) {
		if kind != "asset" {
			return "", errors.New("no tier for " + kind)
		}
		var s string
		err := pool.QueryRow(ctx, `SELECT sensitivity FROM assets WHERE id = $1`, id).Scan(&s)
		return outbox.Sensitivity(s), err
	})
	d.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		if kind != "asset" {
			return "", errors.New("no visibility for " + kind)
		}
		return outbox.VisibilityPublic, nil
	})
	p := &pendingFx{fx: f, ctx: ctx, d: d, m: assetsend.MetadataWork{Pool: pool, Writer: f.writer, BaseURL: base}}
	p.dispatch()
	f.withMetadataProfile(t)
	f.clearWork(t)
	return p
}

// dispatch runs the actual Dispatcher.RunOnce until a scan processes
// nothing (at most 50 scans).
func (p *pendingFx) dispatch() {
	p.t.Helper()
	for range 50 {
		n, s, e := p.d.RunOnce(p.ctx)
		p.t.Logf("dispatcher scan: processed %d, skipped %d, enqueued %d", n, s, e)
		if n == 0 {
			return
		}
	}
	p.t.Fatal("the dispatcher was still processing work after 50 scans")
}

// work runs the actual MetadataWork.RunOnce until it finds no work (at
// most 20 pages).
func (p *pendingFx) work() {
	p.t.Helper()
	for range 20 {
		worked, err := p.m.RunOnce(p.ctx, 0)
		if err != nil {
			p.t.Fatalf("metadata work: %v", err)
		}
		if !worked {
			return
		}
	}
	p.t.Fatal("metadata work still found work after 20 pages")
}

func (p *pendingFx) count(sql string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.pool.QueryRow(p.ctx, sql, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *pendingFx) dispatchWork(id uuid.UUID) int {
	p.t.Helper()
	return p.count(`SELECT count(*) FROM federation_dispatch_work WHERE activity_id = $1`, id)
}

// outboxRows are id's outbox rows to the peer as a whole, by status.
func (p *pendingFx) outboxRows(id uuid.UUID) map[string]int {
	p.t.Helper()
	rows, err := p.pool.Query(p.ctx, `SELECT status, count(*) FROM federation_outbox
		WHERE activity_id = $1 AND peer_id = $2 AND target_user_url IS NULL GROUP BY status`, id, p.peer)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			p.t.Fatal(err)
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		p.t.Fatal(err)
	}
	return out
}

// cursor is the dispatch progress cursor (uuid.Nil when none).
func (p *pendingFx) cursor() uuid.UUID {
	p.t.Helper()
	var id *uuid.UUID
	err := p.pool.QueryRow(p.ctx, `SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
		return uuid.Nil
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return *id
}

// before reports whether activity x sorts strictly before activity y by
// (created_at, id), the dispatcher's order.
func (p *pendingFx) before(x, y uuid.UUID) bool {
	p.t.Helper()
	var b bool
	if err := p.pool.QueryRow(p.ctx, `SELECT (x.created_at, x.id) < (y.created_at, y.id) FROM activities x, activities y
		WHERE x.id = $1 AND y.id = $2`, x, y).Scan(&b); err != nil {
		p.t.Fatal(err)
	}
	return b
}

// describe is an activity's type, source, created_at and whether its
// dispatch work is still pending, for a failure message.
func (p *pendingFx) describe(id uuid.UUID) string {
	var typ, source string
	var at time.Time
	var pending bool
	if err := p.pool.QueryRow(p.ctx, `SELECT activity_type, source, created_at,
		EXISTS (SELECT 1 FROM federation_dispatch_work w WHERE w.activity_id = a.id) FROM activities a WHERE a.id = $1`, id).
		Scan(&typ, &source, &at, &pending); err != nil {
		return "unreadable: " + err.Error()
	}
	return typ + " " + source + " created " + at.UTC().Format(time.RFC3339Nano) + " pending " + map[bool]string{true: "yes", false: "no"}[pending]
}

// hold locks exactly announcement's dispatch work row in an open
// transaction until release is called (or the test ends).
func (p *pendingFx) hold(announcement uuid.UUID) (release func()) {
	p.t.Helper()
	tx, err := p.pool.Begin(p.ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			if err := tx.Rollback(context.Background()); err != nil {
				p.t.Errorf("release the held work row: %v", err)
			}
		}
	}
	p.t.Cleanup(release)
	var held uuid.UUID
	if err := tx.QueryRow(p.ctx, `SELECT activity_id FROM federation_dispatch_work WHERE activity_id = $1 FOR UPDATE`, announcement).
		Scan(&held); err != nil || held != announcement {
		p.t.Fatalf("hold the announcement's dispatch work row: %v", err)
	}
	return release
}

// later records, with the actual Writer in its own committed transaction,
// an unrelated local Like of another instance's post. No received-grant
// check is wired, so the resolver gives it no recipient: it is only a later
// local activity for the dispatcher to handle and the cursor to reach.
func (p *pendingFx) later() uuid.UUID {
	p.t.Helper()
	ref := p.owner
	target := "https://elsewhere-" + uuid.NewString()[:8] + ".example/posts/" + uuid.NewString()
	var rec *activities.Record
	if err := pgx.BeginFunc(p.ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		rec, err = p.writer.RecordActivity(p.ctx, tx, activities.Input{
			Type:         federation.ActivityLike,
			ActivityURI:  base + "/activities/" + uuid.NewString(),
			ActorUserRef: &ref,
			ActorURI:     base + "/users/owner",
			Object:       &activities.ObjectRef{URI: target, Kind: activities.ObjectKindPost},
			Payload:      map[string]any{"target_uri": target, "target_peer_id": p.peer.String()},
		})
		return err
	}); err != nil {
		p.t.Fatal(err)
	}
	return rec.ID
}

// captures are a's metadata Updates as (version named, captured title).
func (p *pendingFx) captures(a asset) [][2]string {
	p.t.Helper()
	rows, err := p.pool.Query(p.ctx, `SELECT COALESCE(payload->>'version_id', ''), payload->>'metadata_capture' FROM activities
		WHERE source = 'local' AND activity_type = 'Update' AND object_local_id = $1 AND payload ? 'metadata_capture'
		ORDER BY published_at, id`, a.id.String())
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var version, raw string
		if err := rows.Scan(&version, &raw); err != nil {
			p.t.Fatal(err)
		}
		var c assetsend.MetadataCapture
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			p.t.Fatal(err)
		}
		title := ""
		if c.Snapshot.Title != nil {
			title = *c.Snapshot.Title
		}
		out = append(out, [2]string{version, title})
	}
	if err := rows.Err(); err != nil {
		p.t.Fatal(err)
	}
	return out
}

// heldBehindTheCursor holds announcement's dispatch work row, records a
// later B and lets the actual dispatcher handle B, so the cursor is at B
// (or at a local activity recorded after B), past the announcement, which
// stays pending with no outbox row. It returns B and the release of the
// held row.
func (p *pendingFx) heldBehindTheCursor(announcement uuid.UUID) (b uuid.UUID, release func()) {
	p.t.Helper()
	if n := p.dispatchWork(announcement); n != 1 {
		p.t.Fatalf("the announcement has %d dispatch work rows, want 1", n)
	}
	release = p.hold(announcement)
	b = p.later()
	if !p.before(announcement, b) {
		p.t.Fatal("the announcement does not sort before B")
	}
	p.dispatch()
	// The cursor is at B, or at a local activity recorded after B (any
	// other writer in the database): either way past the held announcement.
	if c := p.cursor(); c == uuid.Nil || (c != b && !p.before(b, c)) || !p.before(announcement, c) {
		p.t.Fatalf("the cursor is %s (%s), want B %s (%s) or later", c, p.describe(c), b, p.describe(b))
	}
	if n := p.dispatchWork(b); n != 0 {
		p.t.Fatalf("B was not handled: %d dispatch work rows", n)
	}
	if n := p.dispatchWork(announcement); n != 1 {
		p.t.Fatalf("the held announcement has %d dispatch work rows, want 1", n)
	}
	if o := p.outboxRows(announcement); len(o) != 0 {
		p.t.Fatalf("the held announcement has outbox rows %v", o)
	}
	return b, release
}

// edit changes a's title with an ordinary UPDATE, whose producer (migration
// 00100) queues the metadata work.
func (p *pendingFx) edit(a asset, title string) {
	p.t.Helper()
	p.dropFixtureWork()
	if _, err := p.pool.Exec(p.ctx, `UPDATE assets SET title = $2 WHERE id = $1`, a.id, title); err != nil {
		p.t.Fatal(err)
	}
	if n := p.count(`SELECT count(*) FROM federation_metadata_work`); n == 0 {
		p.t.Fatal("the title change queued no metadata work")
	}
}

// afterRelease releases the held announcement and lets the actual
// dispatcher run: the announcement is enqueued exactly once, the cursor
// never moves back, and a repeated reconcile announces nothing again.
func (p *pendingFx) afterRelease(a asset, announcement, b uuid.UUID, release func()) {
	p.t.Helper()
	release()
	p.dispatch()
	if o := p.outboxRows(announcement); len(o) != 1 || o["queued"] != 1 {
		p.t.Fatalf("after release the announcement has outbox rows %v, want one queued", o)
	}
	if n := p.dispatchWork(announcement); n != 0 {
		p.t.Fatalf("after release the announcement has %d dispatch work rows", n)
	}
	if c := p.cursor(); c != b && !p.before(b, c) {
		p.t.Fatalf("the cursor moved back from B %s to %s", b, c)
	}
	if ids := p.reconcileAgain(a); len(ids) != 0 {
		p.t.Fatalf("a reconcile after release announced %v again", ids)
	}
	p.work()
	if n := p.count(`SELECT count(*) FROM federation_metadata_work`); n != 0 {
		p.t.Fatalf("%d metadata work rows left", n)
	}
}

// The rev796 schedule: the only announcement of the asset is held while
// the cursor passes it. A title change is still captured, naming that
// version, while the announcement is pending; nothing is lost and nothing
// is announced twice.
func TestPendingDispatch_MetadataOfAnAnnouncementBehindTheCursor(t *testing.T) {
	p := newPendingFx(t)
	a, g := p.eligible("Pending old title", "", "", "")
	if o := p.outboxRows(g.announcement); len(o) != 0 {
		t.Fatalf("the announcement already has outbox rows %v", o)
	}
	b, release := p.heldBehindTheCursor(g.announcement)
	fresh := "Pending fresh " + uuid.NewString()[:8]
	p.edit(a, fresh)
	p.work()
	got := p.captures(a)
	if len(got) != 1 || got[0] != [2]string{a.version.String(), fresh} {
		t.Fatalf("captures %v, want one of %q naming version %s", got, fresh, a.version)
	}
	if n := p.count(`SELECT count(*) FROM federation_metadata_work`); n != 0 {
		t.Fatalf("%d metadata work rows left after the capture", n)
	}
	if ids := p.reconcileAgain(a); len(ids) != 0 {
		t.Fatalf("a reconcile while the announcement is pending announced %v again", ids)
	}
	if n := p.versionAnnouncements(a, a.version); n != 1 {
		t.Fatalf("version 1 announced %d times while pending", n)
	}
	p.afterRelease(a, g.announcement, b, release)
	if n := p.versionAnnouncements(a, a.version); n != 1 {
		t.Fatalf("version 1 announced %d times", n)
	}
	if got := p.captures(a); len(got) != 1 {
		t.Fatalf("captures after release %v, want the one", got)
	}
}

// The highest usable version is a pending one behind the cursor: version 1
// was dispatched (queued) by the actual dispatcher, version 2's
// announcement is held while the cursor passes it. The capture names
// version 2, and a reconcile does not announce version 2 again.
func TestPendingDispatch_TheHighestVersionIsAPendingOneBehindTheCursor(t *testing.T) {
	p := newPendingFx(t)
	a, g := p.eligible("Pending old title", "", "", "")
	p.dispatch()
	if o := p.outboxRows(g.announcement); len(o) != 1 || o["queued"] != 1 {
		t.Fatalf("version 1's announcement has outbox rows %v, want one queued", o)
	}
	v1 := a.version
	ids, err := p.sealNext(&a, 2, true, nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("version 2: %v %v", ids, err)
	}
	if n, _ := p.announced(ids); !equal(n, 2) {
		t.Fatalf("announced %v, want version 2", n)
	}
	b, release := p.heldBehindTheCursor(ids[0])
	fresh := "Pending fresh " + uuid.NewString()[:8]
	p.edit(a, fresh)
	p.work()
	got := p.captures(a)
	if len(got) != 1 || got[0] != [2]string{a.version.String(), fresh} || a.version == v1 {
		t.Fatalf("captures %v, want one of %q naming version 2 %s (not version 1 %s)", got, fresh, a.version, v1)
	}
	if again := p.reconcileAgain(a); len(again) != 0 {
		t.Fatalf("a reconcile while version 2 is pending announced %v again", again)
	}
	p.afterRelease(a, ids[0], b, release)
	if n := p.versionAnnouncements(a, a.version); n != 1 {
		t.Fatalf("version 2 announced %d times", n)
	}
	if got := p.captures(a); len(got) != 1 {
		t.Fatalf("captures after release %v, want the one", got)
	}
}
