// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 receiver core against a live database, the fs storage backend,
// the production casfetch client, the production inbound grant record
// (inboundgrants.Store, written through ApplyShare / ApplyUnshare) and a
// controllable origin stub. The two-instance proof is
// internal/federation/twostack. Skips without AA_DB_PASSWORD.

package assetingest_test

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
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
		// An explicit pool: the concurrency test holds several
		// connections at once (lease heartbeat, import lock, commit),
		// and the default max(4, NumCPU) is 4 on the CI runner.
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable pool_max_conns=16",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		pwd, env("AA_DB_NAME", "artist_alley")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// origin is a controllable stand-in for a peer's §8.5 endpoint: it
// serves the bodies it holds, by hash, and can tamper, truncate, refuse
// or fail per hash.
type origin struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	mode  map[string]string // "" ok | tamper | short | length | missing | unavailable | hold
	gets  map[string]*atomic.Int32
	heads atomic.Int32
	// hold: the GET sends its headers, then waits for release (or its
	// request to end) before the body. inflight counts held GETs;
	// maxInflight is the most at once.
	release     chan struct{}
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func newOrigin(t *testing.T) *origin {
	o := &origin{files: map[string][]byte{}, mode: map[string]string{}, gets: map[string]*atomic.Int32{},
		release: make(chan struct{})}
	// TLS: a federation origin is https (federation.OriginOf).
	o.srv = httptest.NewTLSServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *origin) add(body []byte) string {
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	o.mu.Lock()
	o.files[h] = body
	o.gets[h] = &atomic.Int32{}
	o.mu.Unlock()
	return h
}

func (o *origin) setMode(hash, mode string) { o.mu.Lock(); o.mode[hash] = mode; o.mu.Unlock() }

func (o *origin) getCount(hash string) int32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.gets[hash]; ok {
		return c.Load()
	}
	return 0
}

func (o *origin) serve(w http.ResponseWriter, r *http.Request) {
	deny := func(code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "not_available"})
	}
	if r.Header.Get("Signature") == "" {
		w.WriteHeader(401)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	hash := parts[len(parts)-1]
	o.mu.Lock()
	body, ok := o.files[hash]
	mode := o.mode[hash]
	cnt := o.gets[hash]
	o.mu.Unlock()
	if !ok || mode == "missing" || r.URL.Query().Get("size") != fmt.Sprint(len(body)) {
		deny(403)
		return
	}
	if r.Method == http.MethodHead {
		o.heads.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(200)
		return
	}
	cnt.Add(1)
	switch mode {
	case "unavailable":
		w.WriteHeader(503)
	case "tamper":
		b := bytes.Clone(body)
		b[0] ^= 0xff
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	case "short":
		_, _ = w.Write(body[:len(body)/2])
	case "length":
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
		_, _ = w.Write(append(bytes.Clone(body), 'x'))
	case "hold":
		n := o.inflight.Add(1)
		for m := o.maxInflight.Load(); n > m && !o.maxInflight.CompareAndSwap(m, n); m = o.maxInflight.Load() {
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-o.release:
		case <-r.Context().Done():
		}
		o.inflight.Add(-1)
		_, _ = w.Write(body)
	default:
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}
}

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

type fx struct {
	t       *testing.T
	pool    *pgxpool.Pool
	svc     *storage.Service
	origin  *origin
	peerID  uuid.UUID
	grants  *inboundgrants.Store
	client  *casfetch.Client
	ingest  *assetingest.Service
	signer  casfetch.Signer
	userRef int64
}

const bothProfiles = `["share-lifecycle-1", "archival-1", "original-transfer-1"]`

