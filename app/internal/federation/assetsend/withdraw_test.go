// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1575 withdrawal (spec v1 §17.3) at the sender: which peers
// RecordWithdrawals writes a Delete for, and what Source.Tombstone
// builds, defers or refuses. Skips without AA_DB_PASSWORD.
//
// An announcement "built" is one whose member Source.Member built (which
// records what it tells the peer): sent, or in flight while its outbox
// row is still queued. One merely queued was never built.

package assetsend_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
)

// addPeer is another connected peer with both profiles.
func (f *fx) addPeer() uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status,
		    handshake_by_user_ref, capabilities)
		VALUES ($1, 'receiver', 'k', TRUE, 'connected', $2, '["share-lifecycle-1","archival-1","original-transfer-1"]'::jsonb)
		RETURNING id`, "https://receiver-"+uuid.NewString()[:8]+".example", f.owner).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, id) })
	return id
}

// announceTo grants a to peer and records its announcement, whose outbox
// row to peer is then in status. Every status but "queued" is of a built
// announcement (its aa:Share delivered and its member built by
// Source.Member, as the delivery worker does before the request);
// "inflight" is built and still queued.
func (f *fx) announceTo(a asset, peer uuid.UUID, status string) grant {
	f.t.Helper()
	built := status != "queued"
	if status == "inflight" {
		status = "queued"
	}
	saved := f.peer
	f.peer = peer
	g := f.grantAndRecord(a, nil)
	if g.announcement == uuid.Nil {
		f.t.Fatal("no announcement recorded")
	}
	if built {
		f.shareDelivered(g, "sent")
		f.build(g, peer)
	}
	f.peer = saved
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, $3)`,
		g.announcement, peer, status); err != nil {
		f.t.Fatal(err)
	}
	return g
}

// build has Source.Member build g's member for peer, as the delivery
// worker does right before the request.
func (f *fx) build(g grant, peer uuid.UUID) {
	f.t.Helper()
	if _, err := f.src.Member(context.Background(), g.announcement, peer, g.objectURI, g.payload); err != nil {
		f.t.Fatalf("build the announcement: %v", err)
	}
}

