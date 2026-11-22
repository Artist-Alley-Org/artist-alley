// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1575 (spec v1 §17.2.1): a later version of a received asset, on two
// production servers. A uploads, seals, grants and announces version 1;
// B imports it. A's owner uploads version 2 through A's real API, A seals
// it and announces it (on a regrant: the sender announces a version when
// a share is granted). While B fetches version 2's original (held at A's
// front), B's user is still served version 1 and version 2 is pending.
// Released, version 2 replaces version 1 on B in one step: version 1 is
// superseded (kept with its pin), version 2 complete and linked to it,
// and B's user is served version 2 only. A then deletes the asset and B
// withdraws both versions; the notice names version 2.
//
// Test-only, as in TestTwoStack_UploadToView: archival-1 and
// original-transfer-1 are advertised for the gate only. Neither is in
// production's peer.KnownCapabilities.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTwoStack_AssetVersionSuccessor(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	member := g.memberOnB()
	ctx := context.Background()

	o, share, ann1, id1 := g.importedOnB()
	assetURI := g.a.url + "/assets/" + o.asset.String()
	recordPins := func(id string) int {
		return g.countOnB(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = $1`, id)
	}
	served := func(path string) (int, []byte) { return g.getOnB(member, path) }
	listed := func() []string {
		st, raw := served("/federation/received-assets")
		var l struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if st != http.StatusOK || json.Unmarshal(raw, &l) != nil {
			t.Fatalf("list: %d %s", st, raw)
		}
		var ids []string
		for _, it := range l.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	versionOf := func(raw []byte) int {
		var d struct {
			Asset struct {
				VersionNumber int `json:"version_number"`
			} `json:"asset"`
		}
		_ = json.Unmarshal(raw, &d)
		return d.Asset.VersionNumber
	}
	if st, got := served("/federation/received-assets/" + id1 + "/original"); st != http.StatusOK || !bytes.Equal(got, o.body) {
		t.Fatalf("version 1's original on B: %d", st)
	}

	// Version 2 on A, through A's real API.
	g.revoke(share)
	g.waitOnB(assetURI, "ended/revoked")
	body2 := pngBytes(t)
	hash2, receipt := g.uploadOnA(body2)
	var v struct {
		Number int `json:"number"`
	}
	if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
		"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, &v); st != http.StatusCreated || v.Number != 2 {
		t.Fatalf("version 2 on A: %d %+v", st, v)
	}
	g.sealedOnA(o)

	// B's fetch of version 2's original waits at A's front.
	var held atomic.Int32
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery+r.URL.Path, hash2) {
			held.Add(1)
			<-release
		}
		h.ServeHTTP(w, r)
	})
	defer g.a.setFront(nil)
	g.grantWith(o, map[string]any{"allow_original": true}) // announces version 2
	var ann2 string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && (ann2 == "" || held.Load() == 0) {
		if ann := g.announcementsOnA(o); len(ann) == 2 {
			ann2 = ann[1]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ann2 == "" || held.Load() == 0 {
		t.Fatalf("B never fetched version 2 (announcement %q, held %d)", ann2, held.Load())
	}

	// While version 2 is being fetched: B's record of it is pending and
	// B's user is still served version 1.
	g.recordStateOnB(ann2, "pending")
	g.recordStateOnB(ann1, "complete")
	if st, raw := served("/federation/received-assets/" + id1); st != http.StatusOK || versionOf(raw) != 1 {
		t.Fatalf("version 1 during version 2's transfer: %d %s", st, raw)
	}
	if st, got := served("/federation/received-assets/" + id1 + "/original"); st != http.StatusOK || !bytes.Equal(got, o.body) {
		t.Fatalf("version 1's original during version 2's transfer: %d", st)
	}
	if ids := listed(); len(ids) != 1 || ids[0] != id1 {
		t.Fatalf("listed during the transfer: %v, want version 1 only", ids)
	}

	// Released: version 2 replaces version 1.
	releaseOnce()
	g.recordStateOnB(ann2, "complete")
	g.recordStateOnB(ann1, "superseded")
	id2 := g.receivedOnB(ann2)
	var pred, predURI string
	if err := g.b.pool.QueryRow(ctx, `SELECT predecessor_id::text, predecessor_version_uri FROM federation_received_assets WHERE id = $1::uuid`, id2).Scan(&pred, &predURI); err != nil ||
		pred != id1 || predURI != assetURI+"/versions/1" {
		t.Fatalf("version 2 follows %q %q (%v)", pred, predURI, err)
	}
	if recordPins(id1) != 1 || recordPins(id2) != 1 {
		t.Fatalf("pins: version 1 %d, version 2 %d", recordPins(id1), recordPins(id2))
	}
	if st, raw := served("/federation/received-assets/" + id2); st != http.StatusOK || versionOf(raw) != 2 {
		t.Fatalf("version 2's detail: %d %s", st, raw)
	}
	if st, got := served("/federation/received-assets/" + id2 + "/original"); st != http.StatusOK || !bytes.Equal(got, body2) {
		t.Fatalf("version 2's original: %d", st)
	}
	if st, _ := served("/federation/received-assets/" + id1); st != http.StatusNotFound {
		t.Fatalf("version 1 after it was replaced: %d, want 404", st)
	}
	if ids := listed(); len(ids) != 1 || ids[0] != id2 {
		t.Fatalf("listed after the replacement: %v, want version 2 only", ids)
	}
	led := g.ledgerOnB(id2)
	if len(led) == 0 || led[len(led)-1] != "ingestion/success/" {
		t.Fatalf("version 2's ledger: %v", led)
	}

	// A deletes the asset: B withdraws every version.
	g.deleteOnA(o, "")
	g.recordStateOnB(ann1, "withdrawn/withdrawn")
	g.recordStateOnB(ann2, "withdrawn/withdrawn")
	for _, id := range []string{id1, id2} {
		st, raw := served("/federation/received-assets/" + id)
		if st != http.StatusGone || !bytes.Contains(raw, []byte(`"last_version":"`+assetURI+`/versions/2"`)) {
			t.Fatalf("record %s after the withdrawal: %d %s", id, st, raw)
		}
		if recordPins(id) != 0 {
			t.Fatalf("record %s keeps %d pins after the withdrawal", id, recordPins(id))
		}
	}
}

// #1575: a new version uploaded on A reaches B under the share that
// already exists, with no revoke or regrant. A's production version
// reconciler seals version 2 and, in that transaction, announces it to
// B under the live share; B receives it as version 1's successor.
func TestTwoStack_ANewVersionIsAnnouncedWithoutARegrant(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	member := g.memberOnB()
	ctx := context.Background()

	o, share, ann1, id1 := g.importedOnB()
	body2 := pngBytes(t)
	hash2, receipt := g.uploadOnA(body2)
	if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
		"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, nil); st != http.StatusCreated {
		t.Fatalf("version 2 on A: %d", st)
	}
	g.sealedOnA(o)
	var ann2 string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ann := g.announcementsOnA(o); len(ann) == 2 {
			ann2 = ann[1]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ann2 == "" {
		t.Fatalf("A announced %d versions, want version 2 announced under the existing share", len(g.announcementsOnA(o)))
	}
	var shareID, versionNumber string
	if err := g.a.pool.QueryRow(ctx, `SELECT a.payload->>'share_id', v.number::text FROM activities a
		JOIN asset_versions v ON v.id::text = a.payload->>'version_id' WHERE a.activity_uri = $1`, ann2).Scan(&shareID, &versionNumber); err != nil ||
		shareID != share || versionNumber != "2" {
		t.Fatalf("A's second announcement is version %s under share %s (%v), want version 2 under %s", versionNumber, shareID, err, share)
	}
	g.recordStateOnB(ann2, "complete")
	g.recordStateOnB(ann1, "superseded")
	id2 := g.receivedOnB(ann2)
	if st, got := g.getOnB(member, "/federation/received-assets/"+id2+"/original"); st != http.StatusOK || !bytes.Equal(got, body2) {
		t.Fatalf("version 2's original on B: %d", st)
	}
	if st, _ := g.getOnB(member, "/federation/received-assets/"+id1); st != http.StatusNotFound {
		t.Fatalf("version 1 after it was replaced: %d", st)
	}
	// Nothing else is announced: A's reconciler running again adds none.
	if n := len(g.announcementsOnA(o)); n != 2 {
		t.Fatalf("A announced %d times, want 2", n)
	}
}

// #1575 (migration 00093): a version replaced before it was sealed is
// never announced, nor are its bytes sent. A's next sealed version names
// version 1 as its predecessor, and B receives it as version 1's
// successor, two numbers up. A rendition successor of it (a re-rendered
// access copy, sealed by A's reconciler) then follows it as usual.
func TestTwoStack_AVersionReplacedBeforeItWasSealedIsPassedOver(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	member := g.memberOnB()
	ctx := context.Background()

	o, share, ann1, id1 := g.importedOnB()
	replace := func(body []byte, expected int, unverified bool) string {
		t.Helper()
		hash, receipt := g.uploadOnA(body)
		if unverified {
			// The original fails its fixity check before A seals it, so
			// version 2 can never be sealed: it stays a draft.
			if _, err := g.a.pool.Exec(ctx, `UPDATE storage_variants SET fixity_status = 'mismatch'
				WHERE object_hash = $1 AND variant_key = 'original'`, hash); err != nil {
				t.Fatal(err)
			}
		}
		if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
			"file_hash": hash, "upload_receipt": receipt, "expected_version": expected, "file_extension": "png"}, nil); st != http.StatusCreated {
			t.Fatalf("version %d on A: %d", expected+1, st)
		}
		return hash
	}
	hash2 := replace(pngBytes(t), 1, true)
	body3 := pngBytes(t)
	hash3 := replace(body3, 2, false)
	g.sealedOnA(o)
	announcement := func(n int) string {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if ann := g.announcementsOnA(o); len(ann) == n {
				return ann[n-1]
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("A announced %d versions, want %d", len(g.announcementsOnA(o)), n)
		return ""
	}
	numberOnA := func(ann string) string {
		t.Helper()
		var shareID, number string
		if err := g.a.pool.QueryRow(ctx, `SELECT a.payload->>'share_id', v.number::text FROM activities a
			JOIN asset_versions v ON v.id::text = a.payload->>'version_id' WHERE a.activity_uri = $1`, ann).Scan(&shareID, &number); err != nil || shareID != share {
			t.Fatalf("announcement %s: share %s (%v)", ann, shareID, err)
		}
		return number
	}
	ann3 := announcement(2)
	if n := numberOnA(ann3); n != "3" {
		t.Fatalf("A's second announcement is version %s, want version 3 (version 2 stayed a draft)", n)
	}
	g.recordStateOnB(ann3, "complete")
	g.recordStateOnB(ann1, "superseded")
	id3 := g.receivedOnB(ann3)
	if n := g.countOnB(`SELECT count(*) FROM federation_received_assets s JOIN federation_received_assets p ON p.id = s.predecessor_id
		WHERE s.id::text = $1 AND p.id::text = $2 AND s.version_number = 3 AND p.version_number = 1`, id3, id1); n != 1 {
		t.Fatal("B's version 3 is not linked to its version 1")
	}
	if st, got := g.getOnB(member, "/federation/received-assets/"+id3+"/original"); st != http.StatusOK || !bytes.Equal(got, body3) {
		t.Fatalf("version 3's original on B: %d", st)
	}
	if n := g.countOnB(`SELECT count(*) FROM storage_objects WHERE hash = $1`, hash2); n != 0 {
		t.Fatal("the abandoned version's bytes reached B")
	}

	// A re-rendered access copy of version 3: A's reconciler seals a
	// rendition successor, version 4, which follows version 3.
	if _, err := g.storageOf(g.a).PutVariant(ctx, hash3, "hires", bytes.NewReader(pngBytes(t)), "image/webp"); err != nil {
		t.Fatal(err)
	}
	ann4 := announcement(3)
	if n := numberOnA(ann4); n != "4" {
		t.Fatalf("A's third announcement is version %s, want the rendition successor 4", n)
	}
	g.recordStateOnB(ann4, "complete")
	g.recordStateOnB(ann3, "superseded")
	if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND state <> 'rejected'`, g.peerAonB); n < 3 {
		t.Fatalf("B holds %d versions", n)
	}
	if n := len(g.announcementsOnA(o)); n != 3 {
		t.Fatalf("A announced %d times, want 3", n)
	}
}

