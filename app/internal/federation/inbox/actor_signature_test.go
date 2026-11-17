// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1526 stage 10b: per-actor envelope signature verification in
// PostInbox and PostInboxBatch. The fixture's actor
// (<peerURL>/users/alice) has a key the stub resolver returns; every
// test below breaks one link and checks the typed refusal, that
// nothing is stored, and (for the binding check) that no key lookup
// happened at all.

package inbox_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// unsignedLike is a Like exactly as a pre-#1526 sender emits it: the
// structural signature block with the placeholder value.
func (fx *fixture) unsignedLike() []byte {
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": "2026-11-17T08:00:00Z",
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		fx.t.Fatal(err)
	}
	return b
}

// withField returns envelope b with one top-level field replaced.
func withField(t *testing.T, b []byte, key string, value any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type sigCase struct {
	name       string
	envelope   func(fx *fixture) []byte
	wantStatus int
	wantReason federation.InboxStatus
	// noLookup: refused before the key resolver is consulted.
	noLookup bool
}

func actorSignatureCases() []sigCase {
	return []sigCase{
		{
			name:       "pre-#1526 placeholder signature",
			envelope:   func(fx *fixture) []byte { return fx.unsignedLike() },
			wantStatus: http.StatusBadRequest, wantReason: federation.InboxStatusSigMalformed, noLookup: true,
		},
		{
			name: "payload changed after signing",
			envelope: func(fx *fixture) []byte {
				return withField(fx.t, fx.newPlaintextEnvelope(), "object", fx.peerURL+"/posts/"+uuid.NewString())
			},
			wantStatus: http.StatusUnauthorized, wantReason: federation.InboxStatusSigInvalid,
		},
		{
			name: "signed with another key",
			envelope: func(fx *fixture) []byte {
				_, other, _ := ed25519.GenerateKey(rand.Reader)
				return signEnvelopeWith(fx.t, fx.unsignedLike(), other)
			},
			wantStatus: http.StatusUnauthorized, wantReason: federation.InboxStatusSigInvalid,
		},
		{
			name: "actor on another instance (spoof)",
			envelope: func(fx *fixture) []byte {
				spoof := withField(fx.t, fx.unsignedLike(), "actor", "https://other-instance.example/users/alice")
				spoof = withField(fx.t, spoof, "signature", map[string]string{
					"type": "Ed25519", "publicKey": "https://other-instance.example/users/alice#main-key", "value": "AAAAAAAAAAAA",
				})
				return fx.signEnvelope(spoof)
			},
			wantStatus: http.StatusForbidden, wantReason: federation.InboxStatusUnknownActor, noLookup: true,
		},
		{
			name: "actor path is not /users/<name>",
			envelope: func(fx *fixture) []byte {
				e := withField(fx.t, fx.unsignedLike(), "actor", fx.peerURL+"/users/alice/extra")
				e = withField(fx.t, e, "signature", map[string]string{
					"type": "Ed25519", "publicKey": fx.peerURL + "/users/alice/extra#main-key", "value": "AAAAAAAAAAAA",
				})
				return fx.signEnvelope(e)
			},
			wantStatus: http.StatusForbidden, wantReason: federation.InboxStatusUnknownActor, noLookup: true,
		},
		{
			name: "key id is not the actor's #main-key",
			envelope: func(fx *fixture) []byte {
				signed := fx.newPlaintextEnvelope()
				var m map[string]any
				_ = json.Unmarshal(signed, &m)
				sig := m["signature"].(map[string]any)
				sig["publicKey"] = fx.peerURL + "/users/alice#other-key"
				out, _ := json.Marshal(m)
				return out
			},
			wantStatus: http.StatusUnauthorized, wantReason: federation.InboxStatusUnknownKey, noLookup: true,
		},
		{
			name: "actor without a key on the peer",
			envelope: func(fx *fixture) []byte {
				e := withField(fx.t, fx.unsignedLike(), "actor", fx.peerURL+"/users/ghost")
				e = withField(fx.t, e, "signature", map[string]string{
					"type": "Ed25519", "publicKey": fx.peerURL + "/users/ghost#main-key", "value": "AAAAAAAAAAAA",
				})
				return fx.signEnvelope(e)
			},
			wantStatus: http.StatusUnprocessableEntity, wantReason: federation.InboxStatusUnknownActor,
		},
	}
}

func TestPostInbox_ActorSignature_Refusals(t *testing.T) {
	for _, tc := range actorSignatureCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			env := tc.envelope(fx)
			id := activityIDOf(t, env)
			before := fx.actorKeys.callCount()

			rr := httptest.NewRecorder()
			fx.handler.PostInbox(rr, fx.newSignedRequest(env))

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			assertReasonInBody(t, rr.Body.Bytes(), tc.wantReason)
			if n := fx.countRows(id); n != 0 {
				t.Errorf("refused envelope stored: %d rows", n)
			}
			if tc.noLookup && fx.actorKeys.callCount() != before {
				t.Errorf("key resolver consulted %d times; the refusal must come first", fx.actorKeys.callCount()-before)
			}
		})
	}
}

