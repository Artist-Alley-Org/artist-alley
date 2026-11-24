// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 against a live database: the durable metadata work ledger
// (metadata_work.go). Each test reads the ledger, the cursors and the
// recorded activities after every page, with a fresh processor for each
// page. Skips without AA_DB_PASSWORD.

package assetsend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

type workRow struct {
	id, kind                       string
	asset, assetCursor, peerCursor *uuid.UUID
}

// clearWork empties the ledger before and after the test (nothing else
// in the package writes it).
func (f *fx) clearWork(t *testing.T) {
	t.Helper()
	f.exec(`DELETE FROM federation_metadata_work`)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_metadata_work`) })
}

// dropFixtureWork deletes the work migration 00100's producers queued
// while the test built its fixtures (an asset's description, its seeded
// values, its grants, a peer's profiles), so the test sees only the work
// it asserts on.
func (f *fx) dropFixtureWork() {
	f.t.Helper()
	f.exec(`DELETE FROM federation_metadata_work`)
}

func (f *fx) ledger() []workRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id, kind, asset_id, asset_cursor, peer_cursor FROM federation_metadata_work ORDER BY created_at, id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []workRow
	for rows.Next() {
		var w workRow
		if err := rows.Scan(&w.id, &w.kind, &w.asset, &w.assetCursor, &w.peerCursor); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, w)
	}
	return out
}

// processor is a fresh MetadataWork with username as the resolver.
func (f *fx) processor(username func(context.Context, int64) string) assetsend.MetadataWork {
	if username == nil {
		username = func(context.Context, int64) string { return "owner" }
	}
	return assetsend.MetadataWork{Pool: f.pool, Writer: f.writer, BaseURL: base, Username: username}
}

func (f *fx) runWork(limit int) bool {
	f.t.Helper()
	worked, err := f.processor(nil).RunOnce(context.Background(), limit)
	if err != nil {
		f.t.Fatal(err)
	}
	return worked
}

func (f *fx) enqueue(a asset) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		id, err = assetsend.EnqueueAssetMetadataWork(context.Background(), tx, a.id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// updatesTo are the asset's metadata Updates, oldest first, as (peer of
// their share, activity id, published).
type sentUpdate struct {
	peer      uuid.UUID
	id        uuid.UUID
	published time.Time
	title     string
}

func (f *fx) metadataUpdates(a asset) []sentUpdate {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT s.peer_id, act.id, act.published_at, act.payload->>'metadata_capture'
		  FROM activities act JOIN federation_shares s ON s.id::text = act.payload->>'share_id'
		 WHERE act.source = 'local' AND act.activity_type = 'Update' AND act.object_local_id = $1 AND act.payload ? 'metadata_capture'
		 ORDER BY act.published_at, act.id`, a.id.String())
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []sentUpdate
	for rows.Next() {
		var u sentUpdate
		var capture string
		if err := rows.Scan(&u.peer, &u.id, &u.published, &capture); err != nil {
			f.t.Fatal(err)
		}
		var c assetsend.MetadataCapture
		if err := json.Unmarshal([]byte(capture), &c); err != nil {
			f.t.Fatal(err)
		}
		if c.Snapshot.Title != nil {
			u.title = *c.Snapshot.Title
		}
		out = append(out, u)
	}
	return out
}

// sharedWith makes n more peers with all three profiles, each granted a
// (its announcement recorded, its aa:Share sent), besides f.peer; it
// returns all the peers in UUID order.
func (f *fx) sharedWith(a asset, n int) []uuid.UUID {
	f.t.Helper()
	peers := []uuid.UUID{f.peer}
	for range n {
		p := f.addPeer()
		f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, p, allMetadataProfiles)
		if g := f.grantPeer(a, p, true); g.announcement == uuid.Nil {
			f.t.Fatal("not announced")
		}
		peers = append(peers, p)
	}
	slices.SortFunc(peers, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	return peers
}

