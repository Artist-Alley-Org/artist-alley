// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #403 (v0.4.0 Sprint 3) — integrity sweeps, both directions.
//
// These drive the real detection logic against a real fs backend and a
// real Postgres, seeding deliberate mismatches rather than asserting on
// mocks: an object deleted behind a live row (missing_object), bytes on
// disk that nothing references (orphan_object), and a corrupted object
// whose content no longer hashes to its key (checksum_mismatch).
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func sweepTestPool(t *testing.T) *pgxpool.Pool {
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
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedVariantRow inserts a storage_variants row (and its parent object)
// and removes both on cleanup, so the sweep sees it as "the DB expects
// these bytes".
func seedVariantRow(t *testing.T, pool *pgxpool.Pool, hash, variant string, size int64) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		 VALUES ($1,$2,'application/octet-stream','fs') ON CONFLICT (hash) DO NOTHING`, hash, size)
	if err != nil {
		t.Fatalf("seed object: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes)
		 VALUES ($1,$2,$3) ON CONFLICT (object_hash, variant_key) DO UPDATE SET size_bytes = EXCLUDED.size_bytes`,
		hash, variant, size)
	if err != nil {
		t.Fatalf("seed variant: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash=$1`, hash)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash=$1`, hash)
	})
}

func startRun(t *testing.T, pool *pgxpool.Pool, q *Queries, kind string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := q.CreateSweepRun(context.Background(), CreateSweepRunParams{
		ID:   pgtype.UUID{Bytes: id, Valid: true},
		Kind: kind,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM storage_sweep_runs WHERE id=$1`,
			pgtype.UUID{Bytes: id, Valid: true})
	})
	return id
}

func findingsFor(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT finding, object_hash || '/' || variant_key FROM storage_sweep_findings WHERE run_id=$1`,
		pgtype.UUID{Bytes: runID, Valid: true})
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var finding, subject string
		if err := rows.Scan(&finding, &subject); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		out[subject] = finding
	}
	return out
}

// TestSweep_DetectsMissingAndCorrupt drives the DB->disk direction:
// a row whose bytes were deleted, and a row whose bytes were corrupted.
func TestSweep_DetectsMissingAndCorrupt(t *testing.T) {
	pool := sweepTestPool(t)
	q := New(pool)
	backend := newMemBackend()
	svc := NewService(backend, pool)
	h := NewChecksumVerifyHandler(pool, svc, nil, nil)
	ctx := context.Background()

	healthy := []byte("healthy-object-bytes")
	healthyHash := sha256Hex(healthy)
	corrupt := []byte("corrupt-object-bytes")
	corruptHash := sha256Hex(corrupt)
	missing := []byte("missing-object-bytes")
	missingHash := sha256Hex(missing)

	// Healthy: bytes on disk match the hash they're filed under.
	mustPut(t, backend, healthyHash, VariantOriginal, healthy)
	seedVariantRow(t, pool, healthyHash, VariantOriginal, int64(len(healthy)))

	// Corrupt: filed under corruptHash, but the bytes were flipped.
	mustPut(t, backend, corruptHash, VariantOriginal, []byte("CORRUPTED-object-byte"))
	seedVariantRow(t, pool, corruptHash, VariantOriginal, int64(len(corrupt)))

	// Missing: the DB expects bytes that were never written.
	seedVariantRow(t, pool, missingHash, VariantOriginal, int64(len(missing)))

	runID := startRun(t, pool, q, "checksum_verify")
	// Walk the whole table; the cursor loop terminates when a batch
	// returns no rows.
	p := sweepPayload{RunID: runID}
	for i := 0; i < 200; i++ {
		next, _, _, err := h.verifyChecksums(ctx, p)
		if err != nil {
			t.Fatalf("verifyChecksums: %v", err)
		}
		if next.Cursor == "" && next.CursorVariant == "" {
			break
		}
		p = next
	}

	got := findingsFor(t, pool, runID)
	if f := got[corruptHash+"/"+VariantOriginal]; f != FindingChecksumMismatch {
		t.Errorf("corrupted object: got finding %q, want %q", f, FindingChecksumMismatch)
	}
	if f := got[missingHash+"/"+VariantOriginal]; f != FindingMissingObject {
		t.Errorf("missing object: got finding %q, want %q", f, FindingMissingObject)
	}
	if f, ok := got[healthyHash+"/"+VariantOriginal]; ok {
		t.Errorf("healthy object was flagged %q; a clean object must produce no finding", f)
	}
}

