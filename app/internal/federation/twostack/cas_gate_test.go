// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #72 gate: original-file transfer between two production instances on
// separate databases and storage roots.
//
// Two test-only stand-ins, both labelled where they are installed:
//
//   - TEST-ONLY ADVERTISEMENT. original-transfer-1 is not in
//     peer.KnownCapabilities in production (it waits for #1571 and
//     #1574). The gate adds it for the duration of the test, BEFORE the
//     real handshake, so the profile is negotiated by the production
//     handshake code rather than written into a row.
//   - TEST-ONLY INBOUND GRANT RECORD. The receiver's inbound model is
//     #1571; production uses casfetch.FailClosedInboundGrants, which
//     refuses everything. fixtureInbound stands in for it here.
//
// Everything else is production: http.New on each database, the root
// CAS route and the #1573 policy on A, the casfetch client with B's
// identity, database and storage on B, grants and revokes through A's
// admin API, B's actor document served by B for actor-signed fetches.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// fixtureInbound is the TEST-ONLY INBOUND GRANT RECORD standing in for
// #1571. allow=false refuses every query.
type fixtureInbound struct {
	mu    sync.Mutex
	allow bool
}

func (f *fixtureInbound) Allows(context.Context, casfetch.GrantQuery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.allow {
		return casfetch.ErrNoInboundGrant
	}
	return nil
}

func (f *fixtureInbound) set(v bool) { f.mu.Lock(); f.allow = v; f.mu.Unlock() }

type casGate struct {
	*cluster
	inbound  *fixtureInbound
	signerB  casfetch.Signer
	peerAonB uuid.UUID
	peerBonA uuid.UUID
}

func newCASGate(t *testing.T) *casGate {
	t.Helper()
	// TEST-ONLY ADVERTISEMENT, before the real handshake.
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.CapOriginalTransfer1)
	t.Cleanup(func() { peer.KnownCapabilities = prev })

	c := newCluster(t)
	g := &casGate{cluster: c, inbound: &fixtureInbound{allow: true}}
	for _, side := range []struct{ in, other *instance }{{c.a, c.b}, {c.b, c.a}} {
		if !contains(c.capabilities(side.in, side.other), string(peer.CapOriginalTransfer1)) {
			t.Fatalf("%s did not negotiate original-transfer-1 with %s through the handshake", side.in.name, side.other.name)
		}
	}
	g.peerAonB, g.peerBonA = c.peerID(c.b, c.a), c.peerID(c.a, c.b)
	id, err := identity.NewManager(c.b.pool, c.logger).Load(context.Background())
	if err != nil {
		t.Fatalf("B identity: %v", err)
	}
	g.signerB = casfetch.InstanceSigner(&outbox.IdentitySigner{
		PrivateKey: id.PrivateKey(), KeyURL: c.b.url + "/federation/instance#main-key"})
	return g
}

// client is a production casfetch client on B. Two calls give two
// independent workers (separate singleflight groups).
func (g *casGate) client(mut ...func(*casfetch.Config)) *casfetch.Client {
	cfg := casfetch.Config{
		IdleTimeout: 2 * time.Second, MinOverall: 20 * time.Second, LeaseTTL: 3 * time.Second,
		Heartbeat: time.Second, WaitPoll: 50 * time.Millisecond, TempDir: g.t.TempDir(),
		LocalBaseURL: func(context.Context) string { return g.b.url },
	}
	for _, m := range mut {
		m(&cfg)
	}
	return casfetch.New(casfetch.Deps{
		Pool: g.b.pool, Storage: g.storageOf(g.b), Peers: peerLookupB{g},
		Grants: g.inbound, Audit: audit.NewRecorder(g.b.pool, g.logger), Logger: g.logger,
		Transport: g.tr, Config: cfg,
	})
}

type peerLookupB struct{ g *casGate }

func (l peerLookupB) ByID(ctx context.Context, id uuid.UUID) (casfetch.Peer, error) {
	p, err := peer.NewRegistry(l.g.b.pool, l.g.logger, nil).ByID(ctx, id)
	if err != nil {
		return casfetch.Peer{}, err
	}
	caps := make([]string, 0, len(p.Capabilities))
	for _, c := range p.Capabilities {
		caps = append(caps, string(c))
	}
	return casfetch.Peer{ID: p.ID, InstanceURL: p.InstanceURL, Enabled: p.Enabled,
		Connected: p.Status == "connected", Capabilities: caps}, nil
}

