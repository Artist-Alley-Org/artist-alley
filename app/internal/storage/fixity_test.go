// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: variant fixity, pinned on a real database and the fs backend.
//
//   - a derived write records the digest and size of the bytes the
//     backend read, at generation 1; a regeneration binds generation 2;
//   - an original records its own address as its digest; a deduplicated
//     re-upload never rewrites a digest;
//   - while a rewrite is in flight the row says unverifiable, never the
//     old digest; a failed Put leaves it unverifiable;
//   - concurrent writers never leave a row describing bytes that are not
//     the stored ones (the slower writer's Put lands last);
//   - the sweep's VerifyVariant catches same-size corruption and missing
//     bytes against the variant's OWN digest, and restores a row whose
//     bytes are back;
//   - a legacy row stays unverifiable through the sweep and is only
//     promoted by an explicit recheck, which refuses inconsistent bytes;
//   - a large variant streams end to end.

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

type fixityRig struct {
	pool *pgxpool.Pool
	svc  *storage.Service
}

func newFixityRig(t *testing.T) *fixityRig {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	return &fixityRig{pool: pool, svc: svc}
}

// object registers a fresh parent object so variant rows can reference it.
func (r *fixityRig) object(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(b)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 1, 'image/png', 'fs')`, hash); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.pool.Exec(context.Background(), `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return hash
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (r *fixityRig) fixity(t *testing.T, hash, key string) storage.VariantFixity {
	t.Helper()
	fx, err := r.svc.ReadVariantFixity(context.Background(), hash, key)
	if err != nil {
		t.Fatalf("ReadVariantFixity %s: %v", key, err)
	}
	return fx
}

