// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #72 receiver client against a live database, the fs storage backend
// and a controllable origin stub. The real two-instance proof is
// internal/federation/twostack. Skips without AA_DB_PASSWORD.

package casfetch_test

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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

const localBase = "https://receiver.example"

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
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		pwd, env("AA_DB_NAME", "artist_alley")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// origin is a controllable stand-in for a peer's CAS endpoint. The real
// origin is covered in package cas and by the twostack gate.
type origin struct {
	srv       *httptest.Server
	body      []byte
	heads     atomic.Int32
	gets      atomic.Int32
	followed  atomic.Int32
	headCode  atomic.Int32 // 0 = 200
	headDeny  func(r *http.Request) bool
	getMode   atomic.Value // string
	getDelay  time.Duration
	beforeGET func()
	afterGET  func()
	// pub, when set, is the key every signed request is verified
	// against; the outcome is in seen.
	pub       ed25519.PublicKey
	mu        sync.Mutex
	lastKeyID string
	seen      []seenRequest
}

// seenRequest is one signed request as the origin received it.
type seenRequest struct {
	Method, Path, RawQuery string
	KeyID                  string // verified key id; "" when pub is unset or verification failed
	VerifyErr              error
}

func (o *origin) record(r *http.Request) {
	sr := seenRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	if o.pub != nil {
		pub := o.pub
		sr.KeyID, sr.VerifyErr = httpsig.Verify(r, []byte{}, func(string) (ed25519.PublicKey, error) { return pub, nil }, time.Now())
	}
	o.mu.Lock()
	o.seen = append(o.seen, sr)
	o.mu.Unlock()
}

func (o *origin) requests() []seenRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]seenRequest(nil), o.seen...)
}

func newOrigin(t *testing.T, body []byte) *origin {
	o := &origin{body: body}
	o.getMode.Store("ok")
	o.srv = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *origin) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/elsewhere" {
		o.followed.Add(1)
		_, _ = w.Write(o.body)
		return
	}
	if r.Header.Get("Signature") == "" || r.URL.Query().Get("size") == "" {
		w.WriteHeader(401)
		return
	}
	if r.URL.Query().Get("size") != fmt.Sprint(len(o.body)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "not_available"})
		return
	}
	o.mu.Lock()
	o.lastKeyID = r.Header.Get("Signature")
	o.mu.Unlock()
	o.record(r)
	deny := func(code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "not_available"})
	}
	if r.Method == http.MethodHead {
		o.heads.Add(1)
		if o.headDeny != nil && o.headDeny(r) {
			deny(403)
			return
		}
		switch c := o.headCode.Load(); c {
		case 0:
			w.Header().Set("Content-Length", fmt.Sprint(len(o.body)))
			w.WriteHeader(200)
		case 302:
			http.Redirect(w, r, o.srv.URL+"/elsewhere", http.StatusFound)
		default:
			deny(int(c))
		}
		return
	}
	o.gets.Add(1)
	if o.beforeGET != nil {
		o.beforeGET()
	}
	if o.afterGET != nil {
		defer o.afterGET()
	}
	if o.getDelay > 0 {
		time.Sleep(o.getDelay)
	}
	flusher := w.(http.Flusher)
	switch o.getMode.Load().(string) {
	case "ok":
		w.Header().Set("Content-Length", fmt.Sprint(len(o.body)))
		_, _ = w.Write(o.body)
	case "redirect":
		http.Redirect(w, r, o.srv.URL+"/elsewhere", http.StatusFound)
	case "oversize":
		_, _ = w.Write(o.body)
		flusher.Flush()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 10))
	case "short":
		_, _ = w.Write(o.body[:len(o.body)/2])
	case "tamper":
		b := bytes.Clone(o.body)
		b[0] ^= 0xff
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	case "length":
		w.Header().Set("Content-Length", fmt.Sprint(len(o.body)+1))
		_, _ = w.Write(append(bytes.Clone(o.body), 'x'))
	case "stall":
		_, _ = w.Write(o.body[:len(o.body)/2])
		flusher.Flush()
		time.Sleep(2 * time.Second)
		_, _ = w.Write(o.body[len(o.body)/2:])
	}
}

// grants is a scripted InboundGrants (the database record, #1571, is
// inboundgrants.Store, exercised in its own package and in twostack).
type grants struct {
	mu    sync.Mutex
	allow bool
	deny  func(q casfetch.GrantQuery) bool
	calls atomic.Int32
}

func (g *grants) Allows(_ context.Context, q casfetch.GrantQuery) error {
	g.calls.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.allow || (g.deny != nil && g.deny(q)) {
		return casfetch.ErrNoInboundGrant
	}
	return nil
}

// AllowsTx answers like Allows: this fixture has no rows to hold.
func (g *grants) AllowsTx(ctx context.Context, _ pgx.Tx, q casfetch.GrantQuery) error {
	return g.Allows(ctx, q)
}

func (g *grants) set(v bool) { g.mu.Lock(); g.allow = v; g.mu.Unlock() }

// peers reads the receiver's peer row on every call, so a change made
// by a test mid-flight is what the client sees next.
type peers struct{ pool *pgxpool.Pool }

func (p peers) ByID(ctx context.Context, id uuid.UUID) (casfetch.Peer, error) {
	var out casfetch.Peer
	var status string
	err := p.pool.QueryRow(ctx, `SELECT id, instance_url, enabled, status,
	        ARRAY(SELECT jsonb_array_elements_text(capabilities))
	   FROM federation_peers WHERE id = $1`, id).
		Scan(&out.ID, &out.InstanceURL, &out.Enabled, &status, &out.Capabilities)
	if err != nil {
		return casfetch.Peer{}, errors.New("no such peer")
	}
	out.Connected = status == "connected"
	return out, nil
}

// gatedBackend wraps the fs backend; beforePut (if set) runs inside Put.
type gatedBackend struct {
	storage.Backend
	beforePut atomic.Pointer[func()]
	beforeGet atomic.Pointer[func()]
}

func (g *gatedBackend) Get(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	if f := g.beforeGet.Load(); f != nil {
		(*f)()
	}
	return g.Backend.Get(ctx, hash, variant)
}

func (g *gatedBackend) setBeforeGet(f func()) {
	if f == nil {
		g.beforeGet.Store(nil)
		return
	}
	g.beforeGet.Store(&f)
}

func (g *gatedBackend) Put(ctx context.Context, hash, variant string, r io.Reader) (*storage.ObjectInfo, error) {
	if f := g.beforePut.Load(); f != nil {
		(*f)()
	}
	return g.Backend.Put(ctx, hash, variant, r)
}

func (g *gatedBackend) setBeforePut(f func()) {
	if f == nil {
		g.beforePut.Store(nil)
		return
	}
	g.beforePut.Store(&f)
}

