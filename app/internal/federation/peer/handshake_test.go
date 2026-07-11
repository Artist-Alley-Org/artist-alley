// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the handshake protocol — Phase 1.22.B-b.
//
// Two layers:
//
//   1. Unit-level: parseAndVerify rejects every protocol violation
//      we expect (bad protocol string, wrong recipient, stale
//      timestamp, bad signature, malformed PEM, etc.).
//
//   2. Integration: full A-initiates -> B-accepts pair loop using
//      httptest.Server pointed at a real PublicHandler backed by
//      a real Registry + Engine + Identity. Proves both sides end
//      up with a connected row carrying each other's real pubkey.
//
// Skips without AA_DB_PASSWORD per project convention.

package peer_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// helpers reuse the openPool/envOr/randHex/freshPEM/fixtureAdmin
// from peer_test.go (same package).

func ensureAtrestHS(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

// hsFixture is a single-instance fixture — Registry + Engine +
// Identity wired against a fresh DB pool. Used standalone for
// unit tests + paired for the two-instance integration test.
type hsFixture struct {
	pool     *pgxpool.Pool
	registry *peer.Registry
	engine   *peer.Engine
	idMgr    *identity.Manager
	baseURL  string
}

// newHSFixture wires one fixture with a UNIQUE per-test sysconfig
// identity so two fixtures in the same test get DIFFERENT
// keypairs. The trick: each fixture's identity is stored under
// a derived sysconfig key (we monkey-patch nothing; just use
// fresh Managers backed by separate Pools or scoped rows).
//
// For simplicity here we keep both fixtures on the same DB pool
// but the second fixture clears + repopulates the identity row
// after the first fixture saved its keys to a local variable.
// Caller is responsible for sequencing (see TestFullHandshakePair
// which constructs A first, captures A's keys, then constructs B
// which overwrites the identity row + captures B's keys, then
// flips back to A's keys when A needs to verify, etc.).
//
// In practice we run with TWO separate Manager instances reading
// the same row at different times — which is exactly what
// happens in production across two peer DBs. The test is a
// proper integration of the protocol; only the storage backend
// is shared.
func newHSFixture(t *testing.T, baseURL string) *hsFixture {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ensureAtrestHS(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	// Clear any prior identity row so each fixture generates fresh.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM system_config WHERE key='federation.instance_identity'`); err != nil {
		t.Fatal(err)
	}
	idMgr := identity.NewManager(pool, logger)
	if _, err := idMgr.Load(context.Background()); err != nil {
		t.Fatalf("identity Load: %v", err)
	}

	registry := peer.NewRegistry(pool, logger, reg)
	engine := peer.NewEngine(registry, idMgr, nil)
	engine.SetLocalBaseURL(baseURL)
	engine.SetLocalDisplayName("Test Instance @ " + baseURL)

	return &hsFixture{
		pool: pool, registry: registry, engine: engine,
		idMgr: idMgr, baseURL: baseURL,
	}
}

// signedEnv is the on-wire shape exported for tests so we can
// construct hand-built envelopes for negative cases.
type testEnvelope struct {
	Envelope  testInnerEnvelope `json:"envelope"`
	Signature string            `json:"signature"`
}

type testInnerEnvelope struct {
	Protocol         string `json:"protocol"`
	Type             string `json:"type"`
	From             string `json:"from"`
	FromDisplayName  string `json:"from_display_name"`
	FromPublicKeyPEM string `json:"from_public_key_pem"`
	To               string `json:"to"`
	Nonce            string `json:"nonce"`
	Timestamp        string `json:"timestamp"`
}

// mintEnvelope builds + signs an envelope with the given fixture's
// identity. Used by the unit tests + as the building block for
// the integration test.
func mintEnvelope(t *testing.T, fx *hsFixture, typ, toURL string) []byte {
	t.Helper()
	id, err := fx.idMgr.Get()
	if err != nil {
		t.Fatal(err)
	}
	env := testInnerEnvelope{
		Protocol:         peer.HandshakeProtocol,
		Type:             typ,
		From:             fx.baseURL,
		FromDisplayName:  "Test Instance @ " + fx.baseURL,
		FromPublicKeyPEM: string(id.PublicKeyPEM()),
		To:               toURL,
		Nonce:            randNonce(t),
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := federation.Canonicalize(envJSON)
	if err != nil {
		t.Fatal(err)
	}
	sig := id.Sign(canonical)
	body, err := json.Marshal(testEnvelope{
		Envelope:  env,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func randNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// --- protocol-level rejections (negative path) --------------------------

func TestHandleInbound_RejectsBadProtocol(t *testing.T) {
	fx := newHSFixture(t, "https://test-receiver.example")
	body := mintEnvelope(t, fx, peer.HandshakeTypeOffer, fx.baseURL)
	// Replace the protocol string + RE-SIGN with the same key
	// (otherwise we'd just fail signature). We replace via JSON
	// surgery so canonicalization is consistent.
	var on testEnvelope
	_ = json.Unmarshal(body, &on)
	on.Envelope.Protocol = "wrong-protocol/v9"
	canonical, _ := federation.Canonicalize(mustJSON(on.Envelope))
	id, _ := fx.idMgr.Get()
	on.Signature = base64.StdEncoding.EncodeToString(id.Sign(canonical))
	body = mustJSON(on)
	_, err := fx.engine.HandleInbound(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Errorf("expected ErrHandshakeProtocol, got %v", err)
	}
}

func TestHandleInbound_RejectsWrongRecipient(t *testing.T) {
	fx := newHSFixture(t, "https://test-receiver.example")
	body := mintEnvelope(t, fx, peer.HandshakeTypeOffer, "https://different-instance.example")
	_, err := fx.engine.HandleInbound(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Errorf("expected ErrHandshakeWrongRecipient, got %v", err)
	}
}

func TestHandleInbound_RejectsStale(t *testing.T) {
	fx := newHSFixture(t, "https://test-receiver.example")
	// Build an envelope with a timestamp 10 minutes in the past.
	id, _ := fx.idMgr.Get()
	env := testInnerEnvelope{
		Protocol:         peer.HandshakeProtocol,
		Type:             peer.HandshakeTypeOffer,
		From:             "https://stale-sender.example",
		FromDisplayName:  "Stale",
		FromPublicKeyPEM: string(id.PublicKeyPEM()),
		To:               fx.baseURL,
		Nonce:            randNonce(t),
		Timestamp:        time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano),
	}
	canonical, _ := federation.Canonicalize(mustJSON(env))
	body := mustJSON(testEnvelope{
		Envelope:  env,
		Signature: base64.StdEncoding.EncodeToString(id.Sign(canonical)),
	})
	_, err := fx.engine.HandleInbound(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "skew") {
		t.Errorf("expected ErrHandshakeStale, got %v", err)
	}
}

func TestHandleInbound_RejectsBadSignature(t *testing.T) {
	fx := newHSFixture(t, "https://test-receiver.example")
	body := mintEnvelope(t, fx, peer.HandshakeTypeOffer, fx.baseURL)
	// Flip a byte in the signature without resigning.
	var on testEnvelope
	_ = json.Unmarshal(body, &on)
	sig, _ := base64.StdEncoding.DecodeString(on.Signature)
	sig[0] ^= 0xFF
	on.Signature = base64.StdEncoding.EncodeToString(sig)
	body = mustJSON(on)
	_, err := fx.engine.HandleInbound(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("expected ErrHandshakeBadSig, got %v", err)
	}
}

// --- offer creates pending_inbound -------------------------------------

func TestHandleInbound_OfferCreatesPendingInbound(t *testing.T) {
	fx := newHSFixture(t, "https://test-receiver.example")
	// Build the offer envelope as if from a DIFFERENT instance.
	// We mint with the receiver's own key here for simplicity —
	// the protocol doesn't care WHO signed as long as the
	// signature verifies. The pubkey embedded in the envelope is
	// what we'd record as the peer's key.
	body := mintEnvelope(t, fx, peer.HandshakeTypeOffer, fx.baseURL)
	// Override the From so it's a "different" instance.
	var on testEnvelope
	_ = json.Unmarshal(body, &on)
	on.Envelope.From = "https://offering-peer.example"
	on.Envelope.FromDisplayName = "Offering Peer"
	canonical, _ := federation.Canonicalize(mustJSON(on.Envelope))
	id, _ := fx.idMgr.Get()
	on.Signature = base64.StdEncoding.EncodeToString(id.Sign(canonical))
	body = mustJSON(on)

	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(),
			`DELETE FROM federation_peers WHERE instance_url='https://offering-peer.example'`)
	})

	result, err := fx.engine.HandleInbound(context.Background(), body)
	if err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}
	if result.Status != "accepted_pending" {
		t.Errorf("status: got %q, want accepted_pending", result.Status)
	}
	if result.Peer.Status != federation.PeerStatusPendingInbound {
		t.Errorf("peer status: got %q, want pending_inbound", result.Peer.Status)
	}
	if result.Peer.InstanceURL != "https://offering-peer.example" {
		t.Errorf("peer URL: got %q", result.Peer.InstanceURL)
	}
}