func (f *fx) setAnnouncement(g grant, peer uuid.UUID, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_outbox SET status = $3 WHERE activity_id = $1 AND peer_id = $2`,
		g.announcement, peer, status); err != nil {
		f.t.Fatal(err)
	}
}

// deleteAsset soft-deletes a and records its withdrawals in one
// transaction, as assets.Handler.DeleteAsset does.
func (f *fx) deleteAsset(id uuid.UUID) []uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE assets SET deleted_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		var err error
		ids, err = assetsend.RecordWithdrawals(ctx, tx, f.writer, assetsend.Deleted{AssetID: id, BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

type withdrawal struct {
	id                            uuid.UUID
	typ, objectURI, kind, localID string
	payload                       map[string]any
	raw                           []byte
}

// str is a top-level string field of the local record.
func (w withdrawal) str(key string) string { v, _ := w.payload[key].(string); return v }

func (f *fx) withdrawalOf(id uuid.UUID) withdrawal {
	f.t.Helper()
	w := withdrawal{id: id}
	if err := f.pool.QueryRow(context.Background(), `SELECT activity_type, object_uri, object_kind, object_local_id, payload FROM activities WHERE id = $1`,
		id).Scan(&w.typ, &w.objectURI, &w.kind, &w.localID, &w.raw); err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(w.raw, &w.payload); err != nil {
		f.t.Fatal(err)
	}
	return w
}

// tombstoneRaw is what Source.Tombstone answers for withdrawal wd to peer
// now, with objectURI and the record as given.
func (f *fx) tombstoneRaw(peer uuid.UUID, wd withdrawal, objectURI string, raw []byte) ([]byte, error) {
	return f.src.Tombstone(context.Background(), wd.id, peer, objectURI, raw)
}

// tombstoneFor is the tombstone withdrawal wd delivers to peer now.
func (f *fx) tombstoneFor(peer uuid.UUID, wd withdrawal) (*federation.ArchivalTombstone, error) {
	f.t.Helper()
	raw, err := f.tombstoneRaw(peer, wd, wd.objectURI, wd.raw)
	if err != nil {
		return nil, err
	}
	ts, perr := federation.ParseArchivalTombstone(raw)
	if perr != nil {
		f.t.Fatalf("§17.3 refuses the tombstone: %v (%s)", perr, raw)
	}
	return ts, nil
}

// withdrawalTo is the recorded withdrawal of ids addressed to peer.
func (f *fx) withdrawalTo(ids []uuid.UUID, peer uuid.UUID) withdrawal {
	f.t.Helper()
	for _, id := range ids {
		if w := f.withdrawalOf(id); w.str("target_peer_id") == peer.String() {
			return w
		}
	}
	f.t.Fatalf("no withdrawal to %s among %d", peer, len(ids))
	return withdrawal{}
}

// newVersion seals version n of a over fresh stored bytes and makes it
// current, as a replacement upload does. It returns the version's hash.
func (f *fx) newVersion(a *asset, n int64) string {
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
	v := uuid.New()
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT asset_version_create($1, $2, $3, $4, 'replacement', $5, 'png', NULL, 'unknown', NULL)`,
			a.id, v, n, a.version, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET current_version_id = $2, file_hash = $3 WHERE id = $1`, a.id, v, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, v)
		return err
	}); err != nil {
		f.t.Fatalf("version %d: %v", n, err)
	}
	a.version, a.hash = v, hash
	return hash
}

// revokeShare ends g's share, so that the asset can be granted again.
func (f *fx) revokeShare(g grant) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`,
		g.share, g.shareActivity); err != nil {
		f.t.Fatal(err)
	}
}

// fixtureAnnouncement records an aa:AssetVersion of a's current version
// to peer with its outbox row in status, as Record and the dispatcher
// would, without a share (a database fixture for many announcements).
// built also records what its member told the peer, exactly as
// Source.Member does when the delivery worker builds it.
func (f *fx) fixtureAnnouncement(a asset, number int64, peer uuid.UUID, status string, built bool) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, object_uri,
		    object_kind, object_local_id, payload)
		VALUES ($1, 'aa:AssetVersion', $2, $3, $4, 'asset', $5, jsonb_build_object('version_id', $6::text)) RETURNING id`,
		base+"/activities/"+uuid.NewString(), base+"/users/owner", f.owner, a.uri, a.id.String(), a.version.String()).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, $3)`, id, peer, status); err != nil {
		f.t.Fatal(err)
	}
	if built {
		if _, err := f.pool.Exec(ctx, `INSERT INTO federation_asset_announcement_claims (activity_id, peer_id, asset_id, version_number, files)
			VALUES ($1, $2, $3, $4, jsonb_build_array(jsonb_build_object('role', 'original', 'sha256', $5::text)))`,
			id, peer, a.id, number, a.hash); err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}