type fx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *storage.Service
	origin *origin
	grants *grants
	client *casfetch.Client
	peer   casfetch.Peer
	asset  uuid.UUID
	hash   string
	body   []byte
	tmp    string
	signer casfetch.Signer
	cfg    casfetch.Config
	be     *gatedBackend
}

func newFx(t *testing.T, mut ...func(*casfetch.Config)) *fx {
	t.Helper()
	ctx := context.Background()
	pool := openPool(t)
	body := make([]byte, 64<<10)
	_, _ = rand.Read(body)
	sum := sha256.Sum256(body)
	f := &fx{t: t, pool: pool, body: body, hash: hex.EncodeToString(sum[:]), asset: uuid.New(), tmp: t.TempDir()}
	f.origin = newOrigin(t, body)
	fsb, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.be = &gatedBackend{Backend: fsb}
	f.svc = storage.NewService(f.be, pool)

	var ref int64
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'casfetch', 1) RETURNING ref`,
		"cf-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'origin', 'k', TRUE, 'connected', $2, '["original-transfer-1"]'::jsonb) RETURNING id`,
		f.origin.srv.URL, ref).Scan(&f.peer.ID); err != nil {
		t.Fatal(err)
	}
	f.peer.InstanceURL, f.peer.Enabled, f.peer.Connected = f.origin.srv.URL, true, true
	f.peer.Capabilities = []string{"original-transfer-1"}
	f.grants = &grants{allow: true}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.signer = casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/federation/instance#main-key"})
	f.cfg = casfetch.Config{
		IdleTimeout: 500 * time.Millisecond, MinOverall: 5 * time.Second, LeaseTTL: 2 * time.Second,
		Heartbeat: 500 * time.Millisecond, WaitPoll: 50 * time.Millisecond, TempDir: f.tmp,
		LocalBaseURL: func(context.Context) string { return localBase },
	}
	for _, m := range mut {
		m(&f.cfg)
	}
	f.client = f.newClient(f.grants)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, f.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, f.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, f.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_import_intents WHERE object_hash = $1`, f.hash)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, f.peer.ID)
		_, _ = pool.Exec(c, `DELETE FROM audit_events WHERE metadata->>'asset_id' = $1`, f.asset.String())
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return f
}

func (f *fx) newClient(g casfetch.InboundGrants) *casfetch.Client {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return casfetch.New(casfetch.Deps{
		Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: g,
		Audit: audit.NewRecorder(f.pool, logger), Logger: logger, Config: f.cfg,
	})
}

func (f *fx) req() casfetch.Request {
	return casfetch.Request{
		PeerID: f.peer.ID, OriginAssetURI: f.origin.srv.URL + "/assets/" + f.asset.String(),
		AssetID: f.asset, SHA256: f.hash, Size: int64(len(f.body)), ContentType: "image/png",
		Signer: f.signer,
	}
}

func (f *fx) count(sql string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, f.hash).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// nothingHeld asserts no object, pin, intent or lease and no temp file.
func (f *fx) nothingHeld() {
	f.t.Helper()
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 {
		f.t.Fatalf("storage_objects rows = %d", n)
	}
	if n := f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`); n != 0 {
		f.t.Fatalf("pins = %d", n)
	}
	f.noLeftovers()
}

func (f *fx) noLeftovers() {
	f.t.Helper()
	if n := f.count(`SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`); n != 0 {
		f.t.Fatalf("intents = %d", n)
	}
	if n := f.count(`SELECT count(*) FROM federation_cas_fetch_leases WHERE object_hash = $1`); n != 0 {
		f.t.Fatalf("leases = %d", n)
	}
	entries, _ := os.ReadDir(f.tmp)
	if len(entries) != 0 {
		f.t.Fatalf("temp files left: %d", len(entries))
	}
}

func (f *fx) ingestPins() int {
	return f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_ingest'`)
}

func wantReason(t *testing.T, err error, want string) {
	t.Helper()
	if got := casfetch.ReasonOf(err); got != want {
		t.Fatalf("reason %q (%v), want %q", got, err, want)
	}
}

func TestFetch_MissDownloadsVerifiesAndPins(t *testing.T) {
	f := newFx(t)
	res, err := f.client.Fetch(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	if res.Deduped || res.Hash != f.hash || res.IngestID == uuid.Nil || res.Pin.SubjectType != casfetch.IngestPinType {
		t.Fatalf("result %+v", res)
	}
	// HEADs: before, after the download, and before the commit.
	if f.origin.gets.Load() != 1 || f.origin.heads.Load() != 3 {
		t.Fatalf("GET %d HEAD %d, want 1 GET and 3 HEADs", f.origin.gets.Load(), f.origin.heads.Load())
	}
	var origin string
	_ = f.pool.QueryRow(context.Background(), `SELECT origin_server_id::text FROM storage_objects WHERE hash = $1`, f.hash).Scan(&origin)
	if origin != f.peer.ID.String() || f.ingestPins() != 1 {
		t.Fatalf("origin_server_id %q, ingest pins %d", origin, f.ingestPins())
	}
	f.noLeftovers()
	rc, _, err := f.svc.Download(context.Background(), f.hash, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, f.body) {
		t.Fatal("stored bytes differ")
	}
}

// Production default: no inbound model, nothing is fetched or reused.
func TestFetch_FailClosedByDefault(t *testing.T) {
	f := newFx(t)
	_, err := f.newClient(nil).Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonNoInboundGrant)
	if f.origin.heads.Load()+f.origin.gets.Load() != 0 {
		t.Fatal("the origin was contacted without an inbound grant")
	}
	f.nothingHeld()
}

func (f *fx) holdLocally() {
	f.t.Helper()
	if _, err := f.svc.UploadOriginal(context.Background(), bytes.NewReader(f.body), "image/png",
		storage.PinRef{SubjectType: "user", SubjectID: "unrelated-upload"}); err != nil {
		f.t.Fatal(err)
	}
}

// Bytes already held grant nothing.
func TestFetch_LocalHit_NeedsInboundGrant(t *testing.T) {
	f := newFx(t)
	f.holdLocally()
	f.grants.set(false)
	_, err := f.client.Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonNoInboundGrant)
	if f.ingestPins() != 0 || f.origin.heads.Load() != 0 {
		t.Fatalf("pinned %d / contacted origin %d without an inbound grant", f.ingestPins(), f.origin.heads.Load())
	}
}

func TestFetch_LocalHit_NeedsOrigin(t *testing.T) {
	f := newFx(t)
	f.holdLocally()
	f.origin.headCode.Store(403)
	_, err := f.client.Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonOriginRefused)
	if f.ingestPins() != 0 || f.origin.gets.Load() != 0 {
		t.Fatal("pinned or fetched after the origin refused")
	}
}

