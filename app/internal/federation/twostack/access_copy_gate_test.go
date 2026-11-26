// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597 (spec v1 §7.5.7 "Access copies", §8.5.1, §16.7; proposed ADR
// 0103): access copies, end to end, on two production servers.
//
// TestTwoStack_AccessCopy: A uploads an image through its real API; A's
// own processing renders its derivatives and A's reconciler seals the
// version with the promoted, immutable copies its policy chose (a
// preview and an access copy, both the wire role access, each its own
// verified object). Then, through A's real admin API, outbox and B's
// real inbox, dispatcher and workers:
//
//   - an original-only share: B completes the version and keeps the
//     access entry as a claim (not_transferred, no reason); A is never
//     asked for it;
//   - a share with both permissions: A's aa:Share carries
//     aa:accessTransfer, B records both, completes the original, and
//     B's bounded access worker fetches each copy from A's signed
//     version-bound route, verifies it and pins it under the record. The
//     claimed files and version agree on A (sealed file set, announcement
//     claims) and B (record, file rows), the bytes are A's, the original
//     is unchanged, and no ingest pin or queued work is left;
//   - a late grant: the original-only share is revoked and regranted
//     with both permissions; B's access worker finds the copies left as
//     claims and receives them under the new grant.
//
// TestTwoStack_AccessCopyToALegacyPeer: the same shares to a peer
// without derived-transfer-1: A's aa:Share carries no aa:accessTransfer
// (even for a share that grants access copies on A), B receives the same
// file list with the access entry as a claim, and A is never asked for
// the copy.
//
// TEST-ONLY ADVERTISEMENT, before the real handshake and restored after:
// archival-1 and derived-transfer-1 (here), original-transfer-1
// (newCASGate). None of them is in production's peer.KnownCapabilities.
// Nothing in the positive path is built by the test: the announcement,
// its claims and B's inbound grant are the production sender's and
// receiver's own writes.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// accessGate is a CAS gate with archival-1 and, unless legacy,
// derived-transfer-1 advertised (test-only), negotiated through the
// pairing's real handshake.
func accessGate(t *testing.T, legacy bool) *casGate {
	t.Helper()
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1)
	if !legacy {
		peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapDerivedTransfer1)
	}
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	g := newCASGate(t)
	for _, side := range []struct{ in, other *instance }{{g.a, g.b}, {g.b, g.a}} {
		caps := g.capabilities(side.in, side.other)
		for _, c := range []peer.PeerCapability{peer.CapArchival1, peer.CapOriginalTransfer1, peer.CapShareLifecycle1} {
			if !contains(caps, string(c)) {
				t.Fatalf("%s did not negotiate %s with %s: %v", side.in.name, c, side.other.name, caps)
			}
		}
		if got := contains(caps, string(peer.CapDerivedTransfer1)); got == legacy {
			t.Fatalf("%s's intersection with %s holds derived-transfer-1 = %v (legacy %v): %v", side.in.name, side.other.name, got, legacy, caps)
		}
	}
	return g
}

// accessCopy is one access copy of a sealed version on A.
type accessCopy struct {
	version int64
	role    string // A's local role: preview or access (both wire role access)
	hash    string
	size    int64
	body    []byte // A's stored bytes of the promoted copy
}

