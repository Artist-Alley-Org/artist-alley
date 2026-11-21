// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1592: what counts as a successful handshake answer, and how
// repeated or crossing envelopes converge. The engine talks to a fake
// HTTP doer here, so each answer is exactly the bytes a case names;
// the same flows over the production router are in
// internal/http/handshake_router_test.go.

package peer_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

const canonicalHandshakePath = "/api/v1/federation/peers/handshake"

// fakeDoer answers every handshake POST with respond(envelope type).
type fakeDoer struct {
	mu      sync.Mutex
	paths   []string
	urls    []string
	types   []string
	respond func(typ string) *http.Response
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Envelope peer.Envelope `json:"envelope"`
	}
	_ = json.Unmarshal(body, &wire)
	f.mu.Lock()
	f.paths = append(f.paths, req.Method+" "+req.URL.Path)
	f.urls = append(f.urls, req.URL.String())
	f.types = append(f.types, wire.Envelope.Type)
	f.mu.Unlock()
	resp := f.respond(wire.Envelope.Type)
	resp.Request = req
	return resp, nil
}

// countingBody counts how many bytes the client read.
type countingBody struct {
	r io.Reader
	n *int64
}

func (c countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}
func (countingBody) Close() error { return nil }

func answer(status int, ctype, body string) *http.Response {
	h := http.Header{}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func ack(status string) string {
	return fmt.Sprintf(`{"status":%q,"peer_id":%q}`, status, uuid.NewString())
}

// contractEngine is an engine on the shared test DB whose handshake
// POSTs go to doer.
func contractEngine(t *testing.T, doer peer.HTTPDoer) (*hsFixture, *peer.Engine) {
	t.Helper()
	fx := newHSFixture(t, "https://contract-local.example")
	eng := peer.NewEngine(fx.registry, fx.idMgr, doer)
	eng.SetLocalBaseURL(fx.baseURL)
	eng.SetLocalDisplayName("Contract Local")
	return fx, eng
}

func peerURL(t *testing.T, fx *hsFixture) string {
	t.Helper()
	u := "https://contract-" + randHex(t, 4) + ".example"
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE instance_url = $1`, u)
	})
	return u
}

// signedFrom builds a signed handshake envelope from `from` (key
// priv) to `to`, advertising caps.
func signedFrom(t *testing.T, priv ed25519.PrivateKey, typ, from, to string, caps peer.CapabilitySet) []byte {
	t.Helper()
	pemBytes, err := federation.PublicKeyToPEM(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	env := peer.Envelope{
		Protocol: peer.HandshakeProtocol, Type: typ,
		From: from, FromDisplayName: "Remote " + from, FromPublicKeyPEM: string(pemBytes),
		To: to, Nonce: randNonce(t), Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		SupportedCapabilities: caps,
	}
	canonical, err := federation.Canonicalize(mustJSON(env))
	if err != nil {
		t.Fatal(err)
	}
	return mustJSON(map[string]any{
		"envelope":  env,
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical)),
	})
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func keyPEM(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	b, err := federation.PublicKeyToPEM(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNotCompleted: the row is still in want, with no key or
// capability record of a pairing that did not happen.
func assertNotCompleted(t *testing.T, fx *hsFixture, url string, want federation.PeerStatus) *peer.Peer {
	t.Helper()
	p, err := fx.registry.ByInstanceURL(context.Background(), url)
	if err != nil {
		t.Fatalf("record of %s: %v", url, err)
	}
	if p.Status != want {
		t.Errorf("record of %s is %s, want %s", url, p.Status, want)
	}
	if want == federation.PeerStatusPendingOutbound {
		if !strings.Contains(p.InstancePublicKey, "PENDING") {
			t.Errorf("record of %s lost its placeholder key", url)
		}
		if p.CapabilitiesNegotiatedAt.Valid {
			t.Errorf("record of %s has negotiated capabilities", url)
		}
	}
	return p
}

type answerCase struct {
	name    string
	respond func() *http.Response
	wantErr string // "" = success
}

func offerCases() []answerCase {
	return []answerCase{
		{"valid_accepted_pending", func() *http.Response {
			return answer(200, "application/json; charset=utf-8", ack("accepted_pending"))
		}, ""},
		{"html_200", func() *http.Response {
			return answer(200, "text/html; charset=utf-8", "<!doctype html><title>Artist Alley</title>")
		}, "text/html"},
		{"no_content_type", func() *http.Response { return answer(200, "", ack("accepted_pending")) }, "Content-Type"},
		{"empty_body", func() *http.Response { return answer(200, "application/json", "") }, "malformed"},
		{"malformed_json", func() *http.Response { return answer(200, "application/json", "{") }, "malformed"},
		{"trailing_json", func() *http.Response {
			return answer(200, "application/json", ack("accepted_pending")+ack("accepted_pending"))
		}, "trailing"},
		{"json_array", func() *http.Response { return answer(200, "application/json", "[]") }, "malformed"},
		{"missing_status", func() *http.Response {
			return answer(200, "application/json", fmt.Sprintf(`{"peer_id":%q}`, uuid.NewString()))
		}, "status"},
		{"missing_peer_id", func() *http.Response {
			return answer(200, "application/json", `{"status":"accepted_pending"}`)
		}, "peer_id"},
		{"nil_peer_id", func() *http.Response {
			return answer(200, "application/json", fmt.Sprintf(`{"status":"accepted_pending","peer_id":%q}`, uuid.Nil.String()))
		}, "peer_id"},
		{"bogus_peer_id", func() *http.Response {
			return answer(200, "application/json", `{"status":"accepted_pending","peer_id":"not-a-uuid"}`)
		}, "peer_id"},
		{"bogus_status", func() *http.Response { return answer(200, "application/json", ack("all_good")) }, "all_good"},
		{"ignored_duplicate", func() *http.Response {
			return answer(200, "application/json", ack("ignored_duplicate"))
		}, "ignored_duplicate"},
		{"completed_but_not_here", func() *http.Response {
			return answer(200, "application/json", ack("completed"))
		}, "completed"},
		{"created_201", func() *http.Response {
			return answer(201, "application/json", ack("accepted_pending"))
		}, "HTTP 201"},
		{"no_content_204", func() *http.Response { return answer(204, "", "") }, "HTTP 204"},
		{"redirect_302", func() *http.Response {
			r := answer(302, "text/html", "")
			r.Header.Set("Location", "https://elsewhere.example/api/v1/federation/peers/handshake")
			return r
		}, "redirect"},
		{"refused_400", func() *http.Response {
			return answer(400, "application/json", `{"error":"handshake: addressed to a different instance"}`)
		}, "different instance"},
	}
}

func TestHandshakeAnswer_Offer(t *testing.T) {
	for _, c := range offerCases() {
		t.Run(c.name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(string) *http.Response { return c.respond() }}
			fx, eng := contractEngine(t, doer)
			url := peerURL(t, fx)
			row, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("valid answer refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
			if row == nil {
				t.Fatal("no row returned")
			}
			p := assertNotCompleted(t, fx, url, federation.PeerStatusPendingOutbound)
			if c.wantErr != "" && !strings.Contains(p.Notes, c.wantErr) {
				t.Errorf("notes %q do not carry the reason %q", p.Notes, c.wantErr)
			}
			if len(doer.paths) != 1 || doer.paths[0] != "POST "+canonicalHandshakePath {
				t.Errorf("requests %q, want one POST %s", doer.paths, canonicalHandshakePath)
			}
		})
	}
}

func TestHandshakeAnswer_Confirm(t *testing.T) {
	cases := []answerCase{
		{"valid_completed", func() *http.Response { return answer(200, "application/json", ack("completed")) }, ""},
		{"html_200", func() *http.Response { return answer(200, "text/html", "<!doctype html>") }, "text/html"},
		{"accepted_pending", func() *http.Response {
			return answer(200, "application/json", ack("accepted_pending"))
		}, "accepted_pending"},
		{"ignored_duplicate", func() *http.Response {
			return answer(200, "application/json", ack("ignored_duplicate"))
		}, "ignored_duplicate"},
		{"missing_peer_id", func() *http.Response {
			return answer(200, "application/json", `{"status":"completed"}`)
		}, "peer_id"},
		{"accepted_202", func() *http.Response { return answer(202, "application/json", ack("completed")) }, "HTTP 202"},
		{"refused_400", func() *http.Response {
			return answer(400, "application/json", `{"error":"handshake: no pending outbound for this peer"}`)
		}, "no pending outbound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(string) *http.Response { return c.respond() }}
			fx, eng := contractEngine(t, doer)
			url := peerURL(t, fx)
			res, err := eng.HandleInbound(context.Background(),
				signedFrom(t, newKey(t), peer.HandshakeTypeOffer, url, fx.baseURL, peer.KnownCapabilities))
			if err != nil || res.Status != "accepted_pending" {
				t.Fatalf("seed offer: %v / %+v", err, res)
			}
			err = eng.AcceptInbound(context.Background(), *res.Peer)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("valid answer refused: %v", err)
				}
				assertNotCompleted(t, fx, url, federation.PeerStatusConnected)
			} else {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
				}
				p := assertNotCompleted(t, fx, url, federation.PeerStatusPendingInbound)
				if !strings.Contains(p.Notes, c.wantErr) {
					t.Errorf("notes %q do not carry the reason", p.Notes)
				}
			}
			if len(doer.paths) != 1 || doer.paths[0] != "POST "+canonicalHandshakePath {
				t.Errorf("requests %q, want one POST %s", doer.paths, canonicalHandshakePath)
			}
		})
	}
}

// The answer is read through a small bound, never to the end.
func TestHandshakeAnswer_OversizeIsBounded(t *testing.T) {
	var read int64
	big := `{"status":"accepted_pending","peer_id":"` + uuid.NewString() + `","pad":"` + strings.Repeat("x", 1<<20) + `"}`
	doer := &fakeDoer{respond: func(string) *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: countingBody{r: strings.NewReader(big), n: &read}}
	}}
	fx, eng := contractEngine(t, doer)
	url := peerURL(t, fx)
	_, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool))
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want an oversize refusal", err)
	}
	if read > 4097 {
		t.Errorf("client read %d bytes of the answer, want at most 4097", read)
	}
	assertNotCompleted(t, fx, url, federation.PeerStatusPendingOutbound)
}

// A refusal's body reaches the operator shortened and without
// control characters.
func TestHandshakeAnswer_ErrorTextIsSanitised(t *testing.T) {
	body := "\x1b[31mboom\x00\r\nInjected: header" + strings.Repeat("y", 4000)
	doer := &fakeDoer{respond: func(string) *http.Response { return answer(502, "text/plain", body) }}
	fx, eng := contractEngine(t, doer)
	url := peerURL(t, fx)
	_, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want HTTP 502 with the body's start", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x00\r\n") {
		t.Errorf("error text carries control characters: %q", err.Error())
	}
	if len(err.Error()) > 600 {
		t.Errorf("error text is %d bytes, want it bounded", len(err.Error()))
	}
}

// A failed offer is re-sent on the same pending_outbound row with a
// new nonce, and no second row appears.
func TestInitiateOffer_ResendsForPendingOutbound(t *testing.T) {
	fail := true
	doer := &fakeDoer{respond: func(string) *http.Response {
		if fail {
			return answer(200, "text/html", "<!doctype html>")
		}
		return answer(200, "application/json", ack("accepted_pending"))
	}}
	fx, eng := contractEngine(t, doer)
	url := peerURL(t, fx)
	admin := fixtureAdmin(t, context.Background(), fx.pool)
	first, err := eng.InitiateOffer(context.Background(), url, "", admin)
	if err == nil {
		t.Fatal("HTML answer accepted")
	}
	fail = false
	second, err := eng.InitiateOffer(context.Background(), url, "", admin)
	if err != nil {
		t.Fatalf("re-send: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-send made a new row %s (was %s)", second.ID, first.ID)
	}
	if len(doer.paths) != 2 {
		t.Errorf("POSTs = %d, want 2 (the re-send must go out)", len(doer.paths))
	}
	var n int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_peers WHERE instance_url=$1`, url).Scan(&n)
	if n != 1 {
		t.Errorf("rows for %s = %d, want 1", url, n)
	}
	p := assertNotCompleted(t, fx, url, federation.PeerStatusPendingOutbound)
	if !strings.Contains(p.Notes, "text/html") {
		t.Errorf("the earlier failure is gone from the notes: %q", p.Notes)
	}
}

