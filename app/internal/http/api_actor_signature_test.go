// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1526 production-wiring tests for per-actor envelope signatures,
// with two real instances over httptest sharing one database:
//
//   - instance A sends: the production outbox Worker with the boot
//     wiring (wireOutboxActorSigning, wireOutboxDeliveryEncryption,
//     outboxDeliveryPeerLookup) and serves its actor document with the
//     production actorDocumentHandler;
//   - instance B receives: the production inbox Handler with
//     inboxPeerLookupFor and the production remote.SigningKeyResolver,
//     which fetches A's actor document with B's signed instance GET.
//
// Each instance sees the other through a federation_peers row, exactly
// as two paired instances do. Real Postgres; skips without
// AA_DB_PASSWORD.

package http

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// actor document modes A's server can be switched into.
const (
	docNormal int32 = iota
	docServerError
	docRedirect
	docSlow
)

type twoPeers struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	logger *slog.Logger
	peers  *peer.Registry
	uh     *users.Handler

	srvA, srvB       *httptest.Server
	aSigner, bSigner *outbox.IdentitySigner
	peerAonB         uuid.UUID // A as B sees it
	peerBonA         uuid.UUID // B as A sees it

	aliceRef  int64
	aliceName string
	aliceURI  string
	postID    uuid.UUID // a post local to B, the Like target

	docMode    atomic.Int32
	docFetches atomic.Int32
	resolver   *remote.SigningKeyResolver
}

func newTwoPeers(t *testing.T, bSupportsE2E bool) *twoPeers {
	t.Helper()
	return newTwoPeersWith(t, twoPeersOpts{bSupportsE2E: bSupportsE2E})
}

type twoPeersOpts struct {
	bSupportsE2E bool
	// negotiate pairs the two instances through the production
	// handshake engines (peer.Engine: offer, admin accept, confirm)
	// instead of inserting the peer rows, so each side's stored
	// capability set is the real negotiated intersection. The
	// handshake requires https instance URLs, so both instances
	// then run on TLS test servers.
	negotiate bool
}

