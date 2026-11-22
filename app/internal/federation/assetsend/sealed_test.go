// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetsend_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

// #1575: a newly sealed version is announced under the asset's existing
// shares (RecordSealed, in the sealing transaction), and a peer is
// caught up in order from the highest version it has or will receive.

// sealed is the RecordSealed input of the fixture's asset owner.
func sealedInput(id uuid.UUID) assetsend.Sealed {
	return assetsend.Sealed{AssetID: id, BaseURL: base, Username: func(context.Context, int64) string { return "owner" }}
}

// storedBytes stores fresh verified bytes and returns their hash.
func (f *fx) storedBytes() string {
	f.t.Helper()
	ctx := context.Background()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	hash := hex.EncodeToString(b)
	for _, q := range []string{
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`,
	} {
		if _, err := f.pool.Exec(ctx, q, hash); err != nil {
			f.t.Fatal(err)
		}
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return hash
}

// sealNext creates version n of a over fresh bytes, makes it current and,
// when seal is true, seals it and runs RecordSealed, in ONE transaction,
// as the version reconciler does. fail makes the transaction fail after
// RecordSealed. It returns the announcements recorded.
func (f *fx) sealNext(a *asset, n int64, seal bool, fail error) ([]uuid.UUID, error) {
	f.t.Helper()
	ctx := context.Background()
	hash := f.storedBytes()
	v := uuid.New()
	var ids []uuid.UUID
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT asset_version_create($1, $2, $3, $4, 'replacement', $5, 'png', NULL, 'unknown', NULL)`,
			a.id, v, n, a.version, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET current_version_id = $2, file_hash = $3 WHERE id = $1`, a.id, v, hash); err != nil {
			return err
		}
		if !seal {
			return nil
		}
		if _, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, v); err != nil {
			return err
		}
		var err error
		if ids, err = assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id)); err != nil {
			return err
		}
		return fail
	})
	if err == nil {
		a.version, a.hash = v, hash
	}
	return ids, err
}

// sealCurrent seals a's current draft version and runs RecordSealed in
// the same transaction.
func (f *fx) sealCurrent(a asset) []uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, a.version); err != nil {
			return err
		}
		var err error
		ids, err = assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id))
		return err
	}); err != nil {
		f.t.Fatalf("seal: %v", err)
	}
	return ids
}

// reconcileAgain runs RecordSealed again for a, as a repeated reconcile
// of an unchanged sealed version would if it announced.
func (f *fx) reconcileAgain(a asset) []uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		var err error
		ids, err = assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id))
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

// dispatched records each announcement's outbox row to peer in status, as
// the dispatcher would after it resolved the activity.
func (f *fx) dispatched(ids []uuid.UUID, peer uuid.UUID, status string) {
	f.t.Helper()
	for _, id := range ids {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, $3)
			ON CONFLICT (activity_id, peer_id) WHERE target_user_url IS NULL DO UPDATE SET status = EXCLUDED.status`, id, peer, status); err != nil {
			f.t.Fatal(err)
		}
	}
}

// announced lists the version numbers the announcements name, in the
// order given, with the share each rides on.
func (f *fx) announced(ids []uuid.UUID) (numbers []int64, shares []string) {
	f.t.Helper()
	for _, id := range ids {
		var raw []byte
		var typ, local string
		if err := f.pool.QueryRow(context.Background(), `SELECT activity_type, object_local_id, payload FROM activities WHERE id = $1`, id).
			Scan(&typ, &local, &raw); err != nil {
			f.t.Fatal(err)
		}
		var p map[string]string
		_ = json.Unmarshal(raw, &p)
		var n int64
		if err := f.pool.QueryRow(context.Background(), `SELECT number FROM asset_versions WHERE id::text = $1 AND asset_id::text = $2`,
			p["version_id"], local).Scan(&n); err != nil || typ != "aa:AssetVersion" {
			f.t.Fatalf("announcement %s: %s of %s (%v)", id, typ, p["version_id"], err)
		}
		numbers = append(numbers, n)
		shares = append(shares, p["share_id"])
	}
	return numbers, shares
}

