// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596 (slice 2): version reconciliation, on a real database and the fs
// backend.
//
//   - a draft is sealed with its original and the verified copies the
//     policy chose (each its own pinned object); an unverified or missing
//     copy is omitted, and nothing is sealed over an unverified original;
//   - a changed SELECTED rendition makes exactly one sealed rendition
//     successor (the unchanged copy carried forward); the predecessor
//     keeps its file set, pins and bytes; a rerun, and a change to a
//     variant the policy never chooses, change nothing;
//   - assets sharing the bytes each get their own sealed version on the
//     same copies; an asset still processing is skipped, and a failed
//     one is sealed;
//   - every transient promotion pin is gone after success and after a
//     failure, and stale ones are recovered;
//   - a regeneration during a reconcile seals the copy that was promoted,
//     and the next reconcile adds exactly one successor;
//   - reconcile requests coalesce, a request during a running job is
//     queued after it, terminal processing events and ready inserts make
//     requests, and a verified candidate-key variant write requests one
//     only when no preview job is in flight (never for HLS, sprites or
//     covers);
//   - the database refuses an access copy without provenance (an
//     unrelated held, verified object under the real original and key),
//     one not derived from the version's original, an unheld or an
//     unverified one, and a seal over an unverified original; it accepts
//     the genuine held copy;
//   - a verified variant record and its reconcile request commit together:
//     a request that cannot be made fails the record;
//   - the SQL candidate-key rule matches assets.IsAccessCandidateKey;
//   - the operator query separates backfilled drafts by original fixity.

package assets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

func rbytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type reconcileRig struct {
	*maFixture
	r     *assets.VersionReconciler
	mu    sync.Mutex
	clean []string
}