// A connected row is not re-offered by Pair by URL (InitiateOffer);
// only the explicit Renegotiate action re-offers it (§16.2).
func TestInitiateOffer_ConnectedRowIsNotReoffered(t *testing.T) {
	doer := &fakeDoer{respond: func(typ string) *http.Response {
		return answer(200, "application/json", ack("accepted_pending"))
	}}
	fx, eng := contractEngine(t, doer)
	url := peerURL(t, fx)
	admin := fixtureAdmin(t, context.Background(), fx.pool)
	if _, err := eng.InitiateOffer(context.Background(), url, "", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.HandleInbound(context.Background(),
		signedFrom(t, newKey(t), peer.HandshakeTypeConfirm, url, fx.baseURL, peer.KnownCapabilities)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := eng.InitiateOffer(context.Background(), url, "", admin); err != nil {
		t.Fatal(err)
	}
	if len(doer.paths) != 1 {
		t.Errorf("POSTs = %d, want 1 (Pair by URL does not re-offer a connected row)", len(doer.paths))
	}

	// Renegotiate does send one offer. This peer answers accepted_pending
	// (it has no connected record of us), so nothing changes here.
	before := mustPeer(t, fx, url)
	res, err := eng.Renegotiate(context.Background(), before.ID)
	if !errors.Is(err, peer.ErrRenegotiateNotCompleted) {
		t.Fatalf("Renegotiate against accepted_pending: err = %v, want ErrRenegotiateNotCompleted", err)
	}
	if len(doer.paths) != 2 || doer.types[1] != peer.HandshakeTypeOffer || doer.urls[1] != url+canonicalHandshakePath {
		t.Fatalf("Renegotiate sent %v to %v, want one offer to %s", doer.types, doer.urls, url+canonicalHandshakePath)
	}
	assertUnchanged(t, fx, url, before)
	if res == nil || !slices.Equal(res.Previous, before.Capabilities) || !slices.Equal(res.Current, before.Capabilities) {
		t.Errorf("result %+v, want the unchanged set %v", res, before.Capabilities)
	}
}

// connectedPeer pairs url with this engine (Pair by URL, then the
// peer's confirm signed by key advertising caps) and returns the row.
func connectedPeer(t *testing.T, fx *hsFixture, eng *peer.Engine, url string, key ed25519.PrivateKey, caps peer.CapabilitySet) *peer.Peer {
	t.Helper()
	if _, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool)); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.HandleInbound(context.Background(),
		signedFrom(t, key, peer.HandshakeTypeConfirm, url, fx.baseURL, caps)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	p := mustPeer(t, fx, url)
	if p.Status != federation.PeerStatusConnected || !slices.Equal(p.Capabilities, peer.Intersect(peer.KnownCapabilities, caps)) {
		t.Fatalf("fixture: %s with %v", p.Status, p.Capabilities)
	}
	return p
}

func mustPeer(t *testing.T, fx *hsFixture, url string) *peer.Peer {
	t.Helper()
	p, err := fx.registry.ByInstanceURL(context.Background(), url)
	if err != nil {
		t.Fatalf("record of %s: %v", url, err)
	}
	return p
}

// assertUnchanged: the row still has before's identity, key, status,
// enablement and stored intersection, with the same negotiation time,
// and it is the only row for the URL.
func assertUnchanged(t *testing.T, fx *hsFixture, url string, before *peer.Peer) {
	t.Helper()
	p, err := fx.registry.ByID(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != before.ID || p.InstancePublicKey != before.InstancePublicKey || p.Status != before.Status || p.Enabled != before.Enabled {
		t.Errorf("row changed: %s %s enabled=%v", p.ID, p.Status, p.Enabled)
	}
	if !slices.Equal(p.Capabilities, before.Capabilities) || !p.CapabilitiesNegotiatedAt.Time.Equal(before.CapabilitiesNegotiatedAt.Time) {
		t.Errorf("stored intersection changed: %v at %v, was %v at %v",
			p.Capabilities, p.CapabilitiesNegotiatedAt.Time, before.Capabilities, before.CapabilitiesNegotiatedAt.Time)
	}
	var n int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_peers WHERE instance_url=$1`, url).Scan(&n)
	if n != 1 {
		t.Errorf("rows for %s = %d, want 1", url, n)
	}
}

// Renegotiate of a connected peer that confirms back: the new
// intersection is stored on the same row, and a repeat converges.
func TestRenegotiate_StoresTheNewIntersectionOnTheSameRow(t *testing.T) {
	key := newKey(t)
	var eng *peer.Engine
	var fx *hsFixture
	var url string
	advertised := peer.KnownCapabilities
	doer := &fakeDoer{respond: func(typ string) *http.Response {
		if typ != peer.HandshakeTypeOffer {
			return answer(200, "application/json", ack("accepted_pending"))
		}
		// The peer confirms back first, as handleOffer does.
		if _, err := eng.HandleInbound(context.Background(),
			signedFrom(t, key, peer.HandshakeTypeConfirm, url, fx.baseURL, advertised)); err != nil {
			return answer(500, "application/json", `{"error":"confirm refused"}`)
		}
		return answer(200, "application/json", ack("completed"))
	}}
	fx, eng = contractEngine(t, doer)
	url = peerURL(t, fx)
	old := peer.CapabilitySet{peer.CapE2EEncrypted}
	advertised = old
	before := connectedPeer(t, fx, eng, url, key, old)
	advertised = peer.KnownCapabilities

	res, err := eng.Renegotiate(context.Background(), before.ID)
	if err != nil {
		t.Fatalf("Renegotiate: %v", err)
	}
	want := peer.Intersect(peer.KnownCapabilities, peer.KnownCapabilities)
	if !slices.Equal(res.Previous, old) || !slices.Equal(res.Current, want) {
		t.Fatalf("Previous %v Current %v, want %v then %v", res.Previous, res.Current, old, want)
	}
	after := mustPeer(t, fx, url)
	if after.ID != before.ID || after.InstancePublicKey != before.InstancePublicKey || after.Status != federation.PeerStatusConnected {
		t.Fatalf("the row changed identity, key or status")
	}
	if !after.CapabilitiesNegotiatedAt.Time.After(before.CapabilitiesNegotiatedAt.Time) || !slices.Equal(after.Capabilities, want) {
		t.Fatalf("stored %v at %v", after.Capabilities, after.CapabilitiesNegotiatedAt.Time)
	}
	res2, err := eng.Renegotiate(context.Background(), before.ID)
	if err != nil || !slices.Equal(res2.Previous, want) || !slices.Equal(res2.Current, want) {
		t.Fatalf("repeat: %v, %+v", err, res2)
	}
	var n int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_peers WHERE instance_url=$1`, url).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// Only a connected, enabled row is renegotiated; nothing is sent
// otherwise.
func TestRenegotiate_RefusesWithoutSending(t *testing.T) {
	doer := &fakeDoer{respond: func(string) *http.Response {
		return answer(200, "application/json", ack("accepted_pending"))
	}}
	fx, eng := contractEngine(t, doer)
	ctx := context.Background()

	pendingURL := peerURL(t, fx)
	pending, err := eng.InitiateOffer(ctx, pendingURL, "", fixtureAdmin(t, ctx, fx.pool))
	if err != nil {
		t.Fatal(err)
	}
	sent := len(doer.paths)
	if _, err := eng.Renegotiate(ctx, pending.ID); !errors.Is(err, peer.ErrRenegotiateState) {
		t.Errorf("pending_outbound: err = %v, want ErrRenegotiateState", err)
	}

	url := peerURL(t, fx)
	p := connectedPeer(t, fx, eng, url, newKey(t), peer.KnownCapabilities)
	off := false
	if _, err := fx.registry.Update(ctx, p.ID, peer.UpdateInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	disabled := mustPeer(t, fx, url)
	sent = len(doer.paths)
	if _, err := eng.Renegotiate(ctx, p.ID); !errors.Is(err, peer.ErrRenegotiateState) {
		t.Errorf("disabled: err = %v, want ErrRenegotiateState", err)
	}
	if len(doer.paths) != sent {
		t.Errorf("a refused renegotiation sent %d POSTs", len(doer.paths)-sent)
	}
	assertUnchanged(t, fx, url, disabled)

	if _, err := eng.Renegotiate(ctx, uuid.New()); !errors.Is(err, peer.ErrPeerNotFound) {
		t.Errorf("unknown peer: err = %v, want ErrPeerNotFound", err)
	}
}

type errDoer struct{ n int }

func (d *errDoer) Do(*http.Request) (*http.Response, error) {
	d.n++
	return nil, errors.New("dial tcp: connection refused")
}

// A renegotiation the peer does not complete changes nothing here: a
// transport failure, a refusal, a completed answer without its confirm,
// and a confirm signed with another key.
func TestRenegotiate_FailureChangesNothing(t *testing.T) {
	ctx := context.Background()
	t.Run("transport failure", func(t *testing.T) {
		pair := &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("accepted_pending")) }}
		fx, eng := contractEngine(t, pair)
		url := peerURL(t, fx)
		before := connectedPeer(t, fx, eng, url, newKey(t), peer.CapabilitySet{peer.CapE2EEncrypted})
		down := &errDoer{}
		eng2 := peer.NewEngine(fx.registry, fx.idMgr, down)
		eng2.SetLocalBaseURL(fx.baseURL)
		if _, err := eng2.Renegotiate(ctx, before.ID); err == nil || errors.Is(err, peer.ErrRenegotiateState) {
			t.Fatalf("err = %v, want a transport failure", err)
		}
		if down.n != 1 {
			t.Fatalf("offers attempted = %d, want 1", down.n)
		}
		assertUnchanged(t, fx, url, before)
		if p := mustPeer(t, fx, url); !strings.Contains(p.Notes, "renegotiation failed") {
			t.Errorf("the failure is not in the notes: %q", p.Notes)
		}
	})
	for _, c := range []struct {
		name    string
		respond func(eng *peer.Engine, fx *hsFixture, url string) *http.Response
	}{
		{"refused", func(*peer.Engine, *hsFixture, string) *http.Response {
			return answer(400, "application/json", `{"error":"handshake: peer key differs from the key on record"}`)
		}},
		{"completed without the confirm", func(*peer.Engine, *hsFixture, string) *http.Response {
			return answer(200, "application/json", ack("completed"))
		}},
		{"confirm signed with another key", func(eng *peer.Engine, fx *hsFixture, url string) *http.Response {
			_, err := eng.HandleInbound(context.Background(),
				signedFrom(t, newKey(t), peer.HandshakeTypeConfirm, url, fx.baseURL, peer.KnownCapabilities))
			if !errors.Is(err, peer.ErrHandshakeKeyConflict) {
				t.Errorf("a confirm with another key: err = %v, want ErrHandshakeKeyConflict", err)
			}
			return answer(400, "application/json", `{"error":"could not confirm back"}`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var eng *peer.Engine
			var fx *hsFixture
			var url string
			renegotiating := false
			doer := &fakeDoer{respond: func(typ string) *http.Response {
				if !renegotiating {
					return answer(200, "application/json", ack("accepted_pending"))
				}
				return c.respond(eng, fx, url)
			}}
			fx, eng = contractEngine(t, doer)
			url = peerURL(t, fx)
			before := connectedPeer(t, fx, eng, url, newKey(t), peer.CapabilitySet{peer.CapE2EEncrypted})
			renegotiating = true
			res, err := eng.Renegotiate(ctx, before.ID)
			if !errors.Is(err, peer.ErrRenegotiateNotCompleted) && !errors.Is(err, peer.ErrHandshakeAnswer) {
				t.Fatalf("err = %v, want a failed renegotiation", err)
			}
			assertUnchanged(t, fx, url, before)
			if res == nil || !slices.Equal(res.Current, before.Capabilities) {
				t.Errorf("result %+v, want the unchanged set", res)
			}
		})
	}
}

// An offer from a connected peer signed with another key is refused
// before anything is sent or stored (the receiving side of a
// renegotiation, §16.2).
func TestHandleInbound_ReofferWithAnotherKeyIsRefused(t *testing.T) {
	doer := &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("accepted_pending")) }}
	fx, eng := contractEngine(t, doer)
	url := peerURL(t, fx)
	before := connectedPeer(t, fx, eng, url, newKey(t), peer.KnownCapabilities)
	sent := len(doer.paths)
	_, err := eng.HandleInbound(context.Background(),
		signedFrom(t, newKey(t), peer.HandshakeTypeOffer, url, fx.baseURL, peer.CapabilitySet{}))
	if !errors.Is(err, peer.ErrHandshakeKeyConflict) {
		t.Fatalf("err = %v, want ErrHandshakeKeyConflict", err)
	}
	if len(doer.paths) != sent {
		t.Errorf("a refused re-offer sent %d confirms back", len(doer.paths)-sent)
	}
	assertUnchanged(t, fx, url, before)
}