// versionAnnouncements counts a's aa:AssetVersion activities of a version.
func (f *fx) versionAnnouncements(a asset, version uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM activities WHERE activity_type = 'aa:AssetVersion'
		AND object_local_id = $1 AND payload->>'version_id' = $2`, a.id.String(), version.String()).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func equal(a []int64, b ...int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRecordSealed_ANewVersionIsAnnouncedUnderTheExistingShare(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
	v1 := a.version

	ids, err := f.sealNext(&a, 2, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	nums, shares := f.announced(ids)
	if !equal(nums, 2) || shares[0] != g.share.String() {
		t.Fatalf("announced %v under %v, want version 2 under the share", nums, shares)
	}
	var p map[string]string
	var raw []byte
	var objectURI string
	if err := f.pool.QueryRow(context.Background(), `SELECT object_uri, payload FROM activities WHERE id = $1`, ids[0]).Scan(&objectURI, &raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &p)
	if p["share_activity_id"] != g.shareActivity.String() || objectURI != a.uri {
		t.Fatalf("payload %v about %s", p, objectURI)
	}
	// Built after the share was delivered, it is version 2 following
	// version 1, from its sealed set.
	f.shareDelivered(g, "sent")
	m, err := f.member(grant{announcement: ids[0], objectURI: objectURI, payload: raw})
	if err != nil || m.Version.Number != 2 || m.Version.Predecessor == nil || *m.Version.Predecessor != a.uri+"/versions/1" ||
		m.Version.ID != a.uri+"/versions/2" {
		t.Fatalf("member %+v %v", m.Version, err)
	}
	_ = v1

	// Repeated reconciles, with the announcement queued, then sent, add
	// nothing (not dispatched yet: TestRecordSealed_WhatCountsAsAnnounced).
	for _, status := range []string{"queued", "sent"} {
		f.dispatched(ids, f.peer, status)
		if again := f.reconcileAgain(a); len(again) != 0 {
			t.Fatalf("a repeated reconcile (%q) announced %v again", status, again)
		}
	}
	if n := f.versionAnnouncements(a, a.version); n != 1 {
		t.Fatalf("version 2 announced %d times", n)
	}
}

func TestRecordSealed_APeerIsCaughtUpInOrder(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
	// Versions 2 and 3 seal while the peer is disabled: nothing is
	// announced to it.
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	for n := int64(2); n <= 3; n++ {
		if ids, err := f.sealNext(&a, n, true, nil); err != nil || len(ids) != 0 {
			t.Fatalf("version %d while the peer is disabled: %v %v", n, ids, err)
		}
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	// Version 4: the peer holds version 1, so it gets 2, 3 and 4, in order.
	ids, err := f.sealNext(&a, 4, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nums, _ := f.announced(ids); !equal(nums, 2, 3, 4) {
		t.Fatalf("caught up with %v, want 2 3 4", nums)
	}
	f.dispatched(ids, f.peer, "queued")
	if again := f.reconcileAgain(a); len(again) != 0 {
		t.Fatalf("a repeated reconcile announced %v", again)
	}

	t.Run("a peer that holds nothing gets the current version alone", func(t *testing.T) {
		b := f.newAsset("public", true)
		gb := f.grantAndRecord(b, nil)
		f.dispatched([]uuid.UUID{gb.announcement}, f.peer, "failed") // never reached the peer
		f.revokeShare(gb)
		// A new share, whose own announcement of version 1 also failed.
		gb2 := f.grantAndRecord(b, nil)
		f.dispatched([]uuid.UUID{gb2.announcement}, f.peer, "refused")
		// Version 2 seals while the peer is disabled (announced to nobody).
		f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
		if ids, err := f.sealNext(&b, 2, true, nil); err != nil || len(ids) != 0 {
			t.Fatalf("version 2 while disabled: %v %v", ids, err)
		}
		f.exec(`UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peer)
		// Version 3: neither refused nor failed announcements of version 1
		// count, so the peer holds nothing and gets version 3 alone (not
		// 2 and 3, as if it held version 1).
		ids, err := f.sealNext(&b, 3, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if nums, _ := f.announced(ids); !equal(nums, 3) {
			t.Fatalf("announced %v, want version 3 alone", nums)
		}
	})
}