func (r *fixityRig) stored(t *testing.T, hash, key string) string {
	t.Helper()
	rc, _, err := r.svc.Backend.Get(context.Background(), hash, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestPutVariant_RecordsTheDigestOfTheBytesWritten_AndRegenerationBindsANewGeneration(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	a := []byte("rendered preview, first generation")
	w, err := r.svc.PutVariant(ctx, hash, "preview", bytes.NewReader(a), "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if w.SHA256 != sum(a) || w.Size != int64(len(a)) || w.Generation != 1 {
		t.Fatalf("write = %+v, want digest of a, %d bytes, generation 1", w, len(a))
	}
	fx := r.fixity(t, hash, "preview")
	if fx.SHA256 != sum(a) || fx.Size != int64(len(a)) || fx.Status != storage.FixityVerified || fx.Source != storage.DigestFromWrite || fx.Generation != 1 {
		t.Fatalf("row = %+v", fx)
	}
	if f, d, err := r.verifyVariant(ctx, hash, "preview"); err != nil || f != "" {
		t.Fatalf("sweep on a fresh write: %q %q %v", f, d, err)
	}

	// Regeneration: new bytes, new generation, the old digest gone.
	b := []byte("rendered preview, second generation, longer")
	if _, err := r.svc.PutVariant(ctx, hash, "preview", bytes.NewReader(b), "image/webp"); err != nil {
		t.Fatal(err)
	}
	fx = r.fixity(t, hash, "preview")
	if fx.SHA256 != sum(b) || fx.Size != int64(len(b)) || fx.Generation != 2 || fx.Status != storage.FixityVerified {
		t.Fatalf("after regeneration: %+v", fx)
	}
	if got := r.stored(t, hash, "preview"); got != fx.SHA256 {
		t.Fatalf("row digest %s, stored bytes %s", fx.SHA256, got)
	}

	if _, err := r.svc.PutVariant(ctx, hash, storage.VariantOriginal, bytes.NewReader(a), ""); err == nil {
		t.Fatal("PutVariant wrote an original")
	}
}

func TestPutVariant_ARewriteInFlightNeverClaimsTheOldDigest(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	if _, err := r.svc.PutVariant(ctx, hash, "col", bytes.NewReader([]byte("old")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	var during struct {
		status string
		digest *string
		gen    int64
	}
	r.svc.SetVariantAfterPutHook(func(v string) {
		// The write lock is held: read the row directly, as an ordinary
		// reader would, between the Put and the record step.
		_ = r.pool.QueryRow(context.Background(),
			`SELECT fixity_status, content_sha256, generation FROM storage_variants WHERE object_hash = $1 AND variant_key = $2`,
			hash, v).Scan(&during.status, &during.digest, &during.gen)
	})
	if _, err := r.svc.PutVariant(ctx, hash, "col", bytes.NewReader([]byte("new bytes")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	r.svc.SetVariantAfterPutHook(nil)
	if during.status != storage.FixityUnverifiable || during.digest != nil || during.gen != 2 {
		t.Fatalf("mid-rewrite row: status %q digest %v generation %d, want unverifiable, none, 2", during.status, during.digest, during.gen)
	}

	// A failed Put leaves the row unverifiable, not claiming the old digest.
	failing := &failingBackend{Backend: r.svc.Backend}
	bad := storage.NewService(failing, r.pool)
	if _, err := bad.PutVariant(ctx, hash, "col", bytes.NewReader([]byte("never stored")), "image/webp"); err == nil {
		t.Fatal("failing Put reported success")
	}
	fx := r.fixity(t, hash, "col")
	if fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" || fx.Generation != 3 {
		t.Fatalf("after a failed Put: %+v, want unverifiable with no digest at generation 3", fx)
	}
}

type failingBackend struct{ storage.Backend }

func (f *failingBackend) Put(ctx context.Context, hash, variant string, r io.Reader) (*storage.ObjectInfo, error) {
	_, _ = io.CopyN(io.Discard, r, 2)
	return nil, errors.New("backend unavailable")
}

// gatedReader yields the first half of b, then waits (bounded) for gate
// before yielding the rest: a slow writer whose Put is still running.
type gatedReader struct {
	b    []byte
	off  int
	gate <-chan struct{}
	wait time.Duration
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if g.off == len(g.b)/2 && g.gate != nil {
		select {
		case <-g.gate:
		case <-time.After(g.wait):
		}
		g.gate = nil
	}
	if g.off >= len(g.b) {
		return 0, io.EOF
	}
	end := len(g.b)
	if g.off < len(g.b)/2 {
		end = len(g.b) / 2
	}
	n := copy(p, g.b[g.off:end])
	g.off += n
	return n, nil
}

func TestPutVariant_ConcurrentWritersNeverRecordBytesThatAreNotStored(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)

	// Deterministic: A's Put stalls half-way; B is started meanwhile. If
	// B could write while A's Put is still running, A's bytes would land
	// after B recorded its digest. With the write lock, B waits for A.
	a := bytes.Repeat([]byte("A"), 64*1024)
	b := bytes.Repeat([]byte("B"), 64*1024)
	bDone := make(chan struct{})
	aStarted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var aErr, bErr error
	go func() {
		defer wg.Done()
		close(aStarted)
		_, aErr = r.svc.PutVariant(ctx, hash, "preview", &gatedReader{b: a, gate: bDone, wait: 750 * time.Millisecond}, "image/webp")
	}()
	<-aStarted
	time.Sleep(100 * time.Millisecond) // let A take the lock and begin its Put
	go func() {
		defer wg.Done()
		_, bErr = r.svc.PutVariant(ctx, hash, "preview", bytes.NewReader(b), "image/webp")
		close(bDone)
	}()
	wg.Wait()
	if aErr != nil || bErr != nil {
		t.Fatalf("writers: A %v, B %v", aErr, bErr)
	}
	fx := r.fixity(t, hash, "preview")
	if got := r.stored(t, hash, "preview"); fx.SHA256 != got || fx.Status != storage.FixityVerified {
		t.Fatalf("row records %s (%s) but the stored bytes hash to %s", fx.SHA256, fx.Status, got)
	}

	// A burst of writers: the invariant holds whoever wins.
	const n = 8
	var burst sync.WaitGroup
	for i := 0; i < n; i++ {
		burst.Add(1)
		go func(i int) {
			defer burst.Done()
			body := []byte(strings.Repeat(string(rune('a'+i)), 1000+i*37))
			if _, err := r.svc.PutVariant(ctx, hash, "screen", bytes.NewReader(body), "image/webp"); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	burst.Wait()
	fx = r.fixity(t, hash, "screen")
	if got := r.stored(t, hash, "screen"); fx.SHA256 != got || fx.Generation != n {
		t.Fatalf("after %d writers: row %s generation %d, stored %s", n, fx.SHA256, fx.Generation, got)
	}
}

func TestVerifyVariant_CatchesSameSizeCorruptionAndMissingBytes(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	good := []byte("0123456789abcdef0123456789abcdef")
	if _, err := r.svc.PutVariant(ctx, hash, "hires", bytes.NewReader(good), "image/webp"); err != nil {
		t.Fatal(err)
	}
	// Same length, different bytes, behind the row's back.
	evil := []byte("0123456789abcdef0123456789abcdeX")
	if _, err := r.svc.Backend.Put(ctx, hash, "hires", bytes.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	f, detail, err := r.verifyVariant(ctx, hash, "hires")
	if err != nil || f != storage.FindingChecksumMismatch {
		t.Fatalf("same-size corruption: finding %q (%s) %v", f, detail, err)
	}
	if fx := r.fixity(t, hash, "hires"); fx.Status != storage.FixityMismatch || fx.SHA256 != sum(good) {
		t.Fatalf("row after corruption: %+v", fx)
	}
	// The good bytes come back: verified again.
	if _, err := r.svc.Backend.Put(ctx, hash, "hires", bytes.NewReader(good)); err != nil {
		t.Fatal(err)
	}
	if f, _, err := r.verifyVariant(ctx, hash, "hires"); err != nil || f != "" {
		t.Fatalf("restored bytes: %q %v", f, err)
	}
	if fx := r.fixity(t, hash, "hires"); fx.Status != storage.FixityVerified {
		t.Fatalf("restored row: %+v", fx)
	}
	// Missing bytes.
	if err := r.svc.Backend.Delete(ctx, hash, "hires"); err != nil {
		t.Fatal(err)
	}
	f, _, err = r.verifyVariant(ctx, hash, "hires")
	if err != nil || f != storage.FindingMissingObject {
		t.Fatalf("missing bytes: %q %v", f, err)
	}
	if fx := r.fixity(t, hash, "hires"); fx.Status != storage.FixityMissing {
		t.Fatalf("row after missing bytes: %+v", fx)
	}
}

func TestLegacyRowStaysUnverifiableUntilAnExplicitRecheck(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	legacy := func(key string, body []byte, recordedSize int) {
		t.Helper()
		if _, err := r.svc.Backend.Put(ctx, hash, key, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type) VALUES ($1, $2, $3, 'image/webp')`,
			hash, key, recordedSize); err != nil {
			t.Fatal(err)
		}
	}
	ok := []byte("legacy preview bytes")
	legacy("preview", ok, len(ok))
	legacy("col", []byte("legacy col"), 999) // recorded size disagrees

	fx := r.fixity(t, hash, "preview")
	if fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" || fx.Generation != 0 {
		t.Fatalf("legacy row: %+v", fx)
	}
	// The routine sweep checks size and never promotes.
	if f, _, err := r.verifyVariant(ctx, hash, "preview"); err != nil || f != "" {
		t.Fatalf("sweep on a consistent legacy row: %q %v", f, err)
	}
	if fx := r.fixity(t, hash, "preview"); fx.Status != storage.FixityUnverifiable {
		t.Fatalf("sweep promoted a legacy row: %+v", fx)
	}
	if f, _, err := r.verifyVariant(ctx, hash, "col"); err != nil || f != storage.FindingSizeMismatch {
		t.Fatalf("sweep on a wrong-size legacy row: %q %v", f, err)
	}

	// The explicit backfill, one row per page from this object's cursor,
	// so it never touches rows other tests left in the shared database.
	b1, err := r.svc.BackfillVariantDigests(ctx, hash, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := r.svc.BackfillVariantDigests(ctx, b1.NextHash, b1.NextVariant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Scanned != 1 || b1.NextHash != hash || b1.NextVariant != "col" || b1.Outcomes[storage.RecheckSizeMismatch] != 1 {
		t.Fatalf("page 1: %+v", b1)
	}
	if b2.Scanned != 1 || b2.NextHash != hash || b2.NextVariant != "preview" || b2.Outcomes[storage.RecheckVerified] != 1 {
		t.Fatalf("page 2: %+v", b2)
	}
	fx = r.fixity(t, hash, "preview")
	if fx.Status != storage.FixityVerified || fx.SHA256 != sum(ok) || fx.Source != storage.DigestFromRecheck {
		t.Fatalf("rechecked legacy row: %+v", fx)
	}
	if fx := r.fixity(t, hash, "col"); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
		t.Fatalf("inconsistent legacy row was promoted: %+v", fx)
	}
	// A recheck never replaces a digest recorded at write time.
	if _, err := r.svc.PutVariant(ctx, hash, "screen", bytes.NewReader([]byte("written")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	if out, err := r.svc.RecheckVariant(ctx, hash, "screen"); err != nil || out != storage.RecheckVerified {
		t.Fatalf("recheck of a written row: %q %v", out, err)
	}
	if fx := r.fixity(t, hash, "screen"); fx.Source != storage.DigestFromWrite {
		t.Fatalf("recheck replaced a write digest: %+v", fx)
	}

	// A healed row (bytes found on the backend without one) is unverifiable.
	if _, err := r.svc.Backend.Put(ctx, hash, "poster", bytes.NewReader([]byte("orphan poster"))); err != nil {
		t.Fatal(err)
	}
	healed, err := r.svc.ReconcileVariant(ctx, hash, "poster", nil)
	if err != nil || !healed {
		t.Fatalf("reconcile: %v %v", healed, err)
	}
	if fx := r.fixity(t, hash, "poster"); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
		t.Fatalf("healed row: %+v", fx)
	}
}

func TestUploadOriginal_RecordsItsAddress_AndADedupNeverRewritesIt(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	pin := storage.PinRef{SubjectType: "user", SubjectID: "9159701"}
	res, err := r.svc.UploadOriginal(ctx, bytes.NewReader(payload), "image/png", pin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, res.Hash)
	})
	fx := r.fixity(t, res.Hash, storage.VariantOriginal)
	if fx.SHA256 != res.Hash || fx.SHA256 != sum(payload) || fx.Status != storage.FixityVerified || fx.Source != storage.DigestFromWrite {
		t.Fatalf("original row: %+v", fx)
	}
	// Make it legacy, then upload the same bytes again: no bytes are
	// written, so nothing is promoted.
	if _, err := r.pool.Exec(ctx, `UPDATE storage_variants SET content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL, fixity_status = 'unverifiable' WHERE object_hash = $1 AND variant_key = 'original'`, res.Hash); err != nil {
		t.Fatal(err)
	}
	again, err := r.svc.UploadOriginal(ctx, bytes.NewReader(payload), "image/png", pin)
	if err != nil || !again.Deduped {
		t.Fatalf("re-upload: %+v %v", again, err)
	}
	if fx := r.fixity(t, res.Hash, storage.VariantOriginal); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
		t.Fatalf("a dedup re-upload promoted the original: %+v", fx)
	}
	// The explicit recheck verifies it against its own address.
	if out, err := r.svc.RecheckVariant(ctx, res.Hash, storage.VariantOriginal); err != nil || out != storage.RecheckVerified {
		t.Fatalf("recheck: %q %v", out, err)
	}
	if fx := r.fixity(t, res.Hash, storage.VariantOriginal); fx.SHA256 != res.Hash || fx.Source != storage.DigestFromRecheck {
		t.Fatalf("rechecked original: %+v", fx)
	}
}

// patternReader streams n deterministic pseudo-random bytes without
// holding them in memory.
type patternReader struct {
	n, off int64
	state  uint64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	if rem := p.n - p.off; int64(len(b)) > rem {
		b = b[:rem]
	}
	for i := range b {
		p.state = p.state*6364136223846793005 + 1442695040888963407
		b[i] = byte(p.state >> 56)
	}
	p.off += int64(len(b))
	return len(b), nil
}

func TestPutVariant_LargeStreamedVariant(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	const size = 48 << 20 // 48 MiB, streamed, never buffered
	want := sha256.New()
	if _, err := io.Copy(want, &patternReader{n: size, state: 1597}); err != nil {
		t.Fatal(err)
	}
	w, err := r.svc.PutVariant(ctx, hash, "hls/1080p.mp4", &patternReader{n: size, state: 1597}, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if w.Size != size || w.SHA256 != hex.EncodeToString(want.Sum(nil)) {
		t.Fatalf("large write: %+v", w)
	}
	if f, _, err := r.verifyVariant(ctx, hash, "hls/1080p.mp4"); err != nil || f != "" {
		t.Fatalf("sweep of the large variant: %q %v", f, err)
	}
}

// legacyRow plants bytes (nil: none) and a pre-00082 row with the given
// recorded size, no digest.
func (r *fixityRig) legacyRow(t *testing.T, hash, key string, body []byte, recordedSize int) {
	t.Helper()
	ctx := context.Background()
	if body != nil {
		if _, err := r.svc.Backend.Put(ctx, hash, key, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type) VALUES ($1, $2, $3, 'image/webp')`,
		hash, key, recordedSize); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyZeroLengthRow_IsSizeCheckedAndOnlyEmptyBytesPromote(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	// Recorded 0, but the backend holds bytes: a size mismatch, not healthy.
	r.legacyRow(t, hash, "col", []byte("not empty at all"), 0)
	if f, _, err := r.verifyVariant(ctx, hash, "col"); err != nil || f != storage.FindingSizeMismatch {
		t.Fatalf("sweep of a 0-byte row holding bytes: %q %v, want size_mismatch", f, err)
	}
	if out, err := r.svc.RecheckVariant(ctx, hash, "col"); err != nil || out != storage.RecheckSizeMismatch {
		t.Fatalf("recheck of a 0-byte row holding bytes: %q %v, want size_mismatch", out, err)
	}
	if fx := r.fixity(t, hash, "col"); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
		t.Fatalf("0-byte row holding bytes was promoted: %+v", fx)
	}
	// A truly empty variant rechecks to the digest of no bytes.
	r.legacyRow(t, hash, "poster", []byte{}, 0)
	if f, _, err := r.verifyVariant(ctx, hash, "poster"); err != nil || f != "" {
		t.Fatalf("sweep of an empty variant: %q %v", f, err)
	}
	if out, err := r.svc.RecheckVariant(ctx, hash, "poster"); err != nil || out != storage.RecheckVerified {
		t.Fatalf("recheck of an empty variant: %q %v", out, err)
	}
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if fx := r.fixity(t, hash, "poster"); fx.Status != storage.FixityVerified || fx.SHA256 != emptySHA256 || fx.Size != 0 {
		t.Fatalf("empty variant: %+v", fx)
	}
}

// withoutDigest lists, through the backfill's own query, this object's
// rows it would revisit (filtered to the object so rows other tests left
// in the shared database are never touched).
func (r *fixityRig) withoutDigest(t *testing.T, hash string) map[string]bool {
	t.Helper()
	rows, err := storage.New(r.pool).ListVariantsWithoutDigest(context.Background(), storage.ListVariantsWithoutDigestParams{
		AfterHash: hash, AfterVariant: "", MaxRows: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, row := range rows {
		if row.ObjectHash == hash {
			out[row.VariantKey] = true
		}
	}
	return out
}

func TestBackfill_RevisitsALegacyRowWhoseBytesWereMissing(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	hash := r.object(t)
	body := []byte("the legacy preview, back from a restore")
	r.legacyRow(t, hash, "preview", nil, len(body)) // row, no bytes

	page := func() storage.BackfillBatch {
		t.Helper()
		b, err := r.svc.BackfillVariantDigests(ctx, hash, "", 1) // this object's first row only
		if err != nil {
			t.Fatal(err)
		}
		if b.Scanned != 1 || b.NextHash != hash || b.NextVariant != "preview" {
			t.Fatalf("backfill page: %+v", b)
		}
		return b
	}
	if b := page(); b.Outcomes[storage.RecheckMissing] != 1 {
		t.Fatalf("first backfill: %+v", b)
	}
	if fx := r.fixity(t, hash, "preview"); fx.Status != storage.FixityMissing || fx.SHA256 != "" {
		t.Fatalf("after the first backfill: %+v", fx)
	}
	if !r.withoutDigest(t, hash)["preview"] {
		t.Fatal("the backfill would never revisit the missing row")
	}
	// Bytes come back, but the wrong ones: not promoted, still revisited.
	if _, err := r.svc.Backend.Put(ctx, hash, "preview", bytes.NewReader([]byte("short"))); err != nil {
		t.Fatal(err)
	}
	if b := page(); b.Outcomes[storage.RecheckSizeMismatch] != 1 {
		t.Fatalf("backfill over wrong bytes: %+v", b)
	}
	if fx := r.fixity(t, hash, "preview"); fx.Status != storage.FixityMissing || fx.SHA256 != "" {
		t.Fatalf("wrong bytes promoted the row: %+v", fx)
	}
	// The right bytes: verified from what the backend actually holds.
	if _, err := r.svc.Backend.Put(ctx, hash, "preview", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if b := page(); b.Outcomes[storage.RecheckVerified] != 1 {
		t.Fatalf("backfill over restored bytes: %+v", b)
	}
	if fx := r.fixity(t, hash, "preview"); fx.Status != storage.FixityVerified || fx.SHA256 != sum(body) || fx.Source != storage.DigestFromRecheck {
		t.Fatalf("restored row: %+v", fx)
	}
	if r.withoutDigest(t, hash)["preview"] {
		t.Fatal("a verified row is still listed for backfill")
	}

	// A row with a RECORDED digest that goes missing is the sweep's
	// business: the backfill never lists it, and its digest stays.
	if _, err := r.svc.PutVariant(ctx, hash, "screen", bytes.NewReader([]byte("written")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Backend.Delete(ctx, hash, "screen"); err != nil {
		t.Fatal(err)
	}
	if f, _, err := r.verifyVariant(ctx, hash, "screen"); err != nil || f != storage.FindingMissingObject {
		t.Fatalf("sweep: %q %v", f, err)
	}
	if r.withoutDigest(t, hash)["screen"] {
		t.Fatal("the backfill lists a row whose digest was recorded at write time")
	}
	if fx := r.fixity(t, hash, "screen"); fx.Status != storage.FixityMissing || fx.SHA256 != sum([]byte("written")) {
		t.Fatalf("recorded-digest missing row: %+v", fx)
	}
}

// heldReader signals on its first Read, then blocks until release.
type heldReader struct {
	started chan<- string
	key     string
	release <-chan struct{}
	once    sync.Once
	done    bool
}

func (h *heldReader) Read(p []byte) (int, error) {
	h.once.Do(func() { h.started <- h.key; <-h.release })
	if h.done {
		return 0, io.EOF
	}
	h.done = true
	return copy(p, []byte("bytes of "+h.key)), nil
}

// At the smallest supported pool (8 connections, 3 held by LISTEN
// workers), uploads in flight must leave query connections for everyone
// else: at most two writers hold a connection, the rest wait without one.
func TestPutVariant_AtTheMinimumPoolWritersLeaveQueryCapacity(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	cfg := r.pool.Config()
	cfg.MaxConns = 8
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	listeners := make([]*pgxpool.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, c)
	}
	defer func() {
		for _, c := range listeners {
			c.Release()
		}
	}()
	svc := storage.NewService(r.svc.Backend, pool)
	hash := r.object(t)

	started := make(chan string, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	errs := make([]error, 4)
	// On any failure below, free the held writers and let them finish
	// before the pool closes (cleanups run last-registered first), so a
	// failing run fails at once instead of hanging the package.
	t.Cleanup(func() { unblock(); wg.Wait() })
	for i, key := range []string{"col", "preview", "screen", "hires"} {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			_, errs[i] = svc.PutVariant(ctx, hash, key, &heldReader{started: started, key: key, release: release}, "image/webp")
		}(i, key)
	}
	inPut := 0
	deadline := time.After(10 * time.Second)
	for inPut < 2 {
		select {
		case <-started:
			inPut++
		case <-deadline:
			t.Fatalf("only %d writers reached their Put", inPut)
		}
	}
	// Give any further writer every chance to start: it must not.
	select {
	case k := <-started:
		t.Fatalf("a third writer (%s) holds a connection during the uploads", k)
	case <-time.After(300 * time.Millisecond):
	}
	if n := pool.Stat().AcquiredConns(); n != 5 {
		t.Fatalf("acquired connections = %d, want 5 (3 listeners + 2 writers)", n)
	}
	// Ordinary queries still get connections.
	var q sync.WaitGroup
	qerrs := make([]error, 3)
	for i := 0; i < 3; i++ {
		q.Add(1)
		go func(i int) {
			defer q.Done()
			qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var one int
			qerrs[i] = pool.QueryRow(qctx, `SELECT 1 FROM pg_sleep(0.05)`).Scan(&one)
		}(i)
	}
	q.Wait()
	for i, err := range qerrs {
		if err != nil {
			t.Fatalf("query %d starved while two variants upload: %v", i, err)
		}
	}
	unblock()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	for _, key := range []string{"col", "preview", "screen", "hires"} {
		if fx, err := svc.ReadVariantFixity(ctx, hash, key); err != nil || fx.Status != storage.FixityVerified || fx.SHA256 != sum([]byte("bytes of "+key)) {
			t.Fatalf("%s after the uploads: %+v %v", key, fx, err)
		}
	}
}

// A legacy ORIGINAL whose bytes hash to its own address but whose
// recorded size is wrong is not promoted: the address is right, the
// accounting is not, and a recheck never rewrites size_bytes.
func TestRecheck_LegacyOriginalWithTheRightHashButAWrongRecordedSize(t *testing.T) {
	r := newFixityRig(t)
	ctx := context.Background()
	payload := make([]byte, 3000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.UploadOriginal(ctx, bytes.NewReader(payload), "image/png", storage.PinRef{SubjectType: "user", SubjectID: "9159702"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, res.Hash)
	})
	// As a pre-00082 row would be: no digest, and a wrong recorded size.
	if _, err := r.pool.Exec(ctx, `UPDATE storage_variants
		SET content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL,
		    fixity_status = 'unverifiable', size_bytes = $2
		WHERE object_hash = $1 AND variant_key = 'original'`, res.Hash, len(payload)+7); err != nil {
		t.Fatal(err)
	}
	recordedSize := func() int64 {
		t.Helper()
		var n int64
		if err := r.pool.QueryRow(ctx, `SELECT size_bytes FROM storage_variants WHERE object_hash = $1 AND variant_key = 'original'`, res.Hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if f, _, err := r.verifyVariant(ctx, res.Hash, storage.VariantOriginal); err != nil || f != storage.FindingSizeMismatch {
		t.Fatalf("sweep: %q %v, want size_mismatch", f, err)
	}
	if out, err := r.svc.RecheckVariant(ctx, res.Hash, storage.VariantOriginal); err != nil || out != storage.RecheckSizeMismatch {
		t.Fatalf("recheck: %q %v, want size_mismatch", out, err)
	}
	if fx := r.fixity(t, res.Hash, storage.VariantOriginal); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
		t.Fatalf("original with a wrong recorded size was promoted: %+v", fx)
	}
	if got := recordedSize(); got != int64(len(payload)+7) {
		t.Fatalf("recheck changed size_bytes to %d", got)
	}

	// A deliberate correction of the metadata, then the recheck succeeds.
	if _, err := r.pool.Exec(ctx, `UPDATE storage_variants SET size_bytes = $2 WHERE object_hash = $1 AND variant_key = 'original'`, res.Hash, len(payload)); err != nil {
		t.Fatal(err)
	}
	if out, err := r.svc.RecheckVariant(ctx, res.Hash, storage.VariantOriginal); err != nil || out != storage.RecheckVerified {
		t.Fatalf("recheck after the correction: %q %v", out, err)
	}
	fx := r.fixity(t, res.Hash, storage.VariantOriginal)
	if fx.Status != storage.FixityVerified || fx.SHA256 != res.Hash || fx.Size != int64(len(payload)) || fx.Source != storage.DigestFromRecheck {
		t.Fatalf("corrected original: %+v", fx)
	}
	if got := recordedSize(); got != int64(len(payload)) {
		t.Fatalf("size_bytes = %d after the correction", got)
	}
}

// verifyVariant is VerifyVariant reduced to the sweep's finding and
// detail, for the #1597 tests that predate observations (#1598).
func (r *fixityRig) verifyVariant(ctx context.Context, hash, key string) (string, string, error) {
	o, err := r.svc.VerifyVariant(ctx, hash, key)
	return o.Finding, o.Detail, err
}