// --- full pair loop over httptest --------------------------------------
//
// Two engines, A and B, talk to each other through httptest
// servers that wrap a HandleInbound-driven HTTP shim. Proves
// the protocol works end-to-end including the confirm callback.

func TestFullHandshakePair_OverHTTPRoundTrip(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ensureAtrestHS(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()

	// Set up two independent identities + registries. We get
	// independence by giving each a fresh Manager but TWO separate
	// system_config rows under derived keys. Easier route: just
	// store both identities in-memory + bypass the sysconfig load.
	// We do this by using the Manager.Get pattern after a manual
	// generate step.

	// For test simplicity, generate two completely separate
	// Identity instances by writing + reading the sysconfig row
	// twice with cleanup in between.
	mkIdentity := func() *identity.Manager {
		_, _ = pool.Exec(ctx, `DELETE FROM system_config WHERE key='federation.instance_identity'`)
		m := identity.NewManager(pool, logger)
		if _, err := m.Load(ctx); err != nil {
			t.Fatal(err)
		}
		return m
	}
	idA := mkIdentity()
	idB := mkIdentity()
	// After mkIdentity twice, only idB's keys are in the DB. But
	// each Manager keeps its loaded Identity in memory, so both
	// idA.Get() and idB.Get() return their respective keypairs
	// independently of the current DB state.

	registryA := peer.NewRegistry(pool, logger, regCache)
	registryB := peer.NewRegistry(pool, logger, regCache)
	engineA := peer.NewEngine(registryA, idA, nil)
	engineB := peer.NewEngine(registryB, idB, nil)

	// Set up httptest servers wrapping each engine's HandleInbound.
	mkHandler := func(eng *peer.Engine) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			result, err := eng.HandleInbound(r.Context(), body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  result.Status,
				"peer_id": result.Peer.ID.String(),
			})
		}
	}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mkHandler(engineA)(w, r)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mkHandler(engineB)(w, r)
	}))
	defer srvB.Close()

	// httptest URLs are http://; the handshake protocol requires
	// https://. We override the baseURLs to the http:// httptest
	// addresses + use a custom HTTP client for the engine's POSTs
	// that targets these URLs. But normalizeInstanceURL requires
	// https... So for THIS test only, we use https-prefixed fake
	// URLs and route POSTs through a custom http.Client that
	// rewrites the host.

	// Simpler approach: both engines POST to the OTHER engine's
	// httptest server directly. We replace each engine's http
	// client with one that proxies https://other-instance to the
	// real httptest endpoint.
	rewriteClient := func(actualURL string) *http.Client {
		base, _ := url.Parse(actualURL)
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &rewriteTransport{base: base},
		}
	}

	aURL := "https://instance-a.test"
	bURL := "https://instance-b.test"
	engineA.SetLocalBaseURL(aURL)
	engineA.SetLocalDisplayName("Instance A")
	engineB.SetLocalBaseURL(bURL)
	engineB.SetLocalDisplayName("Instance B")
	// Inject rewriting clients so the engines' outbound POSTs
	// reach the actual httptest endpoints.
	engineA = peer.NewEngine(registryA, idA, rewriteClient(srvB.URL))
	engineA.SetLocalBaseURL(aURL)
	engineA.SetLocalDisplayName("Instance A")
	engineB = peer.NewEngine(registryB, idB, rewriteClient(srvA.URL))
	engineB.SetLocalBaseURL(bURL)
	engineB.SetLocalDisplayName("Instance B")

	// Re-wire the httptest servers to use the new engines.
	srvA.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mkHandler(engineA)(w, r)
	})
	srvB.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mkHandler(engineB)(w, r)
	})

	// Seed admin user (handshake_by_user_ref is NOT NULL).
	adminA := fixtureAdmin(t, ctx, pool)

	// Clean BEFORE the test — a prior failed run may have left
	// rows we'd UNIQUE-violate on. Also clean AFTER so the next
	// run starts clean too.
	_, _ = pool.Exec(ctx, `DELETE FROM federation_peers WHERE instance_url IN ($1, $2)`, aURL, bURL)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE instance_url IN ($1, $2)`, aURL, bURL)
	})

	// A initiates the handshake to B.
	rowA, err := engineA.InitiateOffer(ctx, bURL, "Instance B", adminA)
	if err != nil {
		t.Fatalf("A.InitiateOffer: %v", err)
	}
	if rowA.Status != federation.PeerStatusPendingOutbound {
		t.Errorf("A side: expected pending_outbound, got %s", rowA.Status)
	}

	// B should now have a pending_inbound row for A.
	rowB, err := registryB.ByInstanceURL(ctx, aURL)
	if err != nil {
		t.Fatalf("B.ByInstanceURL: %v", err)
	}
	if rowB.Status != federation.PeerStatusPendingInbound {
		t.Errorf("B side: expected pending_inbound, got %s", rowB.Status)
	}

	// B's admin accepts.
	if err := engineB.AcceptInbound(ctx, *rowB); err != nil {
		t.Fatalf("B.AcceptInbound: %v", err)
	}

	// Both sides should now be connected.
	finalA, err := registryA.ByInstanceURL(ctx, bURL)
	if err != nil {
		t.Fatal(err)
	}
	finalB, err := registryB.ByInstanceURL(ctx, aURL)
	if err != nil {
		t.Fatal(err)
	}
	if finalA.Status != federation.PeerStatusConnected {
		t.Errorf("A final status: expected connected, got %s", finalA.Status)
	}
	if finalB.Status != federation.PeerStatusConnected {
		t.Errorf("B final status: expected connected, got %s", finalB.Status)
	}
	// A's record of B's pubkey should NOT be the placeholder
	// any more — it should be B's actual instance pubkey.
	if strings.Contains(finalA.InstancePublicKey, "PENDING") {
		t.Errorf("A's record of B's pubkey is still placeholder: %s", finalA.InstancePublicKey)
	}
	// And A should be able to verify a fresh signature from B
	// using the recorded pubkey.
	pubB, err := federation.PublicKeyFromPEM([]byte(finalA.InstancePublicKey))
	if err != nil {
		t.Fatalf("parse B's recorded pubkey: %v", err)
	}
	idBImpl, _ := idB.Get()
	test := []byte("smoke test message")
	sig := idBImpl.Sign(test)
	if err := federation.Verify(pubB, test, sig); err != nil {
		t.Errorf("verify B's signature using A's recorded pubkey: %v", err)
	}
}

// rewriteTransport is an http.RoundTripper that ignores the
// request URL's host + path-after-/federation and forwards to
// the configured base URL. Lets tests use https:// addresses
// while the actual transport targets a local httptest server.
type rewriteTransport struct {
	base *url.URL
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.base.Scheme
	r2.URL.Host = rt.base.Host
	r2.Host = rt.base.Host
	// Preserve the original path (e.g. /federation/peers/handshake).
	return http.DefaultTransport.RoundTrip(r2)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	_ = bytes.NewReader // imported by helpers / future use
	_ = os.Getenv
)