func TestRecordSealed_WhatCountsAsAnnounced(t *testing.T) {
	f := newFx(t)

	t.Run("a queued announcement under a live share counts", func(t *testing.T) {
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "queued")
		ids, err := f.sealNext(&a, 2, true, nil)
		if nums, _ := f.announced(ids); err != nil || !equal(nums, 2) {
			t.Fatalf("announced %v (%v), want version 2 only", nums, err)
		}
	})
	t.Run("a sent announcement counts after its share ended", func(t *testing.T) {
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		f.revokeShare(g)
		g2 := f.grantAndRecord(a, nil) // announces version 1 again under the new share
		f.dispatched([]uuid.UUID{g2.announcement}, f.peer, "queued")
		ids, err := f.sealNext(&a, 2, true, nil)
		if nums, sh := f.announced(ids); err != nil || !equal(nums, 2) || sh[0] != g2.share.String() {
			t.Fatalf("announced %v under %v (%v), want version 2 under the new share", nums, sh, err)
		}
	})
	t.Run("a queued announcement under a revoked share does not count", func(t *testing.T) {
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		if _, err := f.sealNext(&a, 2, true, nil); err != nil {
			t.Fatal(err)
		}
		// Version 2's announcement is still queued when the share is
		// revoked: it is refused for good at delivery.
		v2 := a.version
		var v2ann uuid.UUID
		if err := f.pool.QueryRow(context.Background(), `SELECT id FROM activities WHERE activity_type = 'aa:AssetVersion' AND payload->>'version_id' = $1`,
			v2.String()).Scan(&v2ann); err != nil {
			t.Fatal(err)
		}
		f.dispatched([]uuid.UUID{v2ann}, f.peer, "queued")
		f.revokeShare(g)
		// A regrant at version 3 catches the peer up from version 1.
		if _, err := f.sealNext(&a, 3, true, nil); err != nil {
			t.Fatal(err)
		}
		g2 := f.grantAndRecord(a, nil)
		var ids []uuid.UUID
		if err := f.pool.QueryRow(context.Background(), `SELECT array_agg(id ORDER BY created_at, (payload->>'version_id'))
			FROM activities WHERE activity_type = 'aa:AssetVersion' AND payload->>'share_id' = $1`, g2.share.String()).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		nums, _ := f.announced(ids)
		got := map[int64]bool{}
		for _, n := range nums {
			got[n] = true
		}
		if len(nums) != 2 || !got[2] || !got[3] || g2.announcement == uuid.Nil {
			t.Fatalf("the regrant announced %v, want 2 and 3", nums)
		}
		if n, _ := f.announced([]uuid.UUID{g2.announcement}); !equal(n, 3) {
			t.Fatalf("Record returned the announcement of %v, want the current version 3", n)
		}
	})
	t.Run("not dispatched yet counts; dispatched without a row does not", func(t *testing.T) {
		ctx := context.Background()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		ids, err := f.sealNext(&a, 2, true, nil)
		if err != nil || len(ids) != 1 {
			t.Fatalf("version 2: %v %v", ids, err)
		}
		// Both cases in one transaction that is rolled back: the
		// dispatcher's cursor is shared, and is moved only here.
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		// The cursor before version 2's announcement: not dispatched yet,
		// so it counts, and a repeated reconcile adds nothing.
		if _, err := tx.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, g.announcement); err != nil {
			t.Fatal(err)
		}
		if again, err := assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id)); err != nil || len(again) != 0 {
			t.Fatalf("not dispatched yet: announced %v (%v)", again, err)
		}
		// The cursor at it, with no row for the peer (skipped at
		// dispatch): it never reaches the peer, so version 2 is announced
		// again.
		if _, err := tx.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, ids[0]); err != nil {
			t.Fatal(err)
		}
		again, err := assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id))
		if nums, _ := f.announcedTx(tx, again); err != nil || !equal(nums, 2) {
			t.Fatalf("skipped at dispatch: announced %v (%v), want version 2 again", nums, err)
		}
	})
}

