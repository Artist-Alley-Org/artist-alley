// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end inbox-handler tests. Covers every pipeline stage's
// reject path + the happy 202 path. Phase 1.22.D-a-5.
//
// Skips without AA_DB_PASSWORD (real DB; the UNIQUE constraint
// dedup is part of the contract we test).

package inbox_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// --- fixtures ----------------------------------------------------------

type fixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	q         *inbox.Queries
	peerID    uuid.UUID
	peerURL   string
	peerKeyID string
	peerPub   ed25519.PublicKey
	peerPriv  ed25519.PrivateKey
	handler   *inbox.Handler
	rejects   *rejectRecorder
}

// rejectRecorder captures the audit hook's calls so tests can
// assert that the right §12.1 reason was emitted per rejection.
type rejectRecorder struct {
	mu      sync.Mutex
	entries []rejectEntry
}

type rejectEntry struct {
	peerID      uuid.UUID
	reason      federation.InboxStatus
	activityURI string
	msg         string
}

func (r *rejectRecorder) audit(_ context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, rejectEntry{peerID, reason, activityURI, msg})
}

func (r *rejectRecorder) last() *rejectEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 0 {
		return nil
	}
	e := r.entries[len(r.entries)-1]
	return &e
}

// newFixture wires everything: pool + queries + handler + a
// pre-seeded peer with a real ed25519 keypair so HTTP-Sig verify
// can succeed.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a peer fixture. cleanup after.
	peerURL := "https://inbox-test-" + randHex(4) + ".example"
	pubPEM := ed25519PubToPEM(t, pub)
	var grantorRef int64
	username := "inbox-test-" + randHex(4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Inbox Test",
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, $2, $3, 'connected', 'plaintext', TRUE, 'connected', $4)
		 RETURNING id`,
		peerURL, "Inbox Test Peer", pubPEM, grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_inbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
	})

	keyID := peerURL + "/instance#main-key"
	rejects := &rejectRecorder{}
	q := inbox.New(pool)
	handler := inbox.NewHandler(inbox.HandlerDeps{
		Pool:        q,
		Lookup:      &staticPeerLookup{peerID: peerID, peerURL: peerURL, keyID: keyID, pubKey: pub, enabled: true, connected: true},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit: rejects.audit,
	})

	return &fixture{
		t:         t,
		pool:      pool,
		q:         q,
		peerID:    peerID,
		peerURL:   peerURL,
		peerKeyID: keyID,
		peerPub:   pub,
		peerPriv:  priv,
		handler:   handler,
		rejects:   rejects,
	}
}

// staticPeerLookup is the test PeerLookup — returns its
// pre-seeded peer for the configured keyId, ErrPeerNotFound for
// any other.
type staticPeerLookup struct {
	peerID    uuid.UUID
	peerURL   string
	keyID     string
	pubKey    ed25519.PublicKey
	enabled   bool
	connected bool
}

func (l *staticPeerLookup) ByKeyID(_ context.Context, keyID string) (inbox.PeerInfo, error) {
	if keyID != l.keyID {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	return inbox.PeerInfo{
		ID:                l.peerID,
		InstanceURL:       l.peerURL,
		InstancePublicKey: l.pubKey,
		Enabled:           l.enabled,
		Connected:         l.connected,
	}, nil
}

// newSignedRequest builds a POST /federation/inbox request with
// the standard signed-header set + HTTP-Sig from the fixture's
// peer key.
func (fx *fixture) newSignedRequest(envelope []byte) *http.Request {
	req, err := http.NewRequest(http.MethodPost,
		"https://local.example/federation/inbox",
		bytes.NewReader(envelope))
	if err != nil {
		fx.t.Fatal(err)
	}
	req.Header.Set("Host", "local.example")
	if err := httpsig.SignAndAttach(req, envelope, fx.peerKeyID, fx.peerPriv); err != nil {
		fx.t.Fatal(err)
	}
	return req
}

// newPlaintextEnvelope returns a minimally-valid v1 envelope —
// strict-parse + structural-sig check all pass, but the
// signature bytes won't crypto-verify (we don't have the per-
// actor key flow yet, and §5.5 addition C says structural-only
// in 1.22.D-a).
func (fx *fixture) newPlaintextEnvelope() []byte {
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	b, _ := json.Marshal(env)
	return b
}

// --- happy path -------------------------------------------------------

func TestPostInbox_HappyPath_Returns202_AndPersists(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.newPlaintextEnvelope()
	req := fx.newSignedRequest(envelope)

	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("status: got %d want 202; body=%s", rr.Code, rr.Body.String())
	}

	// Row landed with status=pending.
	var status string
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT status FROM federation_inbox WHERE peer_id = $1`,
		fx.peerID,
	).Scan(&status)
	if status != "pending" {
		t.Errorf("inbox row status: got %q want pending", status)
	}
}

