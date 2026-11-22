// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1575: an asset's withdrawal (spec v1 §17.3), on two production
// servers. A uploads, seals, grants and announces an asset; B imports
// it; A's owner deletes it through A's real API; A sends the Delete with
// the aa:tombstone built from the sealed version; B withdraws its record
// (ledger event, pins released, tombstone kept) and answers B's user a
// 410 withdrawal notice for the record and its original, and anonymous
// callers 404. A redelivery changes nothing. A withdrawal after the
// share was revoked is still applied, and shows no notice. A tampered
// withdrawal is refused and applies nothing until the real one is
// re-sent. A newer version A never announced to B is never named to B
// (rev628). A withdrawal still undelivered when A purges the asset is
// sent and applied after the purge (rev628). An announcement whose
// request is in flight when A deletes the asset reaches B, and the
// withdrawal waits for it and names that version (rev630).
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

	"github.com/mscrnt/artist-alley/app/internal/softdelete"
)

// importedOnB uploads, seals, grants and announces a new asset on A and
// waits for B's record of it to complete. It returns the asset, its
// share id, the announcement URI and B's record id.
func (g *casGate) importedOnB() (original, string, string, string) {
	g.t.Helper()
	o := g.uploadAsset(pngBytes(g.t))
	g.sealedOnA(o)
	share := g.grantWith(o, map[string]any{"allow_original": true})
	ann := g.announcementsOnA(o)
	if len(ann) != 1 {
		g.t.Fatalf("A recorded %d announcements, want 1", len(ann))
	}
	g.recordStateOnB(ann[0], "complete")
	return o, share, ann[0], g.receivedOnB(ann[0])
}