type original struct {
	asset uuid.UUID
	hash  string
	body  []byte
}

// newOriginal stores fresh bytes on A and creates A's public asset.
func (g *casGate) newOriginal(tier string) original {
	g.t.Helper()
	body := make([]byte, 256<<10)
	_, _ = rand.Read(body)
	if _, err := g.storageOf(g.a).UploadOriginal(context.Background(), bytes.NewReader(body), "image/png",
		storage.PinRef{SubjectType: "gate", SubjectID: uuid.NewString()}); err != nil {
		g.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	o := original{asset: uuid.New(), hash: hex.EncodeToString(sum[:]), body: body}
	if _, err := g.a.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		 VALUES ($1, 'Gate original', 1, $2, $3, $4, $5)`, o.asset, g.a.admin, o.hash, len(body), tier); err != nil {
		g.t.Fatal(err)
	}
	return o
}

// grant shares the asset with B through A's admin API.
func (g *casGate) grant(o original, toPeer uuid.UUID, target string) string {
	g.t.Helper()
	body := map[string]any{"object_kind": "asset", "object_id": o.asset, "peer_id": toPeer,
		"scope": "view", "allow_original": true}
	if target != "" {
		body["target_user_url"] = target
	}
	var out struct {
		ID            string `json:"id"`
		AllowOriginal bool   `json:"allow_original"`
	}
	if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", body, &out); st != 201 || !out.AllowOriginal {
		g.t.Fatalf("grant: %d %+v", st, out)
	}
	return out.ID
}

func (g *casGate) revoke(shareID string) {
	g.t.Helper()
	if st := g.api(g.a, http.MethodDelete, "/admin/federation/shares/"+shareID, nil, nil); st != 204 {
		g.t.Fatalf("revoke: %d", st)
	}
}

func (g *casGate) req(o original, s casfetch.Signer) casfetch.Request {
	return casfetch.Request{
		PeerID: g.peerAonB, OriginAssetURI: g.a.url + "/assets/" + o.asset.String(), AssetID: o.asset,
		SHA256: o.hash, Size: int64(len(o.body)), ContentType: "image/png", Signer: s,
	}
}

func (g *casGate) onB(o original, sql string) int {
	g.t.Helper()
	var n int
	if err := g.b.pool.QueryRow(context.Background(), sql, o.hash).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// nothingOnB: no object, pin, intent or lease for the hash on B.
func (g *casGate) nothingOnB(o original) {
	g.t.Helper()
	for _, q := range []string{
		`SELECT count(*) FROM storage_objects WHERE hash = $1`,
		`SELECT count(*) FROM storage_pins WHERE object_hash = $1`,
		`SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`,
		`SELECT count(*) FROM federation_cas_fetch_leases WHERE object_hash = $1`,
	} {
		if n := g.onB(o, q); n != 0 {
			g.t.Fatalf("B holds something for the hash (%s = %d)", q, n)
		}
	}
	if _, err := g.storageOf(g.b).Stat(context.Background(), o.hash, storage.VariantOriginal); err == nil {
		g.t.Fatal("B's storage root holds the bytes")
	}
}

func (g *casGate) refusedOnA(o original) string {
	var r string
	_ = g.a.pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason' FROM audit_events WHERE event_type = 'federation.cas.refused'
		   AND metadata->>'asset_id' = $1 ORDER BY occurred_at DESC LIMIT 1`, o.asset.String()).Scan(&r)
	return r
}