// announcedTx is announced, read inside tx.
func (f *fx) announcedTx(tx pgx.Tx, ids []uuid.UUID) ([]int64, error) {
	var out []int64
	for _, id := range ids {
		var n int64
		if err := tx.QueryRow(context.Background(), `SELECT v.number FROM activities a JOIN asset_versions v ON v.id::text = a.payload->>'version_id'
			WHERE a.id = $1`, id).Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func TestRecordSealed_OnlyEligibleShares(t *testing.T) {
	f := newFx(t)
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name  string
		tier  string
		setup func(a asset, g grant)
	}{
		{"a revoked share", "public", func(a asset, g grant) { f.revokeShare(g) }},
		{"an expired share", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_shares SET expires_at = now() - interval '1 second' WHERE id = $1`, g.share)
		}},
		{"a share without the original", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_shares SET allow_original = FALSE WHERE id = $1`, g.share)
		}},
		{"a share to one recipient", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_shares SET target_user_url = 'https://receiver.example/users/bob' WHERE id = $1`, g.share)
		}},
		{"a disabled peer", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
		}},
		{"a disconnected peer", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`, f.peer)
		}},
		{"a peer without original-transfer-1", "public", func(a asset, g grant) {
			f.exec(`UPDATE federation_peers SET capabilities = '["share-lifecycle-1","archival-1"]'::jsonb WHERE id = $1`, f.peer)
		}},
		{"a restricted asset", "public", func(a asset, g grant) {
			f.exec(`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, a.id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.peer = f.addPeer()
			a := f.newAsset(tc.tier, true)
			g := f.grantAndRecord(a, nil)
			f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
			tc.setup(a, g)
			ids, err := f.sealNext(&a, 2, true, nil)
			if err != nil || len(ids) != 0 {
				t.Fatalf("announced %v (%v)", ids, err)
			}
		})
	}
	t.Run("a share still in effect", func(t *testing.T) {
		f.peer = f.addPeer()
		a := f.newAsset("team", true)
		g := f.grantAndRecord(a, nil)
		f.exec(`UPDATE federation_shares SET expires_at = $2 WHERE id = $1`, g.share, future)
		ids, err := f.sealNext(&a, 2, true, nil)
		if nums, _ := f.announced(ids); err != nil || !equal(nums, 2) {
			t.Fatalf("announced %v (%v)", nums, err)
		}
	})
}

func TestRecordSealed_ADraftIsNeverAnnouncedAndARollbackLeavesNothing(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
	// Version 2 stays a draft (version 3 follows it before it is sealed).
	if _, err := f.sealNext(&a, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	draft := a.version
	ids, err := f.sealNext(&a, 3, true, nil)
	if nums, _ := f.announced(ids); err != nil || !equal(nums, 3) {
		t.Fatalf("announced %v (%v), want version 3 only", nums, err)
	}
	if n := f.versionAnnouncements(a, draft); n != 0 {
		t.Fatalf("the draft was announced %d times", n)
	}

	// A seal that fails after RecordSealed commits neither the seal nor
	// any announcement.
	before := a.version
	boom := errors.New("the seal fails")
	if _, err := f.sealNext(&a, 4, true, boom); !errors.Is(err, boom) {
		t.Fatalf("seal: %v", err)
	}
	var versions, announcements int
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM asset_versions WHERE asset_id = $1 AND number = 4),
		(SELECT count(*) FROM activities WHERE activity_type = 'aa:AssetVersion' AND object_local_id = $1::text
		   AND payload->>'version_id' NOT IN (SELECT id::text FROM asset_versions WHERE asset_id = $1))`, a.id).Scan(&versions, &announcements); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || announcements != 0 || a.version != before {
		t.Fatalf("after the failed seal: %d versions 4, %d announcements of no version", versions, announcements)
	}
}

func TestRecord_AGrantDuringASealWaitsForItAndAnnouncesTheNewVersion(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset("public", true)
	// A seal in progress holds the asset row (the reconciler's lock), with
	// version 2 sealed and current but not committed.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	hash := f.storedBytes()
	v2 := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, []any{a.id}},
		{`SELECT asset_version_create($1, $2, 2, $3, 'replacement', $4, 'png', NULL, 'unknown', NULL)`, []any{a.id, v2, a.version, hash}},
		{`UPDATE assets SET current_version_id = $2, file_hash = $3 WHERE id = $1`, []any{a.id, v2, hash}},
		{`SELECT asset_version_seal($1, '[]'::jsonb)`, []any{v2}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		g   grant
		err error
	}
	done := make(chan result, 1)
	go func() { g, err := f.grantAndRecordErr(a, nil); done <- result{g, err} }()
	select {
	case r := <-done:
		t.Fatalf("the grant did not wait for the seal in progress (announced %v, %v)", r.g.announcement, r.err)
	case <-time.After(400 * time.Millisecond):
	}
	// The seal's own announcements see no share yet (the grant has not
	// committed).
	if ids, err := assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id)); err != nil || len(ids) != 0 {
		t.Fatalf("the seal announced %v (%v) before the grant committed", ids, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	g := r.g
	if nums, _ := f.announced([]uuid.UUID{g.announcement}); !equal(nums, 2) {
		t.Fatalf("the grant announced %v, want the version sealed before it, 2", nums)
	}
}

func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), q, args...); err != nil && !strings.Contains(err.Error(), "no rows") {
		f.t.Fatal(err)
	}
}

