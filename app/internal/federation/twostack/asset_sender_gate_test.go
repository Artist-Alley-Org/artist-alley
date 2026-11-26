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
// View (the received-asset read model): a non-admin user of B reads
// the record through B's real /api/v1/federation/received-assets routes,
// the original's bytes are A's, and every request re-checks B's grant
// for the record: a revoke hides it, a view-only regrant shows it again
// without the original, anonymous callers never see it.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
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
		// #1595: A's version has no known base name (the asset was not
		// made from an upload request), so A declares none and B records
		// none.
		var source string
		if err := g.a.pool.QueryRow(ctx, `SELECT original_basename_source FROM asset_versions WHERE id = $1`, version).Scan(&source); err != nil ||
			source != "unknown" {
			t.Fatalf("A's version 1 base name source %q (%v), want unknown", source, err)
		}
		var declared *string
		if err := g.b.pool.QueryRow(ctx, `SELECT f.declared_original_filename FROM federation_received_asset_files f
			JOIN federation_received_assets r ON r.id = f.received_asset_id
			WHERE r.peer_id = $1 AND r.activity_uri = $2 AND f.role = 'original'`, g.peerAonB, ann[0]).Scan(&declared); err != nil || declared != nil {
			t.Fatalf("B's original declares %v (%v), want no name", declared, err)
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

	t.Run("a user of B views the record while the grant allows it", func(t *testing.T) {
		o := g.newOriginal("public")
		g.sealedOnA(o)
		share := g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		var id string
		if err := g.b.pool.QueryRow(ctx, `SELECT id::text FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann[0]).Scan(&id); err != nil {
			t.Fatal(err)
		}
		member := g.memberOnB()
		listed := func(tok string) bool {
			t.Helper()
			st, raw := g.getOnB(tok, "/federation/received-assets?limit=200")
			if st != http.StatusOK {
				t.Fatalf("list: %d %s", st, raw)
			}
			var l struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &l); err != nil {
				t.Fatal(err)
			}
			for _, it := range l.Items {
				if it.ID == id {
					return true
				}
			}
			return false
		}
		detail := func(tok string) (int, bool) {
			t.Helper()
			st, raw := g.getOnB(tok, "/federation/received-assets/"+id)
			var d struct {
				OriginalAvailable bool `json:"original_available"`
				Asset             struct {
					OriginAssetURI string `json:"origin_asset_uri"`
					Original       struct {
						SHA256   string  `json:"sha256"`
						Detected *string `json:"detected_media_type"`
					} `json:"original"`
				} `json:"asset"`
			}
			if st == http.StatusOK {
				if err := json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				if d.Asset.OriginAssetURI != assetURI(o) || d.Asset.Original.SHA256 != o.hash || d.Asset.Original.Detected == nil {
					t.Fatalf("detail: %s", raw)
				}
			}
			return st, d.OriginalAvailable
		}
		originalStatus := func(tok string) int {
			t.Helper()
			st, raw := g.getOnB(tok, "/federation/received-assets/"+id+"/original")
			if st == http.StatusOK && !bytes.Equal(raw, o.body) {
				t.Fatal("B served bytes that are not A's original")
			}
			return st
		}

		// Granted with the original: listed, detailed, downloadable.
		if !listed(member) {
			t.Fatal("B's user does not see the received record")
		}
		if st, avail := detail(member); st != http.StatusOK || !avail {
			t.Fatalf("detail: %d available=%v", st, avail)
		}
		if st := originalStatus(member); st != http.StatusOK {
			t.Fatalf("original: %d", st)
		}
		// Anonymous: nothing.
		if st, _ := g.getOnB("", "/federation/received-assets"); st != http.StatusUnauthorized {
			t.Fatalf("anonymous list: %d", st)
		}
		if st, _ := detail(""); st != http.StatusNotFound {
			t.Fatalf("anonymous detail: %d", st)
		}
		if st := originalStatus(""); st != http.StatusNotFound {
			t.Fatalf("anonymous original: %d", st)
		}
		// Malformed and unknown requests: refused, and (getOnB) no-store.
		for path, want := range map[string]int{
			"/federation/received-assets?cursor=bm90LWEtY3Vyc29y":           http.StatusBadRequest,
			"/federation/received-assets?limit=0":                           http.StatusBadRequest,
			"/federation/received-assets/not-a-uuid":                        http.StatusBadRequest,
			"/federation/received-assets/not-a-uuid/original":               http.StatusBadRequest,
			"/federation/received-assets/" + uuid.NewString():               http.StatusNotFound,
			"/federation/received-assets/" + uuid.NewString() + "/original": http.StatusNotFound,
		} {
			if st, raw := g.getOnB(member, path); st != want {
				t.Fatalf("%s: %d %s, want %d", path, st, raw, want)
			}
		}

		// Revoked on A: gone on B, on the next request.
		g.revoke(share)
		g.waitOnB(assetURI(o), "ended/revoked")
		if listed(member) {
			t.Fatal("a revoked record is still listed")
		}
		if st, _ := detail(member); st != http.StatusNotFound {
			t.Fatalf("revoked detail: %d", st)
		}
		if st := originalStatus(member); st != http.StatusNotFound {
			t.Fatalf("revoked original: %d", st)
		}

		// Regranted without the original: the record shows again, the
		// file does not (B keeps the bytes; only a grant carrying the
		// original releases them).
		g.grantWith(o, nil)
		g.waitOnB(assetURI(o), "live", "ended/revoked")
		if !listed(member) {
			t.Fatal("a regranted record is not listed")
		}
		if st, avail := detail(member); st != http.StatusOK || avail {
			t.Fatalf("view-only detail: %d available=%v", st, avail)
		}
		if st := originalStatus(member); st != http.StatusNotFound {
			t.Fatalf("view-only original: %d", st)
		}
		if n := g.onB(o, `SELECT count(*) FROM assets WHERE file_hash = $1`); n != 0 {
			t.Fatal("viewing created an assets row on B")
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

	// #1574 slice 2. LAST: it removes the peer on B.
	t.Run("a tampered transfer stays visible to B's operator after B removes the peer", func(t *testing.T) {
		o := g.newOriginal("public")
		g.sealedOnA(o)
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method != http.MethodGet {
				h.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(&writerHook{ResponseWriter: w, mutate: func(p []byte) []byte {
				if len(p) > 0 {
					p[0] ^= 0xff
				}
				return p
			}}, r)
		})
		defer g.a.setFront(nil)
		g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "rejected/fixity_mismatch")
		var recordID string
		if err := g.b.pool.QueryRow(ctx, `SELECT id::text FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann[0]).Scan(&recordID); err != nil {
			t.Fatal(err)
		}
		g.nothingOnB(o)

		type ev struct {
			ReceivedAssetID string  `json:"received_asset_id"`
			EventType       string  `json:"event_type"`
			Outcome         string  `json:"outcome"`
			Reason          *string `json:"reason"`
			PeerURL         string  `json:"peer_instance_url"`
			ExpectedSHA256  *string `json:"expected_sha256"`
			ObservedSHA256  *string `json:"observed_sha256"`
			ObservedBasis   *string `json:"observed_basis"`
			RecordExists    bool    `json:"record_exists"`
			PeerExists      bool    `json:"peer_exists"`
		}
		ledger := func() ev {
			t.Helper()
			var out struct {
				Items []ev `json:"items"`
			}
			if st := g.api(g.b, http.MethodGet, "/admin/federation/transfer-events?received_asset_id="+recordID, nil, &out); st != http.StatusOK {
				t.Fatalf("B's ledger: %d", st)
			}
			if len(out.Items) != 1 {
				t.Fatalf("B's ledger has %d rows for the record: %+v", len(out.Items), out.Items)
			}
			return out.Items[0]
		}
		check := func(e ev) {
			t.Helper()
			if e.EventType != "fixity-check" || e.Outcome != "failure" || e.Reason == nil || *e.Reason != "fixity_mismatch" ||
				e.PeerURL != g.a.url || e.ExpectedSHA256 == nil || *e.ExpectedSHA256 != o.hash ||
				e.ObservedSHA256 == nil || *e.ObservedSHA256 == o.hash || e.ObservedBasis == nil || *e.ObservedBasis != "received_bytes" {
				t.Fatalf("B's ledger row: %+v", e)
			}
		}
		before := ledger()
		check(before)
		if !before.RecordExists || !before.PeerExists {
			t.Fatalf("before removal: %+v", before)
		}
		// B's operator removes A, through B's admin API; the record goes
		// with the peer.
		if st := g.api(g.b, http.MethodDelete, "/admin/federation/peers/"+g.peerAonB.String(), nil, nil); st != http.StatusNoContent {
			t.Fatalf("B removing A: %d", st)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE id = $1`, recordID); n != 0 {
			t.Fatal("the record outlived its peer on B")
		}
		after := ledger()
		check(after)
		if after.RecordExists || after.PeerExists {
			t.Fatalf("after removal: %+v", after)
		}
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

// memberOnB creates an ordinary (non-admin) user of B with an API token
// and returns the token.
func (g *casGate) memberOnB() string {
	g.t.Helper()
	ctx := context.Background()
	var ref int64
	if err := g.b.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Gate Member', 1) RETURNING ref`,
		"member-"+strings.ToLower(uuid.NewString()[:8])).Scan(&ref); err != nil {
		g.t.Fatal(err)
	}
	tok, _ := auth.NewAPIToken()
	if _, err := g.b.pool.Exec(ctx, `INSERT INTO api_tokens (user_ref, name, token_hash) VALUES ($1, 'twostack member', $2)`,
		ref, auth.HashAPIToken(tok)); err != nil {
		g.t.Fatal(err)
	}
	return tok
}

// getOnB GETs B's /api/v1 path with the bearer token, or anonymously
// when tok is empty.
func (g *casGate) getOnB(tok, path string) (int, []byte) {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, g.b.url+"/api/v1"+path, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Transport: g.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// Every answer of the received-asset routes, refusals included, is
	// no-store on the wire: a cached 404 would hide a later regrant.
	if strings.HasPrefix(path, "/federation/received-assets") && resp.Header.Get("Cache-Control") != "no-store" {
		g.t.Fatalf("GET %s: %d with Cache-Control %q, want no-store", path, resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	return resp.StatusCode, raw
}
