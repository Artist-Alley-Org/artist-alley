// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Envelope unit tests. The conformance tests in conformance_test.go
// exercise the fixture-driven contract; these tests cover the
// shape-and-edge-case surface that doesn't need to be a wire
// fixture.

package federation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// freshEnv returns a minimal valid unsigned envelope, ready to
// sign.
func freshEnv(t *testing.T) *federation.Envelope {
	t.Helper()
	return &federation.Envelope{
		Context:   federation.ContextV1,
		Type:      federation.ActivityAAShare,
		ID:        "https://test.example/activities/abc",
		Actor:     "https://test.example/users/alice",
		Published: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
		Object:    "https://test.example/posts/xyz",
	}
}

func freshKeypair(t *testing.T) (ed25519.PublicKey, []byte, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, err := federation.PrivateKeyToPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pub, pubPEM, privPEM
}

func TestSignThenVerifyRoundTrip(t *testing.T) {
	env := freshEnv(t)
	_, pubPEM, privPEM := freshKeypair(t)
	if err := env.Sign(privPEM, "https://test.example/users/alice#main-key"); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if env.Signature == nil {
		t.Fatal("no signature after Sign")
	}
	if env.Signature.Type != federation.SignatureAlgEd25519 {
		t.Errorf("signature algorithm: got %q", env.Signature.Type)
	}
	if err := env.Verify(pubPEM); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyFailsOnTamperedBody(t *testing.T) {
	env := freshEnv(t)
	_, pubPEM, privPEM := freshKeypair(t)
	if err := env.Sign(privPEM, "https://test.example/users/alice#main-key"); err != nil {
		t.Fatal(err)
	}
	// Tamper after signing.
	env.Object = "https://test.example/posts/different"
	if err := env.Verify(pubPEM); err == nil {
		t.Fatal("verify must fail on tampered body")
	}
}

func TestVerifyFailsOnWrongKey(t *testing.T) {
	env := freshEnv(t)
	_, _, privPEM := freshKeypair(t)
	_, otherPub, _ := freshKeypair(t)
	if err := env.Sign(privPEM, "https://test.example/users/alice#main-key"); err != nil {
		t.Fatal(err)
	}
	if err := env.Verify(otherPub); err == nil {
		t.Fatal("verify must fail under wrong key")
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	env := freshEnv(t)
	_, _, privPEM := freshKeypair(t)
	if err := env.Sign(privPEM, "https://test.example/users/alice#main-key"); err != nil {
		t.Fatal(err)
	}
	wire, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := federation.Unmarshal(wire)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != env.Type {
		t.Errorf("type drift: %s vs %s", got.Type, env.Type)
	}
	if !got.Published.Equal(env.Published) {
		t.Errorf("published drift: %v vs %v", got.Published, env.Published)
	}
	if got.Signature == nil || got.Signature.Value != env.Signature.Value {
		t.Errorf("signature drift")
	}
}

func TestUnmarshalRejectsBadContext(t *testing.T) {
	bad := []byte(`{"@context":"https://www.w3.org/ns/activitystreams","type":"aa:Share","id":"x","actor":"y","published":"2026-06-04T10:00:00Z"}`)
	_, err := federation.Unmarshal(bad)
	if err == nil || !strings.Contains(err.Error(), "@context") {
		t.Errorf("expected @context rejection, got %v", err)
	}
}

func TestUnmarshalRejectsUnknownActivityType(t *testing.T) {
	bad := []byte(`{"@context":"https://artist-alley.org/protocol/v1","type":"aa:NotAType","id":"x","actor":"y","published":"2026-06-04T10:00:00Z"}`)
	_, err := federation.Unmarshal(bad)
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Errorf("expected type rejection, got %v", err)
	}
}

func TestUnmarshalAcceptsExtraTypeSpecificFields(t *testing.T) {
	// Per-type field allowlists ship in 1.22.E. For 1.22.A any
	// extra top-level key beyond the envelope's named set is
	// accepted into Extra so the round-trip works.
	with := []byte(`{"@context":"https://artist-alley.org/protocol/v1","type":"aa:Share","id":"x","actor":"y","published":"2026-06-04T10:00:00Z","object_kind":"post"}`)
	env, err := federation.Unmarshal(with)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := env.Extra["object_kind"]; !ok {
		t.Errorf("expected object_kind in Extra")
	}
}

func TestPublicKeyFingerprintStable(t *testing.T) {
	pub, _, _ := freshKeypair(t)
	fp1 := federation.PublicKeyFingerprint(pub)
	fp2 := federation.PublicKeyFingerprint(pub)
	if fp1 != fp2 {
		t.Errorf("fingerprint must be deterministic")
	}
	if len(fp1) != 64 {
		t.Errorf("fingerprint should be 64-char hex SHA-256, got len=%d", len(fp1))
	}
}

func TestShareScopeAtLeast(t *testing.T) {
	if !federation.ShareScopeRemix.AtLeast(federation.ShareScopeView) {
		t.Error("edit should grant at least view")
	}
	if federation.ShareScopeView.AtLeast(federation.ShareScopeRemix) {
		t.Error("view should NOT grant edit")
	}
	if federation.ShareScopeComment.AtLeast(federation.ShareScopeAnnotate) {
		t.Error("comment should NOT grant annotate")
	}
	if !federation.ShareScopeAnnotate.AtLeast(federation.ShareScopeComment) {
		t.Error("annotate should grant comment")
	}
	if federation.ShareScope("nonsense").AtLeast(federation.ShareScopeView) {
		t.Error("unknown scope is never sufficient")
	}
}

func TestInboxStatusIsReject(t *testing.T) {
	cases := map[federation.InboxStatus]bool{
		federation.InboxStatusPending:        false,
		federation.InboxStatusProcessed:      false,
		federation.InboxStatusInvalidContext: true,
		federation.InboxStatusSigInvalid:     true,
		federation.InboxStatusUnsharedObject: true,
		federation.InboxStatus("bogus"):      false,
	}
	for s, want := range cases {
		if got := s.IsReject(); got != want {
			t.Errorf("IsReject(%q) = %v, want %v", s, got, want)
		}
	}
}