// rev657: what counts is the contiguous chain a peer holds, not its
// highest usable version. A usable version above a missing one does not
// stand in for it: the receiver cannot accept it without its predecessor.
func TestRecordSealed_AMissingPredecessorIsRepairedNotSkipped(t *testing.T) {
	f := newFx(t)
	for _, dead := range []string{"failed", "refused", "cancelled"} {
		t.Run("on a seal, v2 "+dead+" under v3 still queued", func(t *testing.T) {
			f.peer = f.addPeer()
			a := f.newAsset("public", true)
			g := f.grantAndRecord(a, nil)
			f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent") // v1 reached the peer
			ids2, err := f.sealNext(&a, 2, true, nil)
			if err != nil || len(ids2) != 1 {
				t.Fatalf("version 2: %v %v", ids2, err)
			}
			f.dispatched(ids2, f.peer, "queued")
			ids3, err := f.sealNext(&a, 3, true, nil)
			if nums, _ := f.announced(ids3); err != nil || !equal(nums, 3) {
				t.Fatalf("version 3: %v %v", nums, err)
			}
			f.dispatched(ids3, f.peer, "queued")
			f.dispatched(ids2, f.peer, dead) // v2 never reaches the peer; v3 still queued, share live
			ids4, err := f.sealNext(&a, 4, true, nil)
			if nums, sh := f.announced(ids4); err != nil || !equal(nums, 2, 3, 4) || sh[0] != g.share.String() {
				t.Fatalf("version 4 announced %v under %v (%v), want 2 3 4: version 2 repaired and what follows it again", nums, sh, err)
			}
			// Repaired and all on their way: a repeated reconcile adds nothing.
			f.dispatched(ids4, f.peer, "queued")
			if again := f.reconcileAgain(a); len(again) != 0 {
				nums, _ := f.announced(again)
				t.Fatalf("a repeated reconcile announced %v", nums)
			}
		})
	}
	t.Run("on a regrant, v2 failed under v3 sent, the old share revoked", func(t *testing.T) {
		f.peer = f.addPeer()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		ids2, err := f.sealNext(&a, 2, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.dispatched(ids2, f.peer, "queued") // still on its way when version 3 seals
		ids3, err := f.sealNext(&a, 3, true, nil)
		if nums, _ := f.announced(ids3); err != nil || !equal(nums, 3) {
			t.Fatalf("version 3: %v %v", nums, err)
		}
		f.dispatched(ids3, f.peer, "sent")   // delivered, but its predecessor never was:
		f.dispatched(ids2, f.peer, "failed") // version 2 failed after version 3 was sent
		f.revokeShare(g)
		g2 := f.grantAndRecord(a, nil)
		var all []uuid.UUID
		if err := f.pool.QueryRow(context.Background(), `SELECT array_agg(id) FROM activities WHERE activity_type = 'aa:AssetVersion'
			AND payload->>'share_id' = $1`, g2.share.String()).Scan(&all); err != nil {
			t.Fatal(err)
		}
		nums, _ := f.announced(all)
		got := map[int64]bool{}
		for _, n := range nums {
			got[n] = true
		}
		if len(nums) != 2 || !got[2] || !got[3] {
			t.Fatalf("the regrant announced %v, want 2 and 3 (version 2 repaired, then the current one)", nums)
		}
		if n, _ := f.announced([]uuid.UUID{g2.announcement}); !equal(n, 3) {
			t.Fatalf("Record returned the announcement of %v, want the current version 3", n)
		}
	})
	t.Run("a version that stays a draft is passed over, not announced on every seal", func(t *testing.T) {
		f.peer = f.addPeer()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		if _, err := f.sealNext(&a, 2, false, nil); err != nil { // stays a draft
			t.Fatal(err)
		}
		ids3, err := f.sealNext(&a, 3, true, nil)
		if nums, _ := f.announced(ids3); err != nil || !equal(nums, 3) {
			t.Fatalf("version 3: %v %v", nums, err)
		}
		f.dispatched(ids3, f.peer, "sent")
		for n := int64(4); n <= 5; n++ {
			ids, err := f.sealNext(&a, n, true, nil)
			if nums, _ := f.announced(ids); err != nil || !equal(nums, n) {
				t.Fatalf("version %d announced %v (%v), want %d alone", n, nums, err, n)
			}
			f.dispatched(ids, f.peer, "sent")
		}
	})
	t.Run("a missing version before a draft: the draft is still never announced", func(t *testing.T) {
		f.peer = f.addPeer()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
		ids2, err := f.sealNext(&a, 2, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.dispatched(ids2, f.peer, "queued")
		if _, err := f.sealNext(&a, 3, false, nil); err != nil { // version 3 stays a draft
			t.Fatal(err)
		}
		draft := a.version
		ids4, err := f.sealNext(&a, 4, true, nil)
		if nums, _ := f.announced(ids4); err != nil || !equal(nums, 4) {
			t.Fatalf("version 4: %v %v", nums, err)
		}
		f.dispatched(ids4, f.peer, "queued")
		f.dispatched(ids2, f.peer, "failed")
		// Version 5: version 2 is missing, so 2, 4 and 5 are announced;
		// the draft between them never is.
		ids5, err := f.sealNext(&a, 5, true, nil)
		if nums, _ := f.announced(ids5); err != nil || !equal(nums, 2, 4, 5) {
			t.Fatalf("version 5 announced %v (%v), want 2 4 5", nums, err)
		}
		if n := f.versionAnnouncements(a, draft); n != 0 {
			t.Fatalf("the draft was announced %d times", n)
		}
	})
	t.Run("a dead version below a usable lowest one is not backfilled", func(t *testing.T) {
		f.peer = f.addPeer()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.dispatched([]uuid.UUID{g.announcement}, f.peer, "failed") // v1 never reached the peer
		ids2, err := f.sealNext(&a, 2, true, nil)
		if nums, _ := f.announced(ids2); err != nil || !equal(nums, 2) {
			t.Fatalf("version 2: %v %v", nums, err)
		}
		f.dispatched(ids2, f.peer, "sent") // the peer's first version is 2
		ids3, err := f.sealNext(&a, 3, true, nil)
		if nums, _ := f.announced(ids3); err != nil || !equal(nums, 3) {
			t.Fatalf("version 3 announced %v (%v), want 3 alone: the peer's chain starts at 2", nums, err)
		}
	})
}

// memberOf builds the aa:asset member announcement id sends (its share's
// aa:Share must be delivered), and returns it with its raw bytes.
func (f *fx) memberOf(id uuid.UUID) (federation.ArchivalAsset, string) {
	f.t.Helper()
	var objectURI string
	var payload []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT object_uri, payload FROM activities WHERE id = $1`, id).
		Scan(&objectURI, &payload); err != nil {
		f.t.Fatal(err)
	}
	raw, err := f.src.Member(context.Background(), id, f.peer, objectURI, payload)
	if err != nil {
		f.t.Fatalf("member of %s: %v", id, err)
	}
	m, perr := federation.ParseArchivalAsset(raw)
	if perr != nil {
		f.t.Fatalf("Member returned a member §17.2 refuses: %v (%s)", perr, raw)
	}
	return *m, string(raw)
}

// predecessorOf is the member's version predecessor ("" for none).
func predecessorOf(m federation.ArchivalAsset) string {
	if m.Version.Predecessor == nil {
		return ""
	}
	return *m.Version.Predecessor
}

// A version that stayed a draft is never announced, so the next sealed
// version names the version sealed before the draft as its predecessor
// (#1575, migration 00093), and the receiver holding that one can accept
// it. The predecessor is fixed: every member built for the version is the
// same.
func TestMember_NamesThePreviousSealedVersionAsItsPredecessor(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	f.shareDelivered(g, "sent")
	f.dispatched([]uuid.UUID{g.announcement}, f.peer, "sent")
	sealed := func(n int64) uuid.UUID {
		t.Helper()
		ids, err := f.sealNext(&a, n, true, nil)
		if nums, _ := f.announced(ids); err != nil || !equal(nums, n) {
			t.Fatalf("version %d announced %v (%v), want %d alone", n, nums, err, n)
		}
		f.dispatched(ids, f.peer, "sent")
		return ids[0]
	}
	draft := func(n int64) uuid.UUID {
		t.Helper()
		if _, err := f.sealNext(&a, n, false, nil); err != nil {
			t.Fatal(err)
		}
		return a.version
	}
	want := func(id uuid.UUID, n int64, pred int64) string {
		t.Helper()
		m, raw := f.memberOf(id)
		if m.Version.Number != n || predecessorOf(m) != fmt.Sprintf("%s/versions/%d", a.uri, pred) {
			t.Fatalf("version %d's member names %q as its predecessor, want version %d", m.Version.Number, predecessorOf(m), pred)
		}
		return raw
	}

	v2 := draft(2) // abandoned: replaced before it was sealed
	id3 := sealed(3)
	raw3 := want(id3, 3, 1)
	if _, again := f.memberOf(id3); again != raw3 {
		t.Fatalf("version 3's member changed between two builds:\n%s\n%s", raw3, again)
	}
	v4, v5 := draft(4), draft(5)
	want(sealed(6), 6, 3)
	want(sealed(7), 7, 6) // a contiguous successor still names the version below it
	for _, d := range []uuid.UUID{v2, v4, v5} {
		if n := f.versionAnnouncements(a, d); n != 0 {
			t.Fatalf("a draft was announced %d times", n)
		}
	}
	if _, again := f.memberOf(id3); again != raw3 {
		t.Fatal("version 3's member changed after later versions were sealed")
	}

	t.Run("with no sealed version below it, the version it was created over", func(t *testing.T) {
		b := f.newAsset("public", false) // version 1 stays a draft (a backfilled version)
		gb := f.grantAndRecord(b, nil)
		f.shareDelivered(gb, "sent")
		ids, err := f.sealNext(&b, 2, true, nil)
		if nums, _ := f.announced(ids); err != nil || !equal(nums, 2) {
			t.Fatalf("version 2 announced %v (%v)", nums, err)
		}
		m, _ := f.memberOf(ids[0])
		if m.Version.Number != 2 || predecessorOf(m) != b.uri+"/versions/1" {
			t.Fatalf("version 2 names %q", predecessorOf(m))
		}
		// The grant's announcement of the draft is never built: its bytes
		// are never sent, and it can no longer be sealed.
		if _, err := f.member(gb); !errors.Is(err, assetsend.ErrNotReady) {
			t.Fatalf("the draft's announcement: %v, want ErrNotReady", err)
		}
	})

	t.Run("a regrant after an abandoned draft announces the current version after the last delivered one", func(t *testing.T) {
		c := f.newAsset("public", true)
		gc := f.grantAndRecord(c, nil)
		f.shareDelivered(gc, "sent")
		f.dispatched([]uuid.UUID{gc.announcement}, f.peer, "sent") // version 1 delivered
		f.revokeShare(gc)
		if _, err := f.sealNext(&c, 2, false, nil); err != nil { // abandoned
			t.Fatal(err)
		}
		if ids, err := f.sealNext(&c, 3, true, nil); err != nil || len(ids) != 0 {
			t.Fatalf("version 3 under a revoked share: %v %v", ids, err)
		}
		g2 := f.grantAndRecord(c, nil)
		f.shareDelivered(g2, "sent")
		var all []uuid.UUID
		if err := f.pool.QueryRow(context.Background(), `SELECT array_agg(id) FROM activities WHERE activity_type = 'aa:AssetVersion'
			AND payload->>'share_id' = $1`, g2.share.String()).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if nums, _ := f.announced(all); !equal(nums, 3) {
			t.Fatalf("the regrant announced %v, want 3 alone", nums)
		}
		m, _ := f.memberOf(g2.announcement)
		if predecessorOf(m) != c.uri+"/versions/1" {
			t.Fatalf("version 3 names %q", predecessorOf(m))
		}
	})

	t.Run("a failed seal after an abandoned draft announces nothing", func(t *testing.T) {
		d := f.newAsset("public", true)
		gd := f.grantAndRecord(d, nil)
		f.dispatched([]uuid.UUID{gd.announcement}, f.peer, "sent")
		if _, err := f.sealNext(&d, 2, false, nil); err != nil {
			t.Fatal(err)
		}
		current := d.version
		if _, err := f.sealNext(&d, 3, true, errors.New("the sealing transaction failed")); err == nil {
			t.Fatal("the failing seal committed")
		}
		var versions, announcements int
		if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM asset_versions WHERE asset_id = $1),
			  (SELECT count(*) FROM activities WHERE activity_type = 'aa:AssetVersion' AND object_local_id = $1::text)`, d.id).
			Scan(&versions, &announcements); err != nil {
			t.Fatal(err)
		}
		if versions != 2 || announcements != 1 || d.version != current {
			t.Fatalf("after the rollback: %d versions, %d announcements", versions, announcements)
		}
	})
}

