// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #72: ImportVerified and the intent reconciler against a live database.
// Skips without AA_DB_PASSWORD, like the sweep tests.

package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// importFixture: random bytes staged in a temp file, plus cleanup of
// every row the import can write.
type importFixture struct {
	pool    *pgxpool.Pool
	svc     *Service
	backend *hookBackend
	path    string
	hash    string
	size    int64
}

// hookBackend wraps memBackend with fault hooks.
type hookBackend struct {
	*memBackend
	beforePut func()
	putErr    error
	puts      int
}

func (h *hookBackend) Put(ctx context.Context, hash, variant string, r io.Reader) (*ObjectInfo, error) {
	h.puts++
	if h.beforePut != nil {
		h.beforePut()
	}
	info, err := h.memBackend.Put(ctx, hash, variant, r)
	if err != nil {
		return nil, err
	}
	if h.putErr != nil {
		return nil, h.putErr // bytes stored, then a "crash"
	}
	return info, nil
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	pool := sweepTestPool(t)
	b := make([]byte, 3000)
	_, _ = rand.Read(b)
	sum := sha256.Sum256(b)
	path := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	be := &hookBackend{memBackend: newMemBackend()}
	fx := &importFixture{pool: pool, svc: NewService(be, pool), backend: be, path: path,
		hash: hex.EncodeToString(sum[:]), size: int64(len(b))}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, fx.hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_import_intents WHERE object_hash = $1`, fx.hash)
	})
	return fx
}

func (fx *importFixture) req(pin string) ImportRequest {
	return ImportRequest{
		TempPath: fx.path, Hash: fx.hash, Size: fx.size, ContentType: "image/png",
		IntentID: uuid.New(), Pin: PinRef{SubjectType: "federation_ingest", SubjectID: pin},
	}
}

func (fx *importFixture) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(), sql, fx.hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *importFixture) rows(t *testing.T) (objects, variants, pins, intents int) {
	return fx.count(t, `SELECT count(*) FROM storage_objects WHERE hash = $1`),
		fx.count(t, `SELECT count(*) FROM storage_variants WHERE object_hash = $1`),
		fx.count(t, `SELECT count(*) FROM storage_pins WHERE object_hash = $1`),
		fx.count(t, `SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`)
}

func TestImportVerified_RecordsObjectVariantPinAndRunsHook(t *testing.T) {
	fx := newImportFixture(t)
	hooked := false
	r := fx.req("ingest-1")
	r.InTx = func(ctx context.Context, tx pgx.Tx) error { hooked = true; return nil }
	res, err := fx.svc.ImportVerified(context.Background(), r)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Deduped || res.Hash != fx.hash || res.Size != fx.size || !hooked {
		t.Fatalf("result %+v hooked=%v", res, hooked)
	}
	if o, v, p, i := fx.rows(t); o != 1 || v != 1 || p != 1 || i != 0 {
		t.Fatalf("rows objects=%d variants=%d pins=%d intents=%d", o, v, p, i)
	}
	if info, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); err != nil || info.Size != fx.size {
		t.Fatalf("backend: %v %v", info, err)
	}
	// Second import of the same bytes: no Put, deduped, second pin.
	puts := fx.backend.puts
	res2, err := fx.svc.ImportVerified(context.Background(), fx.req("ingest-2"))
	if err != nil || !res2.Deduped || fx.backend.puts != puts {
		t.Fatalf("dedup: %+v %v puts %d->%d", res2, err, puts, fx.backend.puts)
	}
	if _, _, p, _ := fx.rows(t); p != 2 {
		t.Fatalf("pins = %d, want 2", p)
	}
}

// A hook failure rolls back object, variant and pin together.
func TestImportVerified_HookFailureRollsBack(t *testing.T) {
	fx := newImportFixture(t)
	r := fx.req("ingest-1")
	r.InTx = func(context.Context, pgx.Tx) error { return errors.New("lease gone") }
	if _, err := fx.svc.ImportVerified(context.Background(), r); err == nil {
		t.Fatal("import succeeded despite the hook failing")
	}
	if o, v, p, _ := fx.rows(t); o+v+p != 0 {
		t.Fatalf("rows left after rollback: objects=%d variants=%d pins=%d", o, v, p)
	}
}

// Wrong bytes write nothing: no intent, no Put, no rows.
func TestImportVerified_MismatchWritesNothing(t *testing.T) {
	for name, mut := range map[string]func(*ImportRequest){
		"hash": func(r *ImportRequest) {
			// Always a different first digit (a fixed "0" is a no-op for
			// one random hash in sixteen).
			d := "0"
			if r.Hash[0] == '0' {
				d = "1"
			}
			r.Hash = d + r.Hash[1:]
		},
		"size": func(r *ImportRequest) { r.Size++ },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newImportFixture(t)
			r := fx.req("ingest-1")
			mut(&r)
			if _, err := fx.svc.ImportVerified(context.Background(), r); !errors.Is(err, ErrImportMismatch) {
				t.Fatalf("err = %v, want ErrImportMismatch", err)
			}
			if fx.backend.puts != 0 {
				t.Fatal("backend Put happened for mismatched bytes")
			}
			if o, v, p, i := fx.rows(t); o+v+p+i != 0 {
				t.Fatalf("rows written: %d %d %d %d", o, v, p, i)
			}
		})
	}
}

// The intent exists before the Put (the crash-safety order).
func TestImportVerified_IntentPrecedesPut(t *testing.T) {
	fx := newImportFixture(t)
	seen := -1
	fx.backend.beforePut = func() { seen = fx.count(t, `SELECT count(*) FROM storage_import_intents WHERE object_hash = $1`) }
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("ingest-1")); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("intents visible during Put = %d, want 1", seen)
	}
}

// A reconciled (abandoned) intent means nothing is recorded.
func TestImportVerified_AbandonedIntentRecordsNothing(t *testing.T) {
	fx := newImportFixture(t)
	fx.backend.beforePut = func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM storage_import_intents WHERE object_hash = $1`, fx.hash)
	}
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("ingest-1")); !errors.Is(err, ErrImportAbandoned) {
		t.Fatalf("err = %v, want ErrImportAbandoned", err)
	}
	if o, v, p, _ := fx.rows(t); o+v+p != 0 {
		t.Fatalf("rows recorded for an abandoned import: %d %d %d", o, v, p)
	}
}