func TestPostInboxBatch_ActorSignature_Refusals(t *testing.T) {
	for _, tc := range actorSignatureCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			good := fx.newPlaintextEnvelope()
			bad := tc.envelope(fx)
			goodID, badID := activityIDOf(t, good), activityIDOf(t, bad)
			before := fx.actorKeys.callCount()

			res := fx.postBatch(good, bad)

			if len(res) != 2 || res[0].Status != "accepted" {
				t.Fatalf("results: %+v (the valid sibling must be accepted)", res)
			}
			if res[1].Status != "rejected" || res[1].Reason != string(tc.wantReason) {
				t.Errorf("bad envelope: %+v, want rejected/%s", res[1], tc.wantReason)
			}
			if fx.countRows(goodID) != 1 || fx.countRows(badID) != 0 {
				t.Errorf("rows: good=%d bad=%d, want 1/0", fx.countRows(goodID), fx.countRows(badID))
			}
			if tc.noLookup && fx.actorKeys.callCount()-before != 1 {
				t.Errorf("key resolver calls: %d, want 1 (the valid sibling only)", fx.actorKeys.callCount()-before)
			}
		})
	}
}

func TestPostInbox_ActorSignature_Valid_Accepted(t *testing.T) {
	fx := newFixture(t)
	env := fx.newPlaintextEnvelope()
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(env))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202; body=%s", rr.Code, rr.Body.String())
	}
	if fx.countRows(activityIDOf(t, env)) != 1 {
		t.Error("valid envelope not stored")
	}
}

// A key that cannot be obtained right now is retryable and never
// downgrades to accepting the envelope unverified.
func TestPostInbox_ActorKeyTransient_503_NothingStored(t *testing.T) {
	fx := newFixture(t)
	fx.actorKeys.err = errors.New("peer actor document timed out")
	env := fx.newPlaintextEnvelope()
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(env))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Errorf("status %d Retry-After %q; want 503 with Retry-After", rr.Code, rr.Header().Get("Retry-After"))
	}
	if fx.countRows(activityIDOf(t, env)) != 0 {
		t.Error("envelope stored without a verified signature")
	}
}

func TestPostInboxBatch_ActorKeyTransient_Whole503(t *testing.T) {
	fx := newFixture(t)
	fx.actorKeys.err = errors.New("peer actor document timed out")
	env := fx.newPlaintextEnvelope()
	rr := fx.postBatchTo(fx.handler, env)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("batch status: got %d want 503; body=%s", rr.Code, rr.Body.String())
	}
	if fx.countRows(activityIDOf(t, env)) != 0 {
		t.Error("envelope stored without a verified signature")
	}
}

// No resolver wired fails closed (retryable), never open.
func TestPostInbox_NoActorKeyResolver_FailsClosed(t *testing.T) {
	fx := newFixture(t)
	h := inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true},
	})
	env := fx.newPlaintextEnvelope()
	rr := httptest.NewRecorder()
	h.PostInbox(rr, fx.newSignedRequest(env))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", rr.Code)
	}
	if fx.countRows(activityIDOf(t, env)) != 0 {
		t.Error("stored without verification")
	}
}

// The stored-id answer (#1569) still comes first: a retry of an
// accepted activity is answered from its row even though the retry's
// signature is never re-checked.
func TestPostInbox_StoredIDAnsweredBeforeSignatureCheck(t *testing.T) {
	fx := newFixture(t)
	env := fx.newPlaintextEnvelope()
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(env))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first: %d", rr.Code)
	}
	calls := fx.actorKeys.callCount()
	rr = httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(env))
	if rr.Code != http.StatusOK {
		t.Errorf("replay: got %d want 200", rr.Code)
	}
	if fx.actorKeys.callCount() != calls {
		t.Error("replay consulted the key resolver; the stored-id answer must come first")
	}
}

func TestActorBoundToPeer(t *testing.T) {
	const peer = "https://studio-b.example"
	for _, c := range []struct {
		actor string
		want  bool
	}{
		{"https://studio-b.example/users/alice", true},
		{"https://STUDIO-B.example/users/alice", true},
		{"https://studio-b.example/users/a.b_c-d+e@f", true},
		{"http://studio-b.example/users/alice", false},
		{"https://studio-b.example:8443/users/alice", false},
		{"https://evil.example/users/alice", false},
		{"https://studio-b.example.evil.example/users/alice", false},
		{"https://user@studio-b.example/users/alice", false},
		{"https://studio-b.example/users/alice?x=1", false},
		{"https://studio-b.example/users/alice#main-key", false},
		{"https://studio-b.example/users/al%2Fice", false},
		{"https://studio-b.example/users/", false},
		{"https://studio-b.example/users/alice/outbox", false},
		{"https://studio-b.example/users/by-ref", false},
		{"https://studio-b.example/users/by-username", false},
		{"https://studio-b.example/users/..", false},
		{"https://studio-b.example/posts/alice", false},
		{"https://studio-b.example/users/" + strings.Repeat("a", 129), false},
		{"https://studio-b.example/users/" + strings.Repeat("a", 128), true},
	} {
		if got := inbox.ActorBoundToPeer(peer, c.actor); got != c.want {
			t.Errorf("ActorBoundToPeer(%q) = %v, want %v", c.actor, got, c.want)
		}
	}
	if !inbox.ActorBoundToPeer("http://127.0.0.1:4321/", "http://127.0.0.1:4321/users/bob") {
		t.Error("port-bearing peer URL with trailing slash should bind")
	}
}