func TestPostInbox_ReplayWithinCacheTTL_Returns200(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.newPlaintextEnvelope()

	rr1 := httptest.NewRecorder()
	fx.handler.PostInbox(rr1, fx.newSignedRequest(envelope))
	if rr1.Code != http.StatusAccepted {
		t.Fatalf("first POST: %d body=%s", rr1.Code, rr1.Body.String())
	}

	// Same envelope (same id), fresh signed request.
	rr2 := httptest.NewRecorder()
	fx.handler.PostInbox(rr2, fx.newSignedRequest(envelope))
	if rr2.Code != http.StatusOK {
		t.Errorf("replay: got %d want 200 (idempotent receipt); body=%s", rr2.Code, rr2.Body.String())
	}
}

// --- per-stage reject coverage ---------------------------------------

func TestPostInbox_PayloadTooLarge_413(t *testing.T) {
	fx := newFixture(t)
	body := bytes.Repeat([]byte("x"), inbox.MaxBodyBytes+10)
	req := fx.newSignedRequest(body)

	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: got %d want 413", rr.Code)
	}
}

func TestPostInbox_UnknownPeer_401(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.newPlaintextEnvelope()
	req := fx.newSignedRequest(envelope)
	// Mutate keyId in the Signature header so peer lookup fails.
	sig := req.Header.Get("Signature")
	tampered := strings.Replace(sig, fx.peerKeyID, fx.peerKeyID+"-bogus", 1)
	req.Header.Set("Signature", tampered)

	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unknown_peer: got %d want 401", rr.Code)
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnknownPeer)
}

func TestPostInbox_PeerDisabled_403(t *testing.T) {
	fx := newFixture(t)
	// Mutate the lookup: same peer but enabled=false.
	disabled := &staticPeerLookup{
		peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
		pubKey: fx.peerPub, enabled: false, connected: true,
	}
	fx.handler = inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q, Lookup: disabled,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit: fx.rejects.audit,
	})
	req := fx.newSignedRequest(fx.newPlaintextEnvelope())
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("peer_disabled: got %d want 403", rr.Code)
	}
	if last := fx.rejects.last(); last == nil || last.reason != federation.InboxStatusPeerDisabled {
		t.Errorf("audit: got %+v want reason=peer_disabled", last)
	}
}

func TestPostInbox_SigInvalid_401(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.newPlaintextEnvelope()
	req := fx.newSignedRequest(envelope)
	// Replace the signature bytes with garbage of the right length.
	sig := req.Header.Get("Signature")
	// signature="...." → replace with all-zeros base64
	bogus := strings.Replace(sig, `signature="`, `signature="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, 1)
	// Re-set Signature header preserving everything except the value.
	// Easier: just replace the matched substring with bogus
	parts := strings.Split(sig, `signature="`)
	if len(parts) != 2 {
		t.Fatalf("can't parse sig header: %s", sig)
	}
	bogus = parts[0] + `signature="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`
	req.Header.Set("Signature", bogus)

	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("sig_invalid: got %d want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPostInbox_StaleRequest_400(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.newPlaintextEnvelope()
	// Build a request with a backdated Date BEFORE signing, so
	// the signature covers the past Date and the verify step
	// trips on the ±5min replay window check.
	req, err := http.NewRequest(http.MethodPost, "https://local.example/federation/inbox",
		bytes.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Host", "local.example")
	req.Header.Set("Date", time.Now().Add(-1*time.Hour).UTC().Format(http.TimeFormat))
	if err := httpsig.SignAndAttach(req, envelope, fx.peerKeyID, fx.peerPriv); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("stale_request: got %d want 400", rr.Code)
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusStaleRequest)
}

func TestPostInbox_UnknownObject_WrongHost_404(t *testing.T) {
	// Object URL has a host that doesn't match our local base
	// URL → §12.1 unknown_object. Distinct from unshared_object
	// (which fires at dispatch time when the gate finds no
	// share row). Sender's outbox built a foreign URL.
	fx := newFixture(t)
	localBase := "https://local.example"
	fx.handler = inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{
			peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true,
		},
		LocalBaseURL: func(_ context.Context) string { return localBase },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit:  fx.rejects.audit,
	})
	// Build an envelope whose object URL points at fx.peerURL
	// (sender's host) instead of localBase — outbox-built-wrong.
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	body, _ := json.Marshal(env)
	req := fx.newSignedRequest(body)
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown_object (wrong host): got %d want 404; body=%s", rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnknownObject)
}