// A crash after the Put leaves the intent; the reconciler deletes the
// orphan bytes and the intent.
func TestReconcileImportIntents_CrashAfterPut(t *testing.T) {
	fx := newImportFixture(t)
	fx.backend.putErr = errors.New("crash after put")
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("ingest-1")); err == nil {
		t.Fatal("expected the simulated crash")
	}
	if _, _, _, i := fx.rows(t); i != 1 {
		t.Fatalf("intents after crash = %d, want 1", i)
	}
	backdate(t, fx, time.Hour)
	res, err := fx.svc.ReconcileImportIntents(context.Background(), 30*time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesDeleted != 1 || res.IntentsDropped != 1 {
		t.Fatalf("reconcile %+v", res)
	}
	if _, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan bytes still present: %v", err)
	}
}

// A slow but ALIVE import: its Put is blocked far past the stale
// window. The reconciler must skip it (no deletion, intent kept), and the
// import then commits normally: no orphan, no lost row.
func TestReconcileImportIntents_SlowActivePutIsNotReconciled(t *testing.T) {
	fx := newImportFixture(t)
	inPut, release := make(chan struct{}), make(chan struct{})
	var relOnce sync.Once
	unblock := func() { relOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	fx.backend.beforePut = func() { close(inPut); <-release }
	done := make(chan error, 1)
	go func() {
		_, err := fx.svc.ImportVerified(context.Background(), fx.req("slow"))
		done <- err
	}()
	<-inPut
	backdate(t, fx, 3*time.Hour) // the intent looks long abandoned
	res, err := fx.svc.ReconcileImportIntents(context.Background(), 30*time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedAlive != 1 || res.BytesDeleted != 0 || res.IntentsDropped != 0 {
		t.Fatalf("reconcile during an alive Put: %+v", res)
	}
	if _, _, _, i := fx.rows(t); i != 1 {
		t.Fatalf("intent removed under an alive import: %d", i)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatalf("the slow import failed: %v", err)
	}
	if o, _, p, i := fx.rows(t); o != 1 || p != 1 || i != 0 {
		t.Fatalf("after commit: objects=%d pins=%d intents=%d", o, p, i)
	}
	if _, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); err != nil {
		t.Fatalf("bytes lost: %v", err)
	}
}

// A crashed import's intent beside an ALIVE import of the same hash:
// the reconciler leaves the bytes alone while the alive one runs, and
// after it commits only drops the dead intent.
func TestReconcileImportIntents_CrashedBesideAliveImport(t *testing.T) {
	fx := newImportFixture(t)
	fx.backend.putErr = errors.New("crash after put")
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("crashed")); err == nil {
		t.Fatal("expected the simulated crash")
	}
	fx.backend.putErr = nil
	backdate(t, fx, time.Hour)
	inPut, release := make(chan struct{}), make(chan struct{})
	var relOnce sync.Once
	unblock := func() { relOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	fx.backend.beforePut = func() { close(inPut); <-release }
	done := make(chan error, 1)
	go func() {
		_, err := fx.svc.ImportVerified(context.Background(), fx.req("alive"))
		done <- err
	}()
	<-inPut
	res, err := fx.svc.ReconcileImportIntents(context.Background(), 30*time.Minute, 100)
	if err != nil || res.SkippedAlive != 1 || res.BytesDeleted != 0 {
		t.Fatalf("reconcile beside an alive import: %+v %v", res, err)
	}
	fx.backend.beforePut = nil
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res, err = fx.svc.ReconcileImportIntents(context.Background(), 30*time.Minute, 100)
	if err != nil || res.BytesDeleted != 0 || res.IntentsDropped != 1 {
		t.Fatalf("reconcile after the alive import committed: %+v %v", res, err)
	}
	if _, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); err != nil {
		t.Fatalf("committed bytes deleted: %v", err)
	}
}