// withdrawalOnA waits for A's outbox row of o's withdrawal to B and
// returns its id and status.
func (g *casGate) withdrawalOnA(o original) (rowID, status string) {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		err := g.a.pool.QueryRow(context.Background(), `SELECT o.id::text, o.status FROM federation_outbox o
			JOIN activities a ON a.id = o.activity_id
			WHERE a.activity_type = 'Delete' AND a.object_kind = 'asset' AND a.object_local_id = $1 AND o.peer_id = $2`,
			o.asset.String(), g.peerBonA).Scan(&rowID, &status)
		if err == nil {
			return rowID, status
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatal("A never queued the withdrawal")
	return "", ""
}

// outboxStatusOnA waits for A's outbox row id to reach want.
func (g *casGate) outboxStatusOnA(id, want string) {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if err := g.a.pool.QueryRow(context.Background(), `SELECT status FROM federation_outbox WHERE id = $1::uuid`, id).Scan(&got); err == nil && got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("A's outbox row %s is %s, want %s", id, got, want)
}

func (g *casGate) deleteOnA(o original, reason string) {
	g.t.Helper()
	if st := g.api(g.a, http.MethodDelete, "/assets/"+o.asset.String(), map[string]any{"reason": reason}, nil); st != http.StatusNoContent {
		g.t.Fatalf("delete on A: %d", st)
	}
}

func TestTwoStack_AssetWithdrawal(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	member := g.memberOnB()
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }
	recordPins := func(id string) int {
		return g.countOnB(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = $1`, id)
	}
	tombstones := func(o original) int {
		return g.countOnB(`SELECT count(*) FROM federation_received_tombstones WHERE peer_id = $1 AND origin_asset_uri = $2`, g.peerAonB, assetURI(o))
	}
	deaccessions := func(id string) int {
		return g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1::uuid AND event_type = 'deaccession'`, id)
	}

	t.Run("upload, import, view, delete on A, notice on B, replay is a no-op", func(t *testing.T) {
		o, _, ann, id := g.importedOnB()
		kept, _, keptAnn, keptID := g.importedOnB() // another asset, never deleted
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id+"/original"); st != http.StatusOK {
			t.Fatalf("original before the withdrawal: %d", st)
		}
		if recordPins(id) == 0 {
			t.Fatal("the complete record holds no pin")
		}

		g.deleteOnA(o, "local operator note")
		wd, _ := g.withdrawalOnA(o)
		g.recordStateOnB(ann, "withdrawn/withdrawn")
		g.outboxStatusOnA(wd, "sent")

		// B's user: a withdrawal notice for the record and its original;
		// no longer listed; the local delete reason was never sent.
		st, raw := g.getOnB(member, "/federation/received-assets/"+id)
		var n struct {
			ID          string  `json:"id"`
			Origin      string  `json:"origin_asset_uri"`
			WithdrawnAt string  `json:"withdrawn_at"`
			Deleted     string  `json:"deleted"`
			Reason      *string `json:"reason"`
			LastVersion string  `json:"last_version"`
		}
		if st != http.StatusGone || json.Unmarshal(raw, &n) != nil || n.ID != id || n.Origin != assetURI(o) ||
			n.LastVersion != assetURI(o)+"/versions/1" || n.Deleted == "" || n.WithdrawnAt == "" || n.Reason != nil {
			t.Fatalf("detail after the withdrawal: %d %s", st, raw)
		}
		if bytes.Contains(raw, []byte("local operator note")) {
			t.Fatal("the local delete reason reached B")
		}
		if st, raw := g.getOnB(member, "/federation/received-assets/"+id+"/original"); st != http.StatusGone || bytes.Contains(raw, []byte("PNG")) {
			t.Fatalf("original after the withdrawal: %d (%d bytes)", st, len(raw))
		}
		if st, raw := g.getOnB(member, "/federation/received-assets?limit=200"); st != http.StatusOK || bytes.Contains(raw, []byte(id)) {
			t.Fatalf("list after the withdrawal: %d, listed %v", st, bytes.Contains(raw, []byte(id)))
		}
		// Anonymous callers learn nothing.
		for _, p := range []string{"/federation/received-assets/" + id, "/federation/received-assets/" + id + "/original"} {
			if st, _ := g.getOnB("", p); st != http.StatusNotFound {
				t.Fatalf("anonymous %s: %d, want 404", p, st)
			}
		}
		// B's record of it: the ledger, the pins, the tombstone.
		led := g.ledgerOnB(id)
		if len(led) == 0 || led[len(led)-1] != "deaccession/success/" || deaccessions(id) != 1 {
			t.Fatalf("B's ledger: %v", led)
		}
		if n := recordPins(id); n != 0 {
			t.Fatalf("%d record pins after the withdrawal", n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1::uuid AND role = 'original' AND state <> 'withdrawn'`, id); n != 0 {
			t.Fatalf("%d originals not withdrawn", n)
		}
		if tombstones(o) != 1 {
			t.Fatal("B kept no tombstone")
		}
		// The other asset is untouched.
		g.recordStateOnB(keptAnn, "complete")
		if st, _ := g.getOnB(member, "/federation/received-assets/"+keptID+"/original"); st != http.StatusOK || recordPins(keptID) == 0 || tombstones(kept) != 0 {
			t.Fatalf("the other asset: original %d, pins %d, tombstones %d", st, recordPins(keptID), tombstones(kept))
		}

		// A redelivery of the same Delete (as after a lost answer)
		// changes nothing on B.
		if _, err := g.a.pool.Exec(context.Background(), `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now() WHERE id = $1::uuid`, wd); err != nil {
			t.Fatal(err)
		}
		g.outboxStatusOnA(wd, "sent")
		if deaccessions(id) != 1 || tombstones(o) != 1 {
			t.Fatalf("after a redelivery: %d deaccessions, %d tombstones", deaccessions(id), tombstones(o))
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_type = 'Delete' AND envelope_json->>'object' = $1`, assetURI(o)); n != 1 {
			t.Fatalf("B stored %d inbox rows for the withdrawal", n)
		}
	})

	t.Run("a withdrawal after the share was revoked is applied and shows no notice", func(t *testing.T) {
		o, share, ann, id := g.importedOnB()
		g.revoke(share)
		g.waitOnB(assetURI(o), "ended/revoked")
		g.deleteOnA(o, "")
		g.recordStateOnB(ann, "withdrawn/withdrawn")
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id); st != http.StatusNotFound {
			t.Fatalf("detail without a live grant: %d, want 404", st)
		}
		if recordPins(id) != 0 || tombstones(o) != 1 || deaccessions(id) != 1 {
			t.Fatalf("pins %d tombstones %d deaccessions %d", recordPins(id), tombstones(o), deaccessions(id))
		}
	})

	t.Run("a tampered withdrawal applies nothing", func(t *testing.T) {
		o, _, ann, id := g.importedOnB()
		var tampered atomic.Int32
		g.b.setInboxFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/federation/inbox") {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte(`"Delete"`)) && bytes.Contains(body, []byte(o.asset.String())) {
					// One character of the withdrawal changed in transit:
					// the sealed payload when A sealed it, else the listed
					// checksum.
					changed, form := tamper(body, []byte(`"ciphertext":"`), 'A', 'B'), "sealed payload"
					if changed == nil {
						changed, form = tamper(body, []byte(`"value":"`+o.hash[:8]), '0', '1'), "checksum in clear"
					}
					t.Logf("tampering the withdrawal's %s", form)
					if changed == nil {
						t.Logf("withdrawal body with nothing to tamper: %.400s", body)
					} else {
						tampered.Add(1)
						body = changed
					}
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
			h.ServeHTTP(w, r)
		})
		g.deleteOnA(o, "")
		wd, _ := g.withdrawalOnA(o)
		g.outboxStatusOnA(wd, "failed")
		g.b.setInboxFront(nil)
		if tampered.Load() == 0 {
			t.Fatal("the withdrawal never reached B's front")
		}
		g.recordStateOnB(ann, "complete")
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id+"/original"); st != http.StatusOK || tombstones(o) != 0 || deaccessions(id) != 0 {
			t.Fatalf("after the tampered withdrawal: original %d tombstones %d deaccessions %d", st, tombstones(o), deaccessions(id))
		}
		// A's operator re-queues it; the real one applies.
		if st := g.api(g.a, http.MethodPost, "/admin/federation/outbox/"+wd+"/requeue", nil, nil); st != http.StatusOK {
			t.Fatalf("requeue: %d", st)
		}
		g.recordStateOnB(ann, "withdrawn/withdrawn")
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id); st != http.StatusGone {
			t.Fatalf("detail after the real withdrawal: %d", st)
		}
	})

	t.Run("a newer version never announced to B is not named to B", func(t *testing.T) {
		o, share, ann, id := g.importedOnB()
		// Revoked first, so version 2's seal finds no live share and
		// announces it to nobody (#1575: a seal announces under the live
		// shares).
		g.revoke(share)
		g.waitOnB(assetURI(o), "ended/revoked")
		hash2, receipt := g.uploadOnA(pngBytes(t))
		var v struct {
			Number int `json:"number"`
		}
		if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
			"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, &v); st != http.StatusCreated || v.Number != 2 {
			t.Fatalf("version 2 on A: %d %+v", st, v)
		}
		g.sealedOnA(o) // version 2, sealed and current, announced to nobody
		if n := len(g.announcementsOnA(o)); n != 1 {
			t.Fatalf("A announced %d versions, want version 1 only", n)
		}
		g.deleteOnA(o, "")
		g.recordStateOnB(ann, "withdrawn/withdrawn")
		// The share was revoked, so no user sees the notice (404); what B
		// keeps names version 1.
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id); st != http.StatusNotFound {
			t.Fatalf("detail %d, want 404 after the revoke", st)
		}
		var kept []byte
		var last string
		if err := g.b.pool.QueryRow(context.Background(), `SELECT tombstone::text, last_version FROM federation_received_tombstones
			WHERE peer_id = $1 AND origin_asset_uri = $2`, g.peerAonB, assetURI(o)).Scan(&kept, &last); err != nil {
			t.Fatal(err)
		}
		if last != assetURI(o)+"/versions/1" {
			t.Fatalf("B's kept tombstone names %s, want version 1", last)
		}
		if bytes.Contains(kept, []byte(hash2)) || bytes.Contains(kept, []byte("/versions/2")) || !bytes.Contains(kept, []byte(o.hash)) {
			t.Fatalf("B's kept tombstone names what B was never sent: %s", kept)
		}
	})

	t.Run("a withdrawal undelivered at the purge is sent and applied after it", func(t *testing.T) {
		o, _, ann, id := g.importedOnB()
		var held atomic.Int32
		g.b.setInboxFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodPost {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte(`"Delete"`)) && bytes.Contains(body, []byte(o.asset.String())) {
					held.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
			h.ServeHTTP(w, r)
		})
		g.deleteOnA(o, "")
		wd, _ := g.withdrawalOnA(o)
		deadline := time.Now().Add(60 * time.Second)
		for held.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if held.Load() == 0 {
			t.Fatal("A never tried to deliver the withdrawal")
		}
		// A's retention purge removes the asset and its versions.
		ctx := context.Background()
		if _, err := g.a.pool.Exec(ctx, `UPDATE assets SET deleted_at = now() - interval '3 days' WHERE id = $1`, o.asset); err != nil {
			t.Fatal(err)
		}
		if n, err := softdelete.NewService(g.a.pool, nil).HardDeletePastAssets(ctx, 1, 1000); err != nil || n < 1 {
			t.Fatalf("purge on A: %d %v", n, err)
		}
		var left int
		if err := g.a.pool.QueryRow(ctx, `SELECT count(*) FROM asset_versions WHERE asset_id = $1`, o.asset).Scan(&left); err != nil || left != 0 {
			t.Fatalf("the purge left %d versions on A (%v)", left, err)
		}
		g.b.setInboxFront(nil)
		if _, err := g.a.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() WHERE id = $1::uuid AND status = 'queued'`, wd); err != nil {
			t.Fatal(err)
		}
		g.outboxStatusOnA(wd, "sent")
		g.recordStateOnB(ann, "withdrawn/withdrawn")
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id); st != http.StatusGone || recordPins(id) != 0 || deaccessions(id) != 1 {
			t.Fatalf("after the purge: detail %d pins %d deaccessions %d", st, recordPins(id), deaccessions(id))
		}
	})

	t.Run("an announcement in flight at the delete reaches B and is the version withdrawn", func(t *testing.T) {
		o, share, ann1, id := g.importedOnB()
		g.revoke(share)
		g.waitOnB(assetURI(o), "ended/revoked")
		hash2, receipt := g.uploadOnA(pngBytes(t))
		if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
			"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, nil); st != http.StatusCreated {
			t.Fatalf("version 2 on A: %d", st)
		}
		g.sealedOnA(o)
		// B's inbox holds A's announcement of version 2 in flight.
		var held atomic.Int32
		release := make(chan struct{})
		g.b.setInboxFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodPost {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte(`"aa:AssetVersion"`)) && bytes.Contains(body, []byte(o.asset.String())) {
					held.Add(1)
					<-release
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
			h.ServeHTTP(w, r)
		})
		defer g.b.setInboxFront(nil)
		g.grantWith(o, map[string]any{"allow_original": true}) // announces version 2
		deadline := time.Now().Add(60 * time.Second)
		for held.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if held.Load() == 0 {
			close(release)
			t.Fatal("A never sent version 2's announcement")
		}
		// Deleted while that request is in flight (its row still queued).
		g.deleteOnA(o, "")
		wd, _ := g.withdrawalOnA(o)
		var annStatus string
		if err := g.a.pool.QueryRow(context.Background(), `SELECT o.status FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
			WHERE a.activity_type = 'aa:AssetVersion' AND a.object_local_id = $1 AND o.peer_id = $2
			ORDER BY a.created_at DESC LIMIT 1`, o.asset.String(), g.peerBonA).Scan(&annStatus); err != nil || annStatus != "queued" {
			close(release)
			t.Fatalf("version 2's announcement is %q (%v) at the delete, want queued (in flight)", annStatus, err)
		}
		close(release)
		g.outboxStatusOnA(wd, "sent")
		g.recordStateOnB(ann1, "withdrawn/withdrawn")
		var kept []byte
		if err := g.b.pool.QueryRow(context.Background(), `SELECT tombstone::text FROM federation_received_tombstones
			WHERE peer_id = $1 AND origin_asset_uri = $2`, g.peerAonB, assetURI(o)).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(kept, []byte(assetURI(o)+`/versions/2"`)) || !bytes.Contains(kept, []byte(hash2)) || bytes.Contains(kept, []byte(o.hash)) {
			t.Fatalf("B's kept tombstone %s, want version 2, the version B was sent last", kept)
		}
		var attempts int
		if err := g.a.pool.QueryRow(context.Background(), `SELECT attempts FROM federation_outbox WHERE id = $1::uuid`, wd).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatalf("the withdrawal used %d failed attempts while it waited (%v)", attempts, err)
		}
		if st, _ := g.getOnB(member, "/federation/received-assets/"+id); st == http.StatusOK {
			t.Fatal("B still serves the withdrawn record")
		}
	})
}

// tamper changes the character right after the first prefix in body to
// one of two others (both valid in its alphabet), or is nil when body has
// no prefix.
func tamper(body, prefix []byte, a, b byte) []byte {
	i := bytes.Index(body, prefix)
	if i < 0 {
		return nil
	}
	out := bytes.Clone(body)
	k := i + len(prefix)
	if out[k] == a {
		out[k] = b
	} else {
		out[k] = a
	}
	return out
}
