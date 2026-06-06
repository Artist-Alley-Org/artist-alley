// Reference directory server HTTP tests. Cover the full
// challenge → register → list lifecycle plus signature
// verification on /v1/listing.

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/cmd/aa-directory/internal/store"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// newServerForTest wires a serverConfig + httptest.Server with
// --dev-skip-dns enabled (real DNS lookups would make the test
// flaky + network-dependent).
func newServerForTest(t *testing.T) (*httptest.Server, *serverConfig) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	if err := st.SetOperator(store.Operator{
		Name:         "Test Directory",
		OperatorURL:  "https://test-dir.example",
		SpecVersion:  "aa-directory/v1",
		PublicKeyPEM: string(pubPEM),
		Fingerprint:  federation.PublicKeyFingerprint(pub),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{
		store:        st,
		signingKey:   priv,
		operatorHost: "test-dir.example",
		challengeTTL: 1 * time.Hour,
		skipDNS:      true, // tests bypass real DNS
		bearerToken:  "test-bearer-token",
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operator", cfg.handleGetOperator)
	mux.HandleFunc("GET /v1/listing", cfg.handleGetListing)
	mux.HandleFunc("POST /v1/challenge", cfg.handlePostChallenge)
	mux.HandleFunc("POST /v1/register", cfg.handlePostRegister)
	mux.HandleFunc("DELETE /v1/listings/", cfg.handleDeleteListing)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cfg
}

func TestOperatorEndpoint_ReturnsConfiguredIdentity(t *testing.T) {
	srv, _ := newServerForTest(t)
	resp, err := http.Get(srv.URL + "/v1/operator")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	var op store.Operator
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatal(err)
	}
	if op.Name != "Test Directory" {
		t.Errorf("name: got %q", op.Name)
	}
	if op.Fingerprint == "" {
		t.Error("fingerprint should be populated")
	}
}

func TestFullFlow_ChallengeRegisterList(t *testing.T) {
	srv, _ := newServerForTest(t)

	// 1. Mint an instance keypair (the would-be listing).
	pub, _, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)

	// 2. POST /v1/challenge.
	chBody, _ := json.Marshal(map[string]string{"instance_url": "https://test-listing.example"})
	chResp, err := http.Post(srv.URL+"/v1/challenge", "application/json", bytes.NewReader(chBody))
	if err != nil {
		t.Fatal(err)
	}
	defer chResp.Body.Close()
	if chResp.StatusCode != 200 {
		raw, _ := io.ReadAll(chResp.Body)
		t.Fatalf("challenge: got %d: %s", chResp.StatusCode, raw)
	}
	var ch challengeResponse
	if err := json.NewDecoder(chResp.Body).Decode(&ch); err != nil {
		t.Fatal(err)
	}
	if ch.Token == "" {
		t.Fatal("token should be populated")
	}
	if ch.RecordName != "_artist-alley.test-listing.example" {
		t.Errorf("record_name: got %q", ch.RecordName)
	}

	// 3. POST /v1/register with the challenge token. DNS bypass is
	//    on so registration completes without a real DNS lookup.
	regReq := registerRequest{
		InstanceURL:          "https://test-listing.example",
		DisplayName:          "Test Listing",
		InstancePublicKeyPEM: string(pubPEM),
		Region:               "us-west",
		Description:          "Integration-test listing.",
		Tags:                 []string{"test"},
		DNSTXTToken:          ch.Token,
	}
	regBody, _ := json.Marshal(regReq)
	regResp, err := http.Post(srv.URL+"/v1/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != 201 {
		raw, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register: got %d: %s", regResp.StatusCode, raw)
	}

	// 4. GET /v1/listing — should include our entry, signed.
	listResp, err := http.Get(srv.URL + "/v1/listing")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("listing: got %d", listResp.StatusCode)
	}
	var listing listingResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 {
		t.Fatalf("expected 1 entry; got %d", len(listing.Entries))
	}
	if listing.Entries[0].InstanceURL != "https://test-listing.example" {
		t.Errorf("instance_url: got %q", listing.Entries[0].InstanceURL)
	}
	if listing.Entries[0].VerifiedVia != "dev-bypass" {
		t.Errorf("verified_via: got %q (expected dev-bypass since we ran with skipDNS)", listing.Entries[0].VerifiedVia)
	}

	// 5. Verify the signature using the operator pubkey from
	//    /v1/operator. This is what a real subscriber would do.
	opResp, err := http.Get(srv.URL + "/v1/operator")
	if err != nil {
		t.Fatal(err)
	}
	defer opResp.Body.Close()
	var op store.Operator
	if err := json.NewDecoder(opResp.Body).Decode(&op); err != nil {
		t.Fatal(err)
	}
	opPub, err := federation.PublicKeyFromPEM([]byte(op.PublicKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalizeEntries(listing.Entries)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(listing.Directory.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(opPub, canonical, sig) {
		t.Error("listing signature did not verify against operator pubkey")
	}
}

func TestRegister_RejectsUnknownToken(t *testing.T) {
	srv, _ := newServerForTest(t)
	pub, _, _ := federation.GenerateActorKeyPair()
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	body, _ := json.Marshal(registerRequest{
		InstanceURL:          "https://other.example",
		DisplayName:          "Other",
		InstancePublicKeyPEM: string(pubPEM),
		DNSTXTToken:          "never-issued-this-token",
	})
	resp, err := http.Post(srv.URL+"/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRegister_TokenBoundToInstanceURL(t *testing.T) {
	srv, _ := newServerForTest(t)
	// Get a challenge for instance A.
	chBody, _ := json.Marshal(map[string]string{"instance_url": "https://instance-a.example"})
	chResp, _ := http.Post(srv.URL+"/v1/challenge", "application/json", bytes.NewReader(chBody))
	var ch challengeResponse
	_ = json.NewDecoder(chResp.Body).Decode(&ch)
	chResp.Body.Close()
	// Try to use it for instance B.
	pub, _, _ := federation.GenerateActorKeyPair()
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	regBody, _ := json.Marshal(registerRequest{
		InstanceURL:          "https://instance-B-different.example",
		DisplayName:          "B",
		InstancePublicKeyPEM: string(pubPEM),
		DNSTXTToken:          ch.Token,
	})
	resp, err := http.Post(srv.URL+"/v1/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("expected 401 (token bound to different URL), got %d", resp.StatusCode)
	}
}

func TestDeleteListing_RequiresBearer(t *testing.T) {
	srv, _ := newServerForTest(t)
	// Try without auth.
	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/listings/https%3A%2F%2Fanything.example", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no-bearer: expected 401, got %d", resp.StatusCode)
	}
	// With wrong bearer.
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Errorf("wrong-bearer: expected 401, got %d", resp2.StatusCode)
	}
}

// canonicalizeEntries duplicates handler logic; lets the test
// verify signatures without importing the federation package's
// internal helpers more than once.
func canonicalizeEntries(entries []store.Listing) ([]byte, error) {
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return federation.Canonicalize(raw)
}

func TestStore_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertListing(store.Listing{
		InstanceURL: "https://round.example",
		DisplayName: "Round Trip",
		VerifiedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// File should now exist + parse.
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.ListListings(10)
	if len(got) != 1 || got[0].InstanceURL != "https://round.example" {
		t.Errorf("reload mismatch: %+v", got)
	}
	// Sanity check file is valid JSON on disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var any map[string]any
	if err := json.Unmarshal(raw, &any); err != nil {
		t.Errorf("on-disk file invalid JSON: %v", err)
	}
}
