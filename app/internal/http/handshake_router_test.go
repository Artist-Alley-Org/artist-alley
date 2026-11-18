// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1592: the peer handshake through the production router.
//
// Each test instance is the real server from New(), served by an
// httptest TLS server, so an offer or confirm reaches the other
// instance's chi router, its /api/v1 group and its strict handler, the
// same path a deployed peer takes. The embedded-build variant mounts
// the SPA fallback with the same function the embed_web build uses.
//
// What this is NOT: independent deployment evidence. Both instances
// share one test database, so they share the one instance identity
// (one system_config row) and one federation_peers table, told apart
// by URL. Key separation is covered by the peer package's engine
// tests; a two-stack, cross-version pairing is #1582's release gate.

package http

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// hsCanonicalPath is spelled out (not peer.HandshakePath) so this
// file states the contract independently of the client constant.
const hsCanonicalPath = "/api/v1/federation/peers/handshake"

// hsShell stands in for the SvelteKit index.html.
const hsShell = "<!doctype html><html><head><title>Artist Alley</title></head><body></body></html>"

// What an instance's front does with a request before (or instead
// of) the production router.
const (
	frontPass         int32 = iota // hand it to the router
	frontLoseResponse              // the router processes it, the caller gets a 502
	frontHTML                      // answer the SPA shell, 200 text/html, router never sees it
)

type hsInstance struct {
	name  string
	srv   *Server
	ts    *httptest.Server
	url   string
	front atomic.Int32

	mu   sync.Mutex
	seen []string // "METHOD /path" of every handshake-shaped request
}

func (in *hsInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/federation/peers/handshake") {
		in.mu.Lock()
		in.seen = append(in.seen, r.Method+" "+r.URL.Path)
		in.mu.Unlock()
	}
	switch in.front.Load() {
	case frontHTML:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, hsShell)
		return
	case frontLoseResponse:
		in.srv.srv.Handler.ServeHTTP(httptest.NewRecorder(), r)
		http.Error(w, "upstream response lost", http.StatusBadGateway)
		return
	}
	in.srv.srv.Handler.ServeHTTP(w, r)
}

func (in *hsInstance) handshakeRequests() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return slices.Clone(in.seen)
}

func (in *hsInstance) engine() *peer.Engine     { return in.srv.api.fedEngine }
func (in *hsInstance) registry() *peer.Registry { return in.srv.api.peers }

type hsHarness struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	logger *slog.Logger
	admin  int64
	once   sync.Once
}