func (f *fx) setOutbox(activity, peer uuid.UUID, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_outbox SET status = $3 WHERE activity_id = $1 AND peer_id = $2`,
		activity, peer, status); err != nil {
		f.t.Fatal(err)
	}
}

func TestRecordWithdrawals_OnlyPeersTheAssetWasAnnouncedTo(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	sent, inflight, unbuilt, failed, cancelled, never := f.peer, f.addPeer(), f.addPeer(), f.addPeer(), f.addPeer(), f.addPeer()
	gSent := f.announceTo(a, sent, "sent")
	f.announceTo(a, inflight, "inflight")
	f.announceTo(a, unbuilt, "queued") // never built: after the delete it never will be
	f.announceTo(a, failed, "failed")
	f.announceTo(a, cancelled, "cancelled")
	// Another asset announced to never: not this asset's announcement.
	f.announceTo(f.newAsset("public", true), never, "sent")
	// The share to sent is revoked meanwhile: the withdrawal still goes.
	f.revokeShare(gSent)

	ids := f.deleteAsset(a.id)
	if len(ids) != 2 {
		t.Fatalf("%d withdrawals, want 2 (the sent and the in-flight announcement)", len(ids))
	}
	got := map[string]bool{}
	for _, id := range ids {
		w := f.withdrawalOf(id)
		got[w.str("target_peer_id")] = true
		if w.typ != string(federation.ActivityDelete) || w.objectURI != a.uri || w.kind != "asset" || w.localID != a.id.String() ||
			w.str("asset_id") != a.id.String() {
			t.Fatalf("withdrawal %+v", w)
		}
		if _, err := time.Parse(time.RFC3339, w.str("deleted")); err != nil {
			t.Fatalf("deleted %q: %v", w.str("deleted"), err)
		}
		// The local record names the asset, the peer and the time only:
		// no version, no file, no reason.
		if len(w.payload) != 3 || bytes.Contains(w.raw, []byte(a.hash)) {
			t.Fatalf("the local record holds more than the asset, peer and time: %s", w.raw)
		}
	}
	if !got[sent.String()] || !got[inflight.String()] {
		t.Fatalf("withdrawals to %v", got)
	}
}

func TestRecordWithdrawals_NothingWithoutAnAnnouncedSealedLocalAsset(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	// Never granted.
	if ids := f.deleteAsset(f.newAsset("public", true).id); len(ids) != 0 {
		t.Fatalf("a never-announced asset: %d withdrawals", len(ids))
	}
	// Not deleted: RecordWithdrawals is only for a deleted asset.
	a := f.newAsset("public", true)
	f.announceTo(a, f.peer, "sent")
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		ids, err := assetsend.RecordWithdrawals(ctx, tx, f.writer, assetsend.Deleted{AssetID: a.id, BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }})
		if len(ids) != 0 {
			t.Errorf("a live asset: %d withdrawals", len(ids))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Announced while still a draft: the announcement waits for the seal,
	// so nothing was built and there is nothing to withdraw.
	b := f.newAsset("public", false)
	f.announceTo(b, f.peer, "queued")
	if ids := f.deleteAsset(b.id); len(ids) != 0 {
		t.Fatalf("a never-sealed asset: %d withdrawals", len(ids))
	}
}

// The invariant the withdrawal relies on: once the asset is deleted, an
// announcement can no longer be built, so nothing else becomes sent.
func TestMember_RefusesToBuildOnceTheAssetIsDeleted(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset("public", true)
	g := f.announceTo(a, f.peer, "queued")
	f.shareDelivered(g, "sent")
	f.deleteAsset(a.id)
	if _, err := f.src.Member(ctx, g.announcement, f.peer, g.objectURI, g.payload); !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("building after the delete: %v, want refused", err)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_asset_announcement_claims WHERE activity_id = $1`, g.announcement).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d claims recorded for a refused build (%v)", n, err)
	}
	// A delete that has updated the asset row but not committed: a build
	// waits for it (the share lock), then finds the asset deleted, and
	// records nothing. Without the lock it would record claims for an
	// asset whose delete then commits, and the withdrawal would not know.
	b := f.newAsset("public", true)
	gb := f.announceTo(b, f.peer, "queued")
	f.shareDelivered(gb, "sent")
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE assets SET deleted_at = now() WHERE id = $1`, b.id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.src.Member(ctx, gb.announcement, f.peer, gb.objectURI, gb.payload)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a build did not wait for the delete in progress (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("the build after the delete committed: %v, want refused", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_asset_announcement_claims WHERE activity_id = $1`, gb.announcement).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d claims recorded by a build that raced the delete (%v)", n, err)
	}
}