func newReconcileRig(t *testing.T) *reconcileRig {
	rr := &reconcileRig{maFixture: newMAFixture(t)}
	rr.r = &assets.VersionReconciler{Pool: rr.pool, Storage: rr.h.Storage}
	// Registered first, so it runs last: after every asset (and with it
	// every version pin) is gone.
	t.Cleanup(func() {
		c := context.Background()
		rr.mu.Lock()
		defer rr.mu.Unlock()
		for _, h := range rr.clean {
			_, _ = rr.pool.Exec(c, `DELETE FROM jobs WHERE type = 'asset.version_reconcile' AND payload->>'object_hash' = $1`, h)
			_, _ = rr.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type <> 'asset_version'`, h)
			_, _ = rr.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, h)
			_, _ = rr.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, h)
		}
	})
	return rr
}

func (rr *reconcileRig) track(hashes ...string) {
	rr.mu.Lock()
	rr.clean = append(rr.clean, hashes...)
	rr.mu.Unlock()
}

// readyImage is a local PNG asset with fresh bytes, processing done.
func (rr *reconcileRig) readyImage(owner int64) (uuid.UUID, string) {
	rr.t.Helper()
	h, _ := rr.upload(owner, "reconcile")
	rr.track(h)
	a := rr.createWithFile(owner, h)
	rr.setProcessing(a, "ready")
	return a, h
}

func (rr *reconcileRig) setProcessing(a uuid.UUID, status string) {
	rr.t.Helper()
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET processing_status = $2 WHERE id = $1`, a, status); err != nil {
		rr.t.Fatal(err)
	}
}

// ladder writes the given variants of hash; returns their bytes.
func (rr *reconcileRig) ladder(hash string, keys ...string) map[string][]byte {
	rr.t.Helper()
	out := map[string][]byte{}
	for _, k := range keys {
		b := rbytes(rr.t, 900+len(k)*37)
		out[k] = b
		rr.track(digest(b))
		if _, err := rr.h.Storage.PutVariant(rr.ctx, hash, k, bytes.NewReader(b), "image/webp"); err != nil {
			rr.t.Fatalf("PutVariant %s: %v", k, err)
		}
	}
	return out
}

func (rr *reconcileRig) reconcile(hash string) *assets.ReconcileResult {
	rr.t.Helper()
	res, err := rr.r.Reconcile(rr.ctx, hash)
	if err != nil {
		rr.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// copies returns a version's non-original rows, role -> object hash.
func (rr *reconcileRig) copies(version uuid.UUID) map[string]string {
	rr.t.Helper()
	rows, err := rr.pool.Query(rr.ctx, `SELECT role, object_hash FROM asset_version_files WHERE version_id = $1 AND role <> 'original'`, version)
	if err != nil {
		rr.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var role, h string
		if err := rows.Scan(&role, &h); err != nil {
			rr.t.Fatal(err)
		}
		out[role] = h
	}
	return out
}

func (rr *reconcileRig) promotePins(hashes ...string) int {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'version_promote' AND object_hash = ANY($1)`, hashes).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n
}

func (rr *reconcileRig) storedOriginal(hash string) []byte {
	rr.t.Helper()
	rc, _, err := rr.h.Storage.Backend.Get(rr.ctx, hash, storage.VariantOriginal)
	if err != nil {
		rr.t.Fatal(err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		rr.t.Fatal(err)
	}
	return buf.Bytes()
}

func (rr *reconcileRig) requireSealedWith(who string, v versionRow, want map[string]string) {
	rr.t.Helper()
	if v.SealedAt == nil {
		rr.t.Fatalf("%s: version %d is still a draft", who, v.Number)
	}
	got := rr.copies(v.ID)
	if len(got) != len(want) {
		rr.t.Fatalf("%s: copies %v, want %v", who, got, want)
	}
	for role, h := range want {
		if got[role] != h {
			rr.t.Fatalf("%s: %s copy %s, want %s", who, role, got[role], h)
		}
		if !rr.versionPinned(h, v.ID) || rr.gcEligibleAt(h) != nil {
			rr.t.Fatalf("%s: the %s copy is not pinned to its version, or GC-eligible", who, role)
		}
	}
}

func TestReconcile_SealsDraftWithVerifiedCopies(t *testing.T) {
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-seal"))
	b := rr.ladder(h, "col", "preview", "screen", "hires", "hls/master.m3u8")
	res := rr.reconcile(h)
	if res.Sealed != 1 || res.Promoted != 2 || res.Omitted != 0 {
		t.Fatalf("result %+v, want 1 sealed from 2 promotions", res)
	}
	vs := rr.versions(a)
	if len(vs) != 1 {
		t.Fatalf("%d versions, want v1 only", len(vs))
	}
	rr.requireSealedWith("v1", vs[0], map[string]string{"preview": digest(b["preview"]), "access": digest(b["hires"])})
	if rr.promotePins(digest(b["preview"]), digest(b["hires"])) != 0 {
		t.Fatal("transient promotion pins survived a successful reconcile")
	}
}

func TestReconcile_UnverifiedCopiesAndOriginalsAreNeverSealed(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rc-unverified")

	a, h := rr.readyImage(owner)
	b := rr.ladder(h, "preview", "hires")
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL
		WHERE object_hash = $1 AND variant_key = 'hires'`, h); err != nil {
		t.Fatal(err)
	}
	res := rr.reconcile(h)
	if res.Sealed != 1 || res.Omitted != 1 {
		t.Fatalf("result %+v, want sealed with the unverified hires omitted", res)
	}
	rr.requireSealedWith("v1", rr.versions(a)[0], map[string]string{"preview": digest(b["preview"])})

	// An original whose stored bytes are not verified: nothing is sealed.
	a2, h2 := rr.readyImage(owner)
	rr.ladder(h2, "preview", "hires")
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL
		WHERE object_hash = $1 AND variant_key = 'original'`, h2); err != nil {
		t.Fatal(err)
	}
	res = rr.reconcile(h2)
	if res.Skipped != 1 || res.Sealed != 0 || res.Promoted != 0 {
		t.Fatalf("result %+v, want the asset skipped and nothing promoted", res)
	}
	if v := rr.versions(a2)[0]; v.SealedAt != nil {
		t.Fatal("a version was sealed over an unverified original")
	}
}

func TestReconcile_RerenderMakesExactlyOneSuccessorAndKeepsOldBytes(t *testing.T) {
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-rerender"))
	b := rr.ladder(h, "preview", "screen", "hires")
	rr.reconcile(h)
	v1 := rr.versions(a)[0]
	oldHires := digest(b["hires"])

	nb := rr.ladder(h, "hires") // a re-render changes the selected access rung
	res := rr.reconcile(h)
	if res.Successors != 1 {
		t.Fatalf("result %+v, want exactly one successor", res)
	}
	vs := rr.versions(a)
	if len(vs) != 2 {
		t.Fatalf("%d versions, want 2", len(vs))
	}
	v2 := vs[1]
	if v2.Number != 2 || v2.Origin != "rendition" || v2.Predecessor == nil || *v2.Predecessor != v1.ID || v2.OriginalHash != h {
		t.Fatalf("v2 = %+v, want a rendition successor of v1 on the same original", v2)
	}
	if cur := rr.currentVersion(a); cur == nil || *cur != v2.ID {
		t.Fatal("the asset did not move to the successor")
	}
	rr.requireSealedWith("v2", v2, map[string]string{"preview": digest(b["preview"]), "access": digest(nb["hires"])})
	// The predecessor keeps its file set, its pins and its bytes.
	rr.requireSealedWith("v1", rr.versions(a)[0], map[string]string{"preview": digest(b["preview"]), "access": oldHires})
	if got := rr.storedOriginal(oldHires); !bytes.Equal(got, b["hires"]) {
		t.Fatal("the old access copy's bytes changed")
	}
	// A rerun is a no-op.
	if res := rr.reconcile(h); res.Unchanged != 1 || res.Successors != 0 || len(rr.versions(a)) != 2 {
		t.Fatalf("rerun result %+v with %d versions, want unchanged", res, len(rr.versions(a)))
	}
}

func TestReconcile_NonSelectedVariantsNeverMakeASuccessor(t *testing.T) {
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-nonselected"))
	rr.ladder(h, "col", "preview", "screen", "hires")
	rr.reconcile(h)
	rr.ladder(h, "col", "screen", "hls/master.m3u8", "sprites.jpg", "cover")
	if res := rr.reconcile(h); res.Unchanged != 1 || len(rr.versions(a)) != 1 {
		t.Fatalf("result %+v with %d versions, want unchanged", res, len(rr.versions(a)))
	}
}

func TestReconcile_SharedBytesFanOutPerAsset(t *testing.T) {
	rr := newReconcileRig(t)
	o1, o2 := rr.user("rc-share-1"), rr.user("rc-share-2")
	h, _ := rr.upload(o1, "shared")
	rr.track(h)
	a1 := rr.createWithFile(o1, h)
	a2 := rr.createWithFile(o2, h)
	rr.setProcessing(a1, "ready")
	rr.setProcessing(a2, "ready")
	b := rr.ladder(h, "preview", "hires")
	if res := rr.reconcile(h); res.Assets != 2 || res.Sealed != 2 || res.Promoted != 2 {
		t.Fatalf("result %+v, want both assets sealed from one promotion per key", res)
	}
	want := map[string]string{"preview": digest(b["preview"]), "access": digest(b["hires"])}
	rr.requireSealedWith("first asset", rr.versions(a1)[0], want)
	rr.requireSealedWith("second asset", rr.versions(a2)[0], want)
}

func TestReconcile_ProcessingInFlightIsSkipped_FailedIsSealed(t *testing.T) {
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-processing"))
	rr.setProcessing(a, "processing")
	b := rr.ladder(h, "preview", "hires")
	if res := rr.reconcile(h); res.Skipped != 1 || res.Promoted != 0 || rr.versions(a)[0].SealedAt != nil {
		t.Fatalf("result %+v: an asset in processing must be left as it is", res)
	}
	// Processing fails: the terminal event requests a reconcile, which
	// seals with whatever verified copy exists.
	if err := assets.New(rr.pool).MarkAssetFailed(rr.ctx, assets.MarkAssetFailedParams{ID: pgtype.UUID{Bytes: a, Valid: true}, ProcessingError: "boom"}); err != nil {
		t.Fatal(err)
	}
	if n := rr.reconcileJobs(h); n < 1 {
		t.Fatal("a failed processing event requested no reconcile")
	}
	if res := rr.reconcile(h); res.Sealed != 1 {
		t.Fatalf("result %+v, want the failed asset sealed", res)
	}
	rr.requireSealedWith("v1", rr.versions(a)[0], map[string]string{"preview": digest(b["preview"]), "access": digest(b["hires"])})
}

func (rr *reconcileRig) reconcileJobs(hash string) int {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM jobs WHERE type = 'asset.version_reconcile' AND status = 'pending'
		AND payload->>'object_hash' = $1`, hash).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n
}

func TestReconcile_TransientPinsAreReleasedOnFailureAndRecoveredWhenStale(t *testing.T) {
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-pins"))
	b := rr.ladder(h, "preview", "hires")
	injected := errors.New("injected before the seal")
	rr.r.SetReconcileBeforeAssetTxHook(func(uuid.UUID) error { return injected })
	_, err := rr.r.Reconcile(rr.ctx, h)
	rr.r.SetReconcileBeforeAssetTxHook(nil)
	if !errors.Is(err, injected) {
		t.Fatalf("Reconcile: %v, want the injected failure", err)
	}
	if rr.promotePins(digest(b["preview"]), digest(b["hires"])) != 0 {
		t.Fatal("a failed reconcile left its promotion pins")
	}
	if rr.versions(a)[0].SealedAt != nil {
		t.Fatal("a failed reconcile sealed the version")
	}
	// A pin left by a process that died between promotion and release.
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id, created_at)
		VALUES ($1, 'version_promote', 'crashed', now() - interval '2 hours')`, digest(b["hires"])); err != nil {
		t.Fatal(err)
	}
	res := rr.reconcile(h)
	if res.StalePinsReleased < 1 || rr.promotePins(digest(b["hires"])) != 0 || res.Sealed != 1 {
		t.Fatalf("result %+v: the stale pin must be released and the version sealed", res)
	}
}

func TestReconcile_RegenerationDuringAReconcile(t *testing.T) {
	// The access rung is regenerated after the reconcile promoted it and
	// before it sealed: the seal uses the promoted (old, immutable) copy,
	// and the next reconcile adds exactly one successor with the new one.
	rr := newReconcileRig(t)
	a, h := rr.readyImage(rr.user("rc-concurrent"))
	b := rr.ladder(h, "preview", "hires")
	var nb map[string][]byte
	rr.r.SetReconcileAfterPromoteHook(func(string) { nb = rr.ladder(h, "hires") })
	rr.reconcile(h)
	rr.r.SetReconcileAfterPromoteHook(nil)
	rr.requireSealedWith("v1", rr.versions(a)[0], map[string]string{"preview": digest(b["preview"]), "access": digest(b["hires"])})
	if res := rr.reconcile(h); res.Successors != 1 {
		t.Fatalf("result %+v, want one successor for the regenerated rung", res)
	}
	vs := rr.versions(a)
	rr.requireSealedWith("v2", vs[1], map[string]string{"preview": digest(b["preview"]), "access": digest(nb["hires"])})
	if res := rr.reconcile(h); res.Unchanged != 1 || len(rr.versions(a)) != 2 {
		t.Fatalf("rerun %+v, want unchanged", res)
	}
}

func TestReconcile_RequestsCoalesceAndFollowARunningJob(t *testing.T) {
	rr := newReconcileRig(t)
	h := strings.Repeat("e", 63) + "1"
	rr.track(h)
	for i := 0; i < 3; i++ {
		if err := assets.EnqueueVersionReconcile(rr.ctx, rr.pool, h); err != nil {
			t.Fatal(err)
		}
	}
	if n := rr.reconcileJobs(h); n != 1 {
		t.Fatalf("%d pending reconciles after three requests, want 1", n)
	}
	// The job starts: a request now must not be absorbed by it.
	var id uuid.UUID
	if err := rr.pool.QueryRow(rr.ctx, `UPDATE jobs SET status = 'running', started_at = now()
		WHERE type = 'asset.version_reconcile' AND payload->>'object_hash' = $1 RETURNING id`, h).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := assets.EnqueueVersionReconcile(rr.ctx, rr.pool, h); err != nil {
			t.Fatal(err)
		}
	}
	var key string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT idempotency_key FROM jobs WHERE type = 'asset.version_reconcile' AND status = 'pending'
		AND payload->>'object_hash' = $1`, h).Scan(&key); err != nil {
		t.Fatalf("no single follow-up request queued after the running job: %v", err)
	}
	if key != "version-reconcile:"+h+":after:"+id.String() {
		t.Fatalf("follow-up key %q, want it keyed after the running job", key)
	}
}