func newFx(t *testing.T, cfg ...assetingest.Config) *fx {
	t.Helper()
	ctx := context.Background()
	pool := openPool(t)
	f := &fx{t: t, pool: pool, origin: newOrigin(t)}
	fsb, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.svc = storage.NewService(fsb, pool)
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'assetingest', 1) RETURNING ref`,
		"ai-"+uuid.NewString()[:8]).Scan(&f.userRef); err != nil {
		t.Fatal(err)
	}
	f.peerID = f.addPeer(f.origin.srv.URL, bothProfiles)
	local := func(context.Context) string { return localBase }
	f.grants = inboundgrants.New(pool, local, nil, nil)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.signer = casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/federation/instance#main-key"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := audit.NewRecorder(pool, logger)
	f.client = casfetch.New(casfetch.Deps{
		Pool: pool, Storage: f.svc, Peers: peers{pool: pool}, Grants: casfetch.DatabaseGrants{Store: f.grants},
		Audit: rec, Logger: logger, Transport: f.origin.srv.Client().Transport,
		Config: casfetch.Config{
			IdleTimeout: 2 * time.Second, MinOverall: 10 * time.Second, LeaseTTL: 3 * time.Second,
			Heartbeat: time.Second, WaitPoll: 50 * time.Millisecond, TempDir: t.TempDir(), LocalBaseURL: local,
		},
	})
	c := assetingest.Config{}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	f.ingest = assetingest.New(assetingest.Deps{
		Pool: pool, Storage: f.svc, Peers: peers{pool: pool}, Grants: f.grants, Fetcher: f.client,
		Audit: rec, Logger: logger, Config: c,
	})
	t.Cleanup(func() {
		c := context.Background()
		// Records go with their peers (cascade), releasing their pins.
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE handshake_by_user_ref = $1`, f.userRef)
		f.origin.mu.Lock()
		hashes := make([]string, 0, len(f.origin.files))
		for h := range f.origin.files {
			hashes = append(hashes, h)
		}
		f.origin.mu.Unlock()
		for _, h := range hashes {
			_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_import_intents WHERE object_hash = $1`, h)
		}
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, f.userRef)
	})
	return f
}

func (f *fx) addPeer(url, caps string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'origin', 'k', TRUE, 'connected', $2, $3::jsonb) RETURNING id`,
		url, f.userRef, caps).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// asset is one origin asset and its announced file set.
type asset struct {
	id    uuid.UUID
	uri   string
	files []map[string]any
	hash  []string // original hashes, in order
}

func (f *fx) newAsset(n int) *asset {
	a := &asset{id: uuid.New()}
	a.uri = f.origin.srv.URL + "/assets/" + a.id.String()
	for i := 0; i < n; i++ {
		body := make([]byte, 32<<10+i)
		_, _ = rand.Read(body)
		h := f.origin.add(body)
		a.hash = append(a.hash, h)
		a.files = append(a.files, fileEntry("original", h, len(body), fmt.Sprintf("part-%d.png", i)))
	}
	return a
}

func fileEntry(role, hash string, size int, name string) map[string]any {
	e := map[string]any{"role": role, "contentUri": "cas://" + hash,
		"checksums": []map[string]string{{"algorithm": "sha256", "value": hash}}, "size": size, "mediaType": "image/png"}
	if name != "" {
		e["originalFilename"] = name
	}
	return e
}

// member builds the aa:asset member for version (versionID, number).
func (a *asset) member(versionID string, number int, sensitivity string) map[string]any {
	v := map[string]any{"id": versionID, "number": number, "created": "2026-11-19T10:00:00Z", "predecessor": nil}
	if number > 1 {
		v["predecessor"] = versionID + "-prev"
	}
	return map[string]any{"id": a.uri, "version": v, "files": a.files, "sensitivity": sensitivity, "mature": false}
}

func (f *fx) envelope(a *asset, member map[string]any) *federation.Envelope {
	f.t.Helper()
	raw, err := json.Marshal(member)
	if err != nil {
		f.t.Fatal(err)
	}
	return &federation.Envelope{
		Context: federation.ContextV1, Type: federation.ActivityAAAssetVersion,
		ID: f.origin.srv.URL + "/activities/" + uuid.NewString(), Actor: f.origin.srv.URL + "/users/alice",
		Published: time.Now().UTC(), Object: a.uri,
		Extra: map[string]json.RawMessage{federation.MemberArchivalAsset: raw},
	}
}

func (f *fx) version(a *asset) string {
	return f.origin.srv.URL + "/assets/" + a.id.String() + "/versions/1"
}

// grant records a live peer-wide original grant for a, as an applied
// aa:Share would. It returns the share activity URI.
func (f *fx) grant(peerID uuid.UUID, objectURI string, expires *time.Time) string {
	f.t.Helper()
	act := f.origin.srv.URL + "/activities/share-" + uuid.NewString()
	if _, err := f.grants.ApplyShare(context.Background(), inboundgrants.Share{
		PeerID: peerID, ActivityURI: act, GrantorActorURI: f.origin.srv.URL + "/users/alice",
		ObjectKind: federation.ShareObjectKindAsset, ObjectURI: objectURI, Scope: federation.ShareScopeView,
		AllowOriginal: true, ExpiresAt: expires,
	}); err != nil {
		f.t.Fatal(err)
	}
	return act
}