// accessCopiesOnA waits for A to seal o's current version with its
// access copies (A's own rendering and promotion) and returns them, with
// their bytes as A stores them.
func (g *casGate) accessCopiesOnA(o original) []accessCopy {
	g.t.Helper()
	ctx := context.Background()
	g.sealedOnA(o)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := g.a.pool.Query(ctx, `SELECT v.number, f.role, f.object_hash, f.size_bytes FROM assets a
			JOIN asset_versions v ON v.id = a.current_version_id AND v.sealed_at IS NOT NULL
			JOIN asset_version_files f ON f.version_id = v.id AND f.role IN ('preview', 'access')
			WHERE a.id = $1 ORDER BY f.role, f.object_hash`, o.asset)
		if err != nil {
			g.t.Fatal(err)
		}
		var out []accessCopy
		for rows.Next() {
			var c accessCopy
			if err := rows.Scan(&c.version, &c.role, &c.hash, &c.size); err != nil {
				g.t.Fatal(err)
			}
			out = append(out, c)
		}
		rows.Close()
		if len(out) > 0 {
			for i := range out {
				rc, _, err := g.storageOf(g.a).Download(ctx, out[i].hash, storage.VariantOriginal)
				if err != nil {
					g.t.Fatalf("A's promoted copy %s: %v", out[i].hash[:12], err)
				}
				out[i].body, _ = io.ReadAll(rc)
				rc.Close()
				sum := sha256.Sum256(out[i].body)
				if hex.EncodeToString(sum[:]) != out[i].hash || int64(len(out[i].body)) != out[i].size {
					g.t.Fatalf("A's stored copy %s does not match its sealed listing", out[i].hash[:12])
				}
			}
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatal("A never sealed a version listing an access copy")
	return nil
}

// accessRowOnB is B's file row of c in the record of activityURI.
func (g *casGate) accessRowOnB(activityURI string, c accessCopy) (state, reason string) {
	g.t.Helper()
	var r *string
	if err := g.b.pool.QueryRow(context.Background(), `SELECT f.state, f.access_failure_reason FROM federation_received_asset_files f
		JOIN federation_received_assets r ON r.id = f.received_asset_id
		WHERE r.peer_id = $1 AND r.activity_uri = $2 AND f.role = 'access' AND f.object_hash = $3`,
		g.peerAonB, activityURI, c.hash).Scan(&state, &r); err != nil {
		g.t.Fatalf("B has no access entry %s in %s: %v", c.hash[:12], activityURI, err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

// accessVerifiedOnB waits (bounded) for B's access worker to verify c.
func (g *casGate) accessVerifiedOnB(activityURI string, c accessCopy) {
	g.t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	state, reason := "", ""
	for time.Now().Before(deadline) {
		state, reason = g.accessRowOnB(activityURI, c)
		if state == "verified" || reason != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if state != "verified" {
		g.t.Fatalf("B's access entry %s is %s/%s, want verified", c.hash[:12], state, reason)
	}
}

// shareOnB is B's received aa:Share envelope for objectURI's newest
// grant: whether it carries aa:accessTransfer, and the grant's flags.
func (g *casGate) shareOnB(objectURI string) (member string, original, access bool) {
	g.t.Helper()
	ctx := context.Background()
	var raw *string
	if err := g.b.pool.QueryRow(ctx, `SELECT i.envelope_json->>'aa:accessTransfer', gr.allow_original, gr.allow_access_copies
		FROM federation_inbound_grants gr JOIN federation_inbox i ON i.activity_uri = gr.grant_activity_uri AND i.peer_id = gr.peer_id
		WHERE gr.peer_id = $1 AND gr.object_uri = $2 ORDER BY gr.received_at DESC, gr.id DESC LIMIT 1`,
		g.peerAonB, objectURI).Scan(&raw, &original, &access); err != nil {
		g.t.Fatalf("B's grant for %s: %v", objectURI, err)
	}
	if raw != nil {
		member = *raw
	}
	return member, original, access
}

// accessFront counts A's access-route requests per copy hash.
type accessFront struct {
	mu   sync.Mutex
	hits map[string]int
}

func (f *accessFront) serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	if strings.Contains(r.URL.Path, "/versions/") && strings.Contains(r.URL.Path, "/files/access/") {
		parts := strings.Split(r.URL.Path, "/")
		f.mu.Lock()
		f.hits[r.Method+" "+parts[len(parts)-1]]++
		f.mu.Unlock()
	}
	h.ServeHTTP(w, r)
}

func (f *accessFront) count(method, hash string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[method+" "+hash]
}

// filesOnB lists the record's file set as role:hash, by position.
func (g *casGate) filesOnB(activityURI string) []string {
	g.t.Helper()
	rows, err := g.b.pool.Query(context.Background(), `SELECT f.role || ':' || f.object_hash FROM federation_received_asset_files f
		JOIN federation_received_assets r ON r.id = f.received_asset_id
		WHERE r.peer_id = $1 AND r.activity_uri = $2 ORDER BY f.position`, g.peerAonB, activityURI)
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

// claimsOnA is A's recorded claims (wire role and sha256 of each sealed
// file) for its announcement activityURI to B.
func (g *casGate) claimsOnA(activityURI string) string {
	g.t.Helper()
	var files string
	if err := g.a.pool.QueryRow(context.Background(), `SELECT c.files::text FROM federation_asset_announcement_claims c
		JOIN activities a ON a.id = c.activity_id WHERE a.activity_uri = $1 AND c.peer_id = $2`, activityURI, g.peerBonA).Scan(&files); err != nil {
		g.t.Fatalf("A's claims for %s: %v", activityURI, err)
	}
	return files
}

func (g *casGate) noAccessLeftoversOnB() {
	g.t.Helper()
	if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_ingest'`); n != 0 {
		g.t.Fatalf("B holds %d ingest pins", n)
	}
	if n := g.countOnB(`SELECT count(*) FROM federation_received_access_work`); n != 0 {
		g.t.Fatalf("B holds %d access work rows", n)
	}
}

func TestTwoStack_AccessCopy(t *testing.T) {
	g := accessGate(t, false)
	front := &accessFront{hits: map[string]int{}}
	g.a.setFront(front.serve)
	defer g.a.setFront(nil)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }

	// An asset shared with the original only: its access entry stays a
	// claim. (Granted first, so the access worker has examined it by the
	// time the next asset's copy is verified.)
	late := g.uploadAsset(pngBytes(t))
	lateCopies := g.accessCopiesOnA(late)
	lateShare := g.grantWith(late, map[string]any{"allow_original": true})
	var lateAnn string
	t.Run("original-only grant keeps the access entry a claim", func(t *testing.T) {
		ann := g.announcementsOnA(late)
		if len(ann) != 1 {
			t.Fatalf("A announced %d times, want 1", len(ann))
		}
		lateAnn = ann[0]
		g.recordStateOnB(lateAnn, "complete")
		if member, orig, access := g.shareOnB(assetURI(late)); member != "" || !orig || access {
			t.Fatalf("B's grant: aa:accessTransfer %q, original %v, access %v; want no member, original only", member, orig, access)
		}
		for _, c := range lateCopies {
			if st, reason := g.accessRowOnB(lateAnn, c); st != "not_transferred" || reason != "" {
				t.Fatalf("the access entry %s is %s/%s, want an unattempted claim", c.role, st, reason)
			}
		}
	})

	t.Run("both permissions: the copy is fetched, verified and pinned", func(t *testing.T) {
		o := g.uploadAsset(pngBytes(t))
		copies := g.accessCopiesOnA(o)
		var out struct {
			ID                string `json:"id"`
			AllowOriginal     bool   `json:"allow_original"`
			AllowAccessCopies bool   `json:"allow_access_copies"`
		}
		if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", map[string]any{"object_kind": "asset", "object_id": o.asset,
			"peer_id": g.peerBonA, "scope": "view", "allow_original": true, "allow_access_copies": true}, &out); st != http.StatusCreated ||
			!out.AllowOriginal || !out.AllowAccessCopies {
			t.Fatalf("grant: %d %+v", st, out)
		}
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A announced %d times, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		if member, orig, access := g.shareOnB(assetURI(o)); member != "true" || !orig || !access {
			t.Fatalf("B's grant: aa:accessTransfer %q, original %v, access %v; want both", member, orig, access)
		}
		for _, c := range copies {
			g.accessVerifiedOnB(ann[0], c)
		}

		// The claimed version and files agree on A and on B.
		claims := g.claimsOnA(ann[0])
		var recordID string
		var number int64
		if err := g.b.pool.QueryRow(ctx, `SELECT id::text, version_number FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann[0]).Scan(&recordID, &number); err != nil || number != copies[0].version {
			t.Fatalf("B's record is version %d (%v), A sealed the copies in version %d", number, err, copies[0].version)
		}
		files := g.filesOnB(ann[0])
		if !contains(files, "original:"+o.hash) || len(files) != 1+len(copies) {
			t.Fatalf("B's file set %v", files)
		}
		for _, c := range copies {
			if !strings.Contains(claims, c.hash) || !contains(files, "access:"+c.hash) {
				t.Fatalf("copy %s: A's claims %s, B's file set %v", c.hash[:12], claims, files)
			}
			// The bytes on B are A's copy, pinned under the record.
			if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset' AND pin_subject_id = $2`,
				c.hash, recordID); n != 1 {
				t.Fatalf("B's record pins of copy %s: %d", c.hash[:12], n)
			}
			rc, _, err := g.storageOf(g.b).Download(ctx, c.hash, storage.VariantOriginal)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, c.body) {
				t.Fatalf("B's stored copy %s differs from A's", c.hash[:12])
			}
			// From A's version-bound access route, signed by B, once.
			if front.count(http.MethodGet, c.hash) != 1 {
				t.Fatalf("A served %d access GETs of copy %s, want 1", front.count(http.MethodGet, c.hash), c.hash[:12])
			}
		}
		if strings.Count(claims, `"access"`) != len(copies) {
			t.Fatalf("A's claims list %d access files, A sealed %d: %s", strings.Count(claims, `"access"`), len(copies), claims)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1::uuid AND role = 'original'
			AND state = 'verified' AND object_hash = $2`, recordID, o.hash); n != 1 {
			t.Fatal("B's original is not verified")
		}
		var state string
		if err := g.b.pool.QueryRow(ctx, `SELECT state FROM federation_received_assets WHERE id = $1::uuid`, recordID).Scan(&state); err != nil || state != "complete" {
			t.Fatalf("B's record is %s (%v)", state, err)
		}
		g.noAccessLeftoversOnB()

		// The original-only asset's copies were examined in the same
		// passes and stayed claims; A was never asked for them.
		for _, c := range lateCopies {
			if st, reason := g.accessRowOnB(lateAnn, c); st != "not_transferred" || reason != "" {
				t.Fatalf("the original-only asset's access entry %s is %s/%s", c.role, st, reason)
			}
			if front.count(http.MethodGet, c.hash)+front.count(http.MethodHead, c.hash) != 0 {
				t.Fatal("A was asked for an access copy it never granted")
			}
		}
	})

	t.Run("a late access grant reaches the waiting claim", func(t *testing.T) {
		g.revoke(lateShare)
		g.waitOnB(assetURI(late), "ended/revoked")
		g.grantWith(late, map[string]any{"allow_original": true, "allow_access_copies": true})
		g.waitOnB(assetURI(late), "live", "ended/revoked")
		if member, _, access := g.shareOnB(assetURI(late)); member != "true" || !access {
			t.Fatalf("B's new grant: aa:accessTransfer %q, access %v", member, access)
		}
		var recordID string
		if err := g.b.pool.QueryRow(ctx, `SELECT id::text FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, lateAnn).Scan(&recordID); err != nil {
			t.Fatal(err)
		}
		for _, c := range lateCopies {
			g.accessVerifiedOnB(lateAnn, c)
			if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset' AND pin_subject_id = $2`,
				c.hash, recordID); n != 1 {
				t.Fatalf("B's record pins of late copy %s: %d", c.hash[:12], n)
			}
			if front.count(http.MethodGet, c.hash) != 1 {
				t.Fatalf("A served %d GETs of late copy %s, want 1", front.count(http.MethodGet, c.hash), c.hash[:12])
			}
		}
		g.noAccessLeftoversOnB()
	})
}

