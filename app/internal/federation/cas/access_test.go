// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597 access-copy route, against a live database, through the mounted
// handler: real draft-cavage signatures, the fs storage backend, and the
// real decision (shares.Registry.CanPeerFetchAccessCopy) behind a thin
// recorder that only notes what the route asked. The copy is a genuinely
// sealed, promoted access copy (migration 00085's checks all run). The
// full decision matrix is in shares/access_test.go; these tests pin what
// the route adds: the strict shape, the parameters it passes, the answers
// it maps, and the bytes it serves. Ordinary fixtures only. Skips without
// AA_DB_PASSWORD.

package cas_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// allProfiles is every profile derived-transfer-1 needs (§16.7).
const allProfiles = `["archival-1","original-transfer-1","share-lifecycle-1","derived-transfer-1"]`

// recordingPolicy is the real decision; it only remembers each access-copy
// request the route made.
type recordingPolicy struct {
	inner *shares.Registry
	mu    sync.Mutex
	calls []shares.AccessCopyFetchRequest
}

func (p *recordingPolicy) CanPeerFetchOriginal(ctx context.Context, req shares.OriginalFetchRequest) (shares.OriginalFetchDecision, error) {
	return p.inner.CanPeerFetchOriginal(ctx, req)
}

func (p *recordingPolicy) CanPeerFetchAccessCopy(ctx context.Context, req shares.AccessCopyFetchRequest) (shares.AccessCopyFetchDecision, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	return p.inner.CanPeerFetchAccessCopy(ctx, req)
}

func (p *recordingPolicy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *recordingPolicy) last(t *testing.T) shares.AccessCopyFetchRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		t.Fatal("the route did not ask the access-copy decision")
	}
	return p.calls[len(p.calls)-1]
}

// hookBytes is the real storage, with test hooks: failStat makes Stat fail,
// statSize makes Stat report that size, and open replaces Download's reader,
// as a backend that cannot read, or ends early, would.
type hookBytes struct {
	inner    *storage.Service
	mu       sync.Mutex
	failStat bool
	statSize *int64
	open     func() io.ReadCloser
}

func (b *hookBytes) Download(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	b.mu.Lock()
	open := b.open
	b.mu.Unlock()
	if open != nil {
		return open(), &storage.ObjectInfo{}, nil
	}
	return b.inner.Download(ctx, hash, variant)
}

func (b *hookBytes) Stat(ctx context.Context, hash, variant string) (*storage.ObjectInfo, error) {
	b.mu.Lock()
	fail, size := b.failStat, b.statSize
	b.mu.Unlock()
	if fail {
		return nil, errors.New("backend unavailable")
	}
	if size != nil {
		return &storage.ObjectInfo{Size: *size}, nil
	}
	return b.inner.Stat(ctx, hash, variant)
}