// One obligation sends each peer one Update, a page at a time, each page
// committed with its cursor; an empty run then records nothing.
func TestMetadataWork_PagesThroughThePeersOnce(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	peers := f.sharedWith(a, 4) // 5 peers
	f.dropFixtureWork()
	f.enqueue(a)

	wantCursor := []*uuid.UUID{&peers[1], &peers[3]}
	for page, want := range []int{2, 4, 5} {
		if !f.runWork(2) {
			t.Fatalf("page %d: no work", page+1)
		}
		got := f.metadataUpdates(a)
		if len(got) != want {
			t.Fatalf("page %d: %d Updates, want %d", page+1, len(got), want)
		}
		l := f.ledger()
		if page < 2 {
			if len(l) != 1 || l[0].peerCursor == nil || *l[0].peerCursor != *wantCursor[page] {
				t.Fatalf("page %d: ledger %+v", page+1, l)
			}
		} else if len(l) != 0 {
			t.Fatalf("after the last page the ledger holds %+v", l)
		}
	}
	if f.runWork(2) {
		t.Fatal("an empty ledger did work")
	}
	got := f.metadataUpdates(a)
	seen := map[uuid.UUID]int{}
	for i, u := range got {
		seen[u.peer]++
		if i > 0 && !u.published.After(got[i-1].published) {
			t.Fatalf("published not strictly increasing: %v", got)
		}
	}
	for _, p := range peers {
		if seen[p] != 1 {
			t.Fatalf("peer %s got %d Updates", p, seen[p])
		}
	}
	var claims int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_asset_announcement_claims WHERE asset_id = $1`, a.id).Scan(&claims)
	if claims != 0 {
		t.Fatalf("%d claims: the queued announcements were built", claims)
	}
}

// An obligation commits and rolls back with the change that requires it;
// bad input and bad page sizes are refused.
func TestMetadataWork_EnqueueGoesWithTheChange(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.clearWork(t)
	a := f.newAsset("public", true)
	f.dropFixtureWork()
	// The title change queues its own obligation (migration 00100) and
	// the helper adds another, separate one: both commit with the change.
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE assets SET title = 'Committed' WHERE id = $1`, a.id); err != nil {
			return err
		}
		_, err := assetsend.EnqueueAssetMetadataWork(ctx, tx, a.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if l := f.ledger(); len(l) != 2 || l[0].id == l[1].id ||
		l[0].kind != "asset" || *l[0].asset != a.id || l[0].peerCursor != nil ||
		l[1].kind != "asset" || *l[1].asset != a.id || l[1].peerCursor != nil {
		t.Fatalf("ledger %+v", l)
	}
	rollback := errors.New("roll back")
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE assets SET title = 'Rolled back' WHERE id = $1`, a.id); err != nil {
			return err
		}
		if _, err := assetsend.EnqueueAssetMetadataWork(ctx, tx, a.id); err != nil {
			return err
		}
		if _, err := assetsend.EnqueueAllMetadataWork(ctx, tx); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var title string
	_ = f.pool.QueryRow(ctx, `SELECT title FROM assets WHERE id = $1`, a.id).Scan(&title)
	if l := f.ledger(); len(l) != 2 || title != "Committed" {
		t.Fatalf("after a rollback: %d rows, title %q", len(l), title)
	}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := assetsend.EnqueueAssetMetadataWork(ctx, tx, uuid.Nil)
		return err
	}); err == nil {
		t.Fatal("a nil asset was enqueued")
	}
	if _, err := assetsend.EnqueueAssetMetadataWork(ctx, nil, a.id); err == nil {
		t.Fatal("work was enqueued without a transaction")
	}
	if _, err := assetsend.EnqueueAllMetadataWork(ctx, nil); err == nil {
		t.Fatal("work was enqueued without a transaction")
	}
	for _, limit := range []int{-1, assetsend.MaxMetadataWorkPage + 1} {
		if worked, err := f.processor(nil).RunOnce(ctx, limit); !errors.Is(err, assetsend.ErrMetadataWorkPage) || worked {
			t.Fatalf("limit %d: %v %v", limit, worked, err)
		}
	}
	if l := f.ledger(); len(l) != 2 {
		t.Fatalf("a refused page size consumed work: %+v", l)
	}
	if _, err := (assetsend.MetadataWork{Pool: f.pool}).RunOnce(ctx, 2); err == nil {
		t.Fatal("a processor without a writer ran")
	}
}

// A page that fails partway keeps nothing, its first recorded Update
// included; a fresh processor then does the same page.
func TestMetadataWork_ACancelledPageKeepsNothing(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	peers := f.sharedWith(a, 2)
	f.dropFixtureWork()
	f.enqueue(a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := f.processor(func(context.Context, int64) string {
		calls++
		if calls == 2 {
			cancel()
		}
		return "owner"
	}).RunOnce(ctx, 2)
	if err == nil || calls != 2 {
		t.Fatalf("the cancelled page: %v after %d recorder calls", err, calls)
	}
	if got := f.metadataUpdates(a); len(got) != 0 {
		t.Fatalf("%d Updates survived the rollback", len(got))
	}
	if l := f.ledger(); len(l) != 1 || l[0].peerCursor != nil {
		t.Fatalf("ledger after the rollback %+v", l)
	}
	if !f.runWork(2) {
		t.Fatal("no work")
	}
	got := f.metadataUpdates(a)
	if len(got) != 2 || got[0].peer == got[1].peer {
		t.Fatalf("the repeated page: %+v", got)
	}
	if l := f.ledger(); len(l) != 1 || *l[0].peerCursor != peers[1] {
		t.Fatalf("ledger %+v", l)
	}
}

// While one processor holds a row, another skips it rather than spend
// the same page: each peer still gets one Update. Every wait is bounded
// and the held processor is always released, so a lock regression fails
// promptly instead of hanging the suite.
func TestMetadataWork_ALockedRowIsNotSpentTwice(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	f.sharedWith(a, 1)
	f.dropFixtureWork()
	f.enqueue(a)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inside, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	// Runs before cancel: the held processor is let go, then awaited.
	defer func() {
		letGo()
		wg.Wait()
	}()
	go func() {
		defer wg.Done()
		_, err := f.processor(func(context.Context, int64) string {
			once.Do(func() {
				close(inside)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			return "owner"
		}).RunOnce(ctx, 2)
		done <- err
	}()
	select {
	case <-inside:
	case err := <-done:
		t.Fatalf("the first processor finished before holding the row: %v", err)
	case <-ctx.Done():
		t.Fatal("the first processor never reached its first peer")
	}
	worked, err := f.processor(nil).RunOnce(ctx, 2)
	letGo()
	if err != nil || worked {
		t.Fatalf("the second processor: worked %v, %v", worked, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the first processor never finished after its release")
	}
	if got := f.metadataUpdates(a); len(got) != 2 || got[0].peer == got[1].peer {
		t.Fatalf("Updates %+v", got)
	}
	// The page was exactly full: one more, empty, turn completes the row.
	if l := f.ledger(); len(l) != 1 || l[0].peerCursor == nil {
		t.Fatalf("ledger %+v", l)
	}
	if !f.runWork(2) {
		t.Fatal("no work")
	}
	if l := f.ledger(); len(l) != 0 || len(f.metadataUpdates(a)) != 2 {
		t.Fatalf("the empty turn: ledger %+v, %d Updates", l, len(f.metadataUpdates(a)))
	}
}

// A change committed after a page passed a peer gets its own obligation,
// which starts from the first peer. Here the change makes the title
// private: the earlier Update that carried it is refused at delivery, and
// the later one, with a new identity and time, clears it.
func TestMetadataWork_ALaterChangeStartsFromTheFirstPeer(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Public title", "", "", "")
	peers := f.sharedWith(a, 2) // 3 peers
	f.dropFixtureWork()
	f.enqueue(a)
	if !f.runWork(2) {
		t.Fatal("no work")
	}
	first := f.metadataUpdates(a)
	if len(first) != 2 || first[0].title != "Public title" {
		t.Fatalf("first page %+v", first)
	}

	// Making the title mirror private queues, in its transaction, one
	// obligation for every asset (migration 00100), separate from the one
	// in progress.
	id := f.mirrorOf("title")
	f.restoreDefinition(t, id)
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	l := f.ledger()
	if len(l) != 2 || l[0].kind != "asset" || l[0].peerCursor == nil || l[1].kind != "all" || l[1].assetCursor != nil {
		t.Fatalf("ledger %+v", l)
	}
	for f.runWork(2) {
	}
	all := f.metadataUpdates(a)
	if len(all) != 6 {
		t.Fatalf("%d Updates, want 3 for the first change and 3 for the second", len(all))
	}
	perPeer := map[uuid.UUID][]sentUpdate{}
	for _, u := range all {
		perPeer[u.peer] = append(perPeer[u.peer], u)
	}
	for _, p := range peers {
		us := perPeer[p]
		if len(us) != 2 || us[0].id == us[1].id || !us[1].published.After(us[0].published) || us[1].title != "" {
			t.Fatalf("peer %s: %+v", p, us)
		}
	}
	// The first page's Update to the first peer still names the public
	// title: delivery refuses it; the later one is a clear.
	for _, u := range []sentUpdate{first[0], perPeer[first[0].peer][1]} {
		m := f.metaUpdate(u.id)
		_, metadata, err := f.src.MetadataUpdateMembers(ctx, u.id, u.peer, m.objectURI, m.raw)
		if u.title != "" {
			if !errors.Is(err, federation.ErrNotFederated) {
				t.Fatalf("the earlier Update with the now private title: %v", err)
			}
		} else if err != nil || !bytes.Contains(metadata, []byte(`"title":null`)) {
			t.Fatalf("the later Update: %s %v", metadata, err)
		}
	}
}

// An 'all' obligation adds at most a page of asset obligations per call,
// durably; its completion leaves them; another one queued meanwhile is
// separate and starts from the first asset.
func TestMetadataWork_EveryAssetIsAddedAPageAtATime(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)
	shared, _ := f.eligible("Shared", "", "", "")
	others := []asset{f.newAsset("public", true), f.newAsset("team", true)}
	ours := map[uuid.UUID]bool{shared.id: true, others[0].id: true, others[1].id: true}
	f.dropFixtureWork()

	enqueueAll := func() {
		t.Helper()
		if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := assetsend.EnqueueAllMetadataWork(ctx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	enqueueAll()
	if !f.runWork(2) {
		t.Fatal("no work")
	}
	l := f.ledger()
	if len(l) != 3 || l[0].kind != "all" || l[0].assetCursor == nil || l[1].kind != "asset" || l[2].kind != "asset" {
		t.Fatalf("after one expansion: %+v", l)
	}
	enqueueAll() // a second change, while the first is in progress
	l = f.ledger()
	if second := l[len(l)-1]; second.kind != "all" || second.assetCursor != nil {
		t.Fatalf("the second request %+v", second)
	}

	// Expansions continue with fresh processors; each adds at most a page.
	var expansions int
	for {
		before := f.ledger()
		if len(before) == 0 || before[0].kind != "all" {
			break
		}
		cursor := before[0].assetCursor
		if !f.runWork(2) {
			t.Fatal("no work")
		}
		expansions++
		after := f.ledger()
		added := 0
		for _, w := range after {
			if w.kind == "asset" && !slices.ContainsFunc(before, func(b workRow) bool { return b.id == w.id }) {
				added++
			}
		}
		if added > 2 {
			t.Fatalf("one expansion added %d asset rows", added)
		}
		if len(after) > 0 && after[0].id == before[0].id && (after[0].assetCursor == nil || (cursor != nil && *after[0].assetCursor == *cursor)) {
			t.Fatalf("the expansion did not advance: %+v", after[0])
		}
		if expansions > 10000 {
			t.Fatal("the expansion does not end")
		}
	}
	// The first request is complete; its asset rows remain, ours among them.
	l = f.ledger()
	assetRows := map[uuid.UUID]int{}
	allRows := 0
	for _, w := range l {
		if w.kind == "asset" {
			assetRows[*w.asset]++
		} else {
			allRows++
		}
	}
	if allRows != 1 {
		t.Fatalf("%d all rows, want the second request only", allRows)
	}
	for id := range ours {
		if assetRows[id] != 1 {
			t.Fatalf("asset %s has %d rows after the first expansion", id, assetRows[id])
		}
	}
	// Everything drains; the shared asset's peer gets its Update from each
	// request.
	for i := 0; f.runWork(2); i++ {
		if i > 100000 {
			t.Fatal("the ledger does not drain")
		}
	}
	if got := f.metadataUpdates(shared); len(got) != 2 || got[0].peer != f.peer || got[1].peer != f.peer {
		t.Fatalf("the shared asset's Updates %+v", got)
	}
	for _, o := range others {
		if got := f.metadataUpdates(o); len(got) != 0 {
			t.Fatalf("an asset shared with nobody got %+v", got)
		}
	}
}

// insertCanceller cancels, while on, right after a statement on the work
// ledger that inserted rows has SUCCEEDED, so the failure lands after the
// child INSERT of an expansion and before its cursor advance.
type insertCanceller struct {
	mu       sync.Mutex
	on       bool
	sql      string
	inserted int64
	cancel   context.CancelFunc
}

func (c *insertCanceller) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = d.SQL
	return ctx
}

func (c *insertCanceller) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on && d.Err == nil && d.CommandTag.Insert() && d.CommandTag.RowsAffected() > 0 &&
		strings.Contains(c.sql, "INSERT INTO federation_metadata_work") && c.inserted == 0 {
		c.inserted = d.CommandTag.RowsAffected()
		c.cancel()
	}
}

// poolTracedBy is a pool of its own, on the test database, traced by tr.
func (f *fx) poolTracedBy(tr pgx.QueryTracer) *pgxpool.Pool {
	f.t.Helper()
	return f.poolWith(tr, nil)
}

// poolWith is a pool of its own, on the test database, traced by tr (if
// any) with the session parameters given.
func (f *fx) poolWith(tr pgx.QueryTracer, params map[string]string) *pgxpool.Pool {
	f.t.Helper()
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable pool_max_conns=2",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		os.Getenv("AA_DB_PASSWORD"), env("AA_DB_NAME", "artist_alley")))
	if err != nil {
		f.t.Fatal(err)
	}
	if tr != nil {
		cfg.ConnConfig.Tracer = tr
	}
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(pool.Close)
	return pool
}

// An expansion that fails after inserting its child rows and before
// advancing keeps nothing: no child row survives and the 'all' row is
// unchanged; a fresh processor then expands the same page.
func TestMetadataWork_ACancelledExpansionKeepsNothing(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.clearWork(t)
	for range 3 { // a full page of 2, whatever else the database holds
		f.newAsset("public", true)
	}
	f.dropFixtureWork()
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := assetsend.EnqueueAllMetadataWork(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := f.ledger()
	if len(before) != 1 || before[0].kind != "all" || before[0].assetCursor != nil {
		t.Fatalf("ledger %+v", before)
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tr := &insertCanceller{cancel: cancel}
	pool := f.poolTracedBy(tr)
	tr.mu.Lock()
	tr.on = true
	tr.mu.Unlock()
	worked, err := assetsend.MetadataWork{Pool: pool, Writer: f.writer, BaseURL: base,
		Username: func(context.Context, int64) string { return "owner" }}.RunOnce(runCtx, 2)
	tr.mu.Lock()
	tr.on = false
	inserted := tr.inserted
	tr.mu.Unlock()
	// Closing the pool ends its sessions, so nothing of the page can still
	// be pending in an open transaction when the ledger is read.
	pool.Close()
	if inserted != 2 {
		t.Fatalf("the child INSERT inserted %d rows before the cancellation, want a full page of 2", inserted)
	}
	if err == nil || !errors.Is(err, context.Canceled) || worked {
		t.Fatalf("the cancelled expansion: worked %v, %v", worked, err)
	}
	after := f.ledger()
	if len(after) != 1 || after[0].id != before[0].id || after[0].kind != "all" || after[0].assetCursor != nil {
		t.Fatalf("after the rollback the ledger is %+v, want only the untouched 'all' row", after)
	}

	// A fresh processor and context expand the same page.
	want := make([]uuid.UUID, 0, 2)
	rows, err := f.pool.Query(ctx, `SELECT id FROM assets WHERE origin_server_id IS NULL AND deleted_at IS NULL ORDER BY id LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !f.runWork(2) {
		t.Fatal("no work")
	}
	l := f.ledger()
	if len(l) != 3 || l[0].id != before[0].id || l[0].assetCursor == nil || *l[0].assetCursor != want[1] {
		t.Fatalf("the repeated expansion: %+v", l)
	}
	got := []uuid.UUID{*l[1].asset, *l[2].asset}
	slices.SortFunc(got, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	if l[1].kind != "asset" || l[2].kind != "asset" || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the repeated expansion added %+v, want asset rows for %v", l[1:], want)
	}
}