func TestTwoStack_AccessCopyToALegacyPeer(t *testing.T) {
	g := accessGate(t, true)
	front := &accessFront{hits: map[string]int{}}
	g.a.setFront(front.serve)
	defer g.a.setFront(nil)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()

	o := g.uploadAsset(pngBytes(t))
	copies := g.accessCopiesOnA(o)
	// A's share grants access copies locally; the peer cannot receive the
	// member, so it is never sent.
	g.grantWith(o, map[string]any{"allow_original": true, "allow_access_copies": true})
	var localAccess bool
	if err := g.a.pool.QueryRow(ctx, `SELECT allow_access_copies FROM federation_shares WHERE object_id = $1 AND revoked_at IS NULL`, o.asset).Scan(&localAccess); err != nil || !localAccess {
		t.Fatalf("A's share access flag %v (%v)", localAccess, err)
	}
	ann := g.announcementsOnA(o)
	if len(ann) != 1 {
		t.Fatalf("A announced %d times, want 1", len(ann))
	}
	g.recordStateOnB(ann[0], "complete")
	if member, orig, access := g.shareOnB(g.a.url + "/assets/" + o.asset.String()); member != "" || !orig || access {
		t.Fatalf("legacy B's grant: aa:accessTransfer %q, original %v, access %v; want no member, original only", member, orig, access)
	}
	// The same file list, the access entry a claim.
	files := g.filesOnB(ann[0])
	if len(files) != 1+len(copies) || !contains(files, "original:"+o.hash) {
		t.Fatalf("legacy B's file set %v", files)
	}
	claims := g.claimsOnA(ann[0])
	for _, c := range copies {
		if !contains(files, "access:"+c.hash) || !strings.Contains(claims, c.hash) {
			t.Fatalf("copy %s: legacy B's file set %v, A's claims %s", c.hash[:12], files, claims)
		}
	}
	// B's stored intersection with A lacks derived-transfer-1 and its
	// grant carries no access permission, so the access worker can never
	// admit the entry (its discovery requires all four profiles and its
	// admission a live access grant); it stays a claim and A is not asked.
	if contains(g.capabilities(g.b, g.a), string(peer.CapDerivedTransfer1)) {
		t.Fatal("legacy B holds derived-transfer-1")
	}
	for _, c := range copies {
		if st, reason := g.accessRowOnB(ann[0], c); st != "not_transferred" || reason != "" {
			t.Fatalf("legacy B's access entry %s is %s/%s", c.role, st, reason)
		}
		if n := front.count(http.MethodGet, c.hash) + front.count(http.MethodHead, c.hash); n != 0 {
			t.Fatalf("A was asked %d times for an access copy by a legacy peer", n)
		}
	}
	g.noAccessLeftoversOnB()
}
