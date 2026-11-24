// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 receiver gate (spec v1 §18.4): an asset Update carrying
// aa:metadata, signed by a real A actor and posted with A's instance key,
// is admitted by B's production inbox, dispatched by B's production
// dispatcher and kept by B's production asset worker: the flags revision
// and the snapshot together, with the origin's provenance.
//
// Test-only, labelled where installed:
//
//   - TEST-ONLY ADVERTISEMENT of archival-1 and asset-metadata-1 (here)
//     and original-transfer-1 (newCASGate), before the real handshake. None
//     is advertised in production.
//   - THE UPDATES. A has no metadata sender yet: the test builds each
//     Update from A's real asset and signs it as the A actor. A's own
//     sender is switched off (senderOffOnA).
//
// This is partial receiver-wire coverage. It is not the §18.6 gate: no
// sender, no API or UI projection, one direction only.

package twostack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// metadataUpdate builds and signs, as the A actor, an Update of o naming
// version 1 (the files announce sends) with the given flag and, when
// metadata is not nil, aa:metadata.
func (g *assetInboxGate) metadataUpdate(o original, mature bool, metadata json.RawMessage, published time.Time) *federation.Envelope {
	g.t.Helper()
	object := g.assetURI(o)
	member := map[string]any{
		"id": object,
		"version": map[string]any{"id": object + "/versions/1", "number": 1,
			"created": "2026-11-22T10:00:00Z", "predecessor": nil},
		"files": []map[string]any{{"role": "original", "contentUri": "cas://" + o.hash,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": o.hash}}, "size": len(o.body),
			"mediaType": "image/png", "originalFilename": "inbox-gate.png"}},
		"sensitivity": "public", "mature": mature,
	}
	raw, _ := json.Marshal(member)
	extra := map[string]json.RawMessage{federation.MemberArchivalAsset: raw}
	if metadata != nil {
		extra[federation.MemberAssetMetadata] = metadata
	}
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate,
		ID: g.a.url + "/activities/" + uuid.NewString(), Actor: g.actor.uri, Object: object, Published: published, Extra: extra}
	if err := env.Sign([]byte(g.actor.keyPEM), g.actor.uri+"#main-key"); err != nil {
		g.t.Fatal(err)
	}
	return env
}

func (g *assetInboxGate) body(env *federation.Envelope) []byte {
	g.t.Helper()
	b, err := env.Marshal()
	if err != nil {
		g.t.Fatal(err)
	}
	return b
}

