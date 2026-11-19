// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: a hard purge never erases an asset its owner restored after the
// purge picked its candidates. Pinned on a real database, with the purge
// held at its race windows by test seams and the waits observed in
// Postgres (pg_blocking_pids):
//
//   - restore commits in the gap after candidate selection: the DELETE
//     re-checks eligibility and skips the restored asset;
//   - restore still open when the DELETE reaches the row: the DELETE
//     waits for the row lock, re-evaluates its WHERE on the restored row
//     and skips it;
//   - purge first: a restore reaching the row after the DELETE waits for
//     the purge and then reports ErrNotFound.
//
// A skipped asset keeps its row, its version and the version's pin, and
// gets no cache fan-out and no audit event; the asset actually deleted
// gets exactly one of each, and only deleted rows are counted.

package softdelete_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

const purgeRetentionDays = 30

type purgeRig struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	svc    *softdelete.Service
	mu     sync.Mutex
	fanned []uuid.UUID
}

func newPurgeRig(t *testing.T) *purgeRig {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") + " port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") + " dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	r := &purgeRig{t: t, ctx: t.Context(), pool: pool}
	r.svc = softdelete.NewService(pool, audit.NewRecorder(pool, slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.svc.OnAssetsHardDeleted = func(_ context.Context, ids []uuid.UUID) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.fanned = append(r.fanned, ids...)
	}
	return r
}

// asset inserts a local asset with fresh bytes, soft-deleted long past
// retention (older than anything else in the database, so it heads the
// batch), and returns it with its version 1 and the version's pin.
func (r *purgeRig) asset() (id uuid.UUID, hash string) {
	r.t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		r.t.Fatal(err)
	}
	hash, id = hex.EncodeToString(b), uuid.New()
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 10, 'image/png', 'fs')`, hash); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO assets (id, title, asset_type, file_hash, file_extension, deleted_at, deleted_reason)
		VALUES ($1, 'purge race', (SELECT MIN(ref) FROM asset_types), $2, 'png', now() - interval '20000 days', 'owner')`, id, hash); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	if r.versionPins(hash) != 1 {
		r.t.Fatal("the fixture asset has no version pin")
	}
	return id, hash
}

func (r *purgeRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (r *purgeRig) versionPins(hash string) int {
	return r.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_version'`, hash)
}

func (r *purgeRig) audited(id uuid.UUID) int {
	return r.count(`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'asset_id' = $2`,
		audit.EventAdminAssetHardDeletedByGC, id.String())
}

func (r *purgeRig) fannedOut(id uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.fanned {
		if f == id {
			n++
		}
	}
	return n
}

// waitBlocked returns once some backend of this database waits on a lock.
func (r *purgeRig) waitBlocked(what string, done <-chan error) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND cardinality(pg_blocking_pids(pid)) > 0`) > 0 {
			return
		}
		select {
		case err := <-done:
			r.t.Fatalf("%s finished before it waited on the row lock: %v", what, err)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s never waited on the row lock", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireKept: the restored asset is live, with its version and pin, and
// was neither fanned out nor audited.
func (r *purgeRig) requireKept(id uuid.UUID, hash string) {
	r.t.Helper()
	if n := r.count(`SELECT count(*) FROM assets WHERE id = $1 AND deleted_at IS NULL`, id); n != 1 {
		r.t.Errorf("the restored asset is gone or not live (%d rows)", n)
	}
	if n := r.count(`SELECT count(*) FROM asset_versions WHERE asset_id = $1`, id); n != 1 {
		r.t.Errorf("the restored asset has %d versions, want 1", n)
	}
	if r.versionPins(hash) != 1 || r.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NOT NULL`, hash) != 0 {
		r.t.Error("the restored asset's bytes lost their version pin or were scheduled for GC")
	}
	if n := r.fannedOut(id); n != 0 {
		r.t.Errorf("the restored asset was fanned out %d times", n)
	}
	if n := r.audited(id); n != 0 {
		r.t.Errorf("the restored asset was audited as hard-deleted %d times", n)
	}
}