func wantReason(t *testing.T, err error, want string) {
	t.Helper()
	if got := casfetch.ReasonOf(err); got != want {
		t.Fatalf("reason %q (%v), want %q", got, err, want)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// writerHook calls before() once, at the first body write (the origin
// has decided to serve), then writes through, optionally mutating.
type writerHook struct {
	http.ResponseWriter
	once   sync.Once
	before func()
	mutate func([]byte) []byte
}

func (w *writerHook) Write(p []byte) (int, error) {
	w.once.Do(func() {
		if w.before != nil {
			w.before()
		}
	})
	if w.mutate != nil {
		p = w.mutate(bytes.Clone(p))
	}
	return w.ResponseWriter.Write(p)
}

func (w *writerHook) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func TestTwoStack_CAS(t *testing.T) {
	g := newCASGate(t)
	ctx := context.Background()

	t.Run("served bytes arrive verified on B", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grant(o, g.peerBonA, "")
		res, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		if err != nil {
			t.Fatal(err)
		}
		if res.Deduped || res.Pin.SubjectType != casfetch.IngestPinType {
			t.Fatalf("%+v", res)
		}
		rc, _, err := g.storageOf(g.b).Download(ctx, o.hash, storage.VariantOriginal)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, o.body) {
			t.Fatal("bytes on B differ from A's")
		}
		var originOnB string
		_ = g.b.pool.QueryRow(ctx, `SELECT origin_server_id::text FROM storage_objects WHERE hash = $1`, o.hash).Scan(&originOnB)
		if originOnB != g.peerAonB.String() {
			t.Fatalf("origin_server_id %q", originOnB)
		}
		// No asset is created on B by #72.
		var assets int
		_ = g.b.pool.QueryRow(ctx, `SELECT count(*) FROM assets WHERE file_hash = $1`, o.hash).Scan(&assets)
		if assets != 0 {
			t.Fatalf("B has %d asset rows for the bytes", assets)
		}
	})

	t.Run("ten concurrent fetches make one GET and one commit", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grant(o, g.peerBonA, "")
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == http.MethodGet {
				time.Sleep(400 * time.Millisecond)
			}
			next.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		before := g.a.casGETs.Load()
		w1, w2 := g.client(), g.client() // two workers
		var wg sync.WaitGroup
		errs := make([]error, 10)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				w := w1
				if i%2 == 1 {
					w = w2
				}
				_, errs[i] = w.Fetch(ctx, g.req(o, g.signerB))
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := g.a.casGETs.Load() - before; n != 1 {
			t.Fatalf("A served %d GETs, want 1", n)
		}
		if n := g.onB(o, `SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 1 {
			t.Fatalf("B objects = %d", n)
		}
		if n := g.onB(o, `SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`) +
			g.onB(o, `SELECT count(*) FROM federation_cas_fetch_leases WHERE object_hash = $1`); n != 0 {
			t.Fatalf("intents or leases left: %d", n)
		}
	})

	t.Run("retry after a timeout succeeds without duplicates", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grant(o, g.peerBonA, "")
		var stalled sync.Once
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == http.MethodGet {
				stall := false
				stalled.Do(func() { stall = true })
				if stall {
					next.ServeHTTP(&writerHook{ResponseWriter: w, before: func() {}, mutate: func(p []byte) []byte {
						time.Sleep(3 * time.Second) // longer than the idle timeout
						return p
					}}, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		cl := g.client(func(c *casfetch.Config) { c.IdleTimeout = time.Second })
		_, err := cl.Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonTimeout)
		g.nothingOnB(o)
		if _, err := cl.Fetch(ctx, g.req(o, g.signerB)); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if n := g.onB(o, `SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 1 {
			t.Fatalf("objects after retry = %d", n)
		}
	})

	t.Run("stale grant: revoked before the fetch", func(t *testing.T) {
		o := g.newOriginal("public")
		g.revoke(g.grant(o, g.peerBonA, ""))
		before := g.a.casGETs.Load()
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if g.a.casGETs.Load() != before {
			t.Fatal("a GET was sent")
		}
		if r := g.refusedOnA(o); r != "no_share_row" {
			t.Fatalf("A's audit reason %q", r)
		}
		g.nothingOnB(o)
	})

	t.Run("revoked while the bytes are moving", func(t *testing.T) {
		o := g.newOriginal("public")
		share := g.grant(o, g.peerBonA, "")
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == http.MethodGet {
				// A already decided to serve; revoke before the body.
				next.ServeHTTP(&writerHook{ResponseWriter: w, before: func() { g.revoke(share) }}, r)
				return
			}
			next.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		g.nothingOnB(o)
	})

	t.Run("wrong peer: granted to another instance", func(t *testing.T) {
		o := g.newOriginal("public")
		var other uuid.UUID
		if err := g.a.pool.QueryRow(ctx,
			`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref, capabilities)
			 VALUES ($1, 'C', 'k', TRUE, 'connected', $2, '["original-transfer-1"]'::jsonb) RETURNING id`,
			"https://c-"+uuid.NewString()[:8]+".example", g.a.admin).Scan(&other); err != nil {
			t.Fatal(err)
		}
		g.grant(o, other, "")
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if r := g.refusedOnA(o); r != "no_share_row" {
			t.Fatalf("A's audit reason %q", r)
		}
		g.nothingOnB(o)
	})

	t.Run("tampered in transit", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grant(o, g.peerBonA, "")
		g.a.setFront(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == http.MethodGet {
				flipped := false
				next.ServeHTTP(&writerHook{ResponseWriter: w, mutate: func(p []byte) []byte {
					if !flipped && len(p) > 0 {
						p[0] ^= 0xff
						flipped = true
					}
					return p
				}}, r)
				return
			}
			next.ServeHTTP(w, r)
		})
		defer g.a.setFront(nil)
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonFixityMismatch)
		g.nothingOnB(o)
	})

	t.Run("local hit needs both grants and makes no GET", func(t *testing.T) {
		o := g.newOriginal("public")
		// B already holds identical bytes from an unrelated upload.
		if _, err := g.storageOf(g.b).UploadOriginal(ctx, bytes.NewReader(o.body), "image/png",
			storage.PinRef{SubjectType: "user", SubjectID: "b-own-upload"}); err != nil {
			t.Fatal(err)
		}
		ingest := func() int {
			return g.onB(o, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_ingest'`)
		}
		heads, gets := g.a.casHEADs.Load(), g.a.casGETs.Load()

		// No inbound grant: refused, origin not asked, nothing pinned.
		g.inbound.set(false)
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		g.inbound.set(true)
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if g.a.casHEADs.Load() != heads || ingest() != 0 {
			t.Fatal("asked A or pinned without an inbound grant")
		}
		// Inbound yes, origin no grant: refused, nothing pinned.
		_, err = g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if ingest() != 0 {
			t.Fatal("pinned without an origin grant")
		}
		// Both: pinned with a HEAD and no GET.
		share := g.grant(o, g.peerBonA, "")
		res, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		if err != nil || !res.Deduped {
			t.Fatalf("%+v %v", res, err)
		}
		if g.a.casGETs.Load() != gets || ingest() != 1 {
			t.Fatalf("GETs %d->%d, ingest pins %d", gets, g.a.casGETs.Load(), ingest())
		}
		// Revoked at the origin: the next reuse is refused.
		g.revoke(share)
		_, err = g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if ingest() != 1 {
			t.Fatal("pinned again after the revoke")
		}
	})

	t.Run("user-scoped grant: only that actor, with its own key", func(t *testing.T) {
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
		g.grant(o, g.peerBonA, bobURI)

		// Instance-signed: the grant names a user, so refused.
		_, err = g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if r := g.refusedOnA(o); r != "wrong_user" {
			t.Fatalf("A's audit reason %q", r)
		}
		// Bob, signing with his key; A fetches and pins Bob's key from
		// B's production actor document.
		res, err := g.client().Fetch(ctx, g.req(o, casfetch.ActorSigner(bobURI, priv)))
		if err != nil || res.Deduped {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("restricted asset is never served", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grant(o, g.peerBonA, "")
		if _, err := g.a.pool.Exec(ctx, `UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, o.asset); err != nil {
			t.Fatal(err)
		}
		_, err := g.client().Fetch(ctx, g.req(o, g.signerB))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if r := g.refusedOnA(o); r != "sensitivity_refused" {
			t.Fatalf("A's audit reason %q", r)
		}
		g.nothingOnB(o)
	})
}

// Production stays fail-closed on receiving: the server's own client
// configuration (no fixture) refuses with no_inbound_grant even between
// negotiated, granted instances.
func TestTwoStack_CAS_ProductionReceiverIsFailClosed(t *testing.T) {
	g := newCASGate(t)
	o := g.newOriginal("public")
	g.grant(o, g.peerBonA, "")
	cl := casfetch.New(casfetch.Deps{
		Pool: g.b.pool, Storage: g.storageOf(g.b), Peers: peerLookupB{g},
		Grants: casfetch.FailClosedInboundGrants{}, Transport: g.tr,
		Config: casfetch.Config{TempDir: t.TempDir(), LocalBaseURL: func(context.Context) string { return g.b.url }},
	})
	_, err := cl.Fetch(context.Background(), g.req(o, g.signerB))
	wantReason(t, err, casfetch.ReasonNoInboundGrant)
	g.nothingOnB(o)
}