func TestTombstone_WaitsForTheAnnouncementThenBuildsTheSealedFileSet(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset("team", true)
	g := f.announceTo(a, f.peer, "inflight")
	// A later draft version is not what the peer received.
	if _, err := f.pool.Exec(ctx, `SELECT asset_version_create($1, gen_random_uuid(), 2, $2, 'replacement', $3, 'png', NULL, 'unknown', NULL)`,
		a.id, a.version, a.hash); err != nil {
		t.Fatalf("draft version 2: %v", err)
	}
	ids := f.deleteAsset(a.id)
	if len(ids) != 1 {
		t.Fatalf("%d withdrawals", len(ids))
	}
	w := f.withdrawalOf(ids[0])
	tomb := func() ([]byte, error) { return f.tombstoneRaw(f.peer, w, w.objectURI, w.raw) }

	if _, err := tomb(); !errors.Is(err, assetsend.ErrNotReady) || !errors.Is(err, federation.ErrWaitingForDelivery) {
		t.Fatalf("while the announcement is in flight: %v, want waiting", err)
	}
	f.setAnnouncement(g, f.peer, "sent")
	raw, err := tomb()
	if err != nil {
		t.Fatalf("after the announcement was sent: %v", err)
	}
	ts, err := federation.ParseArchivalTombstone(raw)
	if err != nil {
		t.Fatalf("§17.3 refuses the tombstone: %v (%s)", err, raw)
	}
	if ts.ID != a.uri || ts.FormerType != string(federation.ObjectAAAsset) || ts.LastVersion != a.uri+"/versions/1" ||
		ts.Deleted != w.str("deleted") || ts.Reason != "" {
		t.Fatalf("tombstone %+v", ts)
	}
	if len(ts.Files) != 1 || ts.Files[0].Role != "original" || ts.Files[0].ContentURI != "cas://"+a.hash ||
		len(ts.Files[0].Checksums) != 1 || ts.Files[0].Checksums[0].Algorithm != "sha256" || ts.Files[0].Checksums[0].Value != a.hash {
		t.Fatalf("files %+v", ts.Files)
	}
	again, err := tomb()
	if err != nil || string(again) != string(raw) {
		t.Fatalf("a retry built another tombstone: %v", err)
	}

	// Refused for good: an announcement that reached nothing, a record
	// that names another object or no time.
	for _, status := range []string{"failed", "cancelled"} {
		f.setAnnouncement(g, f.peer, status)
		if _, err := tomb(); !errors.Is(err, federation.ErrNotFederated) {
			t.Fatalf("announcement %s: %v, want refused", status, err)
		}
	}
	f.setAnnouncement(g, f.peer, "sent")
	if _, err := f.tombstoneRaw(f.addPeer(), w, w.objectURI, w.raw); !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("to a peer never announced to: %v, want refused", err)
	}
	if _, err := f.tombstoneRaw(f.peer, w, base+"/assets/"+uuid.NewString(), w.raw); !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("another object: %v, want refused", err)
	}
	if _, err := f.tombstoneRaw(f.peer, w, w.objectURI, []byte(`{"asset_id":"`+a.id.String()+`"}`)); !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("an incomplete record: %v, want refused", err)
	}
}

