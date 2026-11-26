// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #72 origin endpoint, against a live database: the production handler,
// the #1573 policy (shares.Registry), the fs storage backend and real
// draft-cavage signatures. Skips without AA_DB_PASSWORD.

package cas_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		pwd, env("AA_DB_NAME", "artist_alley"))
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// dbPeerLookup is inbox.PeerLookup over federation_peers, keyed like the
// production adapter: <instance_url>/federation/instance#main-key.
type dbPeerLookup struct{ pool *pgxpool.Pool }

func (l dbPeerLookup) ByKeyID(ctx context.Context, keyID string) (inbox.PeerInfo, error) {
	base := strings.TrimSuffix(strings.SplitN(keyID, "#", 2)[0], "/federation/instance")
	var p inbox.PeerInfo
	var pem, status string
	var caps []string
	err := l.pool.QueryRow(ctx,
		`SELECT id, instance_url, instance_public_key, enabled, status,
		        ARRAY(SELECT jsonb_array_elements_text(capabilities))
		   FROM federation_peers WHERE instance_url = $1`, base).
		Scan(&p.ID, &p.InstanceURL, &pem, &p.Enabled, &status, &caps)
	if err != nil {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	pub, err := federation.PublicKeyFromPEM([]byte(pem))
	if err != nil {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	p.InstancePublicKey, p.Connected, p.Capabilities = pub, status == "connected", caps
	return p, nil
}

// actorKeys stands in for the #1526 resolver: a fixed actor -> key map.
type actorKeys map[string]ed25519.PublicKey

func (a actorKeys) SigningKey(_ context.Context, p inbox.PeerInfo, actorURI string) (ed25519.PublicKey, error) {
	if !inbox.ActorBoundToPeer(p.InstanceURL, actorURI) {
		return nil, inbox.ErrActorUnknown
	}
	k, ok := a[actorURI]
	if !ok {
		return nil, inbox.ErrActorUnknown
	}
	return k, nil
}

type testPeer struct {
	id   uuid.UUID
	url  string
	priv ed25519.PrivateKey
}

func (p testPeer) keyID() string { return p.url + "/federation/instance#main-key" }

type fixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	reg       *shares.Registry
	srv       *httptest.Server
	grantor   int64
	activity  uuid.UUID
	peer      testPeer // the requesting peer, negotiated
	other     testPeer // another negotiated peer, never granted
	alicePriv ed25519.PrivateKey
	aliceURI  string
	asset     uuid.UUID
	hash      string
	body      []byte
	limiter   *inbox.PeerRateLimiter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := openPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fx := &fixture{t: t, pool: pool, reg: shares.NewRegistry(pool, logger, nil)}

	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'CAS', 1) RETURNING ref`,
		"cas-"+uuid.NewString()[:8]).Scan(&fx.grantor); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source)
		 VALUES ($1, 'aa:Share', 'https://origin.example/users/x', $2, '{}'::jsonb, 'local') RETURNING id`,
		"https://origin.example/activities/"+uuid.NewString(), fx.grantor).Scan(&fx.activity); err != nil {
		t.Fatal(err)
	}
	fx.peer = fx.addPeer(`["original-transfer-1"]`, true, "connected")
	fx.other = fx.addPeer(`["original-transfer-1"]`, true, "connected")

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fx.alicePriv, fx.aliceURI = priv, fx.peer.url+"/users/alice"

	root := t.TempDir()
	be, err := fs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := storage.NewService(be, pool)
	fx.body = make([]byte, 5000)
	_, _ = rand.Read(fx.body)
	res, err := svc.UploadOriginal(ctx, bytes.NewReader(fx.body), "image/png", storage.PinRef{SubjectType: "test", SubjectID: "cas"})
	if err != nil {
		t.Fatal(err)
	}
	fx.hash = res.Hash
	fx.asset = fx.addAsset("public", fx.hash)

	fx.limiter = inbox.NewPeerRateLimiter(1000, 1000)
	h := cas.NewHandler(cas.Deps{
		Lookup:    dbPeerLookup{pool: pool},
		ActorKeys: actorKeys{fx.aliceURI: pub},
		Policy:    fx.reg,
		Bytes:     svc,
		Limiter:   fx.limiter,
		Audit:     audit.NewRecorder(pool, logger),
	})
	r := chi.NewRouter()
	h.Mount(r)
	fx.srv = httptest.NewServer(r)
	t.Cleanup(fx.srv.Close)

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE grantor_user_ref = $1`, fx.grantor)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE owner_user_ref = $1`, fx.grantor)
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = ANY($1)`, []uuid.UUID{fx.peer.id, fx.other.id})
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, fx.grantor)
		_, _ = pool.Exec(c, `DELETE FROM audit_events WHERE metadata->>'asset_id' = $1`, fx.asset.String())
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, fx.grantor)
	})
	return fx
}

func (fx *fixture) addPeer(caps string, enabled bool, status string) testPeer {
	fx.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pem, _ := federation.PublicKeyToPEM(pub)
	p := testPeer{url: "https://cas-peer-" + uuid.NewString()[:8] + ".example", priv: priv}
	if err := fx.pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'CAS peer', $2, $3, $4, $5, $6::jsonb) RETURNING id`,
		p.url, string(pem), enabled, status, fx.grantor, caps).Scan(&p.id); err != nil {
		fx.t.Fatal(err)
	}
	return p
}

