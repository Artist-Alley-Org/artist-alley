// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 gate (inbox route and worker): an aa:AssetVersion posted to B's
// real inbox is scheduled by B's production dispatcher and imported by
// B's production asset worker (both started by Server.Run), which fetches
// the original from A's real CAS route under the grant A sent through its
// real outbox. Nothing on B shows the record.
//
// Test-only, labelled where installed:
//
//   - TEST-ONLY ADVERTISEMENT of archival-1 (here) and
//     original-transfer-1 (newCASGate), before the real handshake. Neither
//     is advertised in production.
//   - THE ANNOUNCEMENT. No sender emits aa:AssetVersion yet, so the test
//     builds it from A's real asset, signs it with a real A actor's key
//     (which B pins from A's real actor document) and posts it with A's
//     instance key over TLS. Everything that receives it is B's
//     production server.

package twostack_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

type assetInboxGate struct {
	*casGate
	aPriv []byte // A's instance key (raw ed25519)
	actor remoteActor
}

func (g *assetInboxGate) assetURI(o original) string { return g.a.url + "/assets/" + o.asset.String() }

// announce builds and signs, as the A actor, an aa:AssetVersion of o's
// version 1 whose object (and aa:asset id) is object.
func (g *assetInboxGate) announce(o original, object string, signWith []byte) (string, []byte) {
	g.t.Helper()
	member := map[string]any{
		"id": object,
		"version": map[string]any{"id": object + "/versions/1", "number": 1,
			"created": time.Now().UTC().Format(time.RFC3339), "predecessor": nil},
		"files": []map[string]any{{"role": "original", "contentUri": "cas://" + o.hash,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": o.hash}}, "size": len(o.body),
			"mediaType": "image/png", "originalFilename": "inbox-gate.png"}},
		"sensitivity": "public", "mature": false,
	}
	raw, _ := json.Marshal(member)
	id := g.a.url + "/activities/" + uuid.NewString()
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityAAAssetVersion, ID: id,
		Actor: g.actor.uri, Object: object, Published: time.Now().UTC().Truncate(time.Second),
		Extra: map[string]json.RawMessage{federation.MemberArchivalAsset: raw}}
	if signWith == nil {
		signWith = []byte(g.actor.keyPEM)
	}
	if err := env.Sign(signWith, g.actor.uri+"#main-key"); err != nil {
		g.t.Fatal(err)
	}
	body, err := env.Marshal()
	if err != nil {
		g.t.Fatal(err)
	}
	return id, body
}

// toB is the authzGate helpers with the roles swapped: they post from
// their "B" to their "A", and this gate posts from A to B.
func (g *assetInboxGate) toB(key []byte) *authzGate {
	return &authzGate{cluster: &cluster{t: g.t, a: g.b, b: g.a, logger: g.logger, tr: g.tr}, bPriv: key}
}

// post delivers body to B's inbox, HTTP-signed with key under A's
// instance key id.
func (g *assetInboxGate) post(body, key []byte) int {
	g.t.Helper()
	return g.toB(key).post(body)
}

// inboxOnB waits for B's dispatcher to settle activityURI.
func (g *assetInboxGate) inboxOnB(activityURI string) string {
	g.t.Helper()
	return g.toB(nil).outcome(activityURI)
}

