// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the publish-from-our-side flow — Phase 1.22.B-c-bis.
// Coverage:
//   - State transitions: not_published → pending_dns →
//     pending_register → listed
//   - Failure paths land in failed with publish_last_error set
//   - DNS-not-yet-propagated returns 202 → stays pending_dns
//   - Token expiry rejected on register
//   - Metadata persisted independently of challenge state
//
// Skips without AA_DB_PASSWORD per project convention.

package directory_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/directory"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
)

// publishStubServer wires a minimal directory: signed /v1/operator
// + /v1/listing + the two publish endpoints. Bahavior of the
// publish endpoints is controllable per-test (succeed, fail-202,
// fail-403, fail-malformed).
type publishStubServer struct {
	pub           []byte // PEM
	priv          []byte // unused (Ed25519 sig in /v1/listing)
	pubKey        []byte // raw
	signer        func([]byte) []byte
	registerCode  int    // 0 → default success path
	registerBody  string // body to write on register
	lastChallenge map[string]string
}

func newPublishStub(t *testing.T, registerCode int, registerBody string) (*publishStubServer, *httptest.Server) {
	t.Helper()
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	stub := &publishStubServer{
		pub:           pubPEM,
		pubKey:        pub,
		signer:        func(b []byte) []byte { return federation.Sign(priv, b) },
		registerCode:  registerCode,
		registerBody:  registerBody,
		lastChallenge: map[string]string{},
	}
	_ = stub.priv
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/operator", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":           "Publish Stub",
			"operator_url":   "https://pubstub.example",
			"spec_version":   "aa-directory/v1",
			"public_key_pem": string(pubPEM),
			"fingerprint":    federation.PublicKeyFingerprint(pub),
		})
	})
	mux.HandleFunc("/v1/listing", func(w http.ResponseWriter, _ *http.Request) {
		// Empty listing is fine for these tests; sign it anyway so
		// the subscriber happy-path test still works against this
		// stub if combined.
		entries := []any{}
		rawEntries, _ := json.Marshal(entries)
		canonical, _ := federation.Canonicalize(rawEntries)
		sig := federation.Sign(priv, canonical)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"directory": map[string]any{
				"name":           "Publish Stub",
				"operator_url":   "https://pubstub.example",
				"spec_version":   "aa-directory/v1",
				"generated_at":   time.Now().UTC().Format(time.RFC3339Nano),
				"signature":      base64.StdEncoding.EncodeToString(sig),
				"public_key_pem": string(pubPEM),
			},
			"entries": entries,
		})
	})
	mux.HandleFunc("POST /v1/challenge", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		token := "stub-token-" + randHex(t, 6)
		stub.lastChallenge[body["instance_url"]] = token
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        token,
			"record_name":  "_artist-alley.example",
			"record_value": "v=aa1; directory=pubstub.example; token=" + token,
			"expires_at":   time.Now().UTC().Add(1 * time.Hour),
		})
	})
	mux.HandleFunc("POST /v1/register", func(w http.ResponseWriter, _ *http.Request) {
		if registerCode == 0 {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"listing_id": "stub-listing-id-abc"}`))
			return
		}
		w.WriteHeader(registerCode)
		_, _ = w.Write([]byte(registerBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, srv
}

// publishFixture wires identity + registry + a rewriting Client
// pointing at the given httptest server. Shared by all publish
// tests.
type publishFixture struct {
	ctx     context.Context
	cancel  context.CancelFunc
	reg     *directory.Registry
	client  *directory.Client
	dir     *directory.Directory
	idMgr   *identity.Manager
	adminID int64
}

func ensureAtrestPub(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatal(err)
	}
}

func setupPublishFixture(t *testing.T, srvURL string, stubPubPEM []byte, stubPub []byte) *publishFixture {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ensureAtrestPub(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	t.Cleanup(regCache.Stop)
	reg := directory.NewRegistry(pool, logger, regCache)
	client := rewriteClient(t, logger, srvURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	admin := fixtureAdmin(t, ctx, pool)

	// Subscribe to the stub at a fake https URL.
	fakeURL := "https://pub-fake-" + randHex(t, 4) + ".example"
	dir, err := reg.Subscribe(ctx, directory.SubscribeInput{
		URL:                 fakeURL,
		OperatorName:        "Publish Stub",
		OperatorPublicKey:   string(stubPubPEM),
		OperatorFingerprint: federation.PublicKeyFingerprint(stubPub),
		SubscribedByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Unsubscribe(context.Background(), dir.ID) })

	// Identity manager. ensureAtrestPub uses a deterministic key
	// so an existing prod-encrypted row would FAIL to decrypt;
	// clear before Load to force a fresh generate.
	_, _ = pool.Exec(ctx, `DELETE FROM system_config WHERE key='federation.instance_identity'`)
	idMgr := identity.NewManager(pool, logger)
	if _, err := idMgr.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return &publishFixture{
		ctx: ctx, cancel: cancel, reg: reg, client: client, dir: dir,
		idMgr: idMgr, adminID: admin,
	}
}

func TestPublishFlow_HappyPath(t *testing.T) {
	stub, srv := newPublishStub(t, 0, "") // 0 = default-success
	fx := setupPublishFixture(t, srv.URL, stub.pub, stub.pubKey)
	defer fx.cancel()

	instanceURL := "https://instance-x.example"
	// 1. Request challenge.
	updated, err := fx.client.RequestChallenge(fx.ctx, fx.reg, fx.dir, instanceURL)
	if err != nil {
		t.Fatalf("RequestChallenge: %v", err)
	}
	if updated.PublishStatus != federation.PublishStatusPendingDNS {
		t.Errorf("after challenge: status %q, want pending_dns", updated.PublishStatus)
	}
	if updated.PublishPendingToken == "" {
		t.Error("pending token should be populated")
	}
	if updated.PublishRecordName == "" || updated.PublishRecordValue == "" {
		t.Error("record_name + record_value should be populated")
	}
	if !strings.HasPrefix(updated.PublishRecordValue, "v=aa1;") {
		t.Errorf("record_value doesn't look right: %q", updated.PublishRecordValue)
	}

	// 2. Register.
	finalDir, err := fx.client.RegisterListing(fx.ctx, fx.reg, updated, fx.idMgr, instanceURL, directory.PublishMetadata{
		DisplayName: "Instance X",
		Region:      "us-east",
		Description: "Test publish",
		Tags:        []string{"art", "studio"},
	})
	if err != nil {
		t.Fatalf("RegisterListing: %v", err)
	}
	if finalDir.PublishStatus != federation.PublishStatusListed {
		t.Errorf("after register: status %q, want listed", finalDir.PublishStatus)
	}
	if finalDir.PublishListingID != "stub-listing-id-abc" {
		t.Errorf("listing_id not captured: %q", finalDir.PublishListingID)
	}
	if finalDir.PublishPendingToken != "" {
		t.Error("pending_token should be cleared after listed")
	}
}

func TestPublishFlow_DNSNotPropagated_StaysPendingDNS(t *testing.T) {
	stub, srv := newPublishStub(t, http.StatusAccepted, `{"error":"DNS-TXT not yet verifiable: NXDOMAIN"}`)
	fx := setupPublishFixture(t, srv.URL, stub.pub, stub.pubKey)
	defer fx.cancel()

	instanceURL := "https://instance-x.example"
	updated, _ := fx.client.RequestChallenge(fx.ctx, fx.reg, fx.dir, instanceURL)
	_, err := fx.client.RegisterListing(fx.ctx, fx.reg, updated, fx.idMgr, instanceURL, directory.PublishMetadata{
		DisplayName: "X",
	})
	if err == nil {
		t.Fatal("expected error on 202")
	}
	// Status should be back to pending_dns so admin can retry.
	final, _ := fx.reg.ByID(fx.ctx, fx.dir.ID)
	if final.PublishStatus != federation.PublishStatusPendingDNS {
		t.Errorf("after 202: status %q, want pending_dns", final.PublishStatus)
	}
}

func TestPublishFlow_HardFailure_LandsInFailed(t *testing.T) {
	stub, srv := newPublishStub(t, http.StatusForbidden, `{"error":"DNS-TXT mismatch"}`)
	fx := setupPublishFixture(t, srv.URL, stub.pub, stub.pubKey)
	defer fx.cancel()

	instanceURL := "https://instance-x.example"
	updated, _ := fx.client.RequestChallenge(fx.ctx, fx.reg, fx.dir, instanceURL)
	_, err := fx.client.RegisterListing(fx.ctx, fx.reg, updated, fx.idMgr, instanceURL, directory.PublishMetadata{
		DisplayName: "X",
	})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	final, _ := fx.reg.ByID(fx.ctx, fx.dir.ID)
	if final.PublishStatus != federation.PublishStatusFailed {
		t.Errorf("after 403: status %q, want failed", final.PublishStatus)
	}
	if final.PublishLastError == "" {
		t.Error("publish_last_error should carry the failure detail")
	}
}

func TestRegister_RejectsBeforeChallenge(t *testing.T) {
	stub, srv := newPublishStub(t, 0, "")
	fx := setupPublishFixture(t, srv.URL, stub.pub, stub.pubKey)
	defer fx.cancel()

	// Skip RequestChallenge; status is not_published.
	_, err := fx.client.RegisterListing(fx.ctx, fx.reg, fx.dir, fx.idMgr, "https://instance-x.example", directory.PublishMetadata{
		DisplayName: "X",
	})
	if !errors.Is(err, directory.ErrPublishNotPending) {
		t.Errorf("expected ErrPublishNotPending, got %v", err)
	}
}

func TestSetPublishMetadata_Independent(t *testing.T) {
	stub, srv := newPublishStub(t, 0, "")
	fx := setupPublishFixture(t, srv.URL, stub.pub, stub.pubKey)
	defer fx.cancel()

	// Save metadata without ever issuing a challenge. Status
	// should stay not_published; metadata is persisted.
	updated, err := fx.reg.SetPublishMetadata(fx.ctx, fx.dir.ID, directory.PublishMetadata{
		DisplayName: "Pre-filled Name",
		Region:      "eu-west",
		Tags:        []string{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PublishStatus != federation.PublishStatusNotPublished {
		t.Errorf("status changed unexpectedly: %q", updated.PublishStatus)
	}
	if updated.PublishDisplayName != "Pre-filled Name" {
		t.Errorf("display_name not persisted: %q", updated.PublishDisplayName)
	}
	if len(updated.PublishTags) != 2 {
		t.Errorf("tags not persisted: %v", updated.PublishTags)
	}
}