func (fx *fixture) addAsset(tier, hash string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		 VALUES ($1, 'CAS', 1, $2, $3, $4, $5)`, id, fx.grantor, hash, len(fx.body), tier); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func (fx *fixture) share(asset uuid.UUID, p testPeer, allow bool, target *string, expires *time.Time) *shares.Share {
	fx.t.Helper()
	in := shares.InsertInput{
		GrantorUserRef: fx.grantor, ObjectKind: federation.ShareObjectKindAsset, ObjectID: asset,
		PeerID: p.id, TargetUserURL: target, Scope: federation.ShareScopeRemix, AllowOriginal: allow,
		GrantedActivityID: fx.activity,
	}
	if expires != nil {
		ts := pgtype.Timestamptz{Time: *expires, Valid: true}
		in.ExpiresAt = &ts
	}
	s, err := fx.reg.Insert(context.Background(), in)
	if err != nil {
		fx.t.Fatal(err)
	}
	return s
}

// call sends a signed request. keyID/priv "" means unsigned.
type call struct {
	method string
	asset  uuid.UUID
	class  string
	hash   string
	query  string
	keyID  string
	priv   ed25519.PrivateKey
	date   time.Time
	mutate func(*http.Request) // after signing
}

func (fx *fixture) do(c call) (int, []byte, http.Header) {
	fx.t.Helper()
	if c.method == "" {
		c.method = http.MethodGet
	}
	if c.asset == uuid.Nil {
		c.asset = fx.asset
	}
	if c.class == "" {
		c.class = cas.ClassOriginal
	}
	if c.hash == "" {
		c.hash = fx.hash
	}
	if c.query == "" {
		c.query = fmt.Sprintf("size=%d", len(fx.body))
	}
	req, _ := http.NewRequest(c.method, fx.srv.URL+cas.FilePath(c.asset, c.class, c.hash)+"?"+c.query, nil)
	if c.priv != nil {
		req.Header.Set("Host", req.URL.Host)
		if !c.date.IsZero() {
			req.Header.Set("Date", c.date.UTC().Format(http.TimeFormat))
		}
		if err := httpsig.SignAndAttach(req, []byte{}, c.keyID, c.priv); err != nil {
			fx.t.Fatal(err)
		}
	}
	if c.mutate != nil {
		c.mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (fx *fixture) asPeer(c call) call {
	c.keyID, c.priv = fx.peer.keyID(), fx.peer.priv
	return c
}

func reason(b []byte) string {
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	return m["reason"]
}

func (fx *fixture) lastAuditReason(t *testing.T, event string) string {
	t.Helper()
	var r string
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason' FROM audit_events WHERE event_type = $1 AND metadata->>'asset_id' = $2
		 ORDER BY occurred_at DESC LIMIT 1`, event, fx.asset.String()).Scan(&r)
	return r
}