// requirePurged: the asset is gone with its version and pin, fanned out
// and audited exactly once.
func (r *purgeRig) requirePurged(id uuid.UUID, hash string) {
	r.t.Helper()
	if n := r.count(`SELECT count(*) FROM assets WHERE id = $1`, id); n != 0 {
		r.t.Error("the still-eligible asset was not purged")
	}
	if r.versionPins(hash) != 0 {
		r.t.Error("the purged asset's version pin remains")
	}
	if n := r.fannedOut(id); n != 1 {
		r.t.Errorf("the purged asset was fanned out %d times, want 1", n)
	}
	if n := r.audited(id); n != 1 {
		r.t.Errorf("the purged asset was audited %d times, want 1", n)
	}
}

func (r *purgeRig) requireCandidates(candidates []uuid.UUID, ids ...uuid.UUID) {
	r.t.Helper()
	for _, id := range ids {
		if !slices.Contains(candidates, id) {
			r.t.Errorf("%s is not among the purge's candidates", id)
		}
	}
}

func (r *purgeRig) purge() (int, error) {
	return r.svc.HardDeletePastAssets(context.Background(), purgeRetentionDays, 1000)
}

// The count reports only the rows the DELETE removed.
func (r *purgeRig) requireCountIsDeletedRows(n int) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if n != len(r.fanned) {
		r.t.Errorf("purge reported %d deleted, fanned out %d", n, len(r.fanned))
	}
}

func TestHardDeletePastAssets_RestoreCommittedAfterSelectionIsSkipped(t *testing.T) {
	r := newPurgeRig(t)
	restored, rHash := r.asset()
	purged, pHash := r.asset()
	r.svc.SetPurgeAfterSelectHook(func(candidates []uuid.UUID) {
		r.requireCandidates(candidates, restored, purged)
		if err := r.svc.RestoreAsset(context.Background(), nil, restored, 1); err != nil {
			t.Errorf("restore: %v", err)
		}
	})
	n, err := r.purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.requireKept(restored, rHash)
	r.requirePurged(purged, pHash)
	r.requireCountIsDeletedRows(n)
}

func TestHardDeletePastAssets_RestoreOpenWhenDeleteArrivesIsSkipped(t *testing.T) {
	r := newPurgeRig(t)
	restored, rHash := r.asset()
	purged, pHash := r.asset()
	restoreTx, err := r.pool.Begin(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restoreTx.Rollback(context.Background()) }()
	held := make(chan struct{})
	r.svc.SetPurgeAfterSelectHook(func(candidates []uuid.UUID) {
		r.requireCandidates(candidates, restored, purged)
		if _, err := restoreTx.Exec(context.Background(),
			`UPDATE assets SET deleted_at = NULL, deleted_reason = NULL, updated_at = now() WHERE id = $1 AND deleted_at IS NOT NULL`, restored); err != nil {
			t.Errorf("restore: %v", err)
		}
		close(held)
	})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	errs := make(chan error, 1)
	go func() {
		n, err := r.purge()
		done <- result{n, err}
		errs <- err
	}()
	<-held
	r.waitBlocked("the purge's DELETE", errs)
	if err := restoreTx.Commit(r.ctx); err != nil {
		t.Fatalf("restore commit: %v", err)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("purge: %v", res.err)
	}
	r.requireKept(restored, rHash)
	r.requirePurged(purged, pHash)
	r.requireCountIsDeletedRows(res.n)
}

func TestHardDeletePastAssets_PurgeFirstRestoreReportsNotFound(t *testing.T) {
	r := newPurgeRig(t)
	id, hash := r.asset()
	deleted, release := make(chan struct{}), make(chan struct{})
	r.svc.SetPurgeBeforeCommitHook(func() { close(deleted); <-release })
	purged := make(chan error, 1)
	go func() {
		_, err := r.purge()
		purged <- err
	}()
	<-deleted
	restore := make(chan error, 1)
	go func() { restore <- r.svc.RestoreAsset(context.Background(), nil, id, 1) }()
	r.waitBlocked("the restore", restore)
	close(release)
	if err := <-purged; err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := <-restore; !errors.Is(err, softdelete.ErrNotFound) {
		t.Fatalf("restore after the purge committed: %v, want ErrNotFound", err)
	}
	r.requirePurged(id, hash)
}