func TestReconcile_TriggersRequestReconciles(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rc-triggers")
	// A local asset inserted with bytes and nothing to process.
	h, _ := rr.upload(owner, "trigger-insert")
	rr.track(h)
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (title, asset_type, owner_user_ref, file_hash, file_extension)
		VALUES ('ready insert', (SELECT MIN(ref) FROM asset_types), $1, $2, 'zip')`, owner, h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rr.pool.Exec(context.Background(), `DELETE FROM assets WHERE file_hash = $1`, h) })
	if rr.reconcileJobs(h) != 1 {
		t.Fatal("a ready insert requested no reconcile")
	}
	// A terminal success event (MarkAssetReady).
	h2, _ := rr.upload(owner, "trigger-ready")
	rr.track(h2)
	a2 := rr.createWithFile(owner, h2)
	if rr.reconcileJobs(h2) != 0 {
		t.Fatal("a pending asset requested a reconcile before processing ended")
	}
	if err := assets.New(rr.pool).MarkAssetReady(rr.ctx, pgtype.UUID{Bytes: a2, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if rr.reconcileJobs(h2) != 1 {
		t.Fatal("MarkAssetReady requested no reconcile")
	}

	// A verified variant write requests one in its own statement, only
	// for keys the policy can choose and only when no preview job for the
	// bytes is in flight (the database trigger, migration 00085).
	h3, _ := rr.upload(owner, "trigger-variant")
	rr.track(h3)
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO jobs (type, payload) VALUES ('preview.raster', jsonb_build_object('file_hash', $1::text))`, h3); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM jobs WHERE type = 'preview.raster' AND payload->>'file_hash' = $1`, h3)
	})
	rr.ladder(h3, "hires", "hls/seg_000.ts")
	if n := rr.reconcileJobs(h3); n != 0 {
		t.Fatalf("%d reconciles queued while a preview job was in flight, want none", n)
	}
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE jobs SET status = 'done' WHERE type = 'preview.raster' AND payload->>'file_hash' = $1`, h3); err != nil {
		t.Fatal(err)
	}
	rr.ladder(h3, "hls/seg_001.ts", "sprites.jpg", "cover")
	if n := rr.reconcileJobs(h3); n != 0 {
		t.Fatalf("%d reconciles queued by keys the policy never chooses, want none", n)
	}
	rr.ladder(h3, "hires")
	if rr.reconcileJobs(h3) != 1 {
		t.Fatal("a verified candidate-key write with no preview job in flight requested no reconcile")
	}
}