func (b *hookBytes) set(f func(b *hookBytes)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

type accessFixture struct {
	*fixture
	derived  testPeer // a peer with all four profiles, announced to
	asset    uuid.UUID
	original []byte
	copy     []byte
	origHash string
	copyHash string
	policy   *recordingPolicy
	bytes    *hookBytes
	srv      *httptest.Server
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	fx := newFixture(t)
	ctx := context.Background()
	af := &accessFixture{fixture: fx}
	af.derived = fx.addPeer(allProfiles, true, "connected")
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, af.derived.id)
	})

	root := t.TempDir()
	be, err := fs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := storage.NewService(be, fx.pool)
	upload := func(n int, ct string) ([]byte, string) {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		res, err := svc.UploadOriginal(ctx, bytes.NewReader(b), ct, storage.PinRef{SubjectType: "test", SubjectID: "cas-access"})
		if err != nil {
			t.Fatal(err)
		}
		return b, res.Hash
	}
	af.original, af.origHash = upload(3000, "image/png")
	af.copy, af.copyHash = upload(2000, "image/webp")
	hashes := []string{af.origHash, af.copyHash}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, af.asset)
		_, _ = fx.pool.Exec(c, `DELETE FROM audit_events WHERE metadata->>'asset_id' = $1`, af.asset.String())
		_, _ = fx.pool.Exec(c, `DELETE FROM jobs WHERE type = 'asset.version_reconcile' AND payload->>'object_hash' = ANY($1)`, hashes)
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = ANY($1)`, hashes)
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = ANY($1)`, hashes)
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = ANY($1)`, hashes)
	})

	// Both objects verified at their own digest; the original's verified
	// rendition whose digest is the copy (its provenance, migration 00085);
	// the promotion pin.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = object_hash, content_size_bytes = size_bytes,
		    digest_source = 'write' WHERE variant_key = 'original' AND object_hash = ANY($1)`, []any{hashes}},
		{`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'hires', $3, 'image/webp', $2, $3, 'write', 'verified')`,
			[]any{af.origHash, af.copyHash, len(af.copy)}},
		{`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'version_promote', 'cas-access-test')`,
			[]any{af.copyHash}},
	} {
		if _, err := fx.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	af.asset = uuid.New()
	var version uuid.UUID
	if err := fx.pool.QueryRow(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		VALUES ($1, 'CAS access', 1, $2, $3, $4, 'public') RETURNING current_version_id`,
		af.asset, fx.grantor, af.origHash, len(af.original)).Scan(&version); err != nil {
		t.Fatal(err)
	}
	entry := `[{"role":"access","object_hash":"` + af.copyHash + `","size_bytes":` + strconv.Itoa(len(af.copy)) +
		`,"media_type":"image/webp","derived_from_hash":"` + af.origHash + `","derived_variant_key":"hires"}]`
	if err := pgx.BeginFunc(ctx, fx.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, $2::jsonb)`, version, entry)
		return err
	}); err != nil {
		t.Fatalf("seal with an access copy: %v", err)
	}

	// This origin's record that version 1 was announced to both peers, as
	// assetsend.recordClaims writes it before sending.
	files, _ := json.Marshal([]map[string]string{{"role": "original", "sha256": af.origHash}, {"role": "access", "sha256": af.copyHash}})
	for _, p := range []testPeer{af.derived, fx.peer} {
		var activity uuid.UUID
		if err := fx.pool.QueryRow(ctx,
			`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source, object_local_id)
			 VALUES ($1, $2, 'https://origin.example/users/x', $3, '{}'::jsonb, 'local', $4) RETURNING id`,
			"https://origin.example/activities/"+uuid.NewString(), string(federation.ActivityAAAssetVersion), fx.grantor, af.asset.String(),
		).Scan(&activity); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO federation_asset_announcement_claims (activity_id, peer_id, asset_id, version_number, files)
			 VALUES ($1, $2, $3, 1, $4::jsonb)`, activity, p.id, af.asset, string(files)); err != nil {
			t.Fatal(err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	af.policy = &recordingPolicy{inner: fx.reg}
	af.bytes = &hookBytes{inner: svc}
	h := cas.NewHandler(cas.Deps{
		Lookup:    dbPeerLookup{pool: fx.pool},
		ActorKeys: actorKeys{fx.aliceURI: fx.alicePriv.Public().(ed25519.PublicKey)},
		Policy:    af.policy,
		Bytes:     af.bytes,
		Limiter:   fx.limiter,
		Audit:     audit.NewRecorder(fx.pool, logger),
	})
	r := chi.NewRouter()
	h.Mount(r)
	af.srv = httptest.NewServer(r)
	t.Cleanup(af.srv.Close)
	return af
}

// grant shares the asset with p, carrying the access-copy grant when access
// (set on the row: this slice has no setter).
func (af *accessFixture) grant(p testPeer, original, access bool) *shares.Share {
	af.t.Helper()
	s := af.share(af.asset, p, original, nil, nil)
	if access {
		if _, err := af.pool.Exec(context.Background(),
			`UPDATE federation_shares SET allow_access_copies = TRUE WHERE id = $1`, s.ID); err != nil {
			af.t.Fatal(err)
		}
	}
	return s
}

// send makes one request to target (a path plus "?query"), signed with
// keyID/priv unless priv is nil.
func (af *accessFixture) send(method, target, keyID string, priv ed25519.PrivateKey) (int, []byte, http.Header) {
	af.t.Helper()
	req, err := http.NewRequest(method, af.srv.URL+target, nil)
	if err != nil {
		af.t.Fatal(err)
	}
	if priv != nil {
		req.Header.Set("Host", req.URL.Host)
		if err := httpsig.SignAndAttach(req, []byte{}, keyID, priv); err != nil {
			af.t.Fatal(err)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		af.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// copyTarget is the canonical request target for the copy of version 1.
func (af *accessFixture) copyTarget() string {
	return cas.AccessFilePath(af.asset, 1, af.copyHash) + "?size=" + strconv.Itoa(len(af.copy))
}

func (af *accessFixture) asDerived(method, target string) (int, []byte, http.Header) {
	return af.send(method, target, af.derived.keyID(), af.derived.priv)
}

func (af *accessFixture) lastAudit(t *testing.T, event string) map[string]any {
	t.Helper()
	var raw []byte
	if err := af.pool.QueryRow(context.Background(),
		`SELECT metadata FROM audit_events WHERE event_type = $1 AND metadata->>'asset_id' = $2
		 ORDER BY occurred_at DESC LIMIT 1`, event, af.asset.String()).Scan(&raw); err != nil {
		t.Fatalf("read %s audit: %v", event, err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestCASAccess_ServesGrantedCopy_GETAndHEAD(t *testing.T) {
	af := newAccessFixture(t)
	s := af.grant(af.derived, false, true)

	status, body, h := af.asDerived(http.MethodGet, af.copyTarget())
	if status != http.StatusOK || !bytes.Equal(body, af.copy) {
		t.Fatalf("GET %d, %d bytes; want 200 and the copy's %d bytes", status, len(body), len(af.copy))
	}
	if bytes.Equal(body, af.original) {
		t.Fatal("the route served the original")
	}
	sum := sha256.Sum256(af.copy)
	if h.Get("Content-Length") != strconv.Itoa(len(af.copy)) ||
		h.Get("Repr-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":" ||
		h.Get("Content-Type") != "application/octet-stream" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET headers %v", h)
	}
	got := af.policy.last(t)
	want := shares.AccessCopyFetchRequest{PeerID: af.derived.id, ActorURL: "", AssetID: af.asset, Version: 1,
		SHA256: af.copyHash, Size: int64(len(af.copy))}
	if got != want {
		t.Fatalf("decision asked %+v, want %+v", got, want)
	}
	m := af.lastAudit(t, audit.EventFederationCASServed)
	if m["class"] != cas.ClassAccess || m["version"] != float64(1) || m["share_id"] != s.ID.String() ||
		m["bytes_sent"] != float64(len(af.copy)) || m["hash"] != af.copyHash {
		t.Fatalf("served audit %v", m)
	}

	status, body, h = af.asDerived(http.MethodHead, af.copyTarget())
	if status != http.StatusOK || len(body) != 0 || h.Get("Content-Length") != strconv.Itoa(len(af.copy)) {
		t.Fatalf("HEAD %d, body %d, headers %v", status, len(body), h)
	}
	var served int
	_ = af.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'asset_id' = $2`,
		audit.EventFederationCASServed, af.asset.String()).Scan(&served)
	if served != 2 {
		t.Fatalf("served audit rows = %d, want 2", served)
	}
}

