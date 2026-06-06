// Tests for peer-of-peer discovery — Phase 1.22.B-d.
// Coverage:
//   - VisibleSnapshot returns only opted-in peers (share_in_visible_list)
//   - RefreshFromSource walks the source's /visible + persists
//   - ListSuggestions dedups against our own federation_peers
//   - Per-source-id cache slot invalidates on refresh
//
// Skips without AA_DB_PASSWORD per project convention.

package p2p_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/mscrnt/artist-alley/app/internal/federation/p2p"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

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
	username := "p2p-test-admin-" + randHex(t, 4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "P2P Test Admin",
	).Scan(&ref); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peer_suggestions WHERE source_peer_id IN (SELECT id FROM federation_peers WHERE handshake_by_user_ref=$1)`, ref)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE handshake_by_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
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

// rewriteTransport routes https:// fake URLs to a local httptest
// server so peer URL validation (https-only) passes while the
// actual transport targets the stub.
type rewriteTransport struct{ base *url.URL }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.base.Scheme
	r2.URL.Host = rt.base.Host
	r2.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func rewriteClient(t *testing.T, target string) *p2p.Client {
	t.Helper()
	base, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	c := p2p.NewClient()
	c.HTTP = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{base: base},
	}
	return c
}

// stubVisibleServer answers GET /federation/peers/visible with a
// canned response.
func stubVisibleServer(t *testing.T, peers []map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/federation/peers/visible", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"peers": peers,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestVisibleSnapshot_FiltersByShareToggle(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()
	peerReg := peer.NewRegistry(pool, logger, regCache)

	// Add two peers — one with share_in_visible_list TRUE, one FALSE.
	shareTrue := true
	shareFalse := false
	pVisible, err := peerReg.Add(ctx, peer.AddInput{
		InstanceURL:         "https://visible-" + randHex(t, 4) + ".example",
		DisplayName:         "Visible Peer",
		InstancePublicKey:   freshPEM(t),
		TrustTier:           federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = peerReg.Update(ctx, pVisible.ID, peer.UpdateInput{
		Enabled:            &shareTrue,
		ShareInVisibleList: &shareTrue,
	})
	if err != nil {
		t.Fatal(err)
	}

	pHidden, err := peerReg.Add(ctx, peer.AddInput{
		InstanceURL:         "https://hidden-" + randHex(t, 4) + ".example",
		DisplayName:         "Hidden Peer",
		InstancePublicKey:   freshPEM(t),
		TrustTier:           federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = peerReg.Update(ctx, pHidden.ID, peer.UpdateInput{
		Enabled:            &shareTrue,
		ShareInVisibleList: &shareFalse,
	})

	visible, err := peerReg.VisibleSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundVisible, foundHidden := false, false
	for _, p := range visible {
		if p.InstanceURL == pVisible.InstanceURL {
			foundVisible = true
		}
		if p.InstanceURL == pHidden.InstanceURL {
			foundHidden = true
		}
	}
	if !foundVisible {
		t.Error("opted-in peer should appear in VisibleSnapshot")
	}
	if foundHidden {
		t.Error("non-opted peer should NOT appear in VisibleSnapshot")
	}
}

func TestRefreshFromSource_PersistsAndDedupsOnReFetch(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()
	peerReg := peer.NewRegistry(pool, logger, regCache)
	p2pReg := p2p.NewRegistry(pool, logger, peerReg, regCache)

	// Add a source peer.
	source, err := peerReg.Add(ctx, peer.AddInput{
		InstanceURL:        "https://source-" + randHex(t, 4) + ".example",
		DisplayName:        "Source Peer",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: source advertises 2 peers.
	srv1 := stubVisibleServer(t, []map[string]string{
		{"instance_url": "https://sug-a.example", "display_name": "Sug A",
			"instance_public_key": freshPEM(t), "fingerprint": "fa"},
		{"instance_url": "https://sug-b.example", "display_name": "Sug B",
			"instance_public_key": freshPEM(t), "fingerprint": "fb"},
	})
	client1 := rewriteClient(t, srv1.URL)
	count, err := client1.RefreshFromSource(ctx, p2pReg, source)
	if err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	if count != 2 {
		t.Errorf("count after round 1: got %d, want 2", count)
	}

	// Round 2: source now advertises only sug-b + sug-c (dropped
	// sug-a). The refresh should prune sug-a + upsert sug-c.
	srv2 := stubVisibleServer(t, []map[string]string{
		{"instance_url": "https://sug-b.example", "display_name": "Sug B",
			"instance_public_key": freshPEM(t), "fingerprint": "fb"},
		{"instance_url": "https://sug-c.example", "display_name": "Sug C",
			"instance_public_key": freshPEM(t), "fingerprint": "fc"},
	})
	client2 := rewriteClient(t, srv2.URL)
	count2, err := client2.RefreshFromSource(ctx, p2pReg, source)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != 2 {
		t.Errorf("count after round 2: got %d, want 2", count2)
	}

	// Verify state: only B + C remain.
	suggestions, err := p2pReg.ListSuggestions(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	urlsSeen := map[string]bool{}
	for _, s := range suggestions {
		urlsSeen[s.SuggestedURL] = true
	}
	if urlsSeen["https://sug-a.example"] {
		t.Error("sug-a should have been pruned after round 2")
	}
	if !urlsSeen["https://sug-b.example"] || !urlsSeen["https://sug-c.example"] {
		t.Errorf("missing B or C: %v", urlsSeen)
	}
}

func TestListSuggestions_DedupsAgainstOwnPeers(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := fixtureAdmin(t, ctx, pool)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	defer regCache.Stop()
	peerReg := peer.NewRegistry(pool, logger, regCache)
	p2pReg := p2p.NewRegistry(pool, logger, peerReg, regCache)

	source, err := peerReg.Add(ctx, peer.AddInput{
		InstanceURL:        "https://dedup-source-" + randHex(t, 4) + ".example",
		DisplayName:        "Dedup Source",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Add a peer we already federate with.
	alreadyURL := "https://already-paired-" + randHex(t, 4) + ".example"
	_, err = peerReg.Add(ctx, peer.AddInput{
		InstanceURL:        alreadyURL,
		DisplayName:        "Already Paired",
		InstancePublicKey:  freshPEM(t),
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: admin,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Source advertises a list that INCLUDES the already-paired URL.
	srv := stubVisibleServer(t, []map[string]string{
		{"instance_url": alreadyURL, "display_name": "Already Paired",
			"instance_public_key": freshPEM(t), "fingerprint": "fap"},
		{"instance_url": "https://genuinely-new-" + randHex(t, 4) + ".example",
			"display_name": "Genuinely New",
			"instance_public_key": freshPEM(t), "fingerprint": "fnew"},
	})
	client := rewriteClient(t, srv.URL)
	if _, err := client.RefreshFromSource(ctx, p2pReg, source); err != nil {
		t.Fatal(err)
	}

	// ListSuggestions should EXCLUDE the already-paired URL.
	suggestions, err := p2pReg.ListSuggestions(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suggestions {
		if s.SuggestedURL == alreadyURL {
			t.Errorf("dedup failed: already-paired URL %q in suggestions", alreadyURL)
		}
	}
	// The genuinely-new one should be there with the right
	// provenance. Filter to suggestions FROM THIS test's source
	// to be robust against stale rows left by prior runs (the
	// suggestions table is global; ListSuggestions doesn't
	// filter by source).
	var foundForThisSource *p2p.Suggestion
	for i := range suggestions {
		s := &suggestions[i]
		if s.SourcePeerID != source.ID {
			continue
		}
		if s.SuggestedDisplayName == "Genuinely New" {
			foundForThisSource = s
			break
		}
	}
	if foundForThisSource == nil {
		t.Fatal("genuinely-new suggestion missing for this test's source")
	}
	if foundForThisSource.SourceURL != source.InstanceURL {
		t.Errorf("source provenance: got %q want %q",
			foundForThisSource.SourceURL, source.InstanceURL)
	}
}