func (f *fx) revoke(peerID uuid.UUID, objectURI, share string) {
	f.t.Helper()
	if _, err := f.grants.ApplyUnshare(context.Background(), inboundgrants.Unshare{
		PeerID: peerID, ActivityURI: f.origin.srv.URL + "/activities/unshare-" + uuid.NewString(),
		ActorURI: f.origin.srv.URL + "/users/alice", ObjectKind: federation.ShareObjectKindAsset,
		ObjectURI: objectURI, Revokes: share,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) receive(e *federation.Envelope) (assetingest.Outcome, error) {
	return f.ingest.Receive(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer})
}

func (f *fx) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// heldUnder counts pins of the hash per subject type.
func (f *fx) pins(hash, subjectType string) int {
	return f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = $2`, hash, subjectType)
}

// nothingUsable: no record pin, no ingest pin and nothing verified for
// the hashes; any bytes left are unpinned and scheduled for GC; no
// assets row anywhere names them.
func (f *fx) nothingUsable(hashes ...string) {
	f.t.Helper()
	for _, h := range hashes {
		if n := f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, h); n != 0 {
			f.t.Fatalf("hash %s still pinned (%d pins)", h[:12], n)
		}
		if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, h); n != 0 {
			f.t.Fatalf("hash %s is stored and not scheduled for GC", h[:12])
		}
		if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE object_hash = $1 AND state = 'verified'`, h); n != 0 {
			f.t.Fatalf("hash %s is recorded verified", h[:12])
		}
		if n := f.count(`SELECT count(*) FROM assets WHERE file_hash = $1`, h); n != 0 {
			f.t.Fatalf("an assets row names hash %s", h[:12])
		}
	}
}

func wantReason(t *testing.T, err error, want string) {
	t.Helper()
	if got := assetingest.ReasonOf(err); got != want {
		t.Fatalf("reason %q (%v), want %q", got, err, want)
	}
}