// Work for a missing, deleted or remote asset, or for one no peer can
// receive, drains without recording anything.
func TestMetadataWork_IneligibleWorkDrains(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)

	deleted, _ := f.eligible("Deleted", "", "", "")
	f.exec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, deleted.id)
	remote := asset{id: uuid.New()}
	f.exec(`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity, origin_server_id)
		VALUES ($1, 'Remote', 1, $2, 'public', gen_random_uuid())`, remote.id, f.owner)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, remote.id) })
	f.grantPeer(remote, f.peer, false)
	ungranted := f.newAsset("public", true)
	noProfile := f.newAsset("public", true)
	p := f.addPeer() // archival-1 and original-transfer-1 only
	f.grantPeer(noProfile, p, true)

	f.dropFixtureWork()
	for _, a := range []asset{{id: uuid.New()}, deleted, remote, ungranted, noProfile} {
		f.enqueue(a)
	}
	for i := 0; f.runWork(2); i++ {
		if i > 50 {
			t.Fatal("the ledger does not drain")
		}
	}
	if l := f.ledger(); len(l) != 0 {
		t.Fatalf("ledger %+v", l)
	}
	for _, a := range []asset{deleted, remote, ungranted, noProfile} {
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1`, a.id.String()).Scan(&n)
		if n != 0 {
			t.Fatalf("asset %s: %d Updates", a.id, n)
		}
	}
}

// defaultProcessor is the production form of the processor: no username
// resolver, so the owner is read in the page's transaction.
func (f *fx) defaultProcessor(pool *pgxpool.Pool) assetsend.MetadataWork {
	return assetsend.MetadataWork{Pool: pool, Writer: f.writer, BaseURL: base}
}

// actorOf is the actor URI of activity id.
func (f *fx) actorOf(id uuid.UUID) string {
	f.t.Helper()
	var actor string
	if err := f.pool.QueryRow(context.Background(), `SELECT actor_uri FROM activities WHERE id = $1`, id).Scan(&actor); err != nil {
		f.t.Fatal(err)
	}
	return actor
}

// Without a resolver the processor reads the owner's username in its own
// transaction: the Update names the owner's actor. An owner with no
// username, like one that is gone, is ineligible: the work drains and
// nothing is recorded.
func TestMetadataWork_TheDefaultActorIsTheOwner(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	var username string
	if err := f.pool.QueryRow(ctx, `SELECT username FROM "user" WHERE ref = $1`, f.owner).Scan(&username); err != nil {
		t.Fatal(err)
	}
	nameless, _ := f.eligible("Nameless", "", "", "")
	var other int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES (NULL, 'No name', 1) RETURNING ref`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, nameless.id, other)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, nameless.id, f.owner)
		_, _ = f.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, other)
	})
	f.dropFixtureWork()
	f.enqueue(a)
	f.enqueue(nameless)
	for i := 0; ; i++ {
		worked, err := f.defaultProcessor(f.pool).RunOnce(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
		if i > 10 {
			t.Fatal("the ledger does not drain")
		}
	}
	got := f.metadataUpdates(a)
	if len(got) != 1 {
		t.Fatalf("%d Updates", len(got))
	}
	if actor := f.actorOf(got[0].id); actor != base+"/users/"+username {
		t.Fatalf("actor %q, want the owner %q", actor, username)
	}
	if got := f.metadataUpdates(nameless); len(got) != 0 {
		t.Fatalf("an owner with no username got %+v", got)
	}
	if l := f.ledger(); len(l) != 0 {
		t.Fatalf("ledger %+v", l)
	}
}