func TestFetch_LocalHit_BothAllow_NoGET(t *testing.T) {
	f := newFx(t)
	f.holdLocally()
	res, err := f.client.Fetch(context.Background(), f.req())
	if err != nil || !res.Deduped {
		t.Fatalf("%+v %v", res, err)
	}
	// HEADs: before, and again after the verify read.
	if f.origin.gets.Load() != 0 || f.origin.heads.Load() != 2 || f.ingestPins() != 1 {
		t.Fatalf("GET %d HEAD %d ingest pins %d", f.origin.gets.Load(), f.origin.heads.Load(), f.ingestPins())
	}
}

func TestFetch_StreamFailures(t *testing.T) {
	for mode, want := range map[string]string{
		"oversize": casfetch.ReasonFixityMismatch,
		"short":    casfetch.ReasonFixityMismatch,
		"tamper":   casfetch.ReasonFixityMismatch,
		"length":   casfetch.ReasonLengthMismatch,
		"redirect": casfetch.ReasonRedirectRefused,
		"stall":    casfetch.ReasonTimeout,
	} {
		t.Run(mode, func(t *testing.T) {
			f := newFx(t)
			f.origin.getMode.Store(mode)
			_, err := f.client.Fetch(context.Background(), f.req())
			wantReason(t, err, want)
			if f.origin.followed.Load() != 0 {
				t.Fatal("a redirect was followed")
			}
			f.nothingHeld()
		})
	}
}

func TestFetch_HEADRedirectRefused(t *testing.T) {
	f := newFx(t)
	f.origin.headCode.Store(302)
	_, err := f.client.Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonRedirectRefused)
	if f.origin.followed.Load() != 0 || f.origin.gets.Load() != 0 {
		t.Fatal("redirect followed or GET sent")
	}
}

// Revoked at the origin, or on the receiver, while the bytes were
// moving: discarded, nothing recorded, lease released.
func TestFetch_RevokedDuringDownload(t *testing.T) {
	t.Run("origin", func(t *testing.T) {
		f := newFx(t)
		f.origin.afterGET = func() { f.origin.headCode.Store(403) }
		_, err := f.client.Fetch(context.Background(), f.req())
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		f.nothingHeld()
	})
	t.Run("inbound", func(t *testing.T) {
		f := newFx(t)
		f.origin.afterGET = func() { f.grants.set(false) }
		_, err := f.client.Fetch(context.Background(), f.req())
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		f.nothingHeld()
	})
}

// Twenty callers with the same key: one GET and one object, but every
// caller authorises itself (its own inbound check and origin HEAD) and
// gets its own result and its own ingest pin.
func TestFetch_ConcurrentSameKey_OneGETOwnResults(t *testing.T) {
	f := newFx(t)
	f.origin.getDelay = 300 * time.Millisecond
	const n = 20
	var wg sync.WaitGroup
	results := make([]*casfetch.Result, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.client.Fetch(context.Background(), f.req())
		}(i)
	}
	wg.Wait()
	seen := map[uuid.UUID]bool{}
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].IngestID == uuid.Nil || seen[results[i].IngestID] {
			t.Fatalf("caller %d: ingest id %v not unique", i, results[i].IngestID)
		}
		seen[results[i].IngestID] = true
	}
	if g := f.origin.gets.Load(); g != 1 {
		t.Fatalf("GETs = %d, want 1", g)
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 1 {
		t.Fatalf("objects = %d", n)
	}
	if p := f.ingestPins(); p != n {
		t.Fatalf("ingest pins = %d, want %d (one per caller)", p, n)
	}
	// Every caller asked both authorities itself.
	if h := f.origin.heads.Load(); h < n+1 {
		t.Fatalf("origin HEADs = %d, want at least %d", h, n+1)
	}
	if c := f.grants.calls.Load(); c < n+1 {
		t.Fatalf("inbound checks = %d, want at least %d", c, n+1)
	}
	f.noLeftovers()
}

// A caller whose request differs (size, or an extra checksum the bytes
// do not have) never inherits another caller's success.
func TestFetch_ConcurrentMismatchedRequestsDoNotInherit(t *testing.T) {
	f := newFx(t)
	f.origin.getDelay = 300 * time.Millisecond
	wrongSize := f.req()
	wrongSize.Size++
	wrongSum := f.req()
	wrongSum.Checksums = []federation.ArchivalChecksum{{Algorithm: "sha512", Value: strings.Repeat("0", 128)}}
	var wg sync.WaitGroup
	var good *casfetch.Result
	var eGood, eSize, eSum error
	wg.Add(3)
	go func() { defer wg.Done(); good, eGood = f.client.Fetch(context.Background(), f.req()) }()
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		_, eSize = f.client.Fetch(context.Background(), wrongSize)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		_, eSum = f.client.Fetch(context.Background(), wrongSum)
	}()
	wg.Wait()
	if eGood != nil || good == nil {
		t.Fatalf("matching caller: %v", eGood)
	}
	wantReason(t, eSize, casfetch.ReasonOriginRefused)
	wantReason(t, eSum, casfetch.ReasonFixityMismatch)
	if p := f.ingestPins(); p != 1 {
		t.Fatalf("ingest pins = %d, want only the matching caller's", p)
	}
}

// A caller arriving after a revoke is refused, even though a download
// for the same key began before the revoke.
func TestFetch_LateCallerAfterRevokeRefused(t *testing.T) {
	for _, side := range []string{"origin", "inbound"} {
		t.Run(side, func(t *testing.T) {
			f := newFx(t)
			started := make(chan struct{})
			var once sync.Once
			f.origin.getDelay = 400 * time.Millisecond
			f.origin.afterGET = nil
			var e1, e2 error
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				go func() {
					for f.origin.gets.Load() == 0 {
						time.Sleep(5 * time.Millisecond)
					}
					once.Do(func() { close(started) })
				}()
				_, e1 = f.client.Fetch(context.Background(), f.req())
			}()
			<-started
			if side == "origin" {
				f.origin.headCode.Store(403)
			} else {
				f.grants.set(false)
			}
			_, e2 = f.client.Fetch(context.Background(), f.req())
			wg.Wait()
			want := casfetch.ReasonOriginRefused
			if side == "inbound" {
				want = casfetch.ReasonNoInboundGrant
			}
			wantReason(t, e2, want)
			wantReason(t, e1, casfetch.ReasonRevokedDuringFetch)
			f.nothingHeld()
		})
	}
}