// recordOnB waits for B's record of activityURI to reach a state in want
// and returns state/reason.
func (g *assetInboxGate) recordOnB(activityURI string, want ...string) string {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
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
			for _, w := range want {
				if got == w {
					return got
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("B's record of %s is %s, want one of %v", activityURI, got, want)
	return ""
}

func (g *assetInboxGate) countOnB(sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.b.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

func TestTwoStack_AssetInbox(t *testing.T) {
	// TEST-ONLY ADVERTISEMENT of archival-1, before newCASGate adds
	// original-transfer-1 and pairs the instances.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapArchival1)
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	cg := newCASGate(t)
	cg.runWorkers(cg.a)
	cg.runWorkers(cg.b)
	ctx := context.Background()
	idA, err := identity.NewManager(cg.a.pool, cg.logger).Load(ctx)
	if err != nil {
		t.Fatalf("A identity: %v", err)
	}
	// A real actor on A, with its production signing key.
	username := "announcer" + uuid.NewString()[:6]
	var ref int64
	if err := cg.a.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Announcer', 1) RETURNING ref`,
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
	recordPins := func(o original) int {
		return g.onB(o, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset'`)
	}

	t.Run("post, schedule, import in the background, hold, no refetch", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(g.assetURI(o), "live")
		gets := g.a.casGETs.Load()
		id, body := g.announce(o, g.assetURI(o), nil)
		if st := g.post(body, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("B answered %d", st)
		}
		if got := g.inboxOnB(id); got != "processed" {
			t.Fatalf("B's inbox row: %s", got)
		}
		g.recordOnB(id, "complete")
		if g.a.casGETs.Load() != gets+1 {
			t.Fatalf("A served %d GETs, want 1", g.a.casGETs.Load()-gets)
		}
		if recordPins(o) != 1 || g.onB(o, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_ingest'`) != 0 {
			t.Fatal("B does not hold the original under the record alone")
		}
		if g.onB(o, `SELECT count(*) FROM assets WHERE file_hash = $1`) != 0 {
			t.Fatal("B created an assets row for a received record")
		}
		var grantID, onRecord uuid.UUID
		if err := g.b.pool.QueryRow(ctx, `SELECT id FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`,
			g.peerAonB, g.assetURI(o)).Scan(&grantID); err != nil {
			t.Fatal(err)
		}
		if err := g.b.pool.QueryRow(ctx, `SELECT authorizing_grant_id FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, id).Scan(&onRecord); err != nil || onRecord != grantID {
			t.Fatalf("record authorised by %v, B's live grant is %v (%v)", onRecord, grantID, err)
		}

		// The same delivery again, and a second activity for the same
		// version: no new task, nothing fetched again.
		if st := g.post(body, g.aPriv); st != http.StatusOK {
			t.Fatalf("redelivery: %d", st)
		}
		id2, body2 := g.announce(o, g.assetURI(o), nil)
		if st := g.post(body2, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("second announcement: %d", st)
		}
		if got := g.inboxOnB(id2); got != "processed" {
			t.Fatalf("second announcement row: %s", got)
		}
		time.Sleep(300 * time.Millisecond) // the worker was woken; give it the chance to misbehave
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`,
			g.peerAonB, g.assetURI(o)); n != 1 {
			t.Fatalf("%d records on B for one version", n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, id); n != 1 {
			t.Fatalf("%d inbox rows for one activity", n)
		}
		if g.a.casGETs.Load() != gets+1 {
			t.Fatal("B fetched a complete record again")
		}
	})

	t.Run("without a grant: recorded, A never asked", func(t *testing.T) {
		o := g.newOriginal("public")
		heads, gets := g.a.casHEADs.Load(), g.a.casGETs.Load()
		id, body := g.announce(o, g.assetURI(o), nil)
		if st := g.post(body, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("B answered %d", st)
		}
		if got := g.inboxOnB(id); got != "processed" {
			t.Fatalf("B's inbox row: %s", got)
		}
		if got := g.recordOnB(id, "rejected/no_inbound_grant"); got != "rejected/no_inbound_grant" {
			t.Fatal(got)
		}
		if g.a.casHEADs.Load() != heads || g.a.casGETs.Load() != gets {
			t.Fatal("B asked A without a grant")
		}
		g.nothingOnB(o)
	})

	t.Run("another origin's object or a bad signature is refused at receipt", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(g.assetURI(o), "live")
		_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
		otherPEM, err := federation.PrivateKeyToPEM(otherKey)
		if err != nil {
			t.Fatal(err)
		}
		_, wrongInstance, _ := ed25519.GenerateKey(rand.Reader)
		cases := []struct {
			name   string
			object string
			envKey []byte
			sigKey []byte
		}{
			{"B's own asset", g.b.url + "/assets/" + uuid.NewString(), nil, g.aPriv},
			{"a third instance's asset", "https://third.example/assets/" + uuid.NewString(), nil, g.aPriv},
			{"an envelope not signed by its actor", g.assetURI(o), otherPEM, g.aPriv},
			{"a delivery not signed by A", g.assetURI(o), nil, wrongInstance},
		}
		gets := g.a.casGETs.Load()
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				id, body := g.announce(o, c.object, c.envKey)
				st := g.post(body, c.sigKey)
				if st < 400 || st >= 500 {
					t.Fatalf("B answered %d, want a refusal", st)
				}
				t.Logf("B answered %d", st)
				if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, id); n != 0 {
					t.Fatalf("B stored %d rows for a refused delivery", n)
				}
				if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE activity_uri = $1`, id); n != 0 {
					t.Fatal("B recorded a refused delivery")
				}
			})
		}
		if g.a.casGETs.Load() != gets {
			t.Fatal("B fetched for a refused delivery")
		}
		g.nothingOnB(o)
	})

	t.Run("a held transfer holds neither the inbox nor other imports", func(t *testing.T) {
		slow, fast := g.newOriginal("public"), g.newOriginal("public")
		for _, o := range []original{slow, fast} {
			g.grantWith(o, map[string]any{"allow_original": true})
			g.waitOnB(g.assetURI(o), "live")
		}
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+slow.hash) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				case <-time.After(45 * time.Second):
				}
			}
			h.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		slowID, slowBody := g.announce(slow, g.assetURI(slow), nil)
		if st := g.post(slowBody, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("slow: %d", st)
		}
		select {
		case <-started:
		case <-time.After(60 * time.Second):
			t.Fatal("B's worker never asked A for the slow original")
		}
		fastID, fastBody := g.announce(fast, g.assetURI(fast), nil)
		if st := g.post(fastBody, g.aPriv); st != http.StatusAccepted {
			t.Fatalf("fast: %d", st)
		}
		if got := g.inboxOnB(fastID); got != "processed" {
			t.Fatalf("fast inbox row: %s", got)
		}
		g.recordOnB(fastID, "complete")
		if got := g.recordOnB(slowID, "pending"); got != "pending" {
			t.Fatalf("the slow record is %s while its transfer is held", got)
		}
		if recordPins(slow) != 0 {
			t.Fatal("a held transfer is already held under its record")
		}
		close(release)
		g.recordOnB(slowID, "complete")
		if recordPins(slow) != 1 || recordPins(fast) != 1 {
			t.Fatal("the records do not hold their originals")
		}
	})
}
