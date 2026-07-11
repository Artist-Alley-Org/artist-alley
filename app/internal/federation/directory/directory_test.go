// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Integration tests for the subscriber side — Phase 1.22.B-c.
// Coverage:
//   - URL normalization (https only, no path, trailing-slash strip)
//   - FetchOperator hits /v1/operator, parses + rejects spec mismatch
//   - Poll: full happy path — signature verifies, entries persist,
//     status flips to OK
//   - Poll: signature failure leaves cached entries in place per
//     the spec's local-caching rule
//   - Unsubscribe cascades entries
//   - Cache: ListEntries cache-hits after a Poll
//
// Skips without AA_DB_PASSWORD per project convention.

package directory_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/directory"
)

// rewriteTransport routes https:// fake URLs to a local
// httptest server so the spec-mandated https-only validation
// passes while we test against real handlers.
type rewriteTransport struct{ base *url.URL }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.base.Scheme
	r2.URL.Host = rt.base.Host
	r2.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// rewriteClient returns a Client whose HTTP layer rewrites every
// request to target the given httptest.Server.
func rewriteClient(t *testing.T, logger *slog.Logger, target string) *directory.Client {
	t.Helper()
	base, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	c := directory.NewClient(logger)
	c.HTTP = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &rewriteTransport{base: base},
	}
	return c
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func fixtureAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	username := "dir-test-admin-" + randHex(t, 4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Dir Test Admin",
	).Scan(&ref); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_directories WHERE subscribed_by_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// fakeDirectoryServer is a minimal httptest-backed mock that
// answers /v1/operator + /v1/listing with a signed payload
// using a real Ed25519 keypair. Signature uses RFC 8785
// canonical JSON the same way the reference server does — the
// test confirms the subscriber's verification matches.
type fakeDirectoryServer struct {
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	pubPEM  []byte
	entries []map[string]any
	tamper  bool // when true, emit a bad signature
}

func newFakeDirectoryServer(t *testing.T, entries []map[string]any) (*fakeDirectoryServer, *httptest.Server) {
	t.Helper()
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	fk := &fakeDirectoryServer{
		pub:     pub,
		priv:    priv,
		pubPEM:  pubPEM,
		entries: entries,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/operator", fk.handleOperator)
	mux.HandleFunc("/v1/listing", fk.handleListing)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fk, srv
}

func (f *fakeDirectoryServer) handleOperator(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":           "Fake Directory",
		"operator_url":   "https://fake-dir.example",
		"contact":        "ops@fake-dir.example",
		"spec_version":   "aa-directory/v1",
		"public_key_pem": string(f.pubPEM),
		"fingerprint":    federation.PublicKeyFingerprint(f.pub),
	})
}

func (f *fakeDirectoryServer) handleListing(w http.ResponseWriter, _ *http.Request) {
	rawEntries, _ := json.Marshal(f.entries)
	canonical, _ := federation.Canonicalize(rawEntries)
	sig := ed25519.Sign(f.priv, canonical)
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	if f.tamper {
		// Flip a byte so verification fails.
		bad := append([]byte{}, sig...)
		bad[0] ^= 0xFF
		sigB64 = base64.StdEncoding.EncodeToString(bad)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"directory": map[string]any{
			"name":           "Fake Directory",
			"operator_url":   "https://fake-dir.example",
			"spec_version":   "aa-directory/v1",
			"generated_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"signature":      sigB64,
			"public_key_pem": string(f.pubPEM),
		},
		"entries":     f.entries,
		"next_cursor": nil,
	})
}

func TestNormalizeDirectoryURL_AcceptsValid(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	r := directory.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, url := range []string{
		"https://dir.example",
		"https://dir.example/",
		"  https://dir.example  ",
	} {
		d, err := r.Subscribe(ctx, directory.SubscribeInput{
			URL:                 url,
			OperatorName:        "X",
			OperatorPublicKey:   freshPEM(t),
			OperatorFingerprint: "abc",
			SubscribedByUserRef: admin,
		})
		if err != nil {
			t.Errorf("URL %q: %v", url, err)
			continue
		}
		if d.URL != "https://dir.example" {
			t.Errorf("URL %q normalized to %q", url, d.URL)
		}
		_ = r.Unsubscribe(ctx, d.ID)
	}
}

func TestNormalizeDirectoryURL_Rejects(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	r := directory.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, url := range []string{
		"http://nottls.example",
		"https://dir.example/api",
		"https://",
		"",
	} {
		_, err := r.Subscribe(ctx, directory.SubscribeInput{
			URL:                 url,
			OperatorPublicKey:   freshPEM(t),
			SubscribedByUserRef: admin,
		})
		if !errors.Is(err, directory.ErrInvalidURL) {
			t.Errorf("URL %q: expected ErrInvalidURL, got %v", url, err)
		}
	}
}

func TestFetchOperator_ParsesAndRejectsSpecMismatch(t *testing.T) {
	_, srv := newFakeDirectoryServer(t, nil)
	c := directory.NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	op, err := c.FetchOperator(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if op.Name != "Fake Directory" {
		t.Errorf("name: %q", op.Name)
	}
	if op.SpecVersion != "aa-directory/v1" {
		t.Errorf("spec_version: %q", op.SpecVersion)
	}

	// Bad spec version → ErrSpecMismatch.
	badMux := http.NewServeMux()
	badMux.HandleFunc("/v1/operator", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"spec_version":   "aa-directory/v99",
			"public_key_pem": "x",
			"fingerprint":    "x",
			"name":           "Bad",
			"operator_url":   "https://bad.example",
		})
	})
	badSrv := httptest.NewServer(badMux)
	defer badSrv.Close()
	_, err = c.FetchOperator(context.Background(), badSrv.URL)
	if !errors.Is(err, directory.ErrSpecMismatch) {
		t.Errorf("expected ErrSpecMismatch, got %v", err)
	}
}