func newTwoPeersWith(t *testing.T, opts twoPeersOpts) *twoPeers {
	t.Helper()
	pool := openPoolForSensitivity(t)
	t.Cleanup(pool.Close)
	initAtrestForOutboxWiring(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tp := &twoPeers{t: t, ctx: ctx, pool: pool, logger: logger}
	// Uncached, so a peer row changed by a test is seen at once.
	tp.peers = peer.NewRegistry(pool, logger, nil)
	tp.uh = users.NewHandler(pool, logger, nil)

	muxA, muxB := chi.NewRouter(), chi.NewRouter()
	if opts.negotiate {
		tp.srvA = httptest.NewTLSServer(muxA)
		tp.srvB = httptest.NewTLSServer(muxB)
		// Both TLS test servers present httptest's one built-in
		// certificate. Trust it on the default transport, which the
		// actor-document fetcher and this test's own client use;
		// restored afterwards (no test in this package is parallel).
		dt := http.DefaultTransport.(*http.Transport)
		prev := dt.TLSClientConfig
		roots := x509.NewCertPool()
		roots.AddCert(tp.srvA.Certificate())
		dt.TLSClientConfig = &tls.Config{RootCAs: roots}
		t.Cleanup(func() {
			dt.CloseIdleConnections()
			dt.TLSClientConfig = prev
		})
	} else {
		tp.srvA = httptest.NewServer(muxA)
		tp.srvB = httptest.NewServer(muxB)
	}
	t.Cleanup(tp.srvA.Close)
	t.Cleanup(tp.srvB.Close)

	aPub, aPriv, _ := ed25519.GenerateKey(rand.Reader)
	bPub, bPriv, _ := ed25519.GenerateKey(rand.Reader)
	if opts.negotiate {
		// One database holds one instance identity
		// (system_config federation.instance_identity), and the
		// handshake signs with it, so both test instances use it as
		// their instance key. Their peer rows still differ by URL.
		id, err := identity.NewManager(pool, logger).Load(ctx)
		if err != nil {
			t.Fatalf("instance identity: %v", err)
		}
		aPriv, bPriv = id.PrivateKey(), id.PrivateKey()
		aPub, bPub = aPriv.Public().(ed25519.PublicKey), bPriv.Public().(ed25519.PublicKey)
	}
	tp.aSigner = &outbox.IdentitySigner{PrivateKey: aPriv, KeyURL: tp.srvA.URL + "/federation/instance#main-key"}
	tp.bSigner = &outbox.IdentitySigner{PrivateKey: bPriv, KeyURL: tp.srvB.URL + "/federation/instance#main-key"}

	tp.aliceName = "sig-alice-" + uuid.NewString()[:8]
	tp.aliceURI = tp.srvA.URL + "/users/" + tp.aliceName
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Alice', 1) RETURNING ref`,
		tp.aliceName).Scan(&tp.aliceRef); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if opts.negotiate {
		tp.pairByHandshake()
	} else {
		caps := `[]`
		if opts.bSupportsE2E {
			caps = `["e2e-encrypted","nacl-box","x25519"]`
		}
		aPEM, _ := federation.PublicKeyToPEM(aPub)
		bPEM, _ := federation.PublicKeyToPEM(bPub)
		for _, p := range []struct {
			id   *uuid.UUID
			url  string
			pem  []byte
			caps string
		}{{&tp.peerAonB, tp.srvA.URL, aPEM, `[]`}, {&tp.peerBonA, tp.srvB.URL, bPEM, caps}} {
			if err := pool.QueryRow(ctx,
				`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
				    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref, capabilities)
				 VALUES ($1, 'Sig Peer', $2, 'connected', 'plaintext', TRUE, 'connected', $3, $4::jsonb)
				 RETURNING id`, p.url, string(p.pem), tp.aliceRef, p.caps).Scan(p.id); err != nil {
				t.Fatalf("seed peer: %v", err)
			}
		}
	}
	tp.postID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Target', 'explicit-share')`,
		tp.postID, tp.aliceRef); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, tp.peerBonA)
		_, _ = pool.Exec(c, `DELETE FROM federation_inbox WHERE peer_id = $1`, tp.peerAonB)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, tp.aliceRef)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, tp.postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_user_keys WHERE user_ref = $1`, tp.aliceRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = ANY($1) OR instance_url = ANY($2)`,
			[]uuid.UUID{tp.peerAonB, tp.peerBonA}, []string{tp.srvA.URL, tp.srvB.URL})
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, tp.aliceRef)
	})

	// Instance A: the actor document, production handler, with fault
	// modes for the fetch-failure cases.
	doc := &actorDocumentHandler{
		pool:     pool,
		lookup:   inboxPeerLookupFor(tp.peers),
		baseURL:  func(context.Context) string { return tp.srvA.URL },
		logger:   logger,
		fallback: func() http.Handler { return http.HandlerFunc(spaMarker) },
	}
	muxA.Get("/users/{username}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Signature") != "" {
			tp.docFetches.Add(1)
		}
		switch tp.docMode.Load() {
		case docServerError:
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case docRedirect:
			http.Redirect(w, r, tp.srvA.URL+"/elsewhere", http.StatusFound)
			return
		case docSlow:
			time.Sleep(time.Second)
		}
		doc.ServeHTTP(w, r)
	})

	// Instance B: the inbox, production handler and resolver.
	inboxB := inbox.NewHandler(inbox.HandlerDeps{
		Pool:         inbox.New(pool),
		Lookup:       inboxPeerLookupFor(tp.peers),
		Logger:       logger,
		LocalBaseURL: func(context.Context) string { return tp.srvB.URL },
	})
	tp.resolver = remote.NewSigningKeyResolver(pool, tp.bSigner, logger)
	inboxB.SetActorKeyResolver(tp.resolver)
	muxB.Post("/federation/inbox", inboxB.PostInbox)
	muxB.Post("/federation/inbox/batch", inboxB.PostInboxBatch)
	return tp
}

func spaMarker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte("<!doctype html>SPA"))
}

