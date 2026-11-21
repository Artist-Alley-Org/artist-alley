// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 slice 3: the full receive gate, on two production servers.
//
// TestTwoStack_UploadToView: a real PNG is uploaded through A's upload
// API and created as an asset, A's production processing seals its
// version, A's admin grants the original to B, A announces it, B's
// production inbox and worker import it, and a signed-in user of B sees
// its record and downloads the original through B's real API, byte for
// byte; anonymous callers get nothing; an outage at A is retried; A's
// revoke makes B refuse the next request. (The tampered transfer is
// TestTwoStack_AssetSender's last subtest; the browser view of the same
// API is the UI companion, recorded with the slice's evidence.)
//
// TestTwoStack_ReorderedGrantDispatch: B receives the aa:Share and the
// announcement, but dispatches the announcement first. The record waits
// for its grant (awaiting_grant), fetching nothing; the share then wakes
// it and it completes.
//
// Test-only, as in TestTwoStack_AssetSender: archival-1 and
// original-transfer-1 are advertised for the gate only. Neither is in
// production's peer.KnownCapabilities.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// archivalGate is a CAS gate with archival-1 advertised (test-only).
func archivalGate(t *testing.T) *casGate {
	t.Helper()
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1)
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	return newCASGate(t)
}

// pngBytes is a real, decodable PNG of random pixels.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	px := make([]byte, 96*96*3)
	_, _ = rand.Read(px)
	for i := 0; i < 96*96; i++ {
		img.Set(i%96, i/96, color.RGBA{px[3*i], px[3*i+1], px[3*i+2], 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// uploadAsset uploads body through A's real upload API as A's admin and
// creates a public image asset from it.
func (g *casGate) uploadAsset(body []byte) original {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.a.url+"/api/v1/storage/objects", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.a.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Content-Type", "image/png")
	req.Header.Set("X-Original-Filename", "harbour.png")
	resp, err := (&http.Client{Transport: g.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var up struct {
		Hash    string `json:"hash"`
		Receipt string `json:"upload_receipt"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(raw, &up) != nil || up.Receipt == "" {
		g.t.Fatalf("upload: %d %s", resp.StatusCode, raw)
	}
	sum := sha256.Sum256(body)
	if up.Hash != hex.EncodeToString(sum[:]) {
		g.t.Fatalf("A stored %s for bytes hashing to %x", up.Hash, sum)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if st := g.api(g.a, http.MethodPost, "/assets", map[string]any{"title": "Harbour", "asset_type": 1,
		"file_hash": up.Hash, "upload_receipt": up.Receipt, "file_extension": "png"}, &asset); st != http.StatusCreated {
		g.t.Fatalf("create asset: %d %+v", st, asset)
	}
	// As stored: a new asset is public by default, and points at the
	// uploaded bytes.
	var tier, fileHash string
	if err := g.a.pool.QueryRow(context.Background(), `SELECT sensitivity, file_hash FROM assets WHERE id = $1`,
		asset.ID).Scan(&tier, &fileHash); err != nil || tier != "public" || fileHash != up.Hash {
		g.t.Fatalf("the uploaded asset is %q over %s (%v)", tier, fileHash, err)
	}
	return original{asset: uuid.MustParse(asset.ID), hash: up.Hash, body: body}
}

// receivedOnB is B's record id for activityURI.
func (g *casGate) receivedOnB(activityURI string) string {
	g.t.Helper()
	var id string
	if err := g.b.pool.QueryRow(context.Background(), `SELECT id::text FROM federation_received_assets
		WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, activityURI).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}

// ledgerOnB is B's ledger for a record, oldest first, as type/outcome/reason.
func (g *casGate) ledgerOnB(recordID string) []string {
	g.t.Helper()
	rows, err := g.b.pool.Query(context.Background(), `SELECT event_type || '/' || outcome || '/' || COALESCE(reason, '')
		FROM federation_transfer_events WHERE received_asset_id = $1::uuid ORDER BY occurred_at, file_position NULLS LAST`, recordID)
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

func TestTwoStack_UploadToView(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }
	member := g.memberOnB()

	// view answers B's three routes for the record as tok sees them.
	view := func(tok, id string) (list, detail, orig int, bytesOK bool) {
		t.Helper()
		st, raw := g.getOnB(tok, "/federation/received-assets?limit=200")
		list = st
		if st == http.StatusOK && !bytes.Contains(raw, []byte(id)) {
			list = http.StatusNotFound // not listed
		}
		detail, _ = g.getOnB(tok, "/federation/received-assets/"+id)
		var body []byte
		orig, body = g.getOnB(tok, "/federation/received-assets/"+id+"/original")
		return list, detail, orig, body != nil
	}

	t.Run("upload, seal, grant, announce, import, view and download", func(t *testing.T) {
		body := pngBytes(t)
		o := g.uploadAsset(body)
		g.sealedOnA(o)
		share := g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		id := g.receivedOnB(ann[0])

		// B's user: listed, detailed (declared name and detected type),
		// and the original's bytes are A's uploaded bytes.
		st, raw := g.getOnB(member, "/federation/received-assets/"+id)
		var d struct {
			OriginalAvailable bool `json:"original_available"`
			Asset             struct {
				Original struct {
					SHA256   string `json:"sha256"`
					Declared string `json:"declared_media_type"`
					Name     string `json:"declared_original_filename"`
					Detected string `json:"detected_media_type"`
				} `json:"original"`
			} `json:"asset"`
		}
		if st != http.StatusOK || json.Unmarshal(raw, &d) != nil || !d.OriginalAvailable || d.Asset.Original.SHA256 != o.hash ||
			d.Asset.Original.Detected != "image/png" {
			t.Fatalf("detail %d: %s", st, raw)
		}
		st, got := g.getOnB(member, "/federation/received-assets/"+id+"/original")
		if st != http.StatusOK || !bytes.Equal(got, body) {
			t.Fatalf("original: %d, %d bytes (uploaded %d)", st, len(got), len(body))
		}
		if l, _, _, _ := view(member, id); l != http.StatusOK {
			t.Fatalf("not listed for B's user: %d", l)
		}
		// Anonymous: nothing.
		if l, dt, or, _ := view("", id); l != http.StatusUnauthorized || dt != http.StatusNotFound || or != http.StatusNotFound {
			t.Fatalf("anonymous: list %d detail %d original %d", l, dt, or)
		}
		// The ledger: each original verified, then the completion.
		if led := g.ledgerOnB(id); len(led) != 2 || led[0] != "fixity-check/success/" || led[1] != "ingestion/success/" {
			t.Fatalf("B's ledger: %v", led)
		}
		// A revokes: B refuses the next request.
		g.revoke(share)
		g.waitOnB(assetURI(o), "ended/revoked")
		if l, dt, or, _ := view(member, id); l == http.StatusOK || dt != http.StatusNotFound || or != http.StatusNotFound {
			t.Fatalf("after the revoke: list %d detail %d original %d", l, dt, or)
		}
	})

	t.Run("an outage at A is retried, then the import completes", func(t *testing.T) {
		body := pngBytes(t)
		o := g.uploadAsset(body)
		g.sealedOnA(o)
		var failed atomic.Int32
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodGet && failed.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements, want 1", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		id := g.receivedOnB(ann[0])
		if failed.Load() < 2 {
			t.Fatalf("A served %d GETs; the outage was not retried", failed.Load())
		}
		led := g.ledgerOnB(id)
		if len(led) != 3 || led[0] != "transfer/warning/http_status" || led[2] != "ingestion/success/" {
			t.Fatalf("B's ledger: %v", led)
		}
		if st, got := g.getOnB(member, "/federation/received-assets/"+id+"/original"); st != http.StatusOK || !bytes.Equal(got, body) {
			t.Fatalf("original after the retry: %d", st)
		}
	})
}

func TestTwoStack_ReorderedGrantDispatch(t *testing.T) {
	g := archivalGate(t)
	ctx := context.Background()
	// Only A works for now: B stores what A delivers and dispatches
	// nothing until its workers start.
	g.runWorkers(g.a)
	o := g.newOriginal("public")
	g.sealedOnA(o)
	assetURI := g.a.url + "/assets/" + o.asset.String()
	g.grantWith(o, map[string]any{"allow_original": true})
	ann := g.announcementsOnA(o)
	if len(ann) != 1 {
		t.Fatalf("A recorded %d announcements, want 1", len(ann))
	}
	// Both are in B's inbox, undispatched.
	deadline := time.Now().Add(120 * time.Second)
	for g.countOnB(`SELECT count(*) FROM federation_inbox WHERE status = 'pending'
		AND ((activity_type = 'aa:Share' AND envelope_json->>'object' = $1) OR activity_uri = $2)`, assetURI, ann[0]) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("A never delivered both the share and the announcement to B")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// B's dispatch of the share is held back behind the announcement (its
	// inbox dispatches in received_at order).
	if _, err := g.b.pool.Exec(ctx, `UPDATE federation_inbox SET received_at = (
		    SELECT received_at + interval '1 second' FROM federation_inbox WHERE activity_uri = $2)
		  WHERE activity_type = 'aa:Share' AND envelope_json->>'object' = $1 AND status = 'pending'`, assetURI, ann[0]); err != nil {
		t.Fatal(err)
	}
	gets := g.a.casGETs.Load()
	g.runWorkers(g.b)
	g.recordStateOnB(ann[0], "complete")
	id := g.receivedOnB(ann[0])
	led := g.ledgerOnB(id)
	if len(led) != 3 || led[0] != "ingestion/warning/awaiting_grant" || led[1] != "fixity-check/success/" || led[2] != "ingestion/success/" {
		t.Fatalf("B's ledger: %v (the announcement did not wait for its grant)", led)
	}
	if n := g.countOnB(`SELECT attempts FROM federation_received_assets WHERE id = $1::uuid`, id); n != 1 {
		t.Fatalf("%d attempts: waiting must not use any", n)
	}
	if g.a.casGETs.Load() != gets+1 {
		t.Fatalf("A served %d GETs, want 1", g.a.casGETs.Load()-gets)
	}
}
