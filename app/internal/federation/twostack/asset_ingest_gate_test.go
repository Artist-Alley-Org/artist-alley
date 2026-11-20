// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 gate (receiver core): an asset record announced by A is received
// by B, and its original file is held on B only after a grant A sent
// through its real outbox, a signed fetch from A's real CAS route, and
// B's own size and SHA-256 check.
//
// Test-only, labelled where installed:
//
//   - TEST-ONLY ADVERTISEMENT of archival-1 (here) and
//     original-transfer-1 (newCASGate), before the real handshake. Neither
//     is advertised in production until the rest of #1574 lands.
//   - THE ANNOUNCEMENT. No sender emits aa:AssetVersion yet, and the
//     inbox does not route it to the receiver core, so the test builds the
//     envelope from A's real asset and hands it to B's receiver core
//     directly. Everything it relies on is production: the pairing, A's
//     admin share API and #1573 policy, A's outbox delivering aa:Share to
//     B's inbox, B's inbound grant record, the casfetch client with B's
//     identity, and B's database and storage.
//
// "View" is not part of this slice: a complete record is held and
// verified, and no page or API shows it yet.

package twostack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

func TestTwoStack_AssetIngest(t *testing.T) {
	// TEST-ONLY ADVERTISEMENT of archival-1, before newCASGate adds
	// original-transfer-1 and pairs the instances.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1)
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	g := newCASGate(t)
	for _, side := range []struct{ in, other *instance }{{g.a, g.b}, {g.b, g.a}} {
		caps := g.capabilities(side.in, side.other)
		for _, want := range []string{federation.ProfileArchival1, string(peer.CapOriginalTransfer1), federation.ProfileShareLifecycle1} {
			if !contains(caps, want) {
				t.Fatalf("%s did not negotiate %s with %s through the handshake", side.in.name, want, side.other.name)
			}
		}
	}
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()
	grants := inboundgrants.New(g.b.pool, func(context.Context) string { return g.b.url }, nil, nil)
	ingest := assetingest.New(assetingest.Deps{
		Pool: g.b.pool, Storage: g.storageOf(g.b), Peers: peerLookupB{g}, Grants: grants,
		Fetcher: g.recordClient(grants), Audit: audit.NewRecorder(g.b.pool, g.logger), Logger: g.logger,
	})
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }
	announce := func(o original) *federation.Envelope {
		t.Helper()
		size := len(o.body)
		member := map[string]any{
			"id": assetURI(o),
			"version": map[string]any{"id": assetURI(o) + "/versions/1", "number": 1,
				"created": time.Now().UTC().Format(time.RFC3339), "predecessor": nil},
			"files": []map[string]any{{"role": "original", "contentUri": "cas://" + o.hash,
				"checksums": []map[string]string{{"algorithm": "sha256", "value": o.hash}}, "size": size,
				"mediaType": "image/png", "originalFilename": "gate.png"}},
			"sensitivity": "public", "mature": false,
		}
		raw, err := json.Marshal(member)
		if err != nil {
			t.Fatal(err)
		}
		return &federation.Envelope{
			Context: federation.ContextV1, Type: federation.ActivityAAAssetVersion,
			ID: g.a.url + "/activities/" + uuid.NewString(), Actor: g.a.url + "/users/admin",
			Published: time.Now().UTC(), Object: assetURI(o),
			Extra: map[string]json.RawMessage{federation.MemberArchivalAsset: raw},
		}
	}
	receive := func(e *federation.Envelope) (assetingest.Outcome, error) {
		return ingest.Receive(ctx, assetingest.Announcement{PeerID: g.peerAonB, Envelope: e, Signer: g.signerB})
	}
	recordPins := func(o original) int {
		return g.onB(o, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset'`)
	}
	wantIngest := func(t *testing.T, err error, want string) {
		t.Helper()
		if got := assetingest.ReasonOf(err); got != want {
			t.Fatalf("reason %q (%v), want %q", got, err, want)
		}
	}

	t.Run("upload, grant, fetch, verify, hold", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
		gets := g.a.casGETs.Load()
		out, err := receive(announce(o))
		if err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("receive: %+v %v", out, err)
		}
		if g.a.casGETs.Load() != gets+1 {
			t.Fatalf("A served %d GETs, want 1", g.a.casGETs.Load()-gets)
		}
		if recordPins(o) != 1 || g.onB(o, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_ingest'`) != 0 {
			t.Fatal("B does not hold the original under the record alone")
		}
		if n := g.onB(o, `SELECT count(*) FROM federation_received_asset_files WHERE object_hash = $1 AND state = 'verified'`); n != 1 {
			t.Fatalf("%d verified files on B", n)
		}
		if n := g.onB(o, `SELECT count(*) FROM assets WHERE file_hash = $1`); n != 0 {
			t.Fatal("B created an assets row for a received record")
		}
		var grantOnB uuid.UUID
		if err := g.b.pool.QueryRow(ctx, `SELECT id FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`,
			g.peerAonB, assetURI(o)).Scan(&grantOnB); err != nil || grantOnB != out.GrantID {
			t.Fatalf("authorised by %v, B's live grant from A's aa:Share is %v (%v)", out.GrantID, grantOnB, err)
		}
	})

	t.Run("tampered bytes are rejected and nothing is kept", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
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
		out, err := receive(announce(o))
		wantIngest(t, err, casfetch.ReasonFixityMismatch)
		if out.State != assetingest.StateRejected {
			t.Fatalf("outcome %+v", out)
		}
		g.nothingOnB(o)
	})

	t.Run("a revoked grant gets nothing, and A is not asked", func(t *testing.T) {
		o := g.newOriginal("public")
		id := g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
		g.revoke(id)
		g.waitOnB(assetURI(o), "ended/revoked")
		heads := g.a.casHEADs.Load()
		_, err := receive(announce(o))
		wantIngest(t, err, casfetch.ReasonNoInboundGrant)
		if g.a.casHEADs.Load() != heads {
			t.Fatal("B asked A although its own record refused")
		}
		g.nothingOnB(o)
	})

	t.Run("an outage leaves the record pending, and a retry completes it", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
		e := announce(o)
		out, err := receive(e)
		g.a.setFront(nil)
		if !assetingest.Retryable(err) || out.State != assetingest.StatePending {
			t.Fatalf("outage: %+v %v", out, err)
		}
		g.nothingOnB(o)
		again, err := receive(e)
		if err != nil || again.State != assetingest.StateComplete || again.RecordID != out.RecordID {
			t.Fatalf("retry: %+v %v", again, err)
		}
		if recordPins(o) != 1 {
			t.Fatal("the retried record does not hold the original")
		}
	})
}