// The first caller's cancellation does not take a still-authorised
// later caller down with it, and creates no shared pin.
func TestFetch_FirstCallerCancelledLaterCallerSucceeds(t *testing.T) {
	f := newFx(t)
	f.origin.getDelay = 300 * time.Millisecond // under the 500 ms header timeout
	ctx1, cancel1 := context.WithCancel(context.Background())
	var e1, e2 error
	var r2 *casfetch.Result
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, e1 = f.client.Fetch(ctx1, f.req()) }()
	for f.origin.gets.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	go func() { defer wg.Done(); r2, e2 = f.client.Fetch(context.Background(), f.req()) }()
	time.Sleep(100 * time.Millisecond)
	cancel1()
	wg.Wait()
	if e1 == nil {
		t.Fatal("the cancelled caller succeeded")
	}
	if e2 != nil || r2 == nil || r2.IngestID == uuid.Nil {
		t.Fatalf("later caller: %+v %v", r2, e2)
	}
	if p := f.ingestPins(); p != 1 {
		t.Fatalf("ingest pins = %d, want 1 (the later caller's)", p)
	}
	f.noLeftovers()
}

// The receiver's peer record is re-read at every authorisation and
// re-checked inside the commit.
func TestFetch_PeerDriftFailsClosed(t *testing.T) {
	cases := map[string]struct {
		sql  string
		want []string
	}{
		"disabled":   {`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, []string{casfetch.ReasonPeerUnavailable}},
		"downgraded": {`UPDATE federation_peers SET capabilities = '[]'::jsonb WHERE id = $1`, []string{casfetch.ReasonProfile}},
		"moved":      {`UPDATE federation_peers SET instance_url = 'https://moved.example' WHERE id = $1`, []string{casfetch.ReasonForeignOrigin}},
	}
	for name, tc := range cases {
		t.Run(name+" during the download", func(t *testing.T) {
			f := newFx(t)
			f.origin.afterGET = func() { f.setPeer(tc.sql) }
			_, err := f.client.Fetch(context.Background(), f.req())
			wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
			var e *casfetch.Error
			if !errors.As(err, &e) || e.OriginReason != tc.want[0] {
				t.Fatalf("cause %+v, want %s", e, tc.want[0])
			}
			f.nothingHeld()
		})
		t.Run(name+" during the Put (the final check)", func(t *testing.T) {
			f := newFx(t)
			var once sync.Once
			f.be.setBeforePut(func() { once.Do(func() { f.setPeer(tc.sql) }) })
			_, err := f.client.Fetch(context.Background(), f.req())
			wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
			var e *casfetch.Error
			if !errors.As(err, &e) || e.OriginReason != tc.want[0] {
				t.Fatalf("cause %+v, want %s", e, tc.want[0])
			}
			if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 {
				t.Fatalf("object recorded after drift: %d", n)
			}
			if f.ingestPins() != 0 {
				t.Fatal("pinned after drift")
			}
		})
		t.Run(name+" after the final check (checked in the commit)", func(t *testing.T) {
			f := newFx(t)
			casfetch.SetAfterFinalCheck(f.client, func() { f.setPeer(tc.sql) })
			_, err := f.client.Fetch(context.Background(), f.req())
			wantReason(t, err, tc.want[0])
			if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 {
				t.Fatalf("object recorded after drift: %d", n)
			}
			if f.ingestPins() != 0 {
				t.Fatal("pinned after drift")
			}
		})
		t.Run(name+" while another worker holds the lease", func(t *testing.T) {
			f := newFx(t)
			o := f.heldLeaseSchedule(func() { f.setPeer(tc.sql) })
			o.wantHeldPhase(t)
			wantReason(t, o.err, tc.want[0])
			if o.gets != 0 || o.objects != 0 || o.pins != 0 || o.ingestPins != 0 || o.intents != 0 || o.temps != 0 {
				t.Fatalf("recorded after drift: GETs=%d objects=%d pins=%d ingest pins=%d intents=%d temp files=%d",
					o.gets, o.objects, o.pins, o.ingestPins, o.intents, o.temps)
			}
			// The other holder's own (now expired) lease row stays; no
			// other lease exists for the key.
			if o.leaseRows != 1 || o.leaseOwner != "stalled" || o.leaseLive {
				t.Fatalf("lease after the refusal: rows=%d owner=%q live=%v", o.leaseRows, o.leaseOwner, o.leaseLive)
			}
		})
	}
	// Positive control for the schedule above: with no drift, the caller
	// waits without a GET while the other holder's lease is live, then
	// downloads exactly once after it ends. The barrier itself refuses
	// nothing.
	t.Run("unchanged while another worker holds the lease (control)", func(t *testing.T) {
		f := newFx(t)
		o := f.heldLeaseSchedule(func() {})
		o.wantHeldPhase(t)
		if o.err != nil || o.res == nil || o.res.IngestID == uuid.Nil || o.res.Hash != f.hash {
			t.Fatalf("unchanged peer: result %+v, error %v (reason %q)", o.res, o.err, casfetch.ReasonOf(o.err))
		}
		if o.gets != 1 || o.objects != 1 || o.ingestPins != 1 || o.intents != 0 || o.temps != 0 {
			t.Fatalf("unchanged peer: GETs=%d objects=%d ingest pins=%d intents=%d temp files=%d",
				o.gets, o.objects, o.ingestPins, o.intents, o.temps)
		}
		if o.leaseRows != 0 {
			t.Fatalf("unchanged peer: %d lease rows left", o.leaseRows)
		}
	})
}

// heldLeaseKeyArgs reports whether a lease statement's first four
// arguments are exactly the fixture's full key (peer, asset, hash, signer).
func (f *fx) heldLeaseKeyArgs(args []any) bool {
	if len(args) < 4 {
		return false
	}
	want := []string{f.peer.ID.String(), f.asset.String(), f.hash, f.signer.KeyID}
	for i, w := range want {
		if fmt.Sprint(args[i]) != w {
			return false
		}
	}
	return true
}

// leaseWaitGate is a test-only pgx.QueryTracer on the client's own pool.
// It observes the client's real lease statements for one full key: the
// acquire that another holder's live lease refuses (no row returned),
// then the first held query after it, whose completion it holds (the
// statement has run; its result is not yet returned to the client) until
// released or the query's context ends. Nothing is mocked: the client's
// SQL runs unchanged against the database.
type leaseWaitGate struct {
	match    func(args []any) bool
	reached  chan struct{}
	release  chan struct{}
	once     sync.Once
	rel      sync.Once
	mu       sync.Mutex
	denied   int    // acquires for the key that returned no row
	acquired int    // acquires for the key that returned the lease
	heldTag  string // command tag of the gated held query
	heldErr  error
	other    []string // anything unexpected, reported by the test
}

type leaseTraceKey struct{}

const (
	acquireSQLMark = "INSERT INTO federation_cas_fetch_leases"
	acquireSQLTail = "ON CONFLICT (peer_id, asset_id, object_hash, signer) DO UPDATE"
	heldSQLMark    = "SELECT expires_at > now() FROM federation_cas_fetch_leases"
)

func (g *leaseWaitGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, leaseTraceKey{}, data)
}

func (g *leaseWaitGate) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(leaseTraceKey{}).(pgx.TraceQueryStartData)
	if !ok || !g.match(start.Args) {
		return
	}
	switch {
	case strings.Contains(start.SQL, acquireSQLMark) && strings.Contains(start.SQL, acquireSQLTail):
		g.mu.Lock()
		switch {
		case data.Err != nil:
			g.other = append(g.other, "acquire error: "+data.Err.Error())
		case data.CommandTag.RowsAffected() == 0:
			g.denied++
		default:
			g.acquired++
		}
		g.mu.Unlock()
	case strings.Contains(start.SQL, heldSQLMark):
		g.mu.Lock()
		armed := g.denied > 0
		g.mu.Unlock()
		if !armed {
			return
		}
		g.once.Do(func() {
			g.mu.Lock()
			g.heldTag, g.heldErr = data.CommandTag.String(), data.Err
			g.mu.Unlock()
			close(g.reached)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		})
	}
}

func (g *leaseWaitGate) open() { g.rel.Do(func() { close(g.release) }) }

// heldLeaseOutcome is everything observed in one heldLeaseSchedule,
// collected before any assertion.
type heldLeaseOutcome struct {
	reached                                  bool
	denied, acquired                         int
	heldTag                                  string
	heldErr                                  error
	other                                    []string
	atGateRows                               int
	atGateOwner                              string
	atGateLeaseID                            uuid.UUID
	atGateLive                               bool
	atGateGets, atGateObjects, atGatePins    int
	seeded                                   uuid.UUID
	expired                                  int64
	res                                      *casfetch.Result
	err                                      error
	gets, objects, pins, ingestPins, intents int
	temps, leaseRows                         int
	leaseOwner                               string
	leaseLive                                bool
	setupErr                                 string
}

// wantHeldPhase asserts the wait was actually observed: the client's real
// acquire was refused by the other holder's live lease, its real held
// query completed while that exact lease was live, and nothing had been
// downloaded or recorded by then; the fixture then ended exactly that
// lease.
func (o heldLeaseOutcome) wantHeldPhase(t *testing.T) {
	t.Helper()
	t.Logf("held-lease schedule: reached=%v denied=%d acquired=%d held tag=%q held err=%v other=%v | at gate: rows=%d owner=%q lease=%v (seeded %v) live=%v GETs=%d objects=%d pins=%d | expired rows=%d | result err=%v reason=%q | after: GETs=%d objects=%d pins=%d ingest pins=%d intents=%d temp files=%d lease rows=%d owner=%q live=%v | setup=%q",
		o.reached, o.denied, o.acquired, o.heldTag, o.heldErr, o.other, o.atGateRows, o.atGateOwner, o.atGateLeaseID, o.seeded,
		o.atGateLive, o.atGateGets, o.atGateObjects, o.atGatePins, o.expired, o.err, casfetch.ReasonOf(o.err), o.gets, o.objects,
		o.pins, o.ingestPins, o.intents, o.temps, o.leaseRows, o.leaseOwner, o.leaseLive, o.setupErr)
	if o.setupErr != "" {
		t.Fatalf("schedule: %s", o.setupErr)
	}
	if !o.reached || o.denied < 1 || o.heldErr != nil || o.heldTag != "SELECT 1" || len(o.other) != 0 {
		t.Fatalf("the wait was not observed: reached=%v denied=%d held tag=%q err=%v other=%v", o.reached, o.denied, o.heldTag, o.heldErr, o.other)
	}
	if o.atGateRows != 1 || o.atGateOwner != "stalled" || o.atGateLeaseID != o.seeded || !o.atGateLive {
		t.Fatalf("at the gate the lease was rows=%d owner=%q id=%v live=%v, want the seeded live one", o.atGateRows, o.atGateOwner, o.atGateLeaseID, o.atGateLive)
	}
	if o.atGateGets != 0 || o.atGateObjects != 0 || o.atGatePins != 0 {
		t.Fatalf("before the other holder's lease ended: GETs=%d objects=%d pins=%d", o.atGateGets, o.atGateObjects, o.atGatePins)
	}
	if o.expired != 1 {
		t.Fatalf("the fixture ended %d leases, want exactly the seeded one", o.expired)
	}
}

// heldLeaseSchedule seeds another holder's live lease on the full key
// (unique lease id, owner "stalled", a long fixture expiry that time
// cannot end), starts the real Fetch on the real client with the normal
// configuration over a traced clone of the fixture pool, and waits for
// the client's real held query to complete while that lease is live.
// Then, in this goroutine and on the fixture pool (never the traced
// one), it runs change (a peer drift, or nothing), ends exactly the
// seeded lease with a controlled fixture UPDATE to a past database time
// (it models the other holder releasing; it is not a natural expiry),
// releases the held query and waits for Fetch to end. The Fetch is
// always released, cancelled and joined before the pools close.
func (f *fx) heldLeaseSchedule(change func()) (o heldLeaseOutcome) {
	f.t.Helper()
	t := f.t
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := &leaseWaitGate{match: f.heldLeaseKeyArgs, reached: make(chan struct{}), release: make(chan struct{})}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = g
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := casfetch.New(casfetch.Deps{
		Pool: traced, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants,
		Audit: audit.NewRecorder(f.pool, logger), Logger: logger, Config: f.cfg,
	})
	o.seeded = uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_cas_fetch_leases (peer_id, asset_id, object_hash, signer, lease_id, owner, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'stalled', now() + interval '1 hour')`, f.peer.ID, f.asset, f.hash, f.signer.KeyID, o.seeded); err != nil {
		t.Fatal(err)
	}
	fetchCtx, cancelFetch := context.WithCancel(ctx)
	type result struct {
		res *casfetch.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := client.Fetch(fetchCtx, f.req())
		done <- result{res, err}
	}()
	var joined bool
	var final result
	join := func() {
		if !joined {
			joined = true
			g.open()
			cancelFetch()
			final = <-done
		}
	}
	// Registered after traced.Close, so it runs first: never close a pool
	// under a running Fetch, even when an assertion below fails.
	t.Cleanup(join)

	q := func(sql string, args ...any) int {
		var n int
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			o.setupErr = fmt.Sprintf("observe: %v", err)
		}
		return n
	}
	select {
	case <-g.reached:
		o.reached = true
	case r := <-done:
		joined, final = true, r
		o.setupErr = "Fetch ended before its held query was observed"
	case <-ctx.Done():
		o.setupErr = "the held query was not observed in time"
	}
	if o.reached {
		var expires time.Time
		o.atGateRows = q(`SELECT count(*) FROM federation_cas_fetch_leases WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4`,
			f.peer.ID, f.asset, f.hash, f.signer.KeyID)
		if err := f.pool.QueryRow(ctx, `SELECT lease_id, owner, expires_at, expires_at > now() FROM federation_cas_fetch_leases
			WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4`, f.peer.ID, f.asset, f.hash, f.signer.KeyID).
			Scan(&o.atGateLeaseID, &o.atGateOwner, &expires, &o.atGateLive); err != nil {
			o.setupErr = fmt.Sprintf("read the lease at the gate: %v", err)
		}
		o.atGateGets = int(f.origin.gets.Load())
		o.atGateObjects = q(`SELECT count(*) FROM storage_objects WHERE hash = $1`, f.hash)
		o.atGatePins = q(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, f.hash)
		change()
		tag, err := f.pool.Exec(ctx, `UPDATE federation_cas_fetch_leases SET expires_at = now() - interval '1 second'
			WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4 AND lease_id = $5 AND owner = 'stalled'`,
			f.peer.ID, f.asset, f.hash, f.signer.KeyID, o.seeded)
		if err != nil {
			o.setupErr = fmt.Sprintf("end the seeded lease: %v", err)
		}
		o.expired = tag.RowsAffected()
		g.open()
		select {
		case final = <-done:
			joined = true
		case <-ctx.Done():
			o.setupErr = "Fetch did not end in time after the release"
		}
	}
	join()
	o.res, o.err = final.res, final.err
	g.mu.Lock()
	o.denied, o.acquired, o.heldTag, o.heldErr, o.other = g.denied, g.acquired, g.heldTag, g.heldErr, append([]string(nil), g.other...)
	g.mu.Unlock()
	o.gets = int(f.origin.gets.Load())
	o.objects = q(`SELECT count(*) FROM storage_objects WHERE hash = $1`, f.hash)
	o.pins = q(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, f.hash)
	o.ingestPins = q(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_ingest'`, f.hash)
	o.intents = q(`SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`, f.hash)
	o.leaseRows = q(`SELECT count(*) FROM federation_cas_fetch_leases WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4`,
		f.peer.ID, f.asset, f.hash, f.signer.KeyID)
	if o.leaseRows == 1 {
		if err := f.pool.QueryRow(ctx, `SELECT owner, expires_at > now() FROM federation_cas_fetch_leases
			WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4`, f.peer.ID, f.asset, f.hash, f.signer.KeyID).
			Scan(&o.leaseOwner, &o.leaseLive); err != nil {
			o.setupErr = fmt.Sprintf("read the lease after: %v", err)
		}
	}
	entries, _ := os.ReadDir(f.tmp)
	o.temps = len(entries)
	return o
}

// The lease stays this worker's through a Put longer than LeaseTTL: a
// second worker waits instead of downloading again, then reuses.
func TestFetch_LeaseHeldThroughSlowPut(t *testing.T) {
	f := newFx(t, func(c *casfetch.Config) { c.LeaseTTL = time.Second; c.Heartbeat = 200 * time.Millisecond })
	inPut, release := make(chan struct{}), make(chan struct{})
	var relOnce sync.Once
	unblock := func() { relOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var once sync.Once
	f.be.setBeforePut(func() { once.Do(func() { close(inPut); <-release }) })
	other := f.newClient(f.grants)
	var e1, e2 error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, e1 = f.client.Fetch(context.Background(), f.req()) }()
	<-inPut
	wg.Add(1)
	go func() { defer wg.Done(); _, e2 = other.Fetch(context.Background(), f.req()) }()
	time.Sleep(2500 * time.Millisecond) // > LeaseTTL while the Put is blocked
	if g := f.origin.gets.Load(); g != 1 {
		t.Fatalf("GETs while the first Put was blocked = %d, want 1", g)
	}
	unblock()
	wg.Wait()
	if e1 != nil || e2 != nil {
		t.Fatal(e1, e2)
	}
	if g := f.origin.gets.Load(); g != 1 {
		t.Fatalf("GETs = %d, want 1", g)
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 1 {
		t.Fatalf("objects = %d", n)
	}
}

// A failed renewal, or a lease lost to someone else, fails the import
// closed: nothing is recorded or pinned.
func TestFetch_HeartbeatFailureFailsClosed(t *testing.T) {
	t.Run("renewal error", func(t *testing.T) {
		f := newFx(t, func(c *casfetch.Config) { c.LeaseTTL = time.Second; c.Heartbeat = 100 * time.Millisecond })
		inPut, release := make(chan struct{}), make(chan struct{})
		var relOnce sync.Once
		unblock := func() { relOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var once sync.Once
		f.be.setBeforePut(func() { once.Do(func() { close(inPut); <-release }) })
		var fault atomic.Bool
		casfetch.SetHeartbeatFault(f.client, func() error {
			if fault.Load() {
				return errors.New("database unreachable")
			}
			return nil
		})
		done := make(chan error, 1)
		go func() { _, err := f.client.Fetch(context.Background(), f.req()); done <- err }()
		<-inPut
		fault.Store(true)
		time.Sleep(300 * time.Millisecond)
		unblock()
		wantReason(t, <-done, casfetch.ReasonLeaseLost)
		if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 || f.ingestPins() != 0 {
			t.Fatalf("recorded after a failed renewal: objects=%d pins=%d", n, f.ingestPins())
		}
	})
	t.Run("lease taken away", func(t *testing.T) {
		f := newFx(t, func(c *casfetch.Config) { c.LeaseTTL = time.Second; c.Heartbeat = 100 * time.Millisecond })
		inPut, release := make(chan struct{}), make(chan struct{})
		var relOnce sync.Once
		unblock := func() { relOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var once sync.Once
		f.be.setBeforePut(func() { once.Do(func() { close(inPut); <-release }) })
		done := make(chan error, 1)
		go func() { _, err := f.client.Fetch(context.Background(), f.req()); done <- err }()
		<-inPut
		if _, err := f.pool.Exec(context.Background(),
			`DELETE FROM federation_cas_fetch_leases WHERE object_hash = $1`, f.hash); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		unblock()
		wantReason(t, <-done, casfetch.ReasonLeaseLost)
		if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 || f.ingestPins() != 0 {
			t.Fatalf("recorded without the lease: objects=%d pins=%d", n, f.ingestPins())
		}
	})
}

// Two clients (two workers) sharing a database: the lease, not
// singleflight, keeps it to one GET.
func TestFetch_TwoWorkers_LeaseOneGET(t *testing.T) {
	f := newFx(t)
	f.origin.getDelay = 300 * time.Millisecond
	other := f.newClient(f.grants)
	var wg sync.WaitGroup
	var e1, e2 error
	wg.Add(2)
	go func() { defer wg.Done(); _, e1 = f.client.Fetch(context.Background(), f.req()) }()
	go func() { defer wg.Done(); _, e2 = other.Fetch(context.Background(), f.req()) }()
	wg.Wait()
	if e1 != nil || e2 != nil {
		t.Fatal(e1, e2)
	}
	if g := f.origin.gets.Load(); g != 1 {
		t.Fatalf("GETs = %d, want 1", g)
	}
}

// A second signer never inherits the first signer's result: it is
// asked about on its own, and refused when the origin refuses it.
func TestFetch_DifferentSignerIsolated(t *testing.T) {
	f := newFx(t)
	f.origin.getDelay = 300 * time.Millisecond
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	actor := casfetch.ActorSigner(localBase+"/users/bob", priv)
	f.origin.headDeny = func(r *http.Request) bool { return strings.Contains(r.Header.Get("Signature"), "/users/bob#main-key") }
	var wg sync.WaitGroup
	var e1, e2 error
	var r1 *casfetch.Result
	wg.Add(2)
	go func() { defer wg.Done(); r1, e1 = f.client.Fetch(context.Background(), f.req()) }()
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		req := f.req()
		req.Signer = actor
		_, e2 = f.client.Fetch(context.Background(), req)
	}()
	wg.Wait()
	if e1 != nil || r1 == nil {
		t.Fatalf("instance signer: %v", e1)
	}
	wantReason(t, e2, casfetch.ReasonOriginRefused)
	if f.ingestPins() != 1 {
		t.Fatalf("ingest pins = %d, want only the instance signer's", f.ingestPins())
	}
}

func TestFetch_ExpiredLeaseTakenOver(t *testing.T) {
	f := newFx(t)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO federation_cas_fetch_leases (peer_id, asset_id, object_hash, signer, lease_id, owner, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'crashed', now() - interval '1 minute')`,
		f.peer.ID, f.asset, f.hash, f.signer.KeyID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Fetch(context.Background(), f.req()); err != nil {
		t.Fatal(err)
	}
	f.noLeftovers()
}

// A live lease held by a stalled worker: the waiter waits while it is
// live, then takes it over and fetches.
//
// The wait is observed, not timed (rev803): heldLeaseSchedule seeds the
// stalled worker's lease on the full key with an expiry time cannot reach,
// sees the client's real acquire refused and its real held query complete
// while that exact lease is live, with nothing downloaded or recorded, and
// then ENDS the lease with a controlled fixture update. That models the
// other holder releasing; it is not a natural expiry, and this test does
// not measure how long a lease lasts. (It used to seed a one-second lease
// in database time and require 700ms of elapsed wall time, a bound across
// two clocks that is not the ownership contract.) The takeover is then
// proved by state: the client acquires the lease once, downloads once,
// stores the exact bytes, pins them once for this ingest and leaves no
// intent, temp file or lease behind.
func TestFetch_WaitsForLiveLeaseThenTakesOver(t *testing.T) {
	f := newFx(t)
	o := f.heldLeaseSchedule(func() {})
	o.wantHeldPhase(t)
	if o.acquired != 1 {
		t.Fatalf("acquires that took the lease = %d, want 1", o.acquired)
	}
	if o.err != nil || o.res == nil {
		t.Fatalf("takeover: result %+v, error %v (reason %q)", o.res, o.err, casfetch.ReasonOf(o.err))
	}
	if o.res.Deduped || o.res.Hash != f.hash || o.res.IngestID == uuid.Nil || o.res.Pin.SubjectType != casfetch.IngestPinType {
		t.Fatalf("result %+v", o.res)
	}
	if o.gets != 1 || o.objects != 1 || o.pins != 1 || o.ingestPins != 1 || o.intents != 0 || o.temps != 0 || o.leaseRows != 0 {
		t.Fatalf("after the takeover: GETs=%d objects=%d pins=%d ingest pins=%d intents=%d temp files=%d lease rows=%d",
			o.gets, o.objects, o.pins, o.ingestPins, o.intents, o.temps, o.leaseRows)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rc, _, err := f.svc.Download(ctx, f.hash, storage.VariantOriginal)
	if err != nil {
		t.Fatalf("open the stored bytes: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read the stored bytes: %v, close: %v", readErr, closeErr)
	}
	if !bytes.Equal(got, f.body) {
		t.Fatalf("stored bytes differ: %d bytes, want %d", len(got), len(f.body))
	}
}

func TestFetch_ValidationRefusesBeforeContact(t *testing.T) {
	cases := map[string]struct {
		mut  func(f *fx, r *casfetch.Request)
		want string
	}{
		"foreign origin": {func(f *fx, r *casfetch.Request) {
			r.OriginAssetURI = "https://other.example/assets/" + f.asset.String()
		}, casfetch.ReasonForeignOrigin},
		"other asset": {func(f *fx, r *casfetch.Request) { r.OriginAssetURI = f.origin.srv.URL + "/assets/" + uuid.NewString() }, casfetch.ReasonForeignOrigin},
		"foreign signer": {func(f *fx, r *casfetch.Request) {
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			r.Signer = casfetch.ActorSigner("https://elsewhere.example/users/eve", priv)
		}, casfetch.ReasonForeignSigner},
		"class":        {func(_ *fx, r *casfetch.Request) { r.Class = "preview" }, casfetch.ReasonClassNotAvailable},
		"restricted":   {func(_ *fx, r *casfetch.Request) { r.AnnouncedSensitivity = "restricted" }, casfetch.ReasonSensitivity},
		"embargo":      {func(_ *fx, r *casfetch.Request) { r.AnnouncedSensitivity = "embargo" }, casfetch.ReasonSensitivity},
		"too large":    {func(_ *fx, r *casfetch.Request) { r.Size = 1 << 40 }, casfetch.ReasonTooLarge},
		"bad hash":     {func(_ *fx, r *casfetch.Request) { r.SHA256 = "ABC" }, casfetch.ReasonInvalidRequest},
		"unknown peer": {func(_ *fx, r *casfetch.Request) { r.PeerID = uuid.New() }, casfetch.ReasonPeerUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			r := f.req()
			tc.mut(f, &r)
			_, err := f.client.Fetch(context.Background(), r)
			wantReason(t, err, tc.want)
			if f.origin.heads.Load()+f.origin.gets.Load() != 0 {
				t.Fatal("contacted the origin")
			}
		})
	}
	t.Run("no profile", func(t *testing.T) {
		f := newFx(t)
		f.setPeer(`UPDATE federation_peers SET capabilities = '[]'::jsonb WHERE id = $1`)
		_, err := f.client.Fetch(context.Background(), f.req())
		wantReason(t, err, casfetch.ReasonProfile)
	})
}

func TestFetch_AuditsOutcome(t *testing.T) {
	f := newFx(t)
	f.origin.getMode.Store("tamper")
	_, _ = f.client.Fetch(context.Background(), f.req())
	var reason string
	_ = f.pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason' FROM audit_events WHERE event_type = 'federation.cas.fetch_failed' AND metadata->>'asset_id' = $1`,
		f.asset.String()).Scan(&reason)
	if reason != casfetch.ReasonFixityMismatch {
		t.Fatalf("audited reason %q", reason)
	}
}

func TestReconcile_SweepsLeftovers(t *testing.T) {
	f := newFx(t)
	stale := filepath.Join(f.tmp, "aa-casfetch-"+uuid.NewString()+".part")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO federation_cas_fetch_leases (peer_id, asset_id, object_hash, signer, lease_id, owner, expires_at)
		 VALUES ($1, $2, $3, 'x', $4, 'gone', now() - interval '1 hour')`, f.peer.ID, f.asset, f.hash, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Reconcile(context.Background(), time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	f.noLeftovers()
}

// A malformed actor key fails the fetch; it never panics.
func TestFetch_MalformedActorKey(t *testing.T) {
	f := newFx(t)
	r := f.req()
	r.Signer = casfetch.ActorSigner(localBase+"/users/carol", ed25519.PrivateKey(make([]byte, 119)))
	_, err := f.client.Fetch(context.Background(), r)
	wantReason(t, err, casfetch.ReasonInvalidRequest)
	if f.origin.heads.Load()+f.origin.gets.Load() != 0 {
		t.Fatal("contacted the origin")
	}
}

func (f *fx) setPeer(sql string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, f.peer.ID); err != nil {
		f.t.Fatal(err)
	}
}

// gate blocks the first call of the returned hook until released.
type gate struct {
	in, release chan struct{}
	once, rel   sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{in: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) hook() func() {
	return func() { g.once.Do(func() { close(g.in); <-g.release }) }
}

func (g *gate) open() { g.rel.Do(func() { close(g.release) }) }

// Bytes held locally; the authorisation is revoked while their (long)
// verify read is blocked: no new pin.
func TestFetch_ReuseRevokedDuringReadRecordsNoPin(t *testing.T) {
	for name, revoke := range map[string]func(f *fx){
		"origin":   func(f *fx) { f.origin.headCode.Store(403) },
		"inbound":  func(f *fx) { f.grants.set(false) },
		"disabled": func(f *fx) { f.setPeer(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			f.holdLocally()
			g := newGate(t)
			f.be.setBeforeGet(g.hook())
			done := make(chan error, 1)
			go func() { _, err := f.client.Fetch(context.Background(), f.req()); done <- err }()
			<-g.in
			revoke(f)
			g.open()
			wantReason(t, <-done, casfetch.ReasonRevokedDuringFetch)
			if f.ingestPins() != 0 || f.origin.gets.Load() != 0 {
				t.Fatalf("ingest pins %d, GETs %d", f.ingestPins(), f.origin.gets.Load())
			}
		})
	}
}

// The reuse pin transaction re-checks the peer row under a lock: a
// change between the last check and the pin records no pin.
func TestFetch_ReusePinTransactionChecksPeer(t *testing.T) {
	for name, tc := range map[string]struct{ sql, want string }{
		"disabled":   {`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, casfetch.ReasonPeerUnavailable},
		"downgraded": {`UPDATE federation_peers SET capabilities = '[]'::jsonb WHERE id = $1`, casfetch.ReasonProfile},
		"moved":      {`UPDATE federation_peers SET instance_url = 'https://moved-pin.example' WHERE id = $1`, casfetch.ReasonForeignOrigin},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			f.holdLocally()
			casfetch.SetBeforeReusePin(f.client, func() { f.setPeer(tc.sql) })
			_, err := f.client.Fetch(context.Background(), f.req())
			wantReason(t, err, tc.want)
			if f.ingestPins() != 0 {
				t.Fatal("pinned after the peer changed")
			}
		})
	}
}