// rev628 finding 1: each peer is told only about a version it was
// announced, never the asset's newest version as such.
func TestTombstone_NamesOnlyTheVersionAnnouncedToThatPeer(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	h1 := a.hash
	b, c, d := f.peer, f.addPeer(), f.addPeer()
	f.announceTo(a, b, "sent") // B was sent version 1
	h2 := f.newVersion(&a, 2)
	f.announceTo(a, c, "sent") // C was sent version 2
	f.announceTo(a, d, "sent") // D: version 2 sent ...
	h3 := f.newVersion(&a, 3)  // ... and version 3, sealed, announced to nobody
	ids := f.deleteAsset(a.id)
	if len(ids) != 3 {
		t.Fatalf("%d withdrawals, want 3", len(ids))
	}
	for _, c := range []struct {
		peer uuid.UUID
		n    string
		hash string
	}{{b, "1", h1}, {c, "2", h2}, {d, "2", h2}} {
		ts, err := f.tombstoneFor(c.peer, f.withdrawalTo(ids, c.peer))
		if err != nil {
			t.Fatalf("peer %s: %v", c.peer, err)
		}
		if ts.LastVersion != a.uri+"/versions/"+c.n || len(ts.Files) != 1 || ts.Files[0].Checksums[0].Value != c.hash {
			t.Fatalf("peer %s: tombstone of %s over %v, want version %s over %s", c.peer, ts.LastVersion, ts.Files, c.n, c.hash[:12])
		}
		if bytes.Contains([]byte(ts.LastVersion), []byte("/versions/3")) || ts.Files[0].Checksums[0].Value == h3 {
			t.Fatal("a tombstone names the unannounced version 3")
		}
	}
}

// A newer announcement in flight at delete time is waited for: named
// when it reaches the peer, passed over when it fails.
func TestTombstone_AQueuedNewerAnnouncementIsWaitedFor(t *testing.T) {
	for _, outcome := range []string{"sent", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFx(t)
			a := f.newAsset("public", true)
			h1 := a.hash
			g1 := f.announceTo(a, f.peer, "sent")
			// Revoked and granted again once version 2 exists: a second
			// announcement to the same peer.
			f.revokeShare(g1)
			h2 := f.newVersion(&a, 2)
			g2 := f.announceTo(a, f.peer, "inflight")
			ids := f.deleteAsset(a.id)
			if len(ids) != 1 {
				t.Fatalf("%d withdrawals", len(ids))
			}
			w := f.withdrawalOf(ids[0])
			if _, err := f.tombstoneFor(f.peer, w); !errors.Is(err, assetsend.ErrNotReady) {
				t.Fatalf("while version 2's announcement is in flight: %v, want not ready", err)
			}
			f.setAnnouncement(g2, f.peer, outcome)
			ts, err := f.tombstoneFor(f.peer, w)
			if err != nil {
				t.Fatal(err)
			}
			want, hash := "2", h2
			if outcome == "failed" {
				want, hash = "1", h1
			}
			if ts.LastVersion != a.uri+"/versions/"+want || ts.Files[0].Checksums[0].Value != hash {
				t.Fatalf("after version 2's announcement %s: %s, want version %s", outcome, ts.LastVersion, want)
			}
		})
	}
}