// --- receiving side: repeated and crossing envelopes -------------------

func TestHandleInbound_RepeatedOffer(t *testing.T) {
	fx, eng := contractEngine(t, &fakeDoer{respond: func(string) *http.Response {
		return answer(200, "application/json", ack("completed"))
	}})
	url := peerURL(t, fx)
	key := newKey(t)
	for i := 0; i < 2; i++ {
		res, err := eng.HandleInbound(context.Background(),
			signedFrom(t, key, peer.HandshakeTypeOffer, url, fx.baseURL, peer.KnownCapabilities))
		if err != nil {
			t.Fatalf("offer %d: %v", i+1, err)
		}
		if res.Status != "accepted_pending" {
			t.Errorf("offer %d answered %q, want accepted_pending", i+1, res.Status)
		}
	}
	var n int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_peers WHERE instance_url=$1`, url).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}

	// The same URL with a different key is refused, not stored.
	_, err := eng.HandleInbound(context.Background(),
		signedFrom(t, newKey(t), peer.HandshakeTypeOffer, url, fx.baseURL, peer.KnownCapabilities))
	if !errors.Is(err, peer.ErrHandshakeKeyConflict) {
		t.Fatalf("offer with a different key: err = %v, want ErrHandshakeKeyConflict", err)
	}
	p, _ := fx.registry.ByInstanceURL(context.Background(), url)
	if p.InstancePublicKey != keyPEM(t, key) || p.Status != federation.PeerStatusPendingInbound {
		t.Errorf("a conflicting offer changed the record (%s)", p.Status)
	}
}

func TestHandleInbound_RepeatedConfirm(t *testing.T) {
	fx, eng := contractEngine(t, &fakeDoer{respond: func(string) *http.Response {
		return answer(200, "application/json", ack("accepted_pending"))
	}})
	url := peerURL(t, fx)
	if _, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool)); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	for i := 0; i < 2; i++ {
		res, err := eng.HandleInbound(context.Background(),
			signedFrom(t, key, peer.HandshakeTypeConfirm, url, fx.baseURL, peer.KnownCapabilities))
		if err != nil {
			t.Fatalf("confirm %d: %v", i+1, err)
		}
		if res.Status != "completed" {
			t.Errorf("confirm %d answered %q, want completed", i+1, res.Status)
		}
	}
	p, _ := fx.registry.ByInstanceURL(context.Background(), url)
	if p.Status != federation.PeerStatusConnected || p.InstancePublicKey != keyPEM(t, key) {
		t.Fatalf("after the confirms: %s", p.Status)
	}

	_, err := eng.HandleInbound(context.Background(),
		signedFrom(t, newKey(t), peer.HandshakeTypeConfirm, url, fx.baseURL, peer.KnownCapabilities))
	if !errors.Is(err, peer.ErrHandshakeKeyConflict) {
		t.Fatalf("confirm with a different key: err = %v, want ErrHandshakeKeyConflict", err)
	}
	if p2, _ := fx.registry.ByInstanceURL(context.Background(), url); p2.InstancePublicKey != keyPEM(t, key) {
		t.Error("a conflicting confirm replaced the stored key")
	}
}

// Both sides offered: the receiver confirms back to the offerer, and
// only after that confirm is accepted does it store the offered key
// and go connected.
func TestHandleInbound_CrossingOffers(t *testing.T) {
	t.Run("confirm_back_accepted", func(t *testing.T) {
		doer := &fakeDoer{respond: func(typ string) *http.Response {
			if typ == peer.HandshakeTypeOffer {
				return answer(200, "text/html", "<!doctype html>") // our own offer never lands
			}
			return answer(200, "application/json", ack("completed"))
		}}
		fx, eng := contractEngine(t, doer)
		url := peerURL(t, fx)
		if _, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool)); err == nil {
			t.Fatal("HTML answer accepted")
		}
		key := newKey(t)
		res, err := eng.HandleInbound(context.Background(),
			signedFrom(t, key, peer.HandshakeTypeOffer, url, fx.baseURL, peer.KnownCapabilities))
		if err != nil {
			t.Fatalf("crossing offer: %v", err)
		}
		if res.Status != "completed" {
			t.Errorf("crossing offer answered %q, want completed", res.Status)
		}
		p, _ := fx.registry.ByInstanceURL(context.Background(), url)
		if p.Status != federation.PeerStatusConnected || p.InstancePublicKey != keyPEM(t, key) {
			t.Errorf("after the crossing offer: %s, key placeholder=%v; want connected with the offered key",
				p.Status, strings.Contains(p.InstancePublicKey, "PENDING"))
		}
		if !p.CapabilitiesNegotiatedAt.Valid {
			t.Error("no negotiated capabilities")
		}
		if got := doer.types; len(got) != 2 || got[1] != peer.HandshakeTypeConfirm {
			t.Errorf("envelopes sent %q, want [offer confirm]", got)
		}
	})

	t.Run("confirm_back_refused", func(t *testing.T) {
		doer := &fakeDoer{respond: func(string) *http.Response { return answer(200, "text/html", "<!doctype html>") }}
		fx, eng := contractEngine(t, doer)
		url := peerURL(t, fx)
		if _, err := eng.InitiateOffer(context.Background(), url, "", fixtureAdmin(t, context.Background(), fx.pool)); err == nil {
			t.Fatal("HTML answer accepted")
		}
		if _, err := eng.HandleInbound(context.Background(),
			signedFrom(t, newKey(t), peer.HandshakeTypeOffer, url, fx.baseURL, peer.KnownCapabilities)); err == nil {
			t.Fatal("crossing offer completed although the confirm back was refused")
		}
		assertNotCompleted(t, fx, url, federation.PeerStatusPendingOutbound)
	})
}

// --- #1592 C1: what the signature covers, and URL shape -----------------

// envMap is a handshake envelope as a plain JSON object, so a test
// can add, rename or drop members.
func envMap(t *testing.T, priv ed25519.PrivateKey, typ, from, to string) map[string]any {
	t.Helper()
	return map[string]any{
		"protocol": peer.HandshakeProtocol, "type": typ,
		"from": from, "from_display_name": "Remote " + from, "from_public_key_pem": keyPEM(t, priv),
		"to": to, "nonce": randNonce(t), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// signMap signs the canonical form of signed and sends sent (the same
// map unless a test tampers after signing), plus any extra top-level
// members.
func signMap(t *testing.T, priv ed25519.PrivateKey, signed, sent map[string]any, top map[string]any) []byte {
	t.Helper()
	canonical, err := federation.Canonicalize(mustJSON(signed))
	if err != nil {
		t.Fatal(err)
	}
	wire := map[string]any{"envelope": sent, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical))}
	for k, v := range top {
		wire[k] = v
	}
	return mustJSON(wire)
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// The signature covers the envelope exactly as received, and a v1
// envelope carries only the members v1 defines.
func TestHandleInbound_SignatureCoversReceivedEnvelope(t *testing.T) {
	fx, eng := contractEngine(t, &fakeDoer{respond: func(string) *http.Response {
		return answer(200, "application/json", ack("completed"))
	}})
	ctx := context.Background()

	refused := func(t *testing.T, body []byte, url string, want error) {
		t.Helper()
		res, err := eng.HandleInbound(ctx, body)
		if err == nil {
			t.Fatalf("accepted (%+v), want a refusal", res)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("err = %v, want %v", err, want)
		}
		if _, gerr := fx.registry.ByInstanceURL(ctx, url); !errors.Is(gerr, peer.ErrPeerNotFound) {
			t.Errorf("a refused envelope left a record for %s (%v)", url, gerr)
		}
	}

	t.Run("unsigned_member_added_after_signing", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		signed := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		sent := cloneMap(signed)
		sent["display_hint"] = "added in flight"
		refused(t, signMap(t, key, signed, sent, nil), url, nil)
	})
	t.Run("signed_unknown_member", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		m["display_hint"] = "signed but not v1"
		refused(t, signMap(t, key, m, m, nil), url, peer.ErrHandshakeMalformed)
	})
	t.Run("signed_case_variant_member", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		m["From"] = m["from"]
		delete(m, "from")
		refused(t, signMap(t, key, m, m, nil), url, peer.ErrHandshakeMalformed)
	})
	t.Run("duplicate_member", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		body := signMap(t, key, m, m, nil)
		dup := strings.Replace(string(body), `"from":`, `"from":"https://elsewhere.example","from":`, 1)
		refused(t, []byte(dup), url, peer.ErrHandshakeMalformed)
	})
	t.Run("unknown_top_level_member", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		refused(t, signMap(t, key, m, m, map[string]any{"note": "outside the signature"}), url, peer.ErrHandshakeMalformed)
	})

	accepted := func(t *testing.T, m map[string]any, key ed25519.PrivateKey) *peer.Peer {
		t.Helper()
		res, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil))
		if err != nil {
			t.Fatalf("valid envelope refused: %v", err)
		}
		if res.Status != "accepted_pending" {
			t.Errorf("status %q, want accepted_pending", res.Status)
		}
		p, err := fx.registry.ByInstanceURL(ctx, m["from"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Run("pre_capability_envelope", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		if p := accepted(t, envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL), key); p.CapabilitiesNegotiatedAt.Valid {
			t.Error("an envelope without supported_capabilities recorded a negotiation")
		}
	})
	t.Run("signed_capabilities", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		m["supported_capabilities"] = []string{string(peer.CapE2EEncrypted), "future-token"}
		p := accepted(t, m, key)
		if want := peer.Intersect(peer.KnownCapabilities, peer.CapabilitySet{peer.CapE2EEncrypted, "future-token"}); !p.CapabilitiesNegotiatedAt.Valid || !slicesEqual(p.Capabilities, want) {
			t.Errorf("capabilities %v (negotiated %v), want %v", p.Capabilities, p.CapabilitiesNegotiatedAt.Valid, want)
		}
	})
	t.Run("signed_empty_capabilities", func(t *testing.T) {
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL)
		m["supported_capabilities"] = []string{}
		if p := accepted(t, m, key); !p.CapabilitiesNegotiatedAt.Valid || len(p.Capabilities) != 0 {
			t.Errorf("explicit empty list: capabilities %v, negotiated %v; want empty and negotiated", p.Capabilities, p.CapabilitiesNegotiatedAt.Valid)
		}
	})
}

func slicesEqual(a, b peer.CapabilitySet) bool {
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

// An instance URL is an https origin and nothing else: userinfo, a
// query, a fragment, a path or a bad authority would make the
// handshake POST go somewhere other than the peer's endpoint.
func TestInitiateOffer_InstanceURLShape(t *testing.T) {
	host := func(t *testing.T) string { return "url-" + randHex(t, 4) + ".example" }
	refusedCases := map[string]func(h string) string{
		"userinfo":          func(h string) string { return "https://user@" + h },
		"userinfo_password": func(h string) string { return "https://user:pw@" + h },
		"query":             func(h string) string { return "https://" + h + "?x=1" },
		"query_after_slash": func(h string) string { return "https://" + h + "/?x=1" },
		"empty_query":       func(h string) string { return "https://" + h + "?" },
		"fragment":          func(h string) string { return "https://" + h + "#frag" },
		"empty_fragment":    func(h string) string { return "https://" + h + "/#" },
		"path":              func(h string) string { return "https://" + h + "/api" },
		"port_not_numeric":  func(h string) string { return "https://" + h + ":abc" },
		"port_empty":        func(h string) string { return "https://" + h + ":" },
		"port_zero":         func(h string) string { return "https://" + h + ":0" },
		"port_too_large":    func(h string) string { return "https://" + h + ":99999" },
		"no_hostname":       func(string) string { return "https://:8443" },
		"bad_ipv6":          func(string) string { return "https://[::1" },
		"space_in_host":     func(h string) string { return "https://peer " + h },
		"percent_in_host":   func(h string) string { return "https://pe%72" + h },
		"backslash":         func(h string) string { return "https://evil.example\\@" + h },
		"http":              func(h string) string { return "http://" + h },
	}
	for name, mk := range refusedCases {
		t.Run("refused_"+name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("accepted_pending")) }}
			fx, eng := contractEngine(t, doer)
			u := mk(host(t))
			_, err := eng.InitiateOffer(context.Background(), u, "", fixtureAdmin(t, context.Background(), fx.pool))
			if !errors.Is(err, peer.ErrInstanceURLInvalid) {
				t.Errorf("InitiateOffer(%q): err = %v, want ErrInstanceURLInvalid", u, err)
			}
			if len(doer.urls) != 0 {
				t.Errorf("InitiateOffer(%q) sent %q", u, doer.urls)
			}
		})
	}

	acceptedCases := map[string][2]string{ // input template, normalised template
		"plain":          {"https://%s", "https://%s"},
		"trailing_slash": {"https://%s/", "https://%s"},
		"port":           {"https://%s:8443/", "https://%s:8443"},
		"surrounding_ws": {"  https://%s  ", "https://%s"},
	}
	for name, c := range acceptedCases {
		t.Run("accepted_"+name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("accepted_pending")) }}
			fx, eng := contractEngine(t, doer)
			h := host(t)
			want := fmt.Sprintf(c[1], h)
			t.Cleanup(func() {
				_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE instance_url = $1`, want)
			})
			row, err := eng.InitiateOffer(context.Background(), fmt.Sprintf(c[0], h), "", fixtureAdmin(t, context.Background(), fx.pool))
			if err != nil {
				t.Fatalf("InitiateOffer: %v", err)
			}
			if row.InstanceURL != want {
				t.Errorf("stored %q, want %q", row.InstanceURL, want)
			}
			if len(doer.urls) != 1 || doer.urls[0] != want+canonicalHandshakePath {
				t.Errorf("POSTed to %q, want %q", doer.urls, want+canonicalHandshakePath)
			}
		})
	}
}