// worker is instance A's delivery worker with the boot wiring.
func (tp *twoPeers) worker(recipientPub []byte) *outbox.Worker {
	w := outbox.NewWorker(outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
		tp.pool, tp.aSigner, outboxDeliveryPeerLookup(tp.peers), tp.logger)
	wireOutboxDeliveryEncryption(w, tp.pool, tp.peers, func(context.Context, string) ([]byte, int32, error) {
		if recipientPub == nil {
			return nil, 0, errors.New("no recipient key")
		}
		return recipientPub, 1, nil
	}, audit.NewRecorder(tp.pool, tp.logger))
	wireOutboxActorSigning(w, tp.uh, func(context.Context) string { return tp.srvA.URL })
	return w
}

// queueLike records a Like by alice (as actorURI) on B's post and
// queues it for B. Returns the activity URI and id.
func (tp *twoPeers) queueLike(actorURI string) (string, uuid.UUID) {
	tp.t.Helper()
	uri := tp.srvA.URL + "/activities/" + uuid.NewString()
	var id uuid.UUID
	if err := tp.pool.QueryRow(tp.ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, object_local_id, to_uris, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '[]'::jsonb, '{"content":"like-marker"}'::jsonb)
		 RETURNING id`,
		uri, actorURI, tp.aliceRef, tp.srvB.URL+"/posts/"+tp.postID.String(), tp.postID.String(),
	).Scan(&id); err != nil {
		tp.t.Fatalf("seed activity: %v", err)
	}
	if _, err := tp.pool.Exec(tp.ctx,
		`INSERT INTO federation_outbox (activity_id, peer_id, target_user_url, sensitivity)
		 VALUES ($1, $2, $3, 'public')`, id, tp.peerBonA, tp.srvB.URL+"/users/bob"); err != nil {
		tp.t.Fatalf("seed outbox: %v", err)
	}
	return uri, id
}

func (tp *twoPeers) stored(activityURI string) (bool, []byte) {
	var env []byte
	err := tp.pool.QueryRow(tp.ctx,
		`SELECT envelope_json FROM federation_inbox WHERE activity_uri = $1 AND peer_id = $2`,
		activityURI, tp.peerAonB).Scan(&env)
	return err == nil, env
}

func (tp *twoPeers) outboxStatus(activityID uuid.UUID) (status string, wasEncrypted bool, attempts int) {
	if err := tp.pool.QueryRow(tp.ctx,
		`SELECT status, was_encrypted, attempts FROM federation_outbox WHERE activity_id = $1`, activityID,
	).Scan(&status, &wasEncrypted, &attempts); err != nil {
		tp.t.Fatalf("outbox row: %v", err)
	}
	return
}

func (tp *twoPeers) alicePrivPEM() []byte {
	tp.t.Helper()
	m, err := tp.uh.EnsureActorKeyMaterial(tp.ctx, tp.aliceRef, tp.srvA.URL, tp.aliceName)
	if err != nil {
		tp.t.Fatal(err)
	}
	p, err := m.DecryptSigningPrivateKey()
	if err != nil {
		tp.t.Fatal(err)
	}
	return p
}

// postEnvelope POSTs raw envelope bytes to B's inbox signed with A's
// instance key (an authenticated peer request), returning the status
// and reason.
func (tp *twoPeers) postEnvelope(body []byte) (int, string) {
	tp.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, tp.srvB.URL+"/federation/inbox", bytes.NewReader(body))
	if err := tp.aSigner.Sign(req, body); err != nil {
		tp.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tp.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r.Reason
}

// likeEnvelope builds a Like from actorURI, signed with privPEM when
// non-nil, else carrying the pre-#1526 placeholder.
func (tp *twoPeers) likeEnvelope(actorURI string, privPEM []byte) (string, []byte) {
	tp.t.Helper()
	uri := tp.srvA.URL + "/activities/" + uuid.NewString()
	env := &federation.Envelope{
		Context: federation.ContextV1, Type: federation.ActivityLike, ID: uri, Actor: actorURI,
		Published: time.Now().UTC().Truncate(time.Second),
		Object:    tp.srvB.URL + "/posts/" + tp.postID.String(),
		Signature: &federation.Signature{Type: federation.SignatureAlgEd25519, PublicKey: actorURI + "#main-key", Value: "AAAAAAAAAAAA"},
	}
	if privPEM != nil {
		if err := env.Sign(privPEM, actorURI+"#main-key"); err != nil {
			tp.t.Fatal(err)
		}
	}
	b, err := env.Marshal()
	if err != nil {
		tp.t.Fatal(err)
	}
	return uri, b
}

func TestActorSignatures_TwoPeers_PlaintextSingleThenBatch(t *testing.T) {
	tp := newTwoPeers(t, false)
	w := tp.worker(nil)

	uri1, id1 := tp.queueLike(tp.aliceURI)
	w.RunOnce(tp.ctx)
	if ok, env := tp.stored(uri1); !ok {
		st, _, _ := tp.outboxStatus(id1)
		t.Fatalf("single delivery not accepted by B (A row %s)", st)
	} else if !bytes.Contains(env, []byte(`"signature"`)) || bytes.Contains(env, []byte("AAAAAAAAAAAA")) {
		t.Errorf("stored envelope does not carry a real signature: %s", env)
	}
	if st, _, _ := tp.outboxStatus(id1); st != "sent" {
		t.Errorf("A row: %s want sent", st)
	}
	// B pinned exactly alice's key, after one signed fetch.
	var pinned []byte
	if err := tp.pool.QueryRow(tp.ctx,
		`SELECT signing_public_key FROM federation_remote_actors WHERE actor_uri = $1 AND peer_id = $2`,
		tp.aliceURI, tp.peerAonB).Scan(&pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}
	m, _ := tp.uh.GetActorKeyMaterial(tp.ctx, tp.aliceRef)
	alicePub, _ := federation.PublicKeyFromPEM(m.SigningPublicKeyPEM)
	if !alicePub.Equal(ed25519.PublicKey(pinned)) {
		t.Error("B pinned a key that is not alice's")
	}
	if n := tp.docFetches.Load(); n != 1 {
		t.Errorf("actor document fetches: %d want 1", n)
	}

	// Two more for the same peer go through the batch endpoint.
	uri2, id2 := tp.queueLike(tp.aliceURI)
	uri3, id3 := tp.queueLike(tp.aliceURI)
	w.RunOnce(tp.ctx)
	for _, c := range []struct {
		uri string
		id  uuid.UUID
	}{{uri2, id2}, {uri3, id3}} {
		if ok, _ := tp.stored(c.uri); !ok {
			st, _, _ := tp.outboxStatus(c.id)
			t.Errorf("batch envelope %s not accepted (A row %s)", c.uri, st)
		}
	}
	if n := tp.docFetches.Load(); n != 1 {
		t.Errorf("actor document fetches after batch: %d want 1 (pinned)", n)
	}
}

func TestActorSignatures_TwoPeers_EncryptedSignatureCoversCiphertext(t *testing.T) {
	tp := newTwoPeers(t, true)
	pub, wrapped, err := userkeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.pool.Exec(tp.ctx,
		`INSERT INTO federation_user_keys (user_ref, version, algorithm, public_key, private_key_enc, is_current)
		 VALUES ($1, 1, 'naclbox-x25519-v1', $2, $3, TRUE)`, tp.aliceRef, pub, wrapped); err != nil {
		t.Fatal(err)
	}
	recipientPub, _, err := userkeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	w := tp.worker(recipientPub)
	uri, id := tp.queueLike(tp.aliceURI)
	w.RunOnce(tp.ctx)
	ok, env := tp.stored(uri)
	if !ok {
		st, _, _ := tp.outboxStatus(id)
		t.Fatalf("encrypted delivery not accepted by B (A row %s)", st)
	}
	if !bytes.Contains(env, []byte(`"encryption"`)) || bytes.Contains(env, []byte("like-marker")) {
		t.Errorf("stored envelope is not the encrypted one: %s", env)
	}
	if _, enc, _ := tp.outboxStatus(id); !enc {
		t.Error("A row not marked encrypted")
	}
}

// The pin holds: when A's user key changes, B refuses the newly signed
// envelope (sig_invalid) instead of re-fetching (no automatic rotation).
func TestActorSignatures_TwoPeers_PinnedKeyRefusesChangedKey(t *testing.T) {
	tp := newTwoPeers(t, false)
	w := tp.worker(nil)
	uri1, _ := tp.queueLike(tp.aliceURI)
	w.RunOnce(tp.ctx)
	if ok, _ := tp.stored(uri1); !ok {
		t.Fatal("first delivery not accepted")
	}
	// Replace alice's key on A (what an unsupported manual rotation
	// would do).
	if _, err := tp.pool.Exec(tp.ctx,
		`UPDATE "user" SET signing_public_key_pem = NULL, signing_private_key_enc = NULL WHERE ref = $1`,
		tp.aliceRef); err != nil {
		t.Fatal(err)
	}
	uri2, id2 := tp.queueLike(tp.aliceURI)
	w.RunOnce(tp.ctx)
	if ok, _ := tp.stored(uri2); ok {
		t.Error("an envelope signed with a changed key was accepted")
	}
	if st, _, _ := tp.outboxStatus(id2); st != "failed" {
		t.Errorf("A row: %s want failed (B answered sig_invalid)", st)
	}
	if n := tp.docFetches.Load(); n != 1 {
		t.Errorf("actor document fetches: %d want 1 (never re-fetched)", n)
	}
}

func TestActorSignatures_TwoPeers_ReceiverRefusals(t *testing.T) {
	cases := []struct {
		name        string
		build       func(tp *twoPeers) (string, []byte)
		wantStatus  int
		wantReason  federation.InboxStatus
		wantFetches int32
	}{
		{"pre-#1526 placeholder", func(tp *twoPeers) (string, []byte) {
			return tp.likeEnvelope(tp.aliceURI, nil)
		}, http.StatusBadRequest, federation.InboxStatusSigMalformed, 0},
		{"actor on another host, real key (spoof)", func(tp *twoPeers) (string, []byte) {
			return tp.likeEnvelope("https://elsewhere.example/users/"+tp.aliceName, tp.alicePrivPEM())
		}, http.StatusForbidden, federation.InboxStatusUnknownActor, 0},
		{"tampered after signing", func(tp *twoPeers) (string, []byte) {
			uri, b := tp.likeEnvelope(tp.aliceURI, tp.alicePrivPEM())
			return uri, bytes.Replace(b, []byte(tp.postID.String()), []byte(uuid.NewString()), 1)
		}, http.StatusUnauthorized, federation.InboxStatusSigInvalid, 1},
		{"actor unknown on A (404 document)", func(tp *twoPeers) (string, []byte) {
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			pemBytes, _ := federation.PrivateKeyToPEM(priv)
			return tp.likeEnvelope(tp.srvA.URL+"/users/ghost-"+uuid.NewString()[:6], pemBytes)
		}, http.StatusUnprocessableEntity, federation.InboxStatusUnknownActor, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp := newTwoPeers(t, false)
			uri, body := c.build(tp)
			status, reason := tp.postEnvelope(body)
			if status != c.wantStatus || reason != string(c.wantReason) {
				t.Errorf("B answered %d/%s want %d/%s", status, reason, c.wantStatus, c.wantReason)
			}
			if ok, _ := tp.stored(uri); ok {
				t.Error("refused envelope stored")
			}
			if n := tp.docFetches.Load(); n != c.wantFetches {
				t.Errorf("actor document fetches: %d want %d", n, c.wantFetches)
			}
		})
	}
}

// Actor document fetch failures through the real path: transient ones
// leave A's row queued for retry, terminal ones fail it; never stored.
func TestActorSignatures_TwoPeers_DocumentFetchFailures(t *testing.T) {
	for _, c := range []struct {
		name       string
		mode       int32
		wantStatus string
	}{
		{"5xx", docServerError, "queued"},
		{"timeout", docSlow, "queued"},
		{"redirect", docRedirect, "failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tp := newTwoPeers(t, false)
			tp.resolver.SetFetchTimeout(300 * time.Millisecond)
			tp.docMode.Store(c.mode)
			w := tp.worker(nil)
			uri, id := tp.queueLike(tp.aliceURI)
			w.RunOnce(tp.ctx)
			if ok, _ := tp.stored(uri); ok {
				t.Error("stored without a verified signature")
			}
			// A transient failure is a counted, retried attempt; a
			// terminal one (B answered unknown_actor) fails the row.
			st, _, attempts := tp.outboxStatus(id)
			if st != c.wantStatus || (st == "queued" && attempts < 1) {
				t.Errorf("A row: %s (attempts %d) want %s", st, attempts, c.wantStatus)
			}
			var pinned int
			_ = tp.pool.QueryRow(tp.ctx,
				`SELECT COUNT(*) FROM federation_remote_actors WHERE actor_uri = $1 AND signing_public_key IS NOT NULL`,
				tp.aliceURI).Scan(&pinned)
			if pinned != 0 {
				t.Error("a key was pinned from a failed fetch")
			}
		})
	}
}

// The sender never sends what it cannot sign.
func TestActorSignatures_Sender_NeverSendsUnsigned(t *testing.T) {
	t.Run("activity actor URI is not the user's current one", func(t *testing.T) {
		tp := newTwoPeers(t, false)
		w := tp.worker(nil)
		uri, id := tp.queueLike("https://old-base.example/users/" + tp.aliceName)
		w.RunOnce(tp.ctx)
		if ok, _ := tp.stored(uri); ok {
			t.Error("sent under a stale actor URI")
		}
		if st, _, attempts := tp.outboxStatus(id); st == "sent" || attempts < 1 {
			t.Errorf("A row: %s attempts %d; want a failed attempt, not sent", st, attempts)
		}
	})
	t.Run("no signing hook wired", func(t *testing.T) {
		tp := newTwoPeers(t, false)
		w := outbox.NewWorker(outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
			tp.pool, tp.aSigner, outboxDeliveryPeerLookup(tp.peers), tp.logger)
		uri, id := tp.queueLike(tp.aliceURI)
		w.RunOnce(tp.ctx)
		if ok, _ := tp.stored(uri); ok {
			t.Error("sent without a signature")
		}
		if st, _, _ := tp.outboxStatus(id); st == "sent" {
			t.Error("A row marked sent")
		}
		if n := tp.docFetches.Load(); n != 0 {
			t.Errorf("B fetched %d actor documents for an envelope it never received", n)
		}
	})
}

// The actor document: paired peers only, exact identity, no
// enumeration, browser unchanged.
func TestActorDocument_Access(t *testing.T) {
	tp := newTwoPeers(t, false)
	_ = tp.alicePrivPEM() // alice has a key
	var keylessRef int64
	keylessName := "sig-keyless-" + uuid.NewString()[:8]
	if err := tp.pool.QueryRow(tp.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Keyless', 1) RETURNING ref`,
		keylessName).Scan(&keylessRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tp.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, keylessRef) })
	_, strangerPriv, _ := ed25519.GenerateKey(rand.Reader)
	stranger := &outbox.IdentitySigner{PrivateKey: strangerPriv, KeyURL: "https://stranger.example/federation/instance#main-key"}

	get := func(path string, signer *outbox.IdentitySigner) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, tp.srvA.URL+path, nil)
		if signer != nil {
			if err := signer.Sign(req, []byte{}); err != nil {
				t.Fatal(err)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	// Unsigned: the browser / anonymous path gets the SPA, never JSON.
	resp, body := get("/users/"+tp.aliceName, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "SPA") || strings.Contains(string(body), "publicKey") {
		t.Errorf("unsigned GET: %d %q; want the SPA fallback", resp.StatusCode, body)
	}
	// Signed by a non-peer: refused, no data.
	if resp, body := get("/users/"+tp.aliceName, stranger); resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), "publicKey") {
		t.Errorf("stranger GET: %d %q; want 401", resp.StatusCode, body)
	}
	// Signed by paired B: the document, exact identity.
	resp, body = get("/users/"+tp.aliceName, tp.bSigner)
	var doc remote.ActorDocument
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &doc) != nil {
		t.Fatalf("peer GET: %d %q", resp.StatusCode, body)
	}
	if doc.ID != tp.aliceURI || doc.PublicKey.ID != tp.aliceURI+"#main-key" || doc.PublicKey.Owner != tp.aliceURI {
		t.Errorf("document identity: %+v", doc)
	}
	m, _ := tp.uh.GetActorKeyMaterial(tp.ctx, tp.aliceRef)
	if doc.PublicKey.PublicKeyPEM != string(m.SigningPublicKeyPEM) {
		t.Error("document key is not alice's signing key")
	}
	// Unknown, keyless and reserved names are 404 even for a peer.
	for _, name := range []string{"nobody-" + uuid.NewString()[:6], keylessName, "by-ref", "by-username"} {
		if resp, body := get("/users/"+name, tp.bSigner); resp.StatusCode != http.StatusNotFound {
			t.Errorf("peer GET %s: %d %q; want 404", name, resp.StatusCode, body)
		}
	}
	// A disabled peer is refused.
	if _, err := tp.pool.Exec(tp.ctx, `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, tp.peerBonA); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get("/users/"+tp.aliceName, tp.bSigner); resp.StatusCode == http.StatusOK {
		t.Errorf("disabled peer GET: %d; want refused", resp.StatusCode)
	}
}

// handshakeRelay is a peer.HTTPDoer that hands a handshake envelope to
// the other instance's production engine, in place of the network,
// and answers the way the production endpoint does (#1592): only the
// canonical path, a 200 HandshakeAccepted JSON object on success, a
// 400 JSON error otherwise. The production router itself is exercised
// in handshake_router_test.go.
type handshakeRelay struct {
	to func() *peer.Engine
}

func (h handshakeRelay) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	status, payload := http.StatusOK, []byte(nil)
	if req.URL.Path != peer.HandshakePath {
		status, payload = http.StatusNotFound, []byte(`{"error":"not found"}`)
	} else if res, err := h.to().HandleInbound(req.Context(), body); err != nil {
		status, payload = http.StatusBadRequest, []byte(`{"error":`+strconv.Quote(err.Error())+`}`)
	} else {
		payload, _ = json.Marshal(map[string]string{"status": res.Status, "peer_id": res.Peer.ID.String()})
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Request:    req,
	}, nil
}

// pairByHandshake pairs A and B with the production handshake: A's
// engine sends an offer (advertising peer.KnownCapabilities), B's
// engine records A as pending_inbound with the intersection, B's admin
// accepts, and B's confirm (advertising its own set) completes A's
// pending_outbound row with the intersection. Both stored capability
// sets are therefore negotiated, not inserted.
func (tp *twoPeers) pairByHandshake() {
	tp.t.Helper()
	idMgr := identity.NewManager(tp.pool, tp.logger)
	if _, err := idMgr.Load(tp.ctx); err != nil {
		tp.t.Fatalf("identity: %v", err)
	}
	var engA, engB *peer.Engine
	engA = peer.NewEngine(tp.peers, idMgr, handshakeRelay{to: func() *peer.Engine { return engB }})
	engB = peer.NewEngine(tp.peers, idMgr, handshakeRelay{to: func() *peer.Engine { return engA }})
	engA.SetLocalBaseURL(tp.srvA.URL)
	engB.SetLocalBaseURL(tp.srvB.URL)
	engA.SetLocalDisplayName("Studio A")
	engB.SetLocalDisplayName("Studio B")

	bOnA, err := engA.InitiateOffer(tp.ctx, tp.srvB.URL, "Studio B", tp.aliceRef)
	if err != nil {
		tp.t.Fatalf("offer: %v", err)
	}
	aOnB, err := tp.peers.ByInstanceURL(tp.ctx, tp.srvA.URL)
	if err != nil {
		tp.t.Fatalf("B did not record A's offer: %v", err)
	}
	if err := engB.AcceptInbound(tp.ctx, *aOnB); err != nil {
		tp.t.Fatalf("accept: %v", err)
	}
	tp.peerAonB, tp.peerBonA = aOnB.ID, bOnA.ID
}

// #1526 acceptance: with ed25519-envelope-sig NEGOTIATED by the real
// handshake on both peer records, a validly signed envelope is
// accepted by the production inbox and an invalid one is refused with
// a typed reason and not stored. Verification does not depend on the
// token: the same refusals hold after B's record of A loses it.
func TestActorSignatures_NegotiatedCapability_ProductionInbox(t *testing.T) {
	tp := newTwoPeersWith(t, twoPeersOpts{negotiate: true})

	for _, rec := range []struct {
		name string
		id   uuid.UUID
		url  string
	}{{"B's record of A", tp.peerAonB, tp.srvA.URL}, {"A's record of B", tp.peerBonA, tp.srvB.URL}} {
		p, err := tp.peers.ByID(tp.ctx, rec.id)
		if err != nil {
			t.Fatalf("%s: %v", rec.name, err)
		}
		if p.InstanceURL != rec.url || p.Status != federation.PeerStatusConnected || !p.Enabled {
			t.Fatalf("%s: url %s status %s enabled %v; want a connected pairing", rec.name, p.InstanceURL, p.Status, p.Enabled)
		}
		if !p.Capabilities.Has(peer.CapEd25519EnvelopeSig) || !p.CapabilitiesNegotiatedAt.Valid {
			t.Errorf("%s: capabilities %v negotiated=%v; want %s negotiated",
				rec.name, p.Capabilities, p.CapabilitiesNegotiatedAt.Valid, peer.CapEd25519EnvelopeSig)
		}
	}

	refuse := func(label string, body []byte, uri string, wantStatus int, wantReason federation.InboxStatus) {
		t.Helper()
		status, reason := tp.postEnvelope(body)
		if status != wantStatus || reason != string(wantReason) {
			t.Errorf("%s: B answered %d/%s want %d/%s", label, status, reason, wantStatus, wantReason)
		}
		if ok, _ := tp.stored(uri); ok {
			t.Errorf("%s: refused envelope stored", label)
		}
	}

	// Valid: signed by alice's real key, through B's production inbox.
	validURI, valid := tp.likeEnvelope(tp.aliceURI, tp.alicePrivPEM())
	if status, reason := tp.postEnvelope(valid); status != http.StatusAccepted {
		t.Fatalf("valid envelope: B answered %d/%s want 202", status, reason)
	}
	if ok, _ := tp.stored(validURI); !ok {
		t.Fatal("valid envelope not stored")
	}
	if n := tp.docFetches.Load(); n != 1 {
		t.Errorf("actor document fetches: %d want 1 (over the paired TLS origin)", n)
	}

	// Invalid: tampered after signing, and the pre-#1526 placeholder.
	tamperedURI, tampered := tp.likeEnvelope(tp.aliceURI, tp.alicePrivPEM())
	tampered = bytes.Replace(tampered, []byte(tp.postID.String()), []byte(uuid.NewString()), 1)
	refuse("tampered", tampered, tamperedURI, http.StatusUnauthorized, federation.InboxStatusSigInvalid)
	placeholderURI, placeholder := tp.likeEnvelope(tp.aliceURI, nil)
	refuse("placeholder", placeholder, placeholderURI, http.StatusBadRequest, federation.InboxStatusSigMalformed)

	// Not gated on the token: drop it from B's record of A and the
	// invalid envelopes are refused exactly the same.
	if err := tp.peers.SetCapabilities(tp.ctx, tp.peerAonB, peer.CapabilitySet{}); err != nil {
		t.Fatal(err)
	}
	t2URI, t2 := tp.likeEnvelope(tp.aliceURI, tp.alicePrivPEM())
	t2 = bytes.Replace(t2, []byte(tp.postID.String()), []byte(uuid.NewString()), 1)
	refuse("tampered, token removed", t2, t2URI, http.StatusUnauthorized, federation.InboxStatusSigInvalid)
	p2URI, p2 := tp.likeEnvelope(tp.aliceURI, nil)
	refuse("placeholder, token removed", p2, p2URI, http.StatusBadRequest, federation.InboxStatusSigMalformed)
}