// An importer arriving while the reconciler holds the hash waits, then
// writes its own intent and bytes after the reconciler's delete.
func TestReconcileImportIntents_ImporterWaitsForReconciler(t *testing.T) {
	fx := newImportFixture(t)
	fx.backend.putErr = errors.New("crash after put")
	_, _ = fx.svc.ImportVerified(context.Background(), fx.req("crashed"))
	fx.backend.putErr = nil
	backdate(t, fx, time.Hour)
	// Hold the reconciler's exclusive lock in an open transaction.
	tx, err := fx.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if got, err := New(tx).TryLockImportHashExclusive(context.Background(), fx.hash); err != nil || !got {
		t.Fatalf("exclusive lock: %v %v", got, err)
	}
	locked := make(chan struct{})
	fx.svc.SetImportAfterLockHook(func() { close(locked) })
	done := make(chan error, 1)
	go func() {
		_, err := fx.svc.ImportVerified(context.Background(), fx.req("later"))
		done <- err
	}()
	select {
	case <-locked:
		t.Fatal("importer got the import lock while the reconciler held it")
	case <-time.After(300 * time.Millisecond):
	}
	_ = fx.backend.Delete(context.Background(), fx.hash, VariantOriginal) // the reconciler's delete
	_ = tx.Commit(context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); err != nil {
		t.Fatalf("the later import's bytes are missing: %v", err)
	}
}

// A stale intent whose hash has a committed row: bytes kept.
func TestReconcileImportIntents_RowKeepsBytes(t *testing.T) {
	fx := newImportFixture(t)
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("ingest-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO storage_import_intents (lease_id, object_hash, created_at) VALUES ($1, $2, now() - interval '2 hours')`,
		uuid.New(), fx.hash); err != nil {
		t.Fatal(err)
	}
	res, err := fx.svc.ReconcileImportIntents(context.Background(), 30*time.Minute, 100)
	if err != nil || res.BytesDeleted != 0 || res.IntentsDropped != 1 {
		t.Fatalf("reconcile %+v %v", res, err)
	}
	if _, err := fx.backend.Stat(context.Background(), fx.hash, VariantOriginal); err != nil {
		t.Fatalf("committed bytes deleted: %v", err)
	}
}

func TestReleaseStalePins(t *testing.T) {
	fx := newImportFixture(t)
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.ImportVerified(context.Background(), fx.req("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE storage_pins SET created_at = now() - interval '2 days' WHERE object_hash = $1 AND pin_subject_id = 'old'`, fx.hash); err != nil {
		t.Fatal(err)
	}
	n, err := fx.svc.ReleaseStalePins(context.Background(), "federation_ingest", 24*time.Hour, 100)
	if err != nil || n != 1 {
		t.Fatalf("released %d, %v", n, err)
	}
	if _, _, p, _ := fx.rows(t); p != 1 {
		t.Fatalf("pins = %d, want 1 (the fresh one)", p)
	}
}

func backdate(t *testing.T, fx *importFixture, by time.Duration) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE storage_import_intents SET created_at = now() - make_interval(secs => $2) WHERE object_hash = $1`,
		fx.hash, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}