// sealErr seals version v in a transaction of its own, with the function
// or (direct) with the statements the function runs, and returns the
// error.
func (f *fx) sealErr(v uuid.UUID, direct bool) error {
	ctx := context.Background()
	return pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if !direct {
			_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, v)
			return err
		}
		for _, q := range []string{
			`UPDATE asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = $1`,
			`INSERT INTO asset_version_files (version_id, role, object_hash, size_bytes, media_type)
			 SELECT id, 'original', original_hash, original_size_bytes, original_media_type FROM asset_versions WHERE id = $1`,
			`UPDATE asset_versions SET sealed_at = now() WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, v); err != nil {
				return err
			}
		}
		return nil
	})
}

const onlyLatest = "only the latest version is sealed"

// Only the asset's latest version is sealed (#1575, migration 00093), so
// the sealed versions below a version are final once it exists and the
// predecessor it is announced with never changes.
func TestSeal_OnlyTheLatestVersionIsSealed(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	if _, err := f.sealNext(&a, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	abandoned := a.version
	if _, err := f.sealNext(&a, 3, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, direct := range []bool{false, true} {
		if err := f.sealErr(abandoned, direct); err == nil || !strings.Contains(err.Error(), onlyLatest) {
			t.Fatalf("sealing the abandoned version 2 (direct %v): %v", direct, err)
		}
	}
	if err := f.sealErr(a.version, false); err != nil {
		t.Fatalf("sealing the latest version: %v", err)
	}
}

// waitsOnLock waits until the backend pid is waiting for a lock.
func (f *fx) waitsOnLock(pid int) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).
			Scan(&waiting); err == nil && waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("backend %d never waited for a lock", pid)
}

// inTx runs stmt (with args) in a transaction of its own on another
// goroutine, sends its backend pid once the transaction has begun, then
// commits, and sends the statement's or the commit's error.
func (f *fx) inTx(stmt string, args ...any) (<-chan int, <-chan error) {
	pid, done := make(chan int, 1), make(chan error, 1)
	go func() {
		ctx := context.Background()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			pid <- 0
			done <- err
			return
		}
		var p int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
			pid <- 0
			_ = tx.Rollback(ctx)
			done <- err
			return
		}
		pid <- p
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			_ = tx.Rollback(ctx)
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	return pid, done
}

// A seal and the creation of a later version of the same asset serialise
// on the asset row, which both take first.
func TestSeal_ASealAndANewVersionOfTheSameAssetAreOrdered(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	const create = `SELECT asset_version_create($1, $2, 2, $3, 'replacement', $4, 'png', NULL, 'unknown', NULL)`
	t.Run("a seal waiting for an uncommitted later version is refused once it commits", func(t *testing.T) {
		a := f.newAsset("public", false)
		hash := f.storedBytes()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, create, a.id, uuid.New(), a.version, hash); err != nil {
			t.Fatal(err)
		}
		pid, done := f.inTx(`SELECT asset_version_seal($1, '[]'::jsonb)`, a.version)
		f.waitsOnLock(<-pid)
		select {
		case err := <-done:
			t.Fatalf("the seal finished while a later version was uncommitted: %v", err)
		default:
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil || !strings.Contains(err.Error(), onlyLatest) {
			t.Fatalf("the seal after the later version committed: %v", err)
		}
	})
	t.Run("a later version waits for a seal in progress, and both commit", func(t *testing.T) {
		a := f.newAsset("public", false)
		hash := f.storedBytes()
		// A third transaction holds the original's object row (as a pin
		// writer does), so the seal stops part way, after it has taken the
		// asset row and the version row.
		hold, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback(ctx) //nolint:errcheck
		if _, err := hold.Exec(ctx, `SELECT 1 FROM storage_objects WHERE hash = $1 FOR UPDATE`, a.hash); err != nil {
			t.Fatal(err)
		}
		sealPID, sealed := f.inTx(`SELECT asset_version_seal($1, '[]'::jsonb)`, a.version)
		f.waitsOnLock(<-sealPID)
		next := uuid.New()
		createPID, created := f.inTx(create, a.id, next, a.version, hash)
		f.waitsOnLock(<-createPID)
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-sealed; err != nil {
			t.Fatalf("the seal: %v", err)
		}
		if err := <-created; err != nil {
			t.Fatalf("the later version: %v", err)
		}
		var v1Sealed, v2Draft bool
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT sealed_at IS NOT NULL FROM asset_versions WHERE id = $1),
			  (SELECT sealed_at IS NULL FROM asset_versions WHERE id = $2)`, a.version, next).Scan(&v1Sealed, &v2Draft); err != nil || !v1Sealed || !v2Draft {
			t.Fatalf("version 1 sealed %v, version 2 a draft %v (%v)", v1Sealed, v2Draft, err)
		}
	})
}