func TestReconcile_VerifiedRecordAndRequestCommitTogether(t *testing.T) {
	// The request is made inside the statement that records the verified
	// digest: if it cannot be made, the record is not made either, so no
	// successful verified write can leave a rendition change unrequested.
	rr := newReconcileRig(t)
	owner := rr.user("rc-atomic")
	a, h := rr.readyImage(owner)
	rr.ladder(h, "preview", "hires")
	rr.reconcile(h)
	v1 := rr.versions(a)[0]
	if _, err := rr.pool.Exec(rr.ctx, `DELETE FROM jobs WHERE type = 'asset.version_reconcile' AND payload->>'object_hash' = $1`, h); err != nil {
		t.Fatal(err)
	}
	// Every new reconcile request now fails.
	if _, err := rr.pool.Exec(rr.ctx, `ALTER TABLE jobs ADD CONSTRAINT rc_atomic_inject CHECK (type <> 'asset.version_reconcile') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			if _, err := rr.pool.Exec(context.Background(), `ALTER TABLE jobs DROP CONSTRAINT IF EXISTS rc_atomic_inject`); err != nil {
				t.Errorf("drop injected constraint: %v", err)
			}
		}
	}
	t.Cleanup(drop)
	nb := rbytes(t, 2222)
	rr.track(digest(nb))
	_, err := rr.h.Storage.PutVariant(rr.ctx, h, "hires", bytes.NewReader(nb), "image/webp")
	if err == nil {
		t.Fatal("a verified record committed although its reconcile request could not be made")
	}
	fx, ferr := rr.h.Storage.ReadVariantFixity(rr.ctx, h, "hires")
	if ferr != nil {
		t.Fatal(ferr)
	}
	if fx.Status == storage.FixityVerified && fx.SHA256 == digest(nb) {
		t.Fatal("the new bytes are recorded as verified without a reconcile request")
	}
	var hist int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_variant_digest_history WHERE object_hash = $1 AND content_sha256 = $2`, h, digest(nb)).Scan(&hist); err != nil || hist != 0 {
		t.Fatalf("%d history rows for the unrecorded digest (%v), want 0", hist, err)
	}
	// Once requests can be made again, the same write records and requests.
	drop()
	if _, err := rr.h.Storage.PutVariant(rr.ctx, h, "hires", bytes.NewReader(nb), "image/webp"); err != nil {
		t.Fatalf("PutVariant: %v", err)
	}
	if rr.reconcileJobs(h) != 1 {
		t.Fatal("the recorded write requested no reconcile")
	}
	if res := rr.reconcile(h); res.Successors != 1 {
		t.Fatalf("result %+v, want the requested change as one successor", res)
	}
	if vs := rr.versions(a); len(vs) != 2 || vs[0].ID != v1.ID || rr.copies(vs[1].ID)["access"] != digest(nb) {
		t.Fatal("the successor does not carry the new access copy")
	}
}