// sqlCanceller cancels, while on, when a statement containing marker is
// about to run, recording that it was reached.
type sqlCanceller struct {
	mu      sync.Mutex
	on      bool
	marker  string
	reached int
	cancel  context.CancelFunc
}

func (c *sqlCanceller) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on && strings.Contains(d.SQL, c.marker) {
		c.reached++
		c.cancel()
	}
	return ctx
}

func (c *sqlCanceller) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A failed owner read, a database error or a cancellation, fails the
// whole page: no Update, the cursor unmoved. A fresh processor then
// records it.
func TestMetadataWork_AFailedOwnerReadKeepsNothing(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	f.clearWork(t)
	a, _ := f.eligible("Title", "", "", "")
	f.dropFixtureWork()
	f.enqueue(a)
	unchanged := func(what string) {
		t.Helper()
		if got := f.metadataUpdates(a); len(got) != 0 {
			t.Fatalf("%s: %d Updates survived", what, len(got))
		}
		if l := f.ledger(); len(l) != 1 || l[0].peerCursor != nil {
			t.Fatalf("%s: ledger %+v", what, l)
		}
	}

	// A database error: the user table is held, and the processor's
	// session gives up waiting for it.
	short := f.poolWith(nil, map[string]string{"lock_timeout": "200ms"})
	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx) //nolint:errcheck // a no-op once released
	if _, err := hold.Exec(ctx, `LOCK TABLE "user" IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRun()
	worked, err := f.defaultProcessor(short).RunOnce(runCtx, 2)
	var pgErr *pgconn.PgError
	if err == nil || worked || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the owner read under a held lock: worked %v, %v", worked, err)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	short.Close()
	unchanged("after the failed read")

	// A cancellation at the owner read.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tr := &sqlCanceller{marker: `LEFT JOIN "user"`, cancel: cancel}
	traced := f.poolTracedBy(tr)
	tr.mu.Lock()
	tr.on = true
	tr.mu.Unlock()
	worked, err = f.defaultProcessor(traced).RunOnce(cctx, 2)
	tr.mu.Lock()
	tr.on = false
	reached := tr.reached
	tr.mu.Unlock()
	traced.Close()
	if reached != 1 || err == nil || !errors.Is(err, context.Canceled) || worked {
		t.Fatalf("the cancelled owner read: reached %d, worked %v, %v", reached, worked, err)
	}
	unchanged("after the cancellation")

	// A fresh processor records the Update.
	worked, err = f.defaultProcessor(f.pool).RunOnce(ctx, 2)
	if err != nil || !worked {
		t.Fatalf("the fresh processor: worked %v, %v", worked, err)
	}
	if got := f.metadataUpdates(a); len(got) != 1 {
		t.Fatalf("%d Updates after the fresh run", len(got))
	}
}