func TestTwoStack_MetadataInbox(t *testing.T) {
	// TEST-ONLY ADVERTISEMENT, before newCASGate pairs the instances.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1, peer.PeerCapability(federation.ProfileAssetMetadata1))
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	cg := newCASGate(t)
	for _, side := range []struct{ in, other *instance }{{cg.a, cg.b}, {cg.b, cg.a}} {
		if !slices.Contains(cg.capabilities(side.in, side.other), federation.ProfileAssetMetadata1) {
			t.Fatalf("%s did not negotiate asset-metadata-1 with %s through the handshake", side.in.name, side.other.name)
		}
	}
	cg.senderOffOnA()
	cg.runWorkers(cg.a)
	cg.runWorkers(cg.b)
	ctx := context.Background()
	idA, err := identity.NewManager(cg.a.pool, cg.logger).Load(ctx)
	if err != nil {
		t.Fatalf("A identity: %v", err)
	}
	username := "describer" + uuid.NewString()[:6]
	var ref int64
	if err := cg.a.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Describer', 1) RETURNING ref`,
		username).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	km, err := users.NewHandler(cg.a.pool, cg.logger, nil).EnsureActorKeyMaterial(ctx, ref, cg.a.url, username)
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := km.DecryptSigningPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	g := &assetInboxGate{casGate: cg, aPriv: idA.PrivateKey(),
		actor: remoteActor{uri: cg.a.url + "/users/" + username, keyPEM: string(pemKey)}}
	storageOnB := assetingest.New(assetingest.Deps{Pool: g.b.pool})

	// A verified version 1 on B, under a live original grant.
	o := g.newOriginal("public")
	g.grantWith(o, map[string]any{"allow_original": true})
	g.waitOnB(g.assetURI(o), "live")
	v1, v1Body := g.announce(o, g.assetURI(o), nil)
	if st := g.post(v1Body, g.aPriv); st != http.StatusAccepted {
		t.Fatalf("version 1: B answered %d", st)
	}
	if got := g.inboxOnB(v1); got != "processed" {
		t.Fatalf("version 1 inbox row: %s", got)
	}
	g.recordOnB(v1, "complete")

	revisions := func() int {
		return g.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND origin_asset_uri = $2`,
			g.peerAonB, g.assetURI(o))
	}
	snapshots := func() int {
		return g.countOnB(`SELECT count(*) FROM federation_received_asset_metadata m JOIN federation_received_asset_revisions r
			ON r.id = m.revision_id WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`, g.peerAonB, g.assetURI(o))
	}
	localState := func() string {
		var s string
		if err := g.b.pool.QueryRow(ctx, `SELECT concat_ws(' | ',
			(SELECT to_jsonb(r)::text FROM federation_received_assets r WHERE r.peer_id = $1 AND r.activity_uri = $2),
			(SELECT count(*) FROM federation_received_asset_files),
			(SELECT count(*) FROM storage_pins WHERE object_hash = $3),
			(SELECT count(*) FROM field_definition), (SELECT count(*) FROM asset_field_value),
			(SELECT count(*) FROM assets))`, g.peerAonB, v1, o.hash).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := localState()
	gets := g.a.casGETs.Load()
	revs, snaps := revisions(), snapshots()

	t.Run("an accepted metadata Update keeps the revision and the snapshot together", func(t *testing.T) {
		member := json.RawMessage(`{"title":"Harbour <b>at</b> dusk","description":"Oil on board.","rights":"https://creativecommons.org/licenses/by/4.0/",` +
			`"requiredStatement":{"label":{"en":["Attribution"]},"value":{"none":["Studio A"]}}}`)
		published := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond).Add(321 * time.Nanosecond)
		u := g.metadataUpdate(o, true, member, published)
		body := g.body(u)
		if st := g.post(body, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("B answered %d", st)
		}
		if got := g.inboxOnB(u.ID); got != "processed" {
			t.Fatalf("B's inbox row: %s", got)
		}
		if revisions() != revs+1 || snapshots() != snaps+1 {
			t.Fatalf("%d revisions (was %d), %d snapshots (was %d)", revisions(), revs, snapshots(), snaps)
		}
		var mature bool
		var snapshot []byte
		var secs int64
		var nanos int32
		var origin, value, recipient string
		if err := g.b.pool.QueryRow(ctx, `SELECT r.mature, m.snapshot, m.published_seconds, m.published_nanos,
			    m.origin_signer_key_id, m.origin_signature_value, m.recipient_signer_key_id
			  FROM federation_received_asset_revisions r JOIN federation_received_asset_metadata m ON m.revision_id = r.id
			 WHERE r.peer_id = $1 AND r.activity_uri = $2 AND r.activity_type = 'Update'`, g.peerAonB, u.ID).
			Scan(&mature, &snapshot, &secs, &nanos, &origin, &value, &recipient); err != nil {
			t.Fatal(err)
		}
		var got, want any
		_ = json.Unmarshal(snapshot, &got)
		_ = json.Unmarshal(member, &want)
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if !mature || string(gb) != string(wb) {
			t.Fatalf("kept mature %v, snapshot %s", mature, snapshot)
		}
		if secs != published.Unix() || nanos != int32(published.Nanosecond()) {
			t.Fatalf("published kept as %d.%09d, sent %s", secs, nanos, published.Format(time.RFC3339Nano))
		}
		if origin != g.actor.uri+"#main-key" || value != u.Signature.Value || !strings.HasPrefix(recipient, g.b.url) || recipient == origin {
			t.Fatalf("provenance: origin %q value %q recipient %q", origin, value, recipient)
		}
		cur, err := storageOnB.CurrentMetadata(ctx, g.peerAonB, g.assetURI(o))
		if err != nil || cur == nil || cur.ActivityURI != u.ID || *cur.Snapshot.Title != "Harbour <b>at</b> dusk" {
			t.Fatalf("B's current snapshot: %+v %v", cur, err)
		}
		// The received record, its files and pins, and B's own fields and
		// values are as they were; nothing was fetched.
		if after := localState(); after != before || g.a.casGETs.Load() != gets {
			t.Fatalf("B's local state changed:\n%s\n%s", before, after)
		}
		// The same delivery again: answered from the stored row, nothing more.
		if st := g.post(body, g.aPriv); st != http.StatusOK {
			t.Fatalf("redelivery: %d", st)
		}
		var sig string
		if err := g.b.pool.QueryRow(ctx, `SELECT m.origin_signature_value FROM federation_received_asset_metadata m
			JOIN federation_received_asset_revisions r ON r.id = m.revision_id WHERE r.peer_id = $1 AND r.activity_uri = $2`,
			g.peerAonB, u.ID).Scan(&sig); err != nil || sig != value || revisions() != revs+1 || snapshots() != snaps+1 {
			t.Fatal("a redelivery changed the kept rows")
		}
		revs, snaps = revisions(), snapshots()
	})

	t.Run("a clear-all snapshot is kept; a later flags-only Update adds none", func(t *testing.T) {
		clear := g.metadataUpdate(o, true, json.RawMessage(`{"title":null,"description":null,"rights":null,"requiredStatement":null}`),
			time.Now().UTC().Add(2*time.Second))
		if st := g.post(g.body(clear), g.aPriv); st != http.StatusAccepted {
			t.Fatalf("B answered %d", st)
		}
		if got := g.inboxOnB(clear.ID); got != "processed" {
			t.Fatalf("inbox row: %s", got)
		}
		flags := g.metadataUpdate(o, false, nil, time.Now().UTC().Add(3*time.Second))
		if st := g.post(g.body(flags), g.aPriv); st != http.StatusAccepted {
			t.Fatalf("flags-only: B answered %d", st)
		}
		if got := g.inboxOnB(flags.ID); got != "processed" {
			t.Fatalf("flags-only inbox row: %s", got)
		}
		cur, err := storageOnB.CurrentMetadata(ctx, g.peerAonB, g.assetURI(o))
		if err != nil || cur == nil || cur.ActivityURI != clear.ID || cur.Snapshot.Title != nil || cur.Snapshot.RequiredStatement != nil {
			t.Fatalf("B's current snapshot: %+v %v", cur, err)
		}
		if revisions() != revs+2 || snapshots() != snaps+1 {
			t.Fatalf("%d revisions (was %d), %d snapshots (was %d)", revisions(), revs, snapshots(), snaps)
		}
		revs, snaps = revisions(), snapshots()
		if after := localState(); after != before {
			t.Fatalf("B's local state changed:\n%s\n%s", before, after)
		}
	})

	t.Run("refused on the wire with nothing kept", func(t *testing.T) {
		refused := func(t *testing.T, env *federation.Envelope, want int) {
			t.Helper()
			if st := g.post(g.body(env), g.aPriv); st != want {
				t.Fatalf("B answered %d, want %d", st, want)
			}
			if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, env.ID); n != 0 {
				t.Fatalf("%d inbox rows for a refused Update", n)
			}
			if revisions() != revs || snapshots() != snaps {
				t.Fatal("a refused Update kept a revision or a snapshot")
			}
		}
		refused(t, g.metadataUpdate(o, true, json.RawMessage(`{"title":"","description":null,"rights":null,"requiredStatement":null}`),
			time.Now().UTC().Add(4*time.Second)), http.StatusBadRequest)
		// B's stored intersection for A loses asset-metadata-1. B's peer
		// registry caches the row, so its inbox and dispatcher may still
		// see the old profiles; the asset worker re-reads the row FOR SHARE
		// in the admitting transaction and refuses it there.
		if _, err := g.b.pool.Exec(ctx, `UPDATE federation_peers SET capabilities = capabilities - $2 WHERE id = $1`,
			g.peerAonB, federation.ProfileAssetMetadata1); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = g.b.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = capabilities || to_jsonb($2::text)
				WHERE id = $1 AND NOT capabilities ? $2`, g.peerAonB, federation.ProfileAssetMetadata1)
		})
		dropped := g.metadataUpdate(o, true, json.RawMessage(`{"title":"t","description":null,"rights":null,"requiredStatement":null}`),
			time.Now().UTC().Add(5*time.Second))
		st := g.post(g.body(dropped), g.aPriv)
		switch st {
		case http.StatusUnprocessableEntity: // refused at receipt (no cached row)
			if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, dropped.ID); n != 0 {
				t.Fatalf("%d inbox rows", n)
			}
		case http.StatusAccepted:
			if got := g.inboxOnB(dropped.ID); got != "rejected/"+string(federation.InboxStatusProfileNotNegotiated) {
				t.Fatalf("B's inbox row: %s", got)
			}
		default:
			t.Fatalf("B answered %d", st)
		}
		t.Logf("without asset-metadata-1 B answered %d", st)
		if revisions() != revs || snapshots() != snaps {
			t.Fatal("an Update without the profile kept a revision or a snapshot")
		}
		if after := localState(); after != before {
			t.Fatalf("B's local state changed:\n%s\n%s", before, after)
		}
	})
}