func TestAccessCandidateKey_SQLMatchesGo(t *testing.T) {
	rr := newReconcileRig(t)
	for _, k := range []string{"col", "preview", "screen", "hires", "display-4k", "thumb_s", "4k",
		"original", "poster", "cover", "embedded-preview", "iso", "iso/source.png", "hls/master.m3u8",
		"turntable/0000.png", "views/top.png", "sprites.jpg", "sprites.vtt", "sub.en.vtt", "Hires", "with space", "",
		strings.Repeat("a", 64), strings.Repeat("a", 65), "-lead", "_lead"} {
		var sqlOK bool
		if err := rr.pool.QueryRow(rr.ctx, `SELECT asset_access_candidate_key($1)`, k).Scan(&sqlOK); err != nil {
			t.Fatal(err)
		}
		if sqlOK != assets.IsAccessCandidateKey(k) {
			t.Errorf("key %q: SQL says %v, Go says %v", k, sqlOK, assets.IsAccessCandidateKey(k))
		}
	}
}

func TestSeal_DatabaseRefusesUnheldUnverifiedOrForeignCopies(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rc-dbrules")
	a, h := rr.readyImage(owner)
	v1 := rr.versions(a)[0]
	rr.ladder(h, "hires") // h's real hires: recorded, so it has provenance
	seal := `SELECT asset_version_seal($1, $2::jsonb)`
	entry := func(hash string, size int64, from, key string) string {
		return `[{"role":"access","object_hash":"` + hash + `","size_bytes":` + strconv.FormatInt(size, 10) +
			`,"media_type":"image/webp","derived_from_hash":"` + from + `","derived_variant_key":"` + key + `"}]`
	}
	refusedWith := func(what, want string, args ...any) {
		t.Helper()
		_, err := rr.pool.Exec(rr.ctx, seal, args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", what, err, want)
		}
	}

	// An unrelated object: verified, held by a promotion pin, presented
	// with the right original and a real key. Its digest was never that
	// variant's, so it has no provenance.
	forged, forgedSize := rr.upload(owner, "forged")
	rr.track(forged)
	if err := rr.h.Storage.AddPin(rr.ctx, storage.PinRef{SubjectType: storage.PinSubjectTypeVersionPromote, SubjectID: "rc-forged"}, forged); err != nil {
		t.Fatal(err)
	}
	refusedWith("a forged copy under the real key", "has no recorded provenance", v1.ID, entry(forged, forgedSize, h, "hires"))
	refusedWith("a forged copy under a key never written", "has no recorded provenance", v1.ID, entry(forged, forgedSize, h, "screen"))
	refusedWith("a copy derived from other bytes", "named derivative of the version's own original", v1.ID, entry(forged, forgedSize, forged, "hires"))

	// The genuine copy, but no longer held (its promotion was released).
	p, err := rr.h.Storage.PromoteVariant(rr.ctx, h, "hires")
	if err != nil {
		t.Fatal(err)
	}
	rr.track(p.Hash)
	if err := rr.h.Storage.ReleasePromotion(rr.ctx, p); err != nil {
		t.Fatal(err)
	}
	refusedWith("an unheld genuine copy", "neither a held promotion nor carried forward", v1.ID, entry(p.Hash, p.Size, h, "hires"))

	// Held again, but its own stored bytes are not verified.
	p, err = rr.h.Storage.PromoteVariant(rr.ctx, h, "hires")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rr.h.Storage.ReleasePromotion(context.Background(), p) }()
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL
		WHERE object_hash = $1 AND variant_key = 'original'`, p.Hash); err != nil {
		t.Fatal(err)
	}
	refusedWith("an unverified copy", "is not a verified object of its own", v1.ID, entry(p.Hash, p.Size, h, "hires"))
	if rr.versions(a)[0].SealedAt != nil {
		t.Fatal("a refused seal sealed the version")
	}
	// Verified again: the genuine, held copy seals.
	if out, err := rr.h.Storage.RecheckVariant(rr.ctx, p.Hash, storage.VariantOriginal); err != nil || out != storage.RecheckVerified {
		t.Fatalf("recheck the copy: %s %v", out, err)
	}
	if _, err := rr.pool.Exec(rr.ctx, seal, v1.ID, entry(p.Hash, p.Size, h, "hires")); err != nil {
		t.Fatalf("the genuine held copy: %v", err)
	}

	// And no seal over an unverified original.
	a2, h2 := rr.readyImage(owner)
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL
		WHERE object_hash = $1 AND variant_key = 'original'`, h2); err != nil {
		t.Fatal(err)
	}
	rr.maFixture.refused("a seal over an unverified original", `SELECT asset_version_seal($1)`, rr.versions(a2)[0].ID)
}

