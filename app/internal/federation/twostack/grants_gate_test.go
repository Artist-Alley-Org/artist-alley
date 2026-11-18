// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1571 gate: inbound grants between two production instances.
//
// A grants through its real admin API; A's own outbox dispatcher and
// delivery worker send the signed aa:Share / aa:Unshare to B's real
// inbox; B's own inbox dispatcher applies them to B's inbound grant
// record; a casfetch client on B, authorised by that record
// (casfetch.DatabaseGrants, the production wiring), fetches the original
// from A's real CAS route. Nothing is written into B's grant tables by
// the test.
//
// Test-only, labelled where installed: the original-transfer-1
// advertisement (newCASGate, reserved until #1574) and the httptest
// trust root (main_test.go). share-lifecycle-1 is negotiated by the
// production handshake from the production KnownCapabilities.

package twostack_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// runWorkers starts in's production background workers (Server.Run:
// outbox dispatcher and delivery, inbox dispatcher, sweepers) until the
// test ends.
func (c *cluster) runWorkers(in *instance) {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = in.srv.Run(ctx); close(done) }()
	c.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			c.t.Errorf("%s: workers did not stop", in.name)
		}
	})
}

// grantState is B's record of the grant made by A's activity for obj:
// the newest row's state and end reason, "" when none has arrived.
func (g *casGate) grantStateOnB(objectURI string) (state, reason string) {
	var r *string
	err := g.b.pool.QueryRow(context.Background(),
		`SELECT state, end_reason FROM federation_inbound_grants
		  WHERE peer_id = $1 AND object_uri = $2 ORDER BY received_at DESC, id DESC LIMIT 1`,
		g.peerAonB, objectURI).Scan(&state, &r)
	if err != nil {
		return "", ""
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

// waitOnB waits for B's record of the grants for objectURI to reach
// want (state of every row, newest first), driven only by the
// instances' own workers.
func (g *casGate) waitOnB(objectURI string, want ...string) {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = got[:0]
		rows, err := g.b.pool.Query(context.Background(),
			`SELECT state || COALESCE('/' || end_reason, '') FROM federation_inbound_grants
			  WHERE peer_id = $1 AND object_uri = $2 ORDER BY received_at DESC, id DESC`, g.peerAonB, objectURI)
		if err == nil {
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				got = append(got, s)
			}
			rows.Close()
		}
		if equalStrings(got, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("B's grants for %s: %v, want %v", objectURI, got, want)
}

func asCASError(err error, target **casfetch.Error) bool { return errors.As(err, target) }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// grantWith shares o with B through A's admin API.
func (g *casGate) grantWith(o original, body map[string]any) string {
	g.t.Helper()
	req := map[string]any{"object_kind": "asset", "object_id": o.asset, "peer_id": g.peerBonA, "scope": "view"}
	for k, v := range body {
		req[k] = v
	}
	var out struct {
		ID string `json:"id"`
	}
	if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", req, &out); st != 201 {
		g.t.Fatalf("grant: %d %+v", st, out)
	}
	return out.ID
}

func (g *casGate) recordClient(grants *inboundgrants.Store) *casfetch.Client {
	return casfetch.New(casfetch.Deps{
		Pool: g.b.pool, Storage: g.storageOf(g.b), Peers: peerLookupB{g},
		Grants: casfetch.DatabaseGrants{Store: grants}, Audit: audit.NewRecorder(g.b.pool, g.logger), Logger: g.logger,
		Transport: g.tr,
		Config: casfetch.Config{
			IdleTimeout: 2 * time.Second, MinOverall: 20 * time.Second, LeaseTTL: 3 * time.Second,
			Heartbeat: time.Second, WaitPoll: 50 * time.Millisecond, TempDir: g.t.TempDir(),
			LocalBaseURL: func(context.Context) string { return g.b.url },
		},
	})
}

func TestTwoStack_InboundGrants(t *testing.T) {
	g := newCASGate(t)
	for _, side := range []struct{ in, other *instance }{{g.a, g.b}, {g.b, g.a}} {
		if !contains(g.capabilities(side.in, side.other), federation.ProfileShareLifecycle1) {
			t.Fatalf("%s did not negotiate %s with %s through the handshake", side.in.name, federation.ProfileShareLifecycle1, side.other.name)
		}
	}
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()
	grants := inboundgrants.New(g.b.pool, func(context.Context) string { return g.b.url }, nil, nil)
	assetURI := func(o original) string { return g.a.url + "/assets/" + o.asset.String() }

	t.Run("grant reaches B, then the original is fetched", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
		res, err := g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		if err != nil || res.Deduped {
			t.Fatalf("fetch under the delivered grant: %+v %v", res, err)
		}
	})

	t.Run("no original grant: B refuses before asking A", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, nil)
		g.waitOnB(assetURI(o), "live")
		heads := g.a.casHEADs.Load()
		_, err := g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if g.a.casHEADs.Load() != heads {
			t.Fatal("B asked A although its own record refused")
		}
		g.nothingOnB(o)
	})

	t.Run("downgrade: the regrant supersedes and the original is refused", func(t *testing.T) {
		o := g.newOriginal("public")
		id := g.grantWith(o, map[string]any{"allow_original": true, "scope": "remix"})
		g.waitOnB(assetURI(o), "live")
		g.revoke(id)
		g.grantWith(o, map[string]any{"scope": "view"})
		g.waitOnB(assetURI(o), "live", "ended/revoked")
		var sup *string
		if err := g.b.pool.QueryRow(ctx, `SELECT supersedes_activity_uri FROM federation_inbound_grants
			WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`, g.peerAonB, assetURI(o)).Scan(&sup); err != nil || sup == nil {
			t.Fatalf("the regrant does not name the grant it supersedes: %v %v", sup, err)
		}
		_, err := g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		g.nothingOnB(o)
	})

	t.Run("revoke while the download is in flight", func(t *testing.T) {
		o := g.newOriginal("public")
		id := g.grantWith(o, map[string]any{"allow_original": true})
		g.waitOnB(assetURI(o), "live")
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			if r.Method != http.MethodGet {
				h.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(&writerHook{ResponseWriter: w, before: func() {
				// Mid-body: A revokes, and the Unshare reaches B
				// through the real outbox and inbox before the
				// download finishes.
				g.revoke(id)
				g.waitOnB(assetURI(o), "ended/revoked")
			}}, r)
		})
		defer g.a.setFront(nil)
		_, err := g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		var e *casfetch.Error
		if !asCASError(err, &e) || e.OriginReason != casfetch.ReasonNoInboundGrant {
			t.Fatalf("cause %+v, want B's own record (no_inbound_grant) to refuse first", e)
		}
		g.nothingOnB(o)
	})

	t.Run("expiry by B's clock", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, map[string]any{"allow_original": true, "expires_at": time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339Nano)})
		g.waitOnB(assetURI(o), "live")
		if _, err := grants.Allows(ctx, inboundgrants.Query{PeerID: g.peerAonB, ObjectURI: assetURI(o), Class: inboundgrants.ClassOriginal}); err != nil {
			t.Fatalf("before expiry: %v", err)
		}
		time.Sleep(5 * time.Second)
		if st, _ := g.grantStateOnB(assetURI(o)); st != "live" {
			// The origin's sweeper may already have sent its Unshare;
			// either way the decision below is the receiver's clock.
			t.Logf("state after expiry: %s", st)
		}
		_, err := g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		g.nothingOnB(o)
	})

	t.Run("overlap: a user grant survives the end of the peer-wide one", func(t *testing.T) {
		o := g.newOriginal("public")
		uh := users.NewHandler(g.b.pool, g.logger, nil)
		var bobRef int64
		bobName := "bob" + uuid.NewString()[:6]
		if err := g.b.pool.QueryRow(ctx,
			`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Bob', 1) RETURNING ref`, bobName).Scan(&bobRef); err != nil {
			t.Fatal(err)
		}
		km, err := uh.EnsureActorKeyMaterial(ctx, bobRef, g.b.url, bobName)
		if err != nil {
			t.Fatal(err)
		}
		privPEM, err := km.DecryptSigningPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		priv, err := federation.PrivateKeyFromPEM(privPEM)
		if err != nil {
			t.Fatal(err)
		}
		bobURI := g.b.url + "/users/" + bobName
		wide := g.grantWith(o, map[string]any{"allow_original": true})
		g.grantWith(o, map[string]any{"allow_original": true, "target_user_url": bobURI})
		g.waitOnB(assetURI(o), "live", "live")
		g.revoke(wide)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var ended int
			_ = g.b.pool.QueryRow(ctx, `SELECT count(*) FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2
				AND state = 'ended' AND target_user_uri IS NULL`, g.peerAonB, assetURI(o)).Scan(&ended)
			if ended == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the peer-wide revoke never reached B")
			}
			time.Sleep(100 * time.Millisecond)
		}
		// The instance (peer-wide only) is refused; Bob, through his own
		// grant, still fetches.
		_, err = g.recordClient(grants).Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		res, err := g.recordClient(grants).Fetch(ctx, g.req(o, casfetch.ActorSigner(bobURI, priv)))
		if err != nil {
			t.Fatalf("bob's own grant: %v", err)
		}
		if res.Pin.SubjectType != casfetch.IngestPinType {
			t.Fatalf("pin %+v", res.Pin)
		}
	})
}

