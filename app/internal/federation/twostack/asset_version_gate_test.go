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