// rev628 finding 2: what was delivered is kept apart from the asset, so a
// withdrawal still queued when the asset is purged is sent unchanged.
func TestTombstone_SentUnchangedAfterTheAssetIsPurged(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset("public", true)
	f.announceTo(a, f.peer, "sent")
	ids := f.deleteAsset(a.id)
	w := f.withdrawalOf(ids[0])
	before, err := f.tombstoneRaw(f.peer, w, w.objectURI, w.raw)
	if err != nil {
		t.Fatal(err)
	}
	// The retention purge, through the production service.
	if _, err := f.pool.Exec(ctx, `UPDATE assets SET deleted_at = now() - interval '3 days' WHERE id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	if n, err := softdelete.NewService(f.pool, nil).HardDeletePastAssets(ctx, 1, 1000); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM asset_versions WHERE asset_id = $1`, a.id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the purge left %d versions (%v)", left, err)
	}
	after, err := f.tombstoneRaw(f.peer, w, w.objectURI, w.raw)
	if err != nil {
		t.Fatalf("after the purge: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("after the purge the tombstone changed:\n%s\n%s", before, after)
	}
}

// rev629, 1: a queued second announcement of the version the peer was
// already sent (a regrant) is not waited for, built or not.
func TestTombstone_AQueuedRegrantOfTheSentVersionIsNotWaitedFor(t *testing.T) {
	for _, status := range []string{"queued", "inflight"} {
		t.Run(status, func(t *testing.T) {
			f := newFx(t)
			a := f.newAsset("public", true)
			g1 := f.announceTo(a, f.peer, "sent")
			f.revokeShare(g1)
			f.announceTo(a, f.peer, status) // the same version 1, granted again
			ids := f.deleteAsset(a.id)
			ts, err := f.tombstoneFor(f.peer, f.withdrawalOf(ids[0]))
			if err != nil {
				t.Fatalf("with version 1 sent and a regrant of it %s: %v, want the tombstone now", status, err)
			}
			if ts.LastVersion != a.uri+"/versions/1" || ts.Files[0].Checksums[0].Value != a.hash {
				t.Fatalf("tombstone %s", ts.LastVersion)
			}
		})
	}
}

// rev629, 2, and rev630: however many announcements are in flight when
// the asset is deleted, the one that reaches the peer is the one named,
// whether or not one was delivered before. Each fixture announcement of
// versions 2..18 (or 1..17) is built, so any of them may still reach the
// peer; one lower than the rest reaches it and the others fail.
func TestTombstone_TheVersionThatReachedThePeerIsNamedAmongManyInFlight(t *testing.T) {
	t.Run("17 in flight, none delivered before, the lowest is", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset("public", true)
		hashes := map[int64]string{1: a.hash}
		acts := map[int64]uuid.UUID{1: f.fixtureAnnouncement(a, 1, f.peer, "queued", true)}
		for n := int64(2); n <= 17; n++ {
			hashes[n] = f.newVersion(&a, n)
			acts[n] = f.fixtureAnnouncement(a, n, f.peer, "queued", true)
		}
		unannounced := f.newVersion(&a, 18) // sealed, announced to nobody
		ids := f.deleteAsset(a.id)
		if len(ids) != 1 {
			t.Fatalf("%d withdrawals, want 1", len(ids))
		}
		w := f.withdrawalOf(ids[0])
		if _, err := f.tombstoneFor(f.peer, w); !errors.Is(err, federation.ErrWaitingForDelivery) {
			t.Fatalf("while 17 announcements are in flight: %v, want waiting", err)
		}
		// Version 1's delivery completes after the delete; the rest fail.
		f.setOutbox(acts[1], f.peer, "sent")
		if _, err := f.tombstoneFor(f.peer, w); !errors.Is(err, federation.ErrWaitingForDelivery) {
			t.Fatalf("with higher versions still in flight: %v, want waiting", err)
		}
		for n := int64(2); n <= 17; n++ {
			f.setOutbox(acts[n], f.peer, "failed")
		}
		ts, err := f.tombstoneFor(f.peer, w)
		if err != nil {
			t.Fatalf("after version 1 reached the peer and the rest failed: %v, want its tombstone", err)
		}
		if ts.LastVersion != a.uri+"/versions/1" || len(ts.Files) != 1 || ts.Files[0].Checksums[0].Value != hashes[1] {
			t.Fatalf("tombstone %s over %v, want version 1 over %s", ts.LastVersion, ts.Files, hashes[1][:12])
		}
		if bytes.Contains(w.raw, []byte(unannounced)) {
			t.Fatal("the record names a version never announced to the peer")
		}
	})
	t.Run("version 1 delivered, 17 higher in flight, the lowest of them is", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset("public", true)
		f.announceTo(a, f.peer, "sent")
		hashes := map[int64]string{}
		acts := map[int64]uuid.UUID{}
		for n := int64(2); n <= 18; n++ {
			hashes[n] = f.newVersion(&a, n)
			acts[n] = f.fixtureAnnouncement(a, n, f.peer, "queued", true)
		}
		ids := f.deleteAsset(a.id)
		w := f.withdrawalOf(ids[0])
		f.setOutbox(acts[2], f.peer, "sent")
		for n := int64(3); n <= 18; n++ {
			f.setOutbox(acts[n], f.peer, "failed")
		}
		ts, err := f.tombstoneFor(f.peer, w)
		if err != nil {
			t.Fatal(err)
		}
		if ts.LastVersion != a.uri+"/versions/2" || len(ts.Files) != 1 || ts.Files[0].Checksums[0].Value != hashes[2] {
			t.Fatalf("tombstone %s over %v, want version 2 (the highest delivered) over %s", ts.LastVersion, ts.Files, hashes[2][:12])
		}
	})
	t.Run("version 1 delivered, 20 queued never built, none waited for", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset("public", true)
		h1 := a.hash
		f.announceTo(a, f.peer, "sent")
		for n := int64(2); n <= 21; n++ {
			f.newVersion(&a, n)
			f.fixtureAnnouncement(a, n, f.peer, "queued", false)
		}
		ids := f.deleteAsset(a.id)
		ts, err := f.tombstoneFor(f.peer, f.withdrawalOf(ids[0]))
		if err != nil || ts.LastVersion != a.uri+"/versions/1" || ts.Files[0].Checksums[0].Value != h1 {
			t.Fatalf("with 20 unbuilt announcements queued: %v %v, want version 1 at once", ts, err)
		}
	})
}