func (f *fx) state(id uuid.UUID) (state, reason string) {
	f.t.Helper()
	var r *string
	if err := f.pool.QueryRow(context.Background(), `SELECT state, reason FROM federation_received_assets WHERE id = $1`, id).Scan(&state, &r); err != nil {
		f.t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

func TestReceive_AllowedOriginalImport(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(2)
	// A preservation entry the origin does not serve: kept as a claim,
	// never fetched.
	pres := f.origin.add([]byte("preservation copy"))
	a.files = append(a.files, fileEntry("preservation", pres, len("preservation copy"), ""))
	f.grant(f.peerID, a.uri, nil)
	e := f.envelope(a, a.member(f.version(a), 1, "public"))

	out, err := f.receive(e)
	if err != nil || out.State != assetingest.StateComplete || out.Duplicate {
		t.Fatalf("receive: %+v %v", out, err)
	}
	var grantID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT authorizing_grant_id FROM federation_received_assets WHERE id = $1`, out.RecordID).Scan(&grantID); err != nil || grantID != out.GrantID {
		t.Fatalf("authorizing grant %v (%v), outcome %v", grantID, err, out.GrantID)
	}
	var live uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`,
		f.peerID, a.uri).Scan(&live); err != nil || live != grantID {
		t.Fatalf("the recorded grant %v is not the live inbound grant %v (%v)", grantID, live, err)
	}
	for _, h := range a.hash {
		if f.pins(h, assetingest.PinSubjectType) != 1 || f.pins(h, casfetch.IngestPinType) != 0 {
			t.Fatalf("hash %s: record pins %d, ingest pins %d", h[:12], f.pins(h, assetingest.PinSubjectType), f.pins(h, casfetch.IngestPinType))
		}
		info, err := f.svc.Stat(ctx, h, storage.VariantOriginal)
		if err != nil || info == nil {
			t.Fatalf("hash %s not stored: %v", h[:12], err)
		}
	}
	if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'original' AND state = 'verified'`, out.RecordID); n != 2 {
		t.Fatalf("%d verified originals, want 2", n)
	}
	var presState string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'preservation'`,
		out.RecordID).Scan(&presState); err != nil || presState != "not_transferred" {
		t.Fatalf("preservation entry %q (%v), want not_transferred", presState, err)
	}
	if f.origin.getCount(pres) != 0 || f.pins(pres, assetingest.PinSubjectType) != 0 {
		t.Fatal("a non-original entry was fetched or pinned")
	}
	// The sender's claims are kept as data; nothing becomes an asset.
	var name string
	if err := f.pool.QueryRow(ctx, `SELECT declared_original_filename FROM federation_received_asset_files WHERE received_asset_id = $1 AND position = 0`,
		out.RecordID).Scan(&name); err != nil || name != "part-0.png" {
		t.Fatalf("declared filename %q (%v)", name, err)
	}
	if n := f.count(`SELECT count(*) FROM assets WHERE id = $1 OR file_hash = ANY($2)`, a.id, a.hash); n != 0 {
		t.Fatal("receiving created an assets row")
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'record_id' = $2`,
		audit.EventFederationAssetIngestCompleted, out.RecordID.String()); n != 1 {
		t.Fatalf("%d completed audit events", n)
	}

	t.Run("the same activity again is idempotent", func(t *testing.T) {
		gets := f.origin.getCount(a.hash[0])
		again, err := f.receive(e)
		if err != nil || again.RecordID != out.RecordID || again.State != assetingest.StateComplete || !again.Duplicate {
			t.Fatalf("redelivery: %+v %v", again, err)
		}
		if f.origin.getCount(a.hash[0]) != gets {
			t.Fatal("a redelivery fetched again")
		}
	})
	t.Run("the same activity id with another payload or signer is refused", func(t *testing.T) {
		pins := func() int {
			return f.count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = $1 AND pin_subject_id = $2`,
				assetingest.PinSubjectType, out.RecordID.String())
		}
		records := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1`, f.peerID)
		gets := f.origin.getCount(a.hash[0])
		reuse := func(e2 *federation.Envelope) *federation.Envelope { e2.ID = e.ID; return e2 }
		other := *a
		other.files = []map[string]any{a.files[0]}
		described := a.member(f.version(a), 1, "public")
		described["description"] = map[string]any{"title": "A different title"}
		b := f.newAsset(1)
		f.grant(f.peerID, b.uri, nil)
		actor := f.envelope(a, a.member(f.version(a), 1, "public"))
		actor.Actor = f.origin.srv.URL + "/users/bob"
		for _, tc := range []struct {
			name string
			env  *federation.Envelope
		}{
			{"another file set", reuse(f.envelope(&other, other.member(f.version(a), 1, "public")))},
			{"another actor", reuse(actor)},
			{"another asset URI", reuse(f.envelope(b, b.member(f.version(b), 1, "public")))},
			{"another descriptive claim", reuse(f.envelope(a, described))},
			{"another tier", reuse(f.envelope(a, a.member(f.version(a), 1, "team")))},
		} {
			_, err := f.receive(tc.env)
			if got := assetingest.ReasonOf(err); got != assetingest.ReasonActivityConflict {
				t.Fatalf("%s: reason %q (%v), want %q", tc.name, got, err, assetingest.ReasonActivityConflict)
			}
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signer := casfetch.ActorSigner(localBase+"/users/someone", priv)
		_, err := f.ingest.Receive(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: signer})
		wantReason(t, err, casfetch.ReasonForeignSigner)
		if st, _ := f.state(out.RecordID); st != assetingest.StateComplete || pins() != 2 {
			t.Fatalf("the stored record became %s with %d pins", st, pins())
		}
		if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1`, f.peerID); n != records {
			t.Fatalf("%d records written for refused payloads", n-records)
		}
		if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, b.uri); n != 0 {
			t.Fatal("the reused activity id recorded another asset")
		}
		if f.origin.getCount(a.hash[0]) != gets || f.origin.getCount(b.hash[0]) != 0 {
			t.Fatal("a refused payload was fetched")
		}
		// The same announcement re-encoded (whitespace, member order) is
		// still the same delivery.
		var m map[string]any
		if err := json.Unmarshal(e.Extra[federation.MemberArchivalAsset], &m); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.MarshalIndent(m, "", "    ")
		same := *e
		same.Extra = map[string]json.RawMessage{federation.MemberArchivalAsset: raw}
		again, err := f.receive(&same)
		if err != nil || again.RecordID != out.RecordID || !again.Duplicate || again.State != assetingest.StateComplete {
			t.Fatalf("re-encoded redelivery: %+v %v", again, err)
		}
	})
	t.Run("another activity with the same version and file set is a duplicate", func(t *testing.T) {
		dup, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		if err != nil || dup.RecordID != out.RecordID || !dup.Duplicate {
			t.Fatalf("same version: %+v %v", dup, err)
		}
	})
	t.Run("the same version id with another file set fails closed", func(t *testing.T) {
		other := *a
		other.files = []map[string]any{a.files[0]}
		res, err := f.receive(f.envelope(&other, other.member(f.version(a), 1, "public")))
		wantReason(t, err, assetingest.ReasonVersionConflict)
		if res.State != assetingest.StateRejected || res.RecordID == out.RecordID {
			t.Fatalf("conflict: %+v", res)
		}
		if st, _ := f.state(out.RecordID); st != assetingest.StateComplete {
			t.Fatalf("the accepted record became %s", st)
		}
	})
	t.Run("another version id is not received until #1575", func(t *testing.T) {
		_, err := f.receive(f.envelope(a, a.member(f.version(a)+"-2", 2, "public")))
		wantReason(t, err, assetingest.ReasonVersionUnsupported)
	})
}