// The signed from and to URLs are normalised before any lookup, so a
// trailing slash finds the same record, and a from URL that is not a
// plain origin is refused.
func TestHandleInbound_NormalisesEnvelopeURLs(t *testing.T) {
	ctx := context.Background()

	t.Run("repeated_offer_with_trailing_slash", func(t *testing.T) {
		fx, eng := contractEngine(t, &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("completed")) }})
		key, url := newKey(t), peerURL(t, fx)
		for i, from := range []string{url, url + "/"} {
			m := envMap(t, key, peer.HandshakeTypeOffer, from, fx.baseURL)
			res, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil))
			if err != nil {
				t.Fatalf("offer %d from %q: %v", i+1, from, err)
			}
			if res.Status != "accepted_pending" || res.Peer.InstanceURL != url {
				t.Errorf("offer %d: %q for %q, want accepted_pending for %q", i+1, res.Status, res.Peer.InstanceURL, url)
			}
		}
		var n int
		_ = fx.pool.QueryRow(ctx, `SELECT count(*) FROM federation_peers WHERE instance_url LIKE $1`, url+"%").Scan(&n)
		if n != 1 {
			t.Errorf("records for %s = %d, want 1", url, n)
		}
	})

	t.Run("confirm_with_trailing_slash", func(t *testing.T) {
		fx, eng := contractEngine(t, &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("accepted_pending")) }})
		url := peerURL(t, fx)
		if _, err := eng.InitiateOffer(ctx, url, "", fixtureAdmin(t, ctx, fx.pool)); err != nil {
			t.Fatal(err)
		}
		key := newKey(t)
		m := envMap(t, key, peer.HandshakeTypeConfirm, url+"/", fx.baseURL)
		res, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil))
		if err != nil || res.Status != "completed" {
			t.Fatalf("confirm from %q: %v / %+v", url+"/", err, res)
		}
	})

	t.Run("to_with_trailing_slash", func(t *testing.T) {
		fx, eng := contractEngine(t, &fakeDoer{})
		key, url := newKey(t), peerURL(t, fx)
		m := envMap(t, key, peer.HandshakeTypeOffer, url, fx.baseURL+"/")
		if _, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil)); err != nil {
			t.Fatalf("offer addressed to %q: %v", fx.baseURL+"/", err)
		}
	})

	t.Run("confirm_back_goes_to_the_normalised_origin", func(t *testing.T) {
		doer := &fakeDoer{respond: func(typ string) *http.Response {
			if typ == peer.HandshakeTypeOffer {
				return answer(200, "text/html", "<!doctype html>")
			}
			return answer(200, "application/json", ack("completed"))
		}}
		fx, eng := contractEngine(t, doer)
		url := peerURL(t, fx)
		_, _ = eng.InitiateOffer(ctx, url, "", fixtureAdmin(t, ctx, fx.pool))
		key := newKey(t)
		m := envMap(t, key, peer.HandshakeTypeOffer, url+"/", fx.baseURL)
		if _, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil)); err != nil {
			t.Fatalf("crossing offer: %v", err)
		}
		if len(doer.urls) != 2 || doer.urls[1] != url+canonicalHandshakePath {
			t.Errorf("requests %q, want the confirm back at %q", doer.urls, url+canonicalHandshakePath)
		}
	})

	for name, from := range map[string]string{
		"userinfo": "https://user@from-%s.example",
		"query":    "https://from-%s.example?x=1",
		"fragment": "https://from-%s.example#f",
	} {
		t.Run("refused_from_"+name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(string) *http.Response { return answer(200, "application/json", ack("completed")) }}
			fx, eng := contractEngine(t, doer)
			key := newKey(t)
			u := fmt.Sprintf(from, randHex(t, 4))
			t.Cleanup(func() {
				_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE instance_url = $1`, u)
			})
			m := envMap(t, key, peer.HandshakeTypeOffer, u, fx.baseURL)
			if _, err := eng.HandleInbound(ctx, signMap(t, key, m, m, nil)); !errors.Is(err, peer.ErrHandshakeMalformed) {
				t.Errorf("offer from %q: err = %v, want ErrHandshakeMalformed", u, err)
			}
			var n int
			_ = fx.pool.QueryRow(ctx, `SELECT count(*) FROM federation_peers WHERE instance_url = $1`, u).Scan(&n)
			if n != 0 || len(doer.urls) != 0 {
				t.Errorf("refused offer left %d records and sent %q", n, doer.urls)
			}
		})
	}
}