func TestPostInbox_UnknownObject_BadURLShape_404(t *testing.T) {
	// Object URL is on our host but doesn't match §8.2's
	// <base>/<kind>/<uuid> shape — also unknown_object.
	fx := newFixture(t)
	localBase := "https://local.example"
	fx.handler = inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{
			peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true,
		},
		LocalBaseURL: func(_ context.Context) string { return localBase },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit:  fx.rejects.audit,
	})
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    localBase + "/posts/not-a-uuid",
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	body, _ := json.Marshal(env)
	req := fx.newSignedRequest(body)
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown_object (bad URL shape): got %d want 404", rr.Code)
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnknownObject)
}

func TestPostInbox_EncryptionNotSupported_422(t *testing.T) {
	fx := newFixture(t)
	// Envelope with `encrypted` field present.
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"encrypted": map[string]any{
			"alg":          "naclbox-v1",
			"ephemeralKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			"recipients": []map[string]string{
				{
					"actor":      fx.peerURL + "/users/alice",
					"nonce":      "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
					"ciphertext": "AAAAAAAAAAAA",
				},
			},
		},
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	body, _ := json.Marshal(env)
	req := fx.newSignedRequest(body)
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("encryption_not_supported: got %d want 422; body=%s", rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusEncryptionNotSupported)
}

func TestPostInbox_EnvelopeSigMissing_401(t *testing.T) {
	// Spec §5.6: envelope without a signature block (or with a
	// malformed one) MUST be rejected with envelope_sig_missing.
	// Distinct from sig_invalid (which is the crypto-failure
	// path landing in 1.22.I).
	fx := newFixture(t)
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		// No signature field.
	}
	body, _ := json.Marshal(env)
	req := fx.newSignedRequest(body)
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("envelope_sig_missing: got %d want 401; body=%s", rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusEnvelopeSigMissing)
}

func TestPostInbox_EnvelopeSigMalformed_401(t *testing.T) {
	// Same reject reason — malformed signature block (missing
	// publicKey field) is structurally invalid even though the
	// signature object itself exists. Operators see this same
	// reason as "structurally non-compliant" without conflating
	// with crypto-verify failure.
	fx := newFixture(t)
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		"signature": map[string]string{
			"type":  "Ed25519",
			"value": "AAAAAAAAAAAA",
			// no publicKey
		},
	}
	body, _ := json.Marshal(env)
	req := fx.newSignedRequest(body)
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("envelope_sig_missing (malformed): got %d want 401", rr.Code)
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusEnvelopeSigMissing)
}

func TestPostInbox_RateLimited_429(t *testing.T) {
	fx := newFixture(t)
	// Replace limiter with a very small one to force 429 fast.
	tiny := inbox.NewPeerRateLimiter(2, 2)
	fx.handler = inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{
			peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true,
		},
		Limiter:     tiny,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit: fx.rejects.audit,
	})
	// First 2 succeed.
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		fx.handler.PostInbox(rr, fx.newSignedRequest(fx.newPlaintextEnvelope()))
		if rr.Code != http.StatusAccepted {
			t.Fatalf("warmup %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	// 3rd hits the limit.
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(fx.newPlaintextEnvelope()))
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("3rd: got %d want 429", rr.Code)
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing on 429")
	}
}

// --- helpers ----------------------------------------------------------

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

func ed25519PubToPEM(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	pem, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem)
}

func assertReasonInBody(t *testing.T, body []byte, want federation.InboxStatus) {
	t.Helper()
	var resp struct{ Reason string }
	_ = json.Unmarshal(body, &resp)
	if resp.Reason != string(want) {
		t.Errorf("body reason: got %q want %q (body=%s)", resp.Reason, want, string(body))
	}
}