func newHSHarness(t *testing.T) *hsHarness {
	t.Helper()
	// A server's pool, sized as production sizes it (#1605: New refuses
	// a pool too small to serve beside its LISTEN loops, which the
	// driver's default can be on a four-CPU runner).
	pool, _ := openServerPool(t)
	initAtrestForOutboxWiring(t)
	h := &hsHarness{t: t, ctx: context.Background(), pool: pool,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := pool.QueryRow(h.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Handshake Admin', 1) RETURNING ref`,
		"hs-admin-"+uuid.NewString()[:8]).Scan(&h.admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return h
}

// trust makes the default transport, which the handshake engine's
// client uses, trust httptest's one built-in certificate; restored
// when the test ends (no test in this package is parallel).
func (h *hsHarness) trust(ts *httptest.Server) {
	h.once.Do(func() {
		dt := http.DefaultTransport.(*http.Transport)
		prev := dt.TLSClientConfig
		roots := x509.NewCertPool()
		roots.AddCert(ts.Certificate())
		dt.TLSClientConfig = &tls.Config{RootCAs: roots}
		h.t.Cleanup(func() {
			dt.CloseIdleConnections()
			dt.TLSClientConfig = prev
		})
	})
}

// forgetURL removes every peer row for url, now and at cleanup.
func (h *hsHarness) forgetURL(url string) {
	del := func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE instance_url = $1`, url)
	}
	del()
	h.t.Cleanup(del)
}

// instance boots one production server. spa mounts the embedded
// build's not-found fallback (serveSPA over an index.html).
func (h *hsHarness) instance(name string, spa bool) *hsInstance {
	h.t.Helper()
	cfg := config.Config{StorageBackend: "fs", StorageFSRoot: h.t.TempDir(), HTTPAddr: "127.0.0.1:0"}
	s, err := New(cfg, h.logger, h.pool, "test")
	if err != nil {
		h.t.Fatalf("New(%s): %v", name, err)
	}
	h.t.Cleanup(s.lifecycleCancel)
	if spa {
		mux, ok := s.srv.Handler.(*chi.Mux)
		if !ok {
			h.t.Fatalf("production handler is %T, want *chi.Mux", s.srv.Handler)
		}
		mountSPAFallback(mux, fstest.MapFS{"index.html": {Data: []byte(hsShell)}})
	}
	in := &hsInstance{name: name, srv: s}
	in.ts = httptest.NewTLSServer(in)
	h.t.Cleanup(in.ts.Close)
	in.url = in.ts.URL
	h.trust(in.ts)
	in.engine().SetLocalBaseURL(in.url)
	in.engine().SetLocalDisplayName(name)
	h.forgetURL(in.url)
	return in
}

// fakePeer is a TLS server that is not an Artist Alley instance.
func (h *hsHarness) fakePeer(handler http.HandlerFunc) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	seen := []string{}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		handler(w, r)
	}))
	h.t.Cleanup(ts.Close)
	h.trust(ts)
	h.forgetURL(ts.URL)
	return ts, &seen
}

func htmlShell(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, hsShell)
}

func (h *hsHarness) row(in *hsInstance, url string) *peer.Peer {
	h.t.Helper()
	p, err := in.registry().ByInstanceURL(h.ctx, url)
	if err != nil {
		h.t.Fatalf("%s: record of %s: %v", in.name, url, err)
	}
	return p
}

func (h *hsHarness) rowCount(url string) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM federation_peers WHERE instance_url = $1`, url).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// assertConnected checks a completed pairing record: connected, the
// real peer key (not the placeholder) and the negotiated capability
// intersection stored.
func (h *hsHarness) assertConnected(in *hsInstance, url string) {
	h.t.Helper()
	p := h.row(in, url)
	if p.Status != federation.PeerStatusConnected {
		h.t.Errorf("%s: record of %s is %s, want connected", in.name, url, p.Status)
	}
	if strings.Contains(p.InstancePublicKey, "PENDING") {
		h.t.Errorf("%s: record of %s still has the placeholder key", in.name, url)
	} else if _, err := federation.PublicKeyFromPEM([]byte(p.InstancePublicKey)); err != nil {
		h.t.Errorf("%s: record of %s has an unparsable key: %v", in.name, url, err)
	}
	if !p.CapabilitiesNegotiatedAt.Valid {
		h.t.Errorf("%s: record of %s has no capabilities_negotiated_at", in.name, url)
	}
	if want := peer.Intersect(peer.KnownCapabilities, peer.KnownCapabilities); !slices.Equal(p.Capabilities, want) {
		h.t.Errorf("%s: record of %s capabilities = %v, want %v", in.name, url, p.Capabilities, want)
	}
}

func assertOnlyCanonical(t *testing.T, who string, got []string, n int) {
	t.Helper()
	want := slices.Repeat([]string{"POST " + hsCanonicalPath}, n)
	if !slices.Equal(got, want) {
		t.Errorf("%s received handshake requests %q, want %q", who, got, want)
	}
}

func adminCtx() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 1, Capabilities: []string{"system.admin"}})
}