// A revoke during a (long) Put, after the post-download check: the
// final check before commit records nothing.
func TestFetch_RevokedDuringPutRecordsNothing(t *testing.T) {
	for name, revoke := range map[string]func(f *fx){
		"origin":  func(f *fx) { f.origin.headCode.Store(403) },
		"inbound": func(f *fx) { f.grants.set(false) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			g := newGate(t)
			f.be.setBeforePut(g.hook())
			done := make(chan error, 1)
			go func() { _, err := f.client.Fetch(context.Background(), f.req()); done <- err }()
			<-g.in
			revoke(f)
			g.open()
			wantReason(t, <-done, casfetch.ReasonRevokedDuringFetch)
			if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 || f.ingestPins() != 0 {
				t.Fatalf("recorded after a revoke during Put: objects=%d pins=%d", n, f.ingestPins())
			}
		})
	}
}

// The import has its own budget: a Put that outlasts the old single
// deadline (download budget + 2 lease periods) but fits the import
// budget completes.
func TestFetch_ImportPhaseHasItsOwnBudget(t *testing.T) {
	f := newFx(t, func(c *casfetch.Config) {
		c.MinOverall = time.Second
		c.LeaseTTL = time.Second
		c.Heartbeat = 200 * time.Millisecond
		c.MinImport = 10 * time.Second
	})
	// Old total: 1 s + 2 x 1 s = 3 s. The Put alone takes 3.5 s.
	f.be.setBeforePut(func() { time.Sleep(3500 * time.Millisecond) })
	res, err := f.client.Fetch(context.Background(), f.req())
	if err != nil || res.Deduped {
		t.Fatalf("%+v %v", res, err)
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 1 {
		t.Fatalf("objects = %d", n)
	}
}

// The temp sweep keeps the file of a live transfer, however old its
// timestamp, and still removes one with no live lease.
func TestReconcile_KeepsTempOfLiveTransfer(t *testing.T) {
	f := newFx(t)
	g := newGate(t)
	f.be.setBeforePut(g.hook())
	done := make(chan error, 1)
	go func() { _, err := f.client.Fetch(context.Background(), f.req()); done <- err }()
	<-g.in
	entries, _ := os.ReadDir(f.tmp)
	if len(entries) != 1 {
		t.Fatalf("temp files during the import: %d", len(entries))
	}
	live := filepath.Join(f.tmp, entries[0].Name())
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(live, old, old)
	orphan := filepath.Join(f.tmp, "aa-casfetch-"+uuid.NewString()+".part")
	_ = os.WriteFile(orphan, []byte("x"), 0o600)
	_ = os.Chtimes(orphan, old, old)
	if err := f.client.Reconcile(context.Background(), time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live transfer's temp file was swept: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphan temp file was kept: %v", err)
	}
	g.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