// TestSweep_DetectsOrphan drives the disk->DB direction — the one that
// is only possible because Backend.List exists.
func TestSweep_DetectsOrphan(t *testing.T) {
	pool := sweepTestPool(t)
	q := New(pool)
	backend := newMemBackend()
	svc := NewService(backend, pool)
	h := NewOrphanScanHandler(pool, svc, nil, nil)
	ctx := context.Background()

	referenced := []byte("referenced-bytes")
	refHash := sha256Hex(referenced)
	orphan := []byte("orphan-bytes-nothing-points-here")
	orphanHash := sha256Hex(orphan)

	mustPut(t, backend, refHash, VariantOriginal, referenced)
	seedVariantRow(t, pool, refHash, VariantOriginal, int64(len(referenced)))

	// On disk, deliberately NOT in the DB.
	mustPut(t, backend, orphanHash, VariantOriginal, orphan)

	runID := startRun(t, pool, q, "orphan_scan")
	p := sweepPayload{RunID: runID}
	for i := 0; i < 200; i++ {
		next, _, _, err := h.scanOrphans(ctx, p)
		if err != nil {
			t.Fatalf("scanOrphans: %v", err)
		}
		if next.Cursor == "" {
			break
		}
		p = next
	}

	got := findingsFor(t, pool, runID)
	if f := got[orphanHash+"/"+VariantOriginal]; f != FindingOrphanObject {
		t.Errorf("unreferenced object: got finding %q, want %q", f, FindingOrphanObject)
	}
	if f, ok := got[refHash+"/"+VariantOriginal]; ok {
		t.Errorf("referenced object was flagged %q; a live object must never be reported as an orphan", f)
	}
}

// memBackend is a minimal in-package storage.Backend for sweep tests.
// The real fs backend lives in a subpackage that imports this one, so
// an in-package test cannot use it without an import cycle. A fake is
// fine here: what is under test is the sweep's detection logic, and the
// backends' own List behaviour is covered by the shared contract test.
type memBackend struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemBackend() *memBackend { return &memBackend{objs: map[string][]byte{}} }

func (m *memBackend) Name() string { return "mem" }

func (m *memBackend) Put(_ context.Context, hash, variant string, r io.Reader) (*ObjectInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[ObjectPath(hash, variant)] = b
	return &ObjectInfo{Size: int64(len(b))}, nil
}

func (m *memBackend) Get(_ context.Context, hash, variant string) (io.ReadCloser, *ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[ObjectPath(hash, variant)]
	if !ok {
		return nil, nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), &ObjectInfo{Size: int64(len(b))}, nil
}

func (m *memBackend) GetRange(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

func (m *memBackend) Delete(_ context.Context, hash, variant string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, ObjectPath(hash, variant))
	return nil
}

func (m *memBackend) Stat(_ context.Context, hash, variant string) (*ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[ObjectPath(hash, variant)]
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectInfo{Size: int64(len(b))}, nil
}

func (m *memBackend) PresignGet(context.Context, string, string, time.Duration) (string, error) {
	return "", ErrUnsupported
}

func (m *memBackend) PresignPut(context.Context, string, string, time.Duration) (string, error) {
	return "", ErrUnsupported
}

func (m *memBackend) List(_ context.Context, cursor string, limit int) ([]ObjectRef, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objs))
	for k := range m.objs {
		if cursor == "" || k > cursor {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	next := ""
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	refs := make([]ObjectRef, 0, len(keys))
	for _, k := range keys {
		h, v, err := ParseObjectPath(k)
		if err != nil {
			continue
		}
		refs = append(refs, ObjectRef{Hash: h, Variant: v, Size: int64(len(m.objs[k]))})
	}
	return refs, next, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustPut(t *testing.T, b Backend, hash, variant string, body []byte) {
	t.Helper()
	if _, err := b.Put(context.Background(), hash, variant, bytes.NewReader(body)); err != nil {
		t.Fatalf("put %s/%s: %v", hash[:8], variant, err)
	}
}