// A peer that did not negotiate share-lifecycle-1 (a pre-#1571 pairing)
// is sent no grant, and B, seeing no profile, applies none.
func TestTwoStack_InboundGrants_OlderPeerGetsNothing(t *testing.T) {
	// TEST-ONLY: drop the token before the handshake, so this pairing is
	// a pre-#1571 one on both sides.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	var without peer.CapabilitySet
	for _, c := range prev {
		if c != peer.CapShareLifecycle1 {
			without = append(without, c)
		}
	}
	peer.KnownCapabilities = without
	t.Cleanup(func() { peer.KnownCapabilities = prev })

	c := newCluster(t)
	for _, side := range []struct{ in, other *instance }{{c.a, c.b}, {c.b, c.a}} {
		if contains(c.capabilities(side.in, side.other), federation.ProfileShareLifecycle1) {
			t.Fatalf("%s negotiated %s", side.in.name, federation.ProfileShareLifecycle1)
		}
	}
	c.runWorkers(c.a)
	c.runWorkers(c.b)
	g := &casGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b)}
	o := g.newOriginal("public")
	id := g.grantWith(o, nil)
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var n int
		_ = c.a.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type = $1
			AND metadata->>'reason' = 'profile_not_negotiated'`, audit.EventFederationEmissionSkipped).Scan(&n)
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A never recorded the profile_not_negotiated skip")
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.revoke(id)
	time.Sleep(2 * time.Second) // give a (wrong) delivery the chance to land
	var queued, received int
	_ = c.a.pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox WHERE peer_id = $1`, g.peerBonA).Scan(&queued)
	_ = c.b.pool.QueryRow(ctx, `SELECT count(*) FROM federation_inbound_grants WHERE peer_id = $1`, g.peerAonB).Scan(&received)
	if queued != 0 || received != 0 {
		t.Fatalf("older pairing: %d outbox rows on A, %d grants on B; want none", queued, received)
	}
}