// Offer, accept and confirm between two production servers, with the
// dev router (no frontend, chi 404 for unknown paths) and with the
// embedded build's SPA fallback (200 text/html for unknown paths).
func TestHandshake_ProductionRouter_Pairs(t *testing.T) {
	for _, spa := range []bool{false, true} {
		// Without the SPA mount this is the router as this build
		// mounts it: chi's 404 untagged, the placeholder-bundle
		// fallback under -tags embed_web.
		name := "dev_router"
		if hasEmbeddedFrontend() {
			name = "embed_web_placeholder_bundle"
		}
		if spa {
			name = "embedded_spa_fallback"
		}
		t.Run(name, func(t *testing.T) {
			h := newHSHarness(t)
			a := h.instance("Studio A", spa)
			b := h.instance("Studio B", spa)

			if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err != nil {
				t.Fatalf("A offer to B: %v", err)
			}
			aOnB := h.row(b, a.url)
			if aOnB.Status != federation.PeerStatusPendingInbound {
				t.Fatalf("B's record of A is %s, want pending_inbound", aOnB.Status)
			}
			if p := h.row(a, b.url); p.Status != federation.PeerStatusPendingOutbound {
				t.Fatalf("A's record of B is %s, want pending_outbound", p.Status)
			}
			if err := b.engine().AcceptInbound(h.ctx, *aOnB); err != nil {
				t.Fatalf("B accept: %v", err)
			}
			h.assertConnected(a, b.url)
			h.assertConnected(b, a.url)
			assertOnlyCanonical(t, "B (offer)", b.handshakeRequests(), 1)
			assertOnlyCanonical(t, "A (confirm)", a.handshakeRequests(), 1)

			// The canonical route wins over the fallback, and the old
			// root path is exactly the trap: with the fallback mounted it
			// answers a POST with 200 and the HTML shell.
			resp, err := b.ts.Client().Post(b.url+"/federation/peers/handshake", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			ct := resp.Header.Get("Content-Type")
			if spa {
				if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/html") || !strings.Contains(string(body), "<!doctype html>") {
					t.Errorf("root path with SPA fallback: %d %q; want 200 text/html shell", resp.StatusCode, ct)
				}
			} else if !hasEmbeddedFrontend() {
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("root path on the dev router: %d; want 404", resp.StatusCode)
				}
			} else if resp.StatusCode < 300 {
				// -tags embed_web with only the checked-in placeholder
				// bundle (no index.html): the production fallback answers
				// 500 "frontend bundle missing". Never a 2xx.
				t.Errorf("root path on the embed_web build without a bundle: %d %q; want a non-2xx", resp.StatusCode, ct)
			}
		})
	}
}

// A peer that answers 200 text/html is not a handshake endpoint: the
// offer and the confirm both fail and nothing is recorded as complete.
func TestHandshake_ProductionRouter_HTMLAnswerIsNotSuccess(t *testing.T) {
	t.Run("offer", func(t *testing.T) {
		h := newHSHarness(t)
		a := h.instance("Studio A", true)
		c, seen := h.fakePeer(htmlShell)

		_, err := a.engine().InitiateOffer(h.ctx, c.URL, "Not a peer", h.admin)
		if err == nil || !strings.Contains(err.Error(), "text/html") {
			t.Fatalf("offer to an HTML peer: err = %v, want a text/html refusal", err)
		}
		p := h.row(a, c.URL)
		if p.Status != federation.PeerStatusPendingOutbound || !strings.Contains(p.InstancePublicKey, "PENDING") {
			t.Errorf("record after a refused offer: %s / key %.20q; want pending_outbound with the placeholder", p.Status, p.InstancePublicKey)
		}
		if p.CapabilitiesNegotiatedAt.Valid {
			t.Error("a refused offer recorded negotiated capabilities")
		}
		if !strings.Contains(p.Notes, "text/html") {
			t.Errorf("notes %q do not tell the operator why", p.Notes)
		}
		assertOnlyCanonical(t, "the HTML peer", *seen, 1)
	})

	t.Run("confirm", func(t *testing.T) {
		h := newHSHarness(t)
		b := h.instance("Studio B", true)
		c, _ := h.fakePeer(htmlShell)

		// C's offer reaches B's real router: C is a peer engine whose
		// own client is pinned to the canonical path, because C is the
		// fixture here, not the client under test.
		idMgr := identity.NewManager(h.pool, h.logger)
		if _, err := idMgr.Load(h.ctx); err != nil {
			t.Fatal(err)
		}
		cEngine := peer.NewEngine(peer.NewRegistry(h.pool, h.logger, nil), idMgr,
			&http.Client{Transport: canonicalPathTransport{}})
		cEngine.SetLocalBaseURL(c.URL)
		cEngine.SetLocalDisplayName("Studio C")
		h.forgetURL(b.url)
		if _, err := cEngine.InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err != nil {
			t.Fatalf("C offer to B: %v", err)
		}
		cOnB := h.row(b, c.URL)

		err := b.engine().AcceptInbound(h.ctx, *cOnB)
		if err == nil || !strings.Contains(err.Error(), "text/html") {
			t.Fatalf("accept with an HTML confirm answer: err = %v, want a text/html refusal", err)
		}
		if p := h.row(b, c.URL); p.Status != federation.PeerStatusPendingInbound {
			t.Errorf("B's record of C after a refused confirm is %s, want pending_inbound (retryable)", p.Status)
		}

		// The admin endpoint reports the failure; it never answers 200
		// with a connected row.
		resp, err := b.srv.api.AcceptFederationPeer(adminCtx(), openapi.AcceptFederationPeerRequestObject{Id: openapi_types.UUID(cOnB.ID)})
		if err != nil {
			t.Fatal(err)
		}
		bad, ok := resp.(openapi.AcceptFederationPeer400JSONResponse)
		if !ok {
			t.Fatalf("admin accept answered %T, want 400", resp)
		}
		if !strings.Contains(bad.Error, "text/html") {
			t.Errorf("admin accept error %q does not say why", bad.Error)
		}
		if p := h.row(b, c.URL); p.Status != federation.PeerStatusPendingInbound {
			t.Errorf("after the admin endpoint: %s, want pending_inbound", p.Status)
		}
	})
}