func TestReceive_FixityFailuresKeepNothing(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{
		{"tamper", casfetch.ReasonFixityMismatch},
		{"short", casfetch.ReasonFixityMismatch},
		{"length", casfetch.ReasonLengthMismatch},
		{"missing", casfetch.ReasonOriginRefused},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFx(t)
			a := f.newAsset(2)
			// The first original arrives intact; the second fails.
			f.origin.setMode(a.hash[1], tc.mode)
			f.grant(f.peerID, a.uri, nil)
			out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
			wantReason(t, err, tc.reason)
			if assetingest.Retryable(err) || out.State != assetingest.StateRejected {
				t.Fatalf("outcome %+v retryable=%v", out, assetingest.Retryable(err))
			}
			if st, r := f.state(out.RecordID); st != assetingest.StateRejected || r != tc.reason {
				t.Fatalf("stored %s/%s", st, r)
			}
			// The intact first file is not kept either: no pin, GC-eligible.
			f.nothingUsable(a.hash...)
			if n := f.count(`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'record_id' = $2 AND metadata->>'reason' = $3`,
				audit.EventFederationAssetIngestRejected, out.RecordID.String(), tc.reason); n != 1 {
				t.Fatalf("%d rejection audit events", n)
			}
		})
	}
}

func TestReceive_GrantRequired(t *testing.T) {
	t.Run("no grant: refused before asking the origin", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if out.RecordID == uuid.Nil || f.origin.heads.Load() != 0 {
			t.Fatalf("outcome %+v, origin HEADs %d", out, f.origin.heads.Load())
		}
		// Refused when the record is bound: no fetch attempt was made.
		if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, out.RecordID); n != 0 {
			t.Fatalf("%d fetch attempts for an announcement without a grant", n)
		}
		if n := f.count(`SELECT count(*) FROM audit_events WHERE event_type LIKE 'federation.cas.%' AND metadata->>'asset_id' = $1`, a.id.String()); n != 0 {
			t.Fatalf("%d casfetch events for an announcement without a grant", n)
		}
		f.nothingUsable(a.hash...)
	})
	t.Run("a grant without the original permission", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		if _, err := f.grants.ApplyShare(context.Background(), inboundgrants.Share{
			PeerID: f.peerID, ActivityURI: f.origin.srv.URL + "/activities/share-" + uuid.NewString(),
			GrantorActorURI: f.origin.srv.URL + "/users/alice", ObjectKind: federation.ShareObjectKindAsset,
			ObjectURI: a.uri, Scope: federation.ShareScopeRemix,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
	t.Run("revoked", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		f.revoke(f.peerID, a.uri, f.grant(f.peerID, a.uri, nil))
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
	t.Run("expired by this database's clock", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		exp := time.Now().Add(1500 * time.Millisecond)
		f.grant(f.peerID, a.uri, &exp)
		time.Sleep(2 * time.Second)
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
	t.Run("a grant recorded for another peer authorises nothing", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		other := newOrigin(t)
		f.grant(f.addPeer(other.srv.URL, bothProfiles), a.uri, nil)
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
	t.Run("revoked between the fetch and the commit", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(2)
		share := f.grant(f.peerID, a.uri, nil)
		assetingest.SetAfterFetch(f.ingest, func() { f.revoke(f.peerID, a.uri, share) })
		out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		if st, _ := f.state(out.RecordID); st != assetingest.StateRejected {
			t.Fatalf("stored %s", st)
		}
		f.nothingUsable(a.hash...)
	})
}

func TestReceive_RefusedBeforeAnyRecord(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	records := func() int {
		return f.count(`SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1`, a.uri)
	}
	t.Run("a peer without archival-1", func(t *testing.T) {
		g := newFx(t)
		if _, err := g.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = '["share-lifecycle-1","original-transfer-1"]'::jsonb WHERE id = $1`, g.peerID); err != nil {
			t.Fatal(err)
		}
		b := g.newAsset(1)
		g.grant(g.peerID, b.uri, nil)
		_, err := g.receive(g.envelope(b, b.member(g.version(b), 1, "public")))
		wantReason(t, err, casfetch.ReasonProfile)
		if g.origin.heads.Load() != 0 {
			t.Fatal("the origin was asked")
		}
	})
	t.Run("another peer announcing this peer's asset", func(t *testing.T) {
		other := newOrigin(t)
		otherPeer := f.addPeer(other.srv.URL, bothProfiles)
		f.grant(otherPeer, a.uri, nil)
		e := f.envelope(a, a.member(f.version(a), 1, "public"))
		e.Actor = other.srv.URL + "/users/mallory"
		_, err := f.ingest.Receive(context.Background(), assetingest.Announcement{PeerID: otherPeer, Envelope: e, Signer: f.signer})
		wantReason(t, err, casfetch.ReasonForeignOrigin)
		if records() != 0 {
			t.Fatal("a cross-peer announcement was recorded")
		}
	})
	t.Run("an actor of another origin", func(t *testing.T) {
		e := f.envelope(a, a.member(f.version(a), 1, "public"))
		e.Actor = "https://elsewhere.example/users/alice"
		_, err := f.receive(e)
		wantReason(t, err, casfetch.ReasonForeignOrigin)
	})
	t.Run("an object that is not an asset URI", func(t *testing.T) {
		m := a.member(f.version(a), 1, "public")
		bad := *a
		bad.uri = f.origin.srv.URL + "/posts/" + a.id.String()
		m["id"] = bad.uri
		_, err := f.receive(f.envelope(&bad, m))
		wantReason(t, err, casfetch.ReasonForeignOrigin)
	})
	t.Run("a malformed member", func(t *testing.T) {
		m := a.member(f.version(a), 1, "public")
		m["files"] = []map[string]any{fileEntry("original", strings.Repeat("0", 64), 3, "../etc/passwd")}
		_, err := f.receive(f.envelope(a, m))
		wantReason(t, err, assetingest.ReasonArchivalInvalid)
	})
	t.Run("a checksum that does not match the content URI", func(t *testing.T) {
		m := a.member(f.version(a), 1, "public")
		e := fileEntry("original", a.hash[0], 1, "")
		e["checksums"] = []map[string]string{{"algorithm": "sha256", "value": strings.Repeat("a", 64)}}
		m["files"] = []map[string]any{e}
		_, err := f.receive(f.envelope(a, m))
		wantReason(t, err, assetingest.ReasonArchivalInvalid)
	})
	t.Run("not an aa:AssetVersion", func(t *testing.T) {
		e := f.envelope(a, a.member(f.version(a), 1, "public"))
		e.Type = federation.ActivityCreate
		_, err := f.receive(e)
		wantReason(t, err, assetingest.ReasonUnsupportedActivity)
	})
	if records() != 0 {
		t.Fatalf("%d records written for refused announcements", records())
	}
	f.nothingUsable(a.hash...)
}

func TestReceive_RecordedRefusals(t *testing.T) {
	t.Run("restricted tier", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "restricted")))
		wantReason(t, err, casfetch.ReasonSensitivity)
		if out.RecordID == uuid.Nil || f.origin.heads.Load() != 0 {
			t.Fatalf("outcome %+v heads %d", out, f.origin.heads.Load())
		}
	})
	t.Run("over the size budget", func(t *testing.T) {
		f := newFx(t, assetingest.Config{MaxTotalBytes: 40 << 10})
		a := f.newAsset(2)
		f.grant(f.peerID, a.uri, nil)
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonTooLarge)
		if f.origin.heads.Load() != 0 {
			t.Fatal("the origin was asked")
		}
	})
	t.Run("a refusal never blocks a later announcement (reordered grant)", func(t *testing.T) {
		f := newFx(t)
		a := f.newAsset(1)
		first, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.grant(f.peerID, a.uri, nil)
		// The refused activity keeps its stored answer.
		if _, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public"))); err != nil {
			t.Fatalf("a new announcement after the grant: %v", err)
		}
		_ = first
	})
}

func TestReceive_OutageThenRetry(t *testing.T) {
	f := newFx(t, assetingest.Config{MaxAttempts: 3})
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	f.origin.setMode(a.hash[1], "unavailable")
	e := f.envelope(a, a.member(f.version(a), 1, "public"))
	out, err := f.receive(e)
	if !assetingest.Retryable(err) || assetingest.ReasonOf(err) != casfetch.ReasonHTTPStatus || out.State != assetingest.StatePending {
		t.Fatalf("outage: %+v %v", out, err)
	}
	if st, r := f.state(out.RecordID); st != assetingest.StatePending || r != casfetch.ReasonHTTPStatus {
		t.Fatalf("stored %s/%s", st, r)
	}
	// Pending holds nothing: the intact first file is not pinned.
	f.nothingUsable(a.hash...)

	f.origin.setMode(a.hash[1], "")
	again, err := f.receive(e)
	if err != nil || again.State != assetingest.StateComplete || again.RecordID != out.RecordID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	for _, h := range a.hash {
		if f.pins(h, assetingest.PinSubjectType) != 1 {
			t.Fatalf("hash %s not pinned after the retry", h[:12])
		}
	}

	t.Run("attempts run out", func(t *testing.T) {
		g := newFx(t, assetingest.Config{MaxAttempts: 2})
		b := g.newAsset(1)
		g.grant(g.peerID, b.uri, nil)
		g.origin.setMode(b.hash[0], "unavailable")
		e := g.envelope(b, b.member(g.version(b), 1, "public"))
		if _, err := g.receive(e); !assetingest.Retryable(err) {
			t.Fatalf("first: %v", err)
		}
		out, err := g.receive(e)
		if assetingest.Retryable(err) || out.State != assetingest.StateRejected {
			t.Fatalf("second: %+v %v", out, err)
		}
		g.nothingUsable(b.hash...)
	})
}

func TestReceive_InterruptedCommitRollsBack(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	e := f.envelope(a, a.member(f.version(a), 1, "public"))
	var seen atomic.Int32
	assetingest.SetInComplete(f.ingest, func(ctx context.Context, tx pgx.Tx) error {
		// Inside the transaction the pins and verified files exist ...
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM storage_pins WHERE pin_subject_type = $1 AND object_hash = ANY($2)`,
			assetingest.PinSubjectType, a.hash).Scan(&n); err != nil || n != 2 {
			return fmt.Errorf("inside the commit: %d pins (%v)", n, err)
		}
		seen.Add(1)
		return errors.New("interrupted") // ... and then the commit fails.
	})
	out, err := f.receive(e)
	if seen.Load() != 1 || !assetingest.Retryable(err) || out.State != assetingest.StatePending {
		t.Fatalf("interrupted: %+v %v (hook ran %d)", out, err, seen.Load())
	}
	f.nothingUsable(a.hash...)

	assetingest.SetInComplete(f.ingest, nil)
	again, err := f.receive(e)
	if err != nil || again.State != assetingest.StateComplete {
		t.Fatalf("resumed: %+v %v", again, err)
	}
	for _, h := range a.hash {
		if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, h); n != 1 {
			t.Fatalf("hash %s still scheduled for GC after completing", h[:12])
		}
	}
}

func TestReceive_ConcurrentDeliveriesCompleteOnce(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	e := f.envelope(a, a.member(f.version(a), 1, "public"))
	var wg sync.WaitGroup
	outs := make([]assetingest.Outcome, 4)
	errs := make([]error, 4)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = f.receive(e)
		}(i)
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil || outs[i].State != assetingest.StateComplete || outs[i].RecordID != outs[0].RecordID {
			t.Fatalf("delivery %d: %+v %v", i, outs[i], errs[i])
		}
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1`, a.uri); n != 1 {
		t.Fatalf("%d records", n)
	}
	if f.pins(a.hash[0], assetingest.PinSubjectType) != 1 || f.pins(a.hash[0], casfetch.IngestPinType) != 0 {
		t.Fatalf("pins: record %d ingest %d", f.pins(a.hash[0], assetingest.PinSubjectType), f.pins(a.hash[0], casfetch.IngestPinType))
	}
}

// The database refuses what the service must never do (migration 00086).
func TestReceivedAssetInvariants(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	pres := f.origin.add([]byte("preservation claim"))
	a.files = append(a.files, fileEntry("preservation", pres, len("preservation claim"), ""))
	f.grant(f.peerID, a.uri, nil)
	done, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if err != nil {
		t.Fatal(err)
	}
	b := f.newAsset(1)
	pending := f.addPending(b)
	intact := func(stage string) {
		t.Helper()
		if st, _ := f.state(done.RecordID); st != assetingest.StateComplete {
			t.Fatalf("%s: the complete record became %s", stage, st)
		}
		if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1`, done.RecordID); n != 2 {
			t.Fatalf("%s: the complete record has %d file rows, want 2", stage, n)
		}
		if f.pins(a.hash[0], assetingest.PinSubjectType) != 1 {
			t.Fatalf("%s: the complete record's pin is gone", stage)
		}
	}

	wantRefused := func(name string, fn func(tx pgx.Tx) error) {
		t.Helper()
		err := pgx.BeginFunc(ctx, f.pool, fn)
		if err == nil || !strings.Contains(err.Error(), "#1574") {
			t.Fatalf("%s: committed (%v), want the #1574 guard", name, err)
		}
	}
	wantRefused("complete without pins", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE federation_received_asset_files SET state = 'verified', verified_at = now() WHERE received_asset_id = $1`, pending); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid() WHERE id = $1`, pending)
		return err
	})
	wantRefused("a pin on a pending record", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, $2, $3)`,
			a.hash[0], assetingest.PinSubjectType, pending.String())
		return err
	})
	wantRefused("a pin naming no record", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, $2, $3)`,
			a.hash[0], assetingest.PinSubjectType, uuid.NewString())
		return err
	})
	wantRefused("removing a complete record's pin", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM storage_pins WHERE pin_subject_type = $1 AND pin_subject_id = $2`,
			assetingest.PinSubjectType, done.RecordID.String())
		return err
	})
	wantRefused("rewriting a complete record", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE federation_received_assets SET sensitivity = 'team' WHERE id = $1`, done.RecordID)
		return err
	})
	wantRefused("rewriting a file's hash", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE federation_received_asset_files SET object_hash = $2 WHERE received_asset_id = $1`, pending, a.hash[0])
		return err
	})

	wantRefused("adding an unverified original to a complete record", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 9, 'original', $2, 1, '[]'::jsonb, 'image/png')`, done.RecordID, b.hash[0])
		return err
	})
	wantRefused("adding a claim to a complete record", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state)
			VALUES ($1, 9, 'access', $2, 1, '[]'::jsonb, 'image/png', 'not_transferred')`, done.RecordID, b.hash[0])
		return err
	})
	wantRefused("removing a complete record's original row", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'original'`, done.RecordID)
		return err
	})
	wantRefused("removing a complete record's claim row", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'preservation'`, done.RecordID)
		return err
	})
	wantRefused("adding a file to a pending record later", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 9, 'original', $2, 1, '[]'::jsonb, 'image/png')`, pending, a.hash[0])
		return err
	})
	intact("after the refused file-set changes")

	t.Run("deleting a record releases its pins", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `DELETE FROM federation_received_assets WHERE id = $1`, done.RecordID); err != nil {
			t.Fatal(err)
		}
		f.nothingUsable(a.hash...)
	})
}

// addPending writes a pending record for b directly, record and file set
// in one transaction (an invariant fixture, not a path the service
// takes).
func (f *fx) addPending(b *asset) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, $3, $4, $5, $6, 1, $7, '{}'::jsonb, 'public', 'k') RETURNING id`,
			f.peerID, f.origin.srv.URL+"/activities/"+uuid.NewString(), f.origin.srv.URL+"/users/alice", b.uri, b.id,
			f.version(b), strings.Repeat("0", 64)).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, b.hash[0])
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