// rev631: an announcement a concurrent delivery moves from queued to
// sent while the withdrawal reads the announcements is neither missed
// (the withdrawal refused for good) nor passed over for an older version:
// the transition is forced right where the read ends (afterRead).
func TestTombstone_AnAnnouncementSentDuringTheReadIsNeitherMissedNorStale(t *testing.T) {
	cases := []struct {
		name        string
		priorSent   bool
		wantVersion string
	}{
		{"no version delivered before", false, "1"},
		{"version 1 delivered before, version 2 in flight", true, "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			ctx := context.Background()
			a := f.newAsset("public", true)
			if c.priorSent {
				f.revokeShare(f.announceTo(a, f.peer, "sent"))
				f.newVersion(&a, 2)
			}
			g := f.announceTo(a, f.peer, "inflight")
			ids := f.deleteAsset(a.id)
			if len(ids) != 1 {
				t.Fatalf("%d withdrawals", len(ids))
			}
			w := f.withdrawalOf(ids[0])
			flipped := false
			src := assetsend.WithAfterRead(f.src, func() {
				if !flipped {
					flipped = true
					f.setAnnouncement(g, f.peer, "sent") // its request completes now
				}
			})
			want := a.uri + "/versions/" + c.wantVersion
			// The read that races the transition: it may wait, or name
			// the version now delivered, but never refuse or name an
			// older one.
			raw, err := src.Tombstone(ctx, w.id, f.peer, w.objectURI, w.raw)
			if !flipped {
				t.Fatal("the transition was not forced")
			}
			switch {
			case errors.Is(err, federation.ErrNotFederated):
				t.Fatalf("the withdrawal was refused for good while its announcement was being delivered: %v", err)
			case err == nil:
				ts, perr := federation.ParseArchivalTombstone(raw)
				if perr != nil || ts.LastVersion != want || ts.Files[0].Checksums[0].Value != a.hash {
					t.Fatalf("during the delivery it named %s (%v), want %s over %s", ts.LastVersion, perr, want, a.hash[:12])
				}
			case !errors.Is(err, federation.ErrWaitingForDelivery):
				t.Fatalf("during the delivery: %v, want waiting or the delivered version", err)
			}
			// The next attempt names the version delivered.
			ts, err := f.tombstoneFor(f.peer, w)
			if err != nil {
				t.Fatalf("after the delivery: %v", err)
			}
			if ts.LastVersion != want || ts.Files[0].Checksums[0].Value != a.hash {
				t.Fatalf("after the delivery: %s, want %s over %s", ts.LastVersion, want, a.hash[:12])
			}
		})
	}
}