// canonicalPathTransport sends every request to the canonical
// handshake path on its own host.
type canonicalPathTransport struct{}

func (canonicalPathTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Path = hsCanonicalPath
	return http.DefaultTransport.RoundTrip(r)
}

// Retries converge: a lost offer response, a lost confirm response, a
// failed offer re-sent once the peer is fixed, and two offers crossing.
func TestHandshake_ProductionRouter_RetriesConverge(t *testing.T) {
	t.Run("offer_response_lost", func(t *testing.T) {
		h := newHSHarness(t)
		a := h.instance("Studio A", true)
		b := h.instance("Studio B", true)

		b.front.Store(frontLoseResponse)
		if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err == nil {
			t.Fatal("offer with a lost response reported success")
		}
		if p := h.row(b, a.url); p.Status != federation.PeerStatusPendingInbound {
			t.Fatalf("B did not record the offer whose response was lost (%s)", p.Status)
		}
		first := h.row(a, b.url)

		b.front.Store(frontPass)
		again, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin)
		if err != nil {
			t.Fatalf("re-sent offer: %v", err)
		}
		if again.ID != first.ID || h.rowCount(b.url) != 1 || h.rowCount(a.url) != 1 {
			t.Errorf("re-send made a new row (A ids %s/%s, rows %d/%d)", first.ID, again.ID, h.rowCount(b.url), h.rowCount(a.url))
		}
		assertOnlyCanonical(t, "B", b.handshakeRequests(), 2)

		if err := b.engine().AcceptInbound(h.ctx, *h.row(b, a.url)); err != nil {
			t.Fatalf("accept: %v", err)
		}
		h.assertConnected(a, b.url)
		h.assertConnected(b, a.url)
	})

	t.Run("failed_offer_resent_after_peer_fixed", func(t *testing.T) {
		h := newHSHarness(t)
		a := h.instance("Studio A", true)
		b := h.instance("Studio B", true)

		b.front.Store(frontHTML)
		if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err == nil {
			t.Fatal("offer answered with HTML reported success")
		}
		b.front.Store(frontPass)
		if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err != nil {
			t.Fatalf("re-sent offer: %v", err)
		}
		if p := h.row(b, a.url); p.Status != federation.PeerStatusPendingInbound {
			t.Errorf("B's record of A after the re-send is %s", p.Status)
		}
		if h.rowCount(b.url) != 1 {
			t.Errorf("A has %d rows for B", h.rowCount(b.url))
		}
		if notes := h.row(a, b.url).Notes; !strings.Contains(notes, "text/html") {
			t.Errorf("A's notes lost the earlier failure: %q", notes)
		}
	})

	t.Run("confirm_response_lost", func(t *testing.T) {
		h := newHSHarness(t)
		a := h.instance("Studio A", true)
		b := h.instance("Studio B", true)

		if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err != nil {
			t.Fatalf("offer: %v", err)
		}
		a.front.Store(frontLoseResponse)
		if err := b.engine().AcceptInbound(h.ctx, *h.row(b, a.url)); err == nil {
			t.Fatal("accept with a lost confirm response reported success")
		}
		if p := h.row(b, a.url); p.Status != federation.PeerStatusPendingInbound {
			t.Fatalf("B's record of A after a lost confirm response is %s, want pending_inbound", p.Status)
		}
		a.front.Store(frontPass)
		if err := b.engine().AcceptInbound(h.ctx, *h.row(b, a.url)); err != nil {
			t.Fatalf("retried accept: %v", err)
		}
		h.assertConnected(a, b.url)
		h.assertConnected(b, a.url)
	})

	t.Run("cross_handshake", func(t *testing.T) {
		h := newHSHarness(t)
		a := h.instance("Studio A", true)
		b := h.instance("Studio B", true)

		// A's offer never lands, so both sides end up having offered.
		b.front.Store(frontHTML)
		if _, err := a.engine().InitiateOffer(h.ctx, b.url, "Studio B", h.admin); err == nil {
			t.Fatal("offer answered with HTML reported success")
		}
		b.front.Store(frontPass)
		if _, err := b.engine().InitiateOffer(h.ctx, a.url, "Studio A", h.admin); err != nil {
			t.Fatalf("B's crossing offer: %v", err)
		}
		h.assertConnected(a, b.url)
		h.assertConnected(b, a.url)
	})
}