// #1575 (spec v1 §17.2.8): an asset's mature and aiProvenance flags, end
// to end. A edits the flags through its real API; B shows them on the
// record it holds, its tier and visibility unchanged. A new file version
// follows; a second edit, made while that version's announcement is still
// on its way, reaches B before the version it names, is kept, and still
// shows once the version is complete. A withdraws the asset; B withdraws
// every version.
func TestTwoStack_AnAssetsFlagsFollowItsOrigin(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	member := g.memberOnB()
	ctx := context.Background()

	o, _, ann1, id1 := g.importedOnB()
	patch := func(body map[string]any) {
		t.Helper()
		if st := g.api(g.a, http.MethodPatch, "/assets/"+o.asset.String(), body, nil); st != http.StatusOK {
			t.Fatalf("PATCH on A: %d", st)
		}
	}
	revisionsOnB := func(versionSuffix string) int {
		return g.countOnB(`SELECT count(*) FROM federation_received_asset_revisions
			WHERE activity_type = 'Update' AND version_uri LIKE '%' || $1 AND origin_asset_uri LIKE '%' || $2`,
			versionSuffix, o.asset.String())
	}
	waitRevisions := func(versionSuffix string, want int) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if revisionsOnB(versionSuffix) >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("B kept %d Updates naming %s, want %d", revisionsOnB(versionSuffix), versionSuffix, want)
	}
	declared := func(id string) map[string]any {
		t.Helper()
		st, raw := g.getOnB(member, "/federation/received-assets/"+id)
		if st != http.StatusOK {
			t.Fatalf("B's detail of %s: %d", id, st)
		}
		var d struct {
			Declared map[string]any `json:"declared"`
			Asset    struct {
				Sensitivity string `json:"sensitivity"`
			} `json:"asset"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		if d.Asset.Sensitivity != "public" {
			t.Fatalf("B's record tier changed: %s", d.Asset.Sensitivity)
		}
		return d.Declared
	}

	// 1. The flags change on A.
	if d := declared(id1); d["mature"] != false || d["aiProvenance"] != nil {
		t.Fatalf("B before the edit: %v", d)
	}
	patch(map[string]any{"mature": true, "ai_provenance": "generated"})
	waitRevisions("/versions/1", 1)
	if d := declared(id1); d["mature"] != true || d["aiProvenance"] != "generated" {
		t.Fatalf("B after the edit: %v", d)
	}

	// 2. A new file version, its announcement held at B's door; a second
	// edit meanwhile names it, and reaches B first.
	var held atomic.Int32
	g.b.setInboxFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(`"aa:AssetVersion"`)) && bytes.Contains(body, []byte(o.asset.String()+"/versions/2")) {
				held.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		h.ServeHTTP(w, r)
	})
	body2 := pngBytes(t)
	hash2, receipt := g.uploadOnA(body2)
	if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
		"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, nil); st != http.StatusCreated {
		t.Fatalf("version 2 on A: %d", st)
	}
	g.sealedOnA(o)
	deadline := time.Now().Add(60 * time.Second)
	for held.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if held.Load() == 0 {
		t.Fatal("A never tried to deliver version 2")
	}
	patch(map[string]any{"mature": false, "clear_ai_provenance": true})
	waitRevisions("/versions/2", 1)
	if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri LIKE '%' || $1 AND version_number = 2`,
		o.asset.String()); n != 0 {
		t.Fatalf("B holds %d records of version 2 before its announcement arrived", n)
	}
	g.b.setInboxFront(nil)
	if _, err := g.a.pool.Exec(ctx, `UPDATE federation_outbox o SET next_attempt_at = now() FROM activities a
		WHERE a.id = o.activity_id AND a.activity_type = 'aa:AssetVersion' AND a.object_local_id = $1 AND o.status = 'queued'`,
		o.asset.String()); err != nil {
		t.Fatal(err)
	}
	ann := g.announcementsOnA(o)
	ann2 := ann[len(ann)-1]
	g.recordStateOnB(ann2, "complete")
	g.recordStateOnB(ann1, "superseded")
	id2 := g.receivedOnB(ann2)
	if d := declared(id2); d["mature"] != false || d["aiProvenance"] != nil {
		t.Fatalf("B's version 2, after the Update that reached it first: %v", d)
	}
	if st, got := g.getOnB(member, "/federation/received-assets/"+id2+"/original"); st != http.StatusOK || !bytes.Equal(got, body2) {
		t.Fatalf("version 2's original on B: %d", st)
	}

	// 3. A withdraws the asset: every version on B is withdrawn.
	g.deleteOnA(o, "")
	g.recordStateOnB(ann1, "withdrawn/withdrawn")
	g.recordStateOnB(ann2, "withdrawn/withdrawn")
	if st, _ := g.getOnB(member, "/federation/received-assets/"+id2); st != http.StatusGone {
		t.Fatalf("version 2 after the withdrawal: %d", st)
	}
}