func TestCAS_ServesGrantedOriginal_GETAndHEAD(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, nil, nil)
	status, body, h := fx.do(fx.asPeer(call{}))
	if status != 200 || !bytes.Equal(body, fx.body) {
		t.Fatalf("GET %d, %d bytes", status, len(body))
	}
	sum := sha256.Sum256(fx.body)
	if h.Get("Content-Length") != fmt.Sprint(len(fx.body)) || !strings.HasPrefix(h.Get("Repr-Digest"), "sha-256=:") ||
		hex.EncodeToString(sum[:]) != fx.hash {
		t.Fatalf("headers %v", h)
	}
	status, body, h = fx.do(fx.asPeer(call{method: http.MethodHead}))
	if status != 200 || len(body) != 0 || h.Get("Content-Length") != fmt.Sprint(len(fx.body)) {
		t.Fatalf("HEAD %d body %d headers %v", status, len(body), h)
	}
	var served int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE event_type = 'federation.cas.served' AND metadata->>'asset_id' = $1`,
		fx.asset.String()).Scan(&served)
	if served != 2 {
		t.Fatalf("served audit rows = %d, want 2", served)
	}
}

func TestCAS_Refusals(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, nil, nil)
	size := len(fx.body)
	cases := []struct {
		name   string
		c      call
		status int
		reason string
	}{
		{"unsigned", call{}, 401, cas.ReasonUnauthenticated},
		{"stale Date", fx.asPeer(call{date: time.Now().Add(-10 * time.Minute)}), 401, cas.ReasonUnauthenticated},
		{"unknown key", call{keyID: "https://nobody.example/federation/instance#main-key", priv: fx.peer.priv}, 401, cas.ReasonUnauthenticated},
		{"signature over another size", fx.asPeer(call{mutate: func(r *http.Request) {
			r.URL.RawQuery = fmt.Sprintf("size=%d", size+1)
		}}), 401, cas.ReasonUnauthenticated},
		{"wrong key for the peer", call{keyID: fx.peer.keyID(), priv: fx.other.priv}, 401, cas.ReasonUnauthenticated},
		{"class access", fx.asPeer(call{class: "access"}), 403, cas.ReasonClassNotAvailable},
		{"class preview", fx.asPeer(call{class: "preview"}), 403, cas.ReasonClassNotAvailable},
		{"unknown class", fx.asPeer(call{class: "thumbnail"}), 400, cas.ReasonBadRequest},
		{"no size", fx.asPeer(call{query: "x=1"}), 400, cas.ReasonBadRequest},
		{"extra query", fx.asPeer(call{query: fmt.Sprintf("size=%d&x=1", size)}), 400, cas.ReasonBadRequest},
		{"non-canonical size", fx.asPeer(call{query: fmt.Sprintf("size=0%d", size)}), 400, cas.ReasonBadRequest},
		{"wrong size", fx.asPeer(call{query: fmt.Sprintf("size=%d", size+1)}), 403, cas.ReasonNotAvailable},
		{"wrong hash", fx.asPeer(call{hash: strings.Repeat("a", 64)}), 403, cas.ReasonNotAvailable},
		{"ungranted peer", call{keyID: fx.other.keyID(), priv: fx.other.priv}, 403, cas.ReasonNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, m := range []string{http.MethodGet, http.MethodHead} {
				c := tc.c
				c.method = m
				status, body, _ := fx.do(c)
				if status != tc.status {
					t.Fatalf("%s: status %d, want %d", m, status, tc.status)
				}
				if m == http.MethodGet && reason(body) != tc.reason {
					t.Fatalf("GET reason %q, want %q", reason(body), tc.reason)
				}
				if m == http.MethodHead && len(body) != 0 {
					t.Fatalf("HEAD carried a body")
				}
			}
		})
	}
}

// Scope never implies the original; expiry and revocation end it.
func TestCAS_GrantStates(t *testing.T) {
	fx := newFixture(t)
	viewOnly := fx.share(fx.asset, fx.peer, false, nil, nil)
	if s, b, _ := fx.do(fx.asPeer(call{})); s != 403 || reason(b) != cas.ReasonNotAvailable {
		t.Fatalf("remix without allow_original: %d %s", s, b)
	}
	if got := fx.lastAuditReason(t, "federation.cas.refused"); got != "original_not_granted" {
		t.Fatalf("audit reason %q", got)
	}
	if _, err := fx.reg.Revoke(context.Background(), viewOnly.ID, fx.activity); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	fx.share(fx.asset, fx.peer, true, nil, &past)
	if s, _, _ := fx.do(fx.asPeer(call{})); s != 403 {
		t.Fatalf("expired grant served: %d", s)
	}
	if got := fx.lastAuditReason(t, "federation.cas.refused"); got != "expired" {
		t.Fatalf("audit reason %q", got)
	}
}

func TestCAS_RevokedGrant(t *testing.T) {
	fx := newFixture(t)
	s := fx.share(fx.asset, fx.peer, true, nil, nil)
	if st, _, _ := fx.do(fx.asPeer(call{})); st != 200 {
		t.Fatalf("before revoke %d", st)
	}
	if _, err := fx.reg.Revoke(context.Background(), s.ID, fx.activity); err != nil {
		t.Fatal(err)
	}
	if st, b, _ := fx.do(fx.asPeer(call{})); st != 403 || reason(b) != cas.ReasonNotAvailable {
		t.Fatalf("after revoke %d %s", st, b)
	}
}

// A user-scoped grant: only that actor, signing with its own key.
func TestCAS_ActorScopedGrant(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, &fx.aliceURI, nil)
	if s, b, _ := fx.do(call{keyID: fx.aliceURI + "#main-key", priv: fx.alicePriv}); s != 200 || !bytes.Equal(b, fx.body) {
		t.Fatalf("alice: %d", s)
	}
	if s, b, _ := fx.do(fx.asPeer(call{})); s != 403 || reason(b) != cas.ReasonNotAvailable {
		t.Fatalf("instance-signed on a user-scoped grant: %d %s", s, b)
	}
	if got := fx.lastAuditReason(t, "federation.cas.refused"); got != "wrong_user" {
		t.Fatalf("audit reason %q", got)
	}
	// Another actor of the same peer has no pinned key here: 401.
	bob := fx.peer.url + "/users/bob"
	if s, _, _ := fx.do(call{keyID: bob + "#main-key", priv: fx.alicePriv}); s != 401 {
		t.Fatalf("bob: %d", s)
	}
	// Alice's key with an actor id on another origin.
	if s, _, _ := fx.do(call{keyID: "https://elsewhere.example/users/alice#main-key", priv: fx.alicePriv}); s != 401 {
		t.Fatalf("foreign-origin actor: %d", s)
	}
}

func TestCAS_TierRefused(t *testing.T) {
	for _, tier := range []string{"restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			fx := newFixture(t)
			if _, err := fx.pool.Exec(context.Background(), `UPDATE assets SET sensitivity = $2 WHERE id = $1`, fx.asset, tier); err != nil {
				t.Fatal(err)
			}
			fx.share(fx.asset, fx.peer, true, nil, nil)
			if s, b, _ := fx.do(fx.asPeer(call{})); s != 403 || reason(b) != cas.ReasonNotAvailable {
				t.Fatalf("%d %s", s, b)
			}
			if got := fx.lastAuditReason(t, "federation.cas.refused"); got != "sensitivity_refused" {
				t.Fatalf("audit reason %q", got)
			}
		})
	}
}

// Unknown asset and existing-but-ungranted asset answer identically.
func TestCAS_NoExistenceOracle(t *testing.T) {
	fx := newFixture(t)
	s1, b1, h1 := fx.do(fx.asPeer(call{}))                  // exists, no grant
	s2, b2, h2 := fx.do(fx.asPeer(call{asset: uuid.New()})) // does not exist
	if s1 != s2 || !bytes.Equal(b1, b2) || h1.Get("Content-Type") != h2.Get("Content-Type") {
		t.Fatalf("responses differ: %d %q vs %d %q", s1, b1, s2, b2)
	}
	if s1 != 403 || reason(b1) != cas.ReasonNotAvailable {
		t.Fatalf("%d %s", s1, b1)
	}
}

func TestCAS_ProfileAndPeerState(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, nil, nil)
	set := func(sql string) {
		t.Helper()
		if _, err := fx.pool.Exec(context.Background(), sql, fx.peer.id); err != nil {
			t.Fatal(err)
		}
	}
	set(`UPDATE federation_peers SET capabilities = '[]'::jsonb WHERE id = $1`)
	if s, b, _ := fx.do(fx.asPeer(call{})); s != 403 || reason(b) != cas.ReasonProfileNotNegotiated {
		t.Fatalf("no profile: %d %s", s, b)
	}
	set(`UPDATE federation_peers SET capabilities = '["original-transfer-1"]'::jsonb, enabled = FALSE WHERE id = $1`)
	if s, _, _ := fx.do(fx.asPeer(call{})); s != 401 {
		t.Fatalf("disabled peer: %d", s)
	}
	set(`UPDATE federation_peers SET enabled = TRUE, status = 'pending_outbound' WHERE id = $1`)
	if s, _, _ := fx.do(fx.asPeer(call{})); s != 401 {
		t.Fatalf("unconnected peer: %d", s)
	}
}

func TestCAS_RateLimited(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, nil, nil)
	*fx.limiter = *inbox.NewPeerRateLimiter(1, 0.001)
	if s, _, _ := fx.do(fx.asPeer(call{})); s != 200 {
		t.Fatalf("first: %d", s)
	}
	s, b, h := fx.do(fx.asPeer(call{}))
	if s != 429 || reason(b) != cas.ReasonRateLimited || h.Get("Retry-After") == "" {
		t.Fatalf("second: %d %s %v", s, b, h)
	}
}

// The policy is the only door: a policy error is a 503, never bytes.
func TestCAS_PolicyErrorServesNothing(t *testing.T) {
	fx := newFixture(t)
	fx.share(fx.asset, fx.peer, true, nil, nil)
	h := cas.NewHandler(cas.Deps{
		Lookup: dbPeerLookup{pool: fx.pool},
		Policy: failingPolicy{},
		Bytes:  nil,
	})
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	fx.srv = srv
	if s, b, _ := fx.do(fx.asPeer(call{})); s != 503 || len(b) == 0 || bytes.Equal(b, fx.body) {
		t.Fatalf("%d %q", s, b)
	}
}

type failingPolicy struct{}

func (failingPolicy) CanPeerFetchOriginal(context.Context, shares.OriginalFetchRequest) (shares.OriginalFetchDecision, error) {
	return shares.OriginalFetchDecision{}, errors.New("database unavailable")
}

func (failingPolicy) CanPeerFetchAccessCopy(context.Context, shares.AccessCopyFetchRequest) (shares.AccessCopyFetchDecision, error) {
	return shares.AccessCopyFetchDecision{}, errors.New("database unavailable")
}

var _ = peer.CapOriginalTransfer1