// A redirect is refused, not followed: following it would re-POST the
// envelope somewhere else and hide a wrong route.
func TestHandshake_ProductionRouter_RedirectIsRefused(t *testing.T) {
	h := newHSHarness(t)
	a := h.instance("Studio A", true)
	b := h.instance("Studio B", true)
	r, _ := h.fakePeer(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, b.url+hsCanonicalPath, http.StatusTemporaryRedirect)
	})

	_, err := a.engine().InitiateOffer(h.ctx, r.URL, "Redirector", h.admin)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("offer to a redirecting peer: err = %v, want a redirect refusal", err)
	}
	if got := b.handshakeRequests(); len(got) != 0 {
		t.Errorf("the redirect was followed to B: %q", got)
	}
}

// One canonical path: the client constant, the OpenAPI servers base
// plus the operation path, and this file's literal all agree.
func TestHandshake_PathAgreesWithOpenAPI(t *testing.T) {
	spec, err := openapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].URL != "/api/v1" {
		t.Fatalf("OpenAPI servers = %v, want the single /api/v1 base", spec.Servers)
	}
	item := spec.Paths.Find("/federation/peers/handshake")
	if item == nil || item.Post == nil {
		t.Fatal("OpenAPI has no POST /federation/peers/handshake")
	}
	// The embedded spec carries codegen's capitalised operation id.
	if !strings.EqualFold(item.Post.OperationID, "postFederationHandshake") {
		t.Fatalf("POST /federation/peers/handshake is %q, want postFederationHandshake", item.Post.OperationID)
	}
	fromSpec := spec.Servers[0].URL + "/federation/peers/handshake"
	for name, p := range map[string]string{"peer.HandshakePath": peer.HandshakePath, "this file": hsCanonicalPath} {
		if p != fromSpec {
			t.Errorf("%s = %q, OpenAPI = %q; want one path", name, p, fromSpec)
		}
	}
}