// A normal signed request without the profiles or without the grant is
// refused with the §12.4 answers; the precise reason stays in the audit.
func TestCASAccess_ProfileAndGrantRefusals(t *testing.T) {
	af := newAccessFixture(t)

	// No grant at all, then an original-only grant: not_available.
	var originalOnly *shares.Share
	for _, original := range []bool{false, true} {
		if original {
			originalOnly = af.grant(af.derived, true, false)
		}
		status, body, _ := af.asDerived(http.MethodGet, af.copyTarget())
		if status != http.StatusForbidden || reason(body) != cas.ReasonNotAvailable || bytes.Contains(body, af.copy[:16]) {
			t.Fatalf("original grant %v: %d %s, want 403 not_available", original, status, body)
		}
	}
	if r := af.lastAudit(t, audit.EventFederationCASRefused)["reason"]; r != string(shares.RejectReasonAccessCopyNotGranted) {
		t.Fatalf("audit reason %v, want access_copy_not_granted", r)
	}

	// A peer without derived-transfer-1, signed by one of its actors: the
	// route passes the authenticated actor, and the decision refuses the
	// profile.
	af.grant(af.peer, false, true)
	status, body, _ := af.send(http.MethodGet, af.copyTarget(), af.aliceURI+"#main-key", af.alicePriv)
	if status != http.StatusForbidden || reason(body) != cas.ReasonProfileNotNegotiated {
		t.Fatalf("missing profile: %d %s, want 403 profile_not_negotiated", status, body)
	}
	if got := af.policy.last(t); got.PeerID != af.peer.id || got.ActorURL != af.aliceURI {
		t.Fatalf("decision asked peer %s actor %q, want %s and %q", got.PeerID, got.ActorURL, af.peer.id, af.aliceURI)
	}

	// Another version of the same asset, now under a grant that carries the
	// access-copy flag (set on the existing share of this tuple):
	// not_available, like every refusal that concerns the asset.
	if _, err := af.pool.Exec(context.Background(),
		`UPDATE federation_shares SET allow_access_copies = TRUE WHERE id = $1`, originalOnly.ID); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := af.asDerived(http.MethodGet, af.copyTarget()); status != http.StatusOK {
		t.Fatalf("version 1 under the flagged grant: %d, want 200", status)
	}
	status, body, _ = af.asDerived(http.MethodGet,
		cas.AccessFilePath(af.asset, 2, af.copyHash)+"?size="+strconv.Itoa(len(af.copy)))
	if status != http.StatusForbidden || reason(body) != cas.ReasonNotAvailable {
		t.Fatalf("version 2: %d %s, want 403 not_available", status, body)
	}
	if r := af.lastAudit(t, audit.EventFederationCASRefused)["reason"]; r != string(shares.RejectReasonVersionNotFound) {
		t.Fatalf("audit reason %v, want version_not_found", r)
	}

	// Unsigned.
	if status, body, _ := af.send(http.MethodGet, af.copyTarget(), "", nil); status != http.StatusUnauthorized ||
		reason(body) != cas.ReasonUnauthenticated {
		t.Fatalf("unsigned: %d %s, want 401", status, body)
	}
}