func TestBackfilledDrafts_SplitByOriginalFixity(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rc-backfill")
	mkDraft := func(label string, verified bool) string {
		h, _ := rr.upload(owner, label)
		rr.track(h)
		if !verified {
			if _, err := rr.pool.Exec(rr.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL
				WHERE object_hash = $1 AND variant_key = 'original'`, h); err != nil {
				t.Fatal(err)
			}
		}
		// A version as migration 00084's backfill made it: the insert
		// trigger off for this transaction, v1 created as backfill_current.
		rr.keepTriggerEnabled("assets", "assets_version_one_trg")
		tx, err := rr.pool.Begin(rr.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(rr.ctx, `ALTER TABLE assets DISABLE TRIGGER assets_version_one_trg`); err != nil {
			t.Fatal(err)
		}
		var id, v uuid.UUID
		if err := tx.QueryRow(rr.ctx, `INSERT INTO assets (title, asset_type, owner_user_ref, file_hash, file_extension)
			VALUES ('backfilled', (SELECT MIN(ref) FROM asset_types), $1, $2, 'png') RETURNING id, current_version_id`, owner, h).Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(rr.ctx, `SELECT asset_version_create($1, $2, 1, NULL, 'backfill_current', $3, 'png', NULL, NULL, NULL)`, id, v, h); err != nil {
			t.Fatal(err)
		}
		// The deferred checks run now, so the trigger can be re-enabled
		// before the commit (ALTER TABLE refuses pending trigger events).
		if _, err := tx.Exec(rr.ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(rr.ctx, `ALTER TABLE assets ENABLE TRIGGER assets_version_one_trg`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(rr.ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = rr.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
		return h
	}
	ok, notYet := mkDraft("verified", true), mkDraft("unverified", false)
	eligible, ineligible, drafts, err := assets.BackfilledDrafts(rr.ctx, rr.pool)
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []string, h string) bool {
		for _, x := range list {
			if x == h {
				return true
			}
		}
		return false
	}
	if !has(eligible, ok) || has(ineligible, ok) || !has(ineligible, notYet) || has(eligible, notYet) || drafts < 2 {
		t.Fatalf("eligible %d / ineligible %d / drafts %d: the verified hash must be eligible and the other not", len(eligible), len(ineligible), drafts)
	}
}

func TestDigestHistory_IsInsertOnlyAndMirrorsVerifiedRows(t *testing.T) {
	rr := newReconcileRig(t)
	_, h := rr.readyImage(rr.user("rc-history"))
	b := rr.ladder(h, "hires")
	var gen int64
	if err := rr.pool.QueryRow(rr.ctx, `SELECT generation FROM storage_variant_digest_history WHERE object_hash = $1 AND variant_key = 'hires' AND content_sha256 = $2`,
		h, digest(b["hires"])).Scan(&gen); err != nil {
		t.Fatalf("the verified hires write left no provenance row: %v", err)
	}
	f := rr.maFixture
	// A row that no verified variant row backs: a forged provenance claim.
	f.refused("a forged history row", `INSERT INTO storage_variant_digest_history (object_hash, variant_key, generation, content_sha256, content_size_bytes)
		VALUES ($1, 'hires', $2, $3, 10)`, h, gen+7, strings.Repeat("f", 64))
	f.refused("changing a history row", `UPDATE storage_variant_digest_history SET content_sha256 = $2 WHERE object_hash = $1`, h, strings.Repeat("f", 64))
	f.refused("removing a history row", `DELETE FROM storage_variant_digest_history WHERE object_hash = $1`, h)
	// A regeneration adds a row and keeps the old one.
	nb := rr.ladder(h, "hires")
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_variant_digest_history WHERE object_hash = $1 AND variant_key = 'hires'
		AND content_sha256 IN ($2, $3)`, h, digest(b["hires"]), digest(nb["hires"])).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d provenance rows after a regeneration (%v), want both generations", n, err)
	}
}