func TestPoll_HappyPath_PersistsEntries(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	entries := []map[string]any{
		{
			"instance_url":            "https://entry-a.example",
			"display_name":            "Entry A",
			"instance_public_key_pem": freshPEM(t),
			"fingerprint":             "fingerA",
			"region":                  "us-west",
			"verified_at":             time.Now().UTC().Format(time.RFC3339Nano),
			"verified_via":            "dns-txt",
			"listing_id":              "L1",
		},
		{
			"instance_url":            "https://entry-b.example",
			"display_name":            "Entry B",
			"instance_public_key_pem": freshPEM(t),
			"fingerprint":             "fingerB",
			"verified_at":             time.Now().UTC().Format(time.RFC3339Nano),
			"verified_via":            "dns-txt",
			"listing_id":              "L2",
		},
	}
	fake, srv := newFakeDirectoryServer(t, entries)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()
	reg := directory.NewRegistry(pool, logger, regCache)

	fakeURL := "https://fake-happy-" + randHex(t, 4) + ".example"
	d, err := reg.Subscribe(ctx, directory.SubscribeInput{
		URL:                 fakeURL,
		OperatorName:        "Fake",
		OperatorPublicKey:   string(fake.pubPEM),
		OperatorFingerprint: federation.PublicKeyFingerprint(fake.pub),
		SubscribedByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Unsubscribe(context.Background(), d.ID) })

	client := rewriteClient(t, logger, srv.URL)
	if err := client.Poll(ctx, reg, d); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	got, err := reg.ListEntries(ctx, d.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	urlSeen := map[string]bool{}
	for _, e := range got {
		urlSeen[e.InstanceURL] = true
	}
	if !urlSeen["https://entry-a.example"] || !urlSeen["https://entry-b.example"] {
		t.Errorf("missing entries: %+v", urlSeen)
	}
	// Status should be OK.
	d2, err := reg.ByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d2.LastPollStatus != directory.PollStatusOK {
		t.Errorf("last_poll_status: got %q, want ok", d2.LastPollStatus)
	}
}

func TestPoll_TamperedSignature_PreservesCachedEntries(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	entries := []map[string]any{{
		"instance_url":            "https://entry-cached.example",
		"display_name":            "Cached",
		"instance_public_key_pem": freshPEM(t),
		"fingerprint":             "f1",
		"verified_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"verified_via":            "dns-txt",
	}}
	fake, srv := newFakeDirectoryServer(t, entries)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()
	reg := directory.NewRegistry(pool, logger, regCache)

	fakeURL := "https://fake-tamper-" + randHex(t, 4) + ".example"
	d, err := reg.Subscribe(ctx, directory.SubscribeInput{
		URL:                 fakeURL,
		OperatorName:        "Fake",
		OperatorPublicKey:   string(fake.pubPEM),
		OperatorFingerprint: federation.PublicKeyFingerprint(fake.pub),
		SubscribedByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Unsubscribe(context.Background(), d.ID) })

	client := rewriteClient(t, logger, srv.URL)
	// First poll: success.
	if err := client.Poll(ctx, reg, d); err != nil {
		t.Fatal(err)
	}
	// Second poll: tampered signature.
	fake.tamper = true
	err = client.Poll(ctx, reg, d)
	if err == nil {
		t.Error("expected error on tampered signature poll")
	}
	// Cached entries SHOULD still be there per the spec's local-
	// caching rule.
	got, err := reg.ListEntries(ctx, d.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("expected cached entries to survive failed poll; got %d", len(got))
	}
	// Status should be signature_failed.
	d2, _ := reg.ByID(ctx, d.ID)
	if d2.LastPollStatus != directory.PollStatusSignatureFailed {
		t.Errorf("last_poll_status: got %q, want signature_failed", d2.LastPollStatus)
	}
}

func TestUnsubscribe_CascadesEntries(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	entries := []map[string]any{{
		"instance_url":            "https://cascade.example",
		"display_name":            "C",
		"instance_public_key_pem": freshPEM(t),
		"fingerprint":             "fc",
		"verified_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"verified_via":            "dns-txt",
	}}
	fake, srv := newFakeDirectoryServer(t, entries)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := directory.NewRegistry(pool, logger, nil)
	fakeURL := "https://fake-cascade-" + randHex(t, 4) + ".example"
	d, err := reg.Subscribe(ctx, directory.SubscribeInput{
		URL:                 fakeURL,
		OperatorName:        "Fake",
		OperatorPublicKey:   string(fake.pubPEM),
		OperatorFingerprint: federation.PublicKeyFingerprint(fake.pub),
		SubscribedByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := rewriteClient(t, logger, srv.URL)
	if err := client.Poll(ctx, reg, d); err != nil {
		t.Fatal(err)
	}
	if err := reg.Unsubscribe(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	// Entries should be gone (CASCADE on FK).
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM federation_directory_entries WHERE directory_id = $1`,
		d.ID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected cascade to remove entries; got %d remaining", count)
	}
}

func freshPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem)
}