// When storage cannot confirm the authorised object before headers are
// written, the answer is a retryable 503 with no bytes.
func TestCASAccess_StoragePreflightFailure(t *testing.T) {
	af := newAccessFixture(t)
	af.grant(af.derived, false, true)
	af.bytes.set(func(b *hookBytes) { b.failStat = true })
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		status, body, h := af.asDerived(method, af.copyTarget())
		if status != http.StatusServiceUnavailable || h.Get("Repr-Digest") != "" || bytes.Contains(body, af.copy[:16]) {
			t.Fatalf("%s: %d, headers %v", method, status, h)
		}
		if method == http.MethodGet && reason(body) != cas.ReasonUnavailable {
			t.Fatalf("GET body %s, want temporarily_unavailable", body)
		}
	}
	if r := af.lastAudit(t, audit.EventFederationCASRefused)["reason"]; r != "bytes_unavailable" {
		t.Fatalf("audit reason %v, want bytes_unavailable", r)
	}
}

// Only the canonical spelling of each scalar is a request; anything else is
// 400 bad_request and never reaches the decision. The original route keeps
// refusing access and preview.
func TestCASAccess_CanonicalShape(t *testing.T) {
	af := newAccessFixture(t)
	af.grant(af.derived, false, true)
	size := strconv.Itoa(len(af.copy))
	base := "/federation/assets/" + af.asset.String() + "/versions/"
	file := "/files/access/" + af.copyHash
	cases := map[string]string{
		"version zero":           base + "0" + file + "?size=" + size,
		"version leading zero":   base + "01" + file + "?size=" + size,
		"version negative":       base + "-1" + file + "?size=" + size,
		"version too large":      base + "9223372036854775808" + file + "?size=" + size,
		"version not an integer": base + "1.0" + file + "?size=" + size,
		"class original":         base + "1/files/original/" + af.copyHash + "?size=" + size,
		"class preview":          base + "1/files/preview/" + af.copyHash + "?size=" + size,
		"class upper case":       base + "1/files/ACCESS/" + af.copyHash + "?size=" + size,
		"hash upper case":        base + "1/files/access/" + fmt.Sprintf("%X", sha256.Sum256(af.copy)) + "?size=" + size,
		"hash short":             base + "1/files/access/" + af.copyHash[:63] + "?size=" + size,
		"asset upper case":       "/federation/assets/" + fmt.Sprintf("%X", af.asset[:]) + "/versions/1" + file + "?size=" + size,
		"size missing":           base + "1" + file,
		"size empty":             base + "1" + file + "?size=",
		"size leading zero":      base + "1" + file + "?size=0" + size,
		"size signed":            base + "1" + file + "?size=+" + size,
		"size twice":             base + "1" + file + "?size=" + size + "&size=" + size,
		"size and another param": base + "1" + file + "?size=" + size + "&x=1",
		"size percent-encoded":   base + "1" + file + "?size=%3" + size[:1] + size[1:],
		"size too large":         base + "1" + file + "?size=9223372036854775808",
		"another param only":     base + "1" + file + "?x=1",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			before := af.policy.count()
			status, body, _ := af.asDerived(http.MethodGet, target)
			if status != http.StatusBadRequest || reason(body) != cas.ReasonBadRequest {
				t.Fatalf("%s: %d %s, want 400 bad_request", target, status, body)
			}
			if af.policy.count() != before {
				t.Fatal("a malformed request reached the decision")
			}
		})
	}

	// The canonical target is served.
	if status, _, _ := af.asDerived(http.MethodGet, af.copyTarget()); status != http.StatusOK {
		t.Fatalf("canonical target: %d", status)
	}
	// The original route still refuses the access class, for any peer.
	status, body, _ := af.asDerived(http.MethodGet, cas.FilePath(af.asset, cas.ClassAccess, af.copyHash)+"?size="+size)
	if status != http.StatusForbidden || reason(body) != cas.ReasonClassNotAvailable {
		t.Fatalf("original route, class access: %d %s, want 403 class_not_available", status, body)
	}
}

