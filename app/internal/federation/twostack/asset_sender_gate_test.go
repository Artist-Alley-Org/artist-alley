// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 gate (sender): an original-file share made through A's real
// admin API makes A announce the asset's sealed version (aa:AssetVersion)
// to B, after the aa:Share, through A's production outbox (resolver,
// delivery, the owner's #1526 signature); B's production inbox,
// dispatcher and worker import the original from A's real CAS route.
// Nothing in the path is built by the test.
//
// Test-only, labelled where installed:
//
//   - TEST-ONLY ADVERTISEMENT of archival-1 (here) and
//     original-transfer-1 (newCASGate), before the real handshake. Neither
//     is advertised in production.
//
// "View" is not part of this slice: B holds and verifies the record, and
// no page or API shows it yet.

package twostack_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// sealedOnA waits for A's production version reconciliation to seal o's
// current version.
func (g *casGate) sealedOnA(o original) uuid.UUID {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var v uuid.UUID
		var sealed bool
		if err := g.a.pool.QueryRow(context.Background(), `SELECT v.id, v.sealed_at IS NOT NULL FROM assets a
			JOIN asset_versions v ON v.id = a.current_version_id WHERE a.id = $1`, o.asset).Scan(&v, &sealed); err == nil && sealed {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatal("A never sealed the asset's version")
	return uuid.Nil
}

// announcementsOnA lists A's aa:AssetVersion activity URIs for o.
func (g *casGate) announcementsOnA(o original) []string {
	g.t.Helper()
	rows, err := g.a.pool.Query(context.Background(), `SELECT activity_uri FROM activities
		WHERE activity_type = 'aa:AssetVersion' AND object_local_id = $1 ORDER BY created_at, id`, o.asset.String())
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// recordStateOnB waits for B's record of activityURI to reach want.
func (g *casGate) recordStateOnB(activityURI, want string) {
	g.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	got := "none"
	for time.Now().Before(deadline) {
		var state string
		var reason *string
		if err := g.b.pool.QueryRow(context.Background(), `SELECT state, reason FROM federation_received_assets
			WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, activityURI).Scan(&state, &reason); err == nil {
			got = state
			if reason != nil {
				got += "/" + *reason
			}
			if got == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("B's record of %s is %s, want %s", activityURI, got, want)
}

func TestTwoStack_AssetSender(t *testing.T) {
	// TEST-ONLY ADVERTISEMENT of archival-1, before newCASGate adds
	// original-transfer-1 and pairs the instances.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1)
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	g := newCASGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }
	onA := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := g.a.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("grant, announce after the share, import, hold", func(t *testing.T) {
		o := g.newOriginal("public")
		version := g.sealedOnA(o)
		gets := g.a.casGETs.Load()
		g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		if g.a.casGETs.Load() != gets+1 {
			t.Fatalf("A served %d GETs, want 1", g.a.casGETs.Load()-gets)
		}

		// What B holds is A's sealed version, from A's owner, after the grant.
		var actor, versionURI, recordGrant string
		var number int64
		if err := g.b.pool.QueryRow(ctx, `SELECT actor_uri, version_uri, version_number, authorizing_grant_id::text
			FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, ann[0]).
			Scan(&actor, &versionURI, &number, &recordGrant); err != nil {
			t.Fatal(err)
		}
		var ownerName string
		if err := g.a.pool.QueryRow(ctx, `SELECT u.username FROM assets a JOIN "user" u ON u.ref = a.owner_user_ref WHERE a.id = $1`,
			o.asset).Scan(&ownerName); err != nil {
			t.Fatal(err)
		}
		var vnum int64
		if err := g.a.pool.QueryRow(ctx, `SELECT number FROM asset_versions WHERE id = $1`, version).Scan(&vnum); err != nil {
			t.Fatal(err)
		}
		if actor != g.a.url+"/users/"+ownerName || number != vnum || versionURI != assetURI(o)+"/versions/1" {
			t.Fatalf("B's record: actor %s version %s (%d)", actor, versionURI, number)
		}
		var liveGrant string
		if err := g.b.pool.QueryRow(ctx, `SELECT id::text FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`,
			g.peerAonB, assetURI(o)).Scan(&liveGrant); err != nil || liveGrant != recordGrant {
			t.Fatalf("authorised by %s, B's live grant is %s (%v)", recordGrant, liveGrant, err)
		}
		if n := g.onB(o, `SELECT count(*) FROM federation_received_asset_files WHERE object_hash = $1 AND role = 'original' AND state = 'verified'`); n != 1 {
			t.Fatalf("%d verified originals on B", n)
		}
		if n := g.onB(o, `SELECT count(*) FROM assets WHERE file_hash = $1`); n != 0 {
			t.Fatal("B created an assets row for a received record")
		}

		// Order: B received the grant before the announcement, and A sent
		// them in that order, each once.
		var shareFirst bool
		if err := g.b.pool.QueryRow(ctx, `SELECT
			(SELECT max(received_at) FROM federation_inbox WHERE peer_id = $1 AND activity_type = 'aa:Share' AND envelope_json->>'object' = $2)
			< (SELECT received_at FROM federation_inbox WHERE activity_uri = $3)`, g.peerAonB, assetURI(o), ann[0]).Scan(&shareFirst); err != nil || !shareFirst {
			t.Fatalf("B did not receive the aa:Share before the announcement (%v)", err)
		}
		if n := onA(`SELECT count(*) FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
			WHERE a.activity_uri = $1 AND o.status = 'sent'`, ann[0]); n != 1 {
			t.Fatalf("A sent the announcement %d times", n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1 AND status = 'processed'`, ann[0]); n != 1 {
			t.Fatalf("B processed %d inbox rows for the announcement", n)
		}

		// A regrant announces again under a new id; B adds no task and A
		// serves nothing again.
		var shareID string
		if err := g.a.pool.QueryRow(ctx, `SELECT id::text FROM federation_shares WHERE object_id = $1 AND revoked_at IS NULL`, o.asset).Scan(&shareID); err != nil {
			t.Fatal(err)
		}
		g.revoke(shareID)
		g.waitOnB(assetURI(o), "ended/revoked")
		g.grantWith(o, map[string]any{"allow_original": true})
		ann = g.announcementsOnA(o)
		if len(ann) != 2 {
			t.Fatalf("A recorded %d announcements after a regrant, want 2", len(ann))
		}
		deadline := time.Now().Add(120 * time.Second)
		for g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1 AND status <> 'pending'`, ann[1]) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("B never settled the second announcement")
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`,
			g.peerAonB, assetURI(o)); n != 1 {
			t.Fatalf("%d records on B for one version", n)
		}
		if g.a.casGETs.Load() != gets+1 {
			t.Fatal("B fetched the held version again")
		}
	})

	t.Run("a share without the original announces nothing", func(t *testing.T) {
		o := g.newOriginal("public")
		g.sealedOnA(o)
		g.grantWith(o, nil)
		g.waitOnB(assetURI(o), "live")
		if n := len(g.announcementsOnA(o)); n != 0 {
			t.Fatalf("A recorded %d announcements", n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1`, assetURI(o)); n != 0 {
			t.Fatal("B recorded an asset nobody announced")
		}
	})

	t.Run("a restricted asset is never announced", func(t *testing.T) {
		o := g.newOriginal("restricted")
		var out struct{ Error string }
		st := g.api(g.a, http.MethodPost, "/admin/federation/shares",
			map[string]any{"object_kind": "asset", "object_id": o.asset, "peer_id": g.peerBonA, "scope": "view", "allow_original": true}, &out)
		if st != http.StatusBadRequest {
			t.Fatalf("A answered %d to an original share of a restricted asset", st)
		}
		if n := len(g.announcementsOnA(o)); n != 0 {
			t.Fatalf("A recorded %d announcements", n)
		}
		g.nothingOnB(o)
	})
}

// senderOffOnA keeps A from announcing anything itself (#1574 sender):
// A's stored intersection for B loses archival-1, which A's sender needs
// and A's CAS origin does not (it checks original-transfer-1 only); B's
// record of A keeps both profiles, so B still receives. For the gates
// that hand B a test-built announcement and count A's transfers for it:
// with A's own sender on, A would announce the same version on the grant
// and B would import it first. Capabilities change only at a handshake.
func (g *casGate) senderOffOnA() {
	g.t.Helper()
	if _, err := g.a.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = capabilities - 'archival-1'
		WHERE id = $1`, g.peerBonA); err != nil {
		g.t.Fatal(err)
	}
}

func (g *casGate) countOnB(sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.b.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}