// allowPolicy allows every access-copy request, for tests of the serving
// boundary itself (an empty object, a store that ends after the headers),
// which a real sealed copy cannot be made to show. The real decision is
// exercised by every other test here and in shares/access_test.go.
type allowPolicy struct{ share *shares.Share }

func (allowPolicy) CanPeerFetchOriginal(context.Context, shares.OriginalFetchRequest) (shares.OriginalFetchDecision, error) {
	return shares.OriginalFetchDecision{}, errors.New("not used")
}

func (p allowPolicy) CanPeerFetchAccessCopy(context.Context, shares.AccessCopyFetchRequest) (shares.AccessCopyFetchDecision, error) {
	return shares.AccessCopyFetchDecision{Allowed: true, Share: p.share}, nil
}

// boundary mounts a second handler with allowPolicy and its own hookBytes,
// sharing the fixture's peers, keys and audit, and returns its server URL.
func (af *accessFixture) boundary(t *testing.T, b *hookBytes) string {
	t.Helper()
	h := cas.NewHandler(cas.Deps{
		Lookup:  dbPeerLookup{pool: af.pool},
		Policy:  allowPolicy{share: &shares.Share{ID: uuid.New()}},
		Bytes:   b,
		Limiter: af.limiter,
		Audit:   audit.NewRecorder(af.pool, slog.New(slog.NewTextHandler(io.Discard, nil))),
	})
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// sendTo sends a request with body (signed over it) to base+target as the
// derived peer. unknownLength hides the body's length, so it is sent chunked.
func (af *accessFixture) sendTo(base, method, target string, body []byte, unknownLength bool) (int, []byte, http.Header) {
	af.t.Helper()
	if body == nil {
		body = []byte{} // the signature's digest is of the empty body
	}
	var rd io.Reader = bytes.NewReader(body)
	if unknownLength {
		rd = io.MultiReader(bytes.NewReader(body)) // no Len: the transport cannot know the length
	}
	req, err := http.NewRequest(method, base+target, rd)
	if err != nil {
		af.t.Fatal(err)
	}
	if unknownLength {
		req.ContentLength = -1
	}
	req.Header.Set("Host", req.URL.Host)
	if err := httpsig.SignAndAttach(req, body, af.derived.keyID(), af.derived.priv); err != nil {
		af.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		af.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) // a short body after a committed 200 ends early; what arrived is returned
	return resp.StatusCode, b, resp.Header
}

// The body must be empty (§8.5.1, §8.5), declared or not; a request with a
// body is 400 bad_request and never reaches the decision.
func TestCASAccess_BodyMustBeEmpty(t *testing.T) {
	af := newAccessFixture(t)
	af.grant(af.derived, false, true)
	for _, c := range []struct {
		name          string
		body          []byte
		unknownLength bool
	}{
		{"declared one byte", []byte("x"), false},
		{"declared JSON", []byte(`{"a":1}`), false},
		{"chunked, length not declared", []byte("payload"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := af.policy.count()
			status, body, _ := af.sendTo(af.srv.URL, http.MethodGet, af.copyTarget(), c.body, c.unknownLength)
			if status != http.StatusBadRequest || reason(body) != cas.ReasonBadRequest {
				t.Fatalf("%d %s, want 400 bad_request", status, body)
			}
			if af.policy.count() != before {
				t.Fatal("a request with a body reached the decision")
			}
			if r := af.lastAuditAny(t, "body_not_empty"); r == 0 {
				t.Fatal("no refused audit row with reason body_not_empty")
			}
		})
	}
	// An empty body, declared or with an unknown length the client
	// resolves to empty, is served.
	for _, unknown := range []bool{false, true} {
		status, body, _ := af.sendTo(af.srv.URL, http.MethodGet, af.copyTarget(), nil, unknown)
		if status != http.StatusOK || !bytes.Equal(body, af.copy) {
			t.Fatalf("empty body (unknown length %v): %d, %d bytes", unknown, status, len(body))
		}
	}
}

// lastAuditAny counts refused rows for this asset's route with reason r,
// whether or not the shape was parsed (a body refusal has no asset id).
func (af *accessFixture) lastAuditAny(t *testing.T, r string) int {
	t.Helper()
	var n int
	if err := af.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'reason' = $2
		   AND metadata->>'peer_id' IS NULL AND metadata->>'class' = 'access'`,
		audit.EventFederationCASRefused, r).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk read failed") }
func (failingReader) Close() error             { return nil }

// A store whose reader fails, or ends before the first bytes, is answered
// with a retryable 503 before any success status: no copy length, no digest.
func TestCASAccess_ReadPreflightFailure(t *testing.T) {
	af := newAccessFixture(t)
	af.grant(af.derived, false, true)
	for _, c := range []struct {
		name string
		open func() io.ReadCloser
	}{
		{"read error", func() io.ReadCloser { return failingReader{} }},
		{"early end", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(af.copy[:100])) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			af.bytes.set(func(b *hookBytes) { b.open = c.open })
			t.Cleanup(func() { af.bytes.set(func(b *hookBytes) { b.open = nil }) })
			status, body, h := af.asDerived(http.MethodGet, af.copyTarget())
			if status != http.StatusServiceUnavailable || reason(body) != cas.ReasonUnavailable {
				t.Fatalf("%d %s, want 503 temporarily_unavailable", status, body)
			}
			if h.Get("Repr-Digest") != "" || h.Get("Content-Length") == strconv.Itoa(len(af.copy)) ||
				h.Get("Content-Type") != "application/json" || bytes.Contains(body, af.copy[:16]) {
				t.Fatalf("an early failure carried success headers or bytes: %v", h)
			}
			m := af.lastAudit(t, audit.EventFederationCASRefused)
			if m["reason"] != "bytes_unavailable" || m["read_error"] == nil {
				t.Fatalf("refused audit %v, want bytes_unavailable with read_error", m)
			}
			// HEAD reads no bytes and is unaffected.
			if status, _, h := af.asDerived(http.MethodHead, af.copyTarget()); status != http.StatusOK ||
				h.Get("Content-Length") != strconv.Itoa(len(af.copy)) {
				t.Fatalf("HEAD: %d %v", status, h)
			}
		})
	}
}

// After the 200 is written the status cannot change: a store that ends
// early, with a bare EOF, is recorded with the bytes actually sent and a
// copy_error.
func TestCASAccess_LateShortCopyIsAudited(t *testing.T) {
	af := newAccessFixture(t)
	const size, stored = 100_000, 50_000
	b := &hookBytes{}
	b.set(func(b *hookBytes) {
		n := int64(size)
		b.statSize = &n
		b.open = func() io.ReadCloser { return io.NopCloser(io.LimitReader(rand.Reader, stored)) }
	})
	base := af.boundary(t, b)
	target := cas.AccessFilePath(af.asset, 1, af.copyHash) + "?size=" + strconv.Itoa(size)
	status, body, h := af.sendTo(base, http.MethodGet, target, nil, false)
	if status != http.StatusOK || h.Get("Content-Length") != strconv.Itoa(size) || len(body) != stored {
		t.Fatalf("%d, Content-Length %s, %d bytes received; want 200, %d, %d", status, h.Get("Content-Length"), len(body), size, stored)
	}
	m := af.lastAudit(t, audit.EventFederationCASServed)
	if m["bytes_sent"] != float64(stored) || m["copy_error"] == nil ||
		!bytes.Contains([]byte(fmt.Sprint(m["copy_error"])), []byte("short copy")) {
		t.Fatalf("served audit %v, want bytes_sent %d and a short-copy error", m, stored)
	}
}

// An empty object is a valid copy: GET is 200 with no bytes and no error,
// HEAD is 200.
func TestCASAccess_EmptyObject(t *testing.T) {
	af := newAccessFixture(t)
	b := &hookBytes{}
	b.set(func(b *hookBytes) {
		zero := int64(0)
		b.statSize = &zero
		b.open = func() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }
	})
	base := af.boundary(t, b)
	target := cas.AccessFilePath(af.asset, 1, af.copyHash) + "?size=0"
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		status, body, h := af.sendTo(base, method, target, nil, false)
		if status != http.StatusOK || len(body) != 0 || h.Get("Content-Length") != "0" {
			t.Fatalf("%s: %d, %d bytes, Content-Length %q", method, status, len(body), h.Get("Content-Length"))
		}
	}
	m := af.lastAudit(t, audit.EventFederationCASServed)
	if m["method"] != http.MethodHead {
		t.Fatalf("last served audit %v, want the HEAD", m)
	}
	var getRow map[string]any
	var raw []byte
	if err := af.pool.QueryRow(context.Background(),
		`SELECT metadata FROM audit_events WHERE event_type = $1 AND metadata->>'asset_id' = $2 AND metadata->>'method' = 'GET'
		 ORDER BY occurred_at DESC LIMIT 1`, audit.EventFederationCASServed, af.asset.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &getRow)
	if getRow["bytes_sent"] != float64(0) || getRow["copy_error"] != nil {
		t.Fatalf("empty GET audit %v, want bytes_sent 0 and no error", getRow)
	}
}
