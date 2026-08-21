// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// White-box integration test for multi-file companion auto-registration
// (#486, #750). Drives Runner.applyAssetCompanions against the real dev
// Postgres + an fs storage backend, proving a seeded .gltf's or .glb's
// declared siblings land as asset_companions rows with downloadable
// bytes — and that a model which genuinely embeds everything lands none.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the other
// seed integration tests.

package seed

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/logging"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// openCompanionTestPool mirrors the DSN convention in admin_test.go
// (package seed_test). This test is white-box (package seed) so it can
// drive the unexported applyAssetCompanions, hence its own helper.
func openCompanionTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	envOr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("dev Postgres not reachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestApplyAssetCompanions_RegistersGLTFSiblings(t *testing.T) {
	pool := openCompanionTestPool(t)
	ctx := context.Background()

	// fs storage backend rooted in a temp dir — no MinIO needed.
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs backend: %v", err)
	}
	svc := storage.NewService(backend, pool)

	r := NewRunner(pool, svc, Options{Logger: logging.Setup("error", "text")})

	// A tiny synthetic multi-file glTF: model.gltf → model.bin + tex.png.
	// Content is made unique per run (nonce) because storage is
	// content-addressed against the SHARED dev DB: identical bytes across
	// runs would dedup to a hash whose blob lives in a prior run's
	// (now-deleted) temp backend, making Download 404. Unique bytes → a
	// fresh hash that always writes to this run's fs root.
	nonce := uuid.NewString()
	dir := t.TempDir()
	gltf := `{"asset":{"version":"2.0"},` +
		`"buffers":[{"uri":"model.bin","byteLength":4}],` +
		`"images":[{"uri":"tex.png"},{"uri":"data:image/png;base64,AAAA"}]}`
	mustWrite(t, filepath.Join(dir, "model.gltf"), gltf)
	mustWrite(t, filepath.Join(dir, "model.bin"), "bin-"+nonce)
	mustWrite(t, filepath.Join(dir, "tex.png"), "\x89PNG\r\n"+nonce)

	pgAsset := newCompanionTestAsset(t, pool)

	r.applyAssetCompanions(ctx, pgAsset, uuid.UUID(pgAsset.Bytes).String(),
		filepath.Join(dir, "model.gltf"))

	// Exactly the two on-disk siblings register; the data: image is skipped.
	got := listCompanions(t, pool, pgAsset)
	if len(got) != 2 {
		t.Fatalf("expected 2 companions, got %d: %+v", len(got), got)
	}
	// model.bin then tex.png (alphabetical).
	if got[0].path != "model.bin" || got[1].path != "tex.png" {
		t.Fatalf("unexpected paths: %q, %q", got[0].path, got[1].path)
	}
	if got[1].ctype != "image/png" {
		t.Errorf("tex.png content-type = %q, want image/png", got[1].ctype)
	}
	if got[0].size <= 0 {
		t.Errorf("model.bin size = %d, want > 0", got[0].size)
	}

	// Bytes are actually retrievable from storage (the pin + upload landed).
	for _, c := range got {
		rc, _, dErr := svc.Download(ctx, c.hash, storage.VariantOriginal)
		if dErr != nil {
			t.Fatalf("download companion %s: %v", c.path, dErr)
		}
		_ = rc.Close()
	}
}

// TestApplyAssetCompanions_RegistersGLBTextures is the seed-path half of
// #750: a GLB that names sibling textures must land companion rows, and a
// GLB that embeds everything must land none. The caller used to pre-filter
// GLB out entirely, so the first case silently registered nothing across
// 363 seeded models.
func TestApplyAssetCompanions_RegistersGLBTextures(t *testing.T) {
	pool := openCompanionTestPool(t)
	ctx := context.Background()

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs backend: %v", err)
	}
	svc := storage.NewService(backend, pool)
	r := NewRunner(pool, svc, Options{Logger: logging.Setup("error", "text")})

	nonce := uuid.NewString()
	dir := t.TempDir()

	// External-reference GLB, laid out like the Kenney kits: the declared
	// URI is a path into a Textures/ subdirectory.
	external := `{"asset":{"version":"2.0"},` +
		`"buffers":[{"byteLength":4}],` +
		`"images":[{"uri":"Textures/planks.png"},{"uri":"Textures/gone.png"}]}`
	mustWriteGLB(t, filepath.Join(dir, "wall.glb"), external)
	if err := os.MkdirAll(filepath.Join(dir, "Textures"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "Textures", "planks.png"), "\x89PNG\r\n"+nonce)
	// Textures/gone.png intentionally absent — declared but unstageable.

	// Embedded GLB: everything inside the container.
	embedded := `{"asset":{"version":"2.0"},` +
		`"buffers":[{"byteLength":4}],"images":[{"bufferView":0}]}`
	mustWriteGLB(t, filepath.Join(dir, "solo.glb"), embedded)

	extAsset := newCompanionTestAsset(t, pool)
	soloAsset := newCompanionTestAsset(t, pool)

	r.applyAssetCompanions(ctx, extAsset, uuid.UUID(extAsset.Bytes).String(),
		filepath.Join(dir, "wall.glb"))
	r.applyAssetCompanions(ctx, soloAsset, uuid.UUID(soloAsset.Bytes).String(),
		filepath.Join(dir, "solo.glb"))

	got := listCompanions(t, pool, extAsset)
	if len(got) != 1 {
		t.Fatalf("expected 1 companion for the external GLB, got %d: %+v", len(got), got)
	}
	if got[0].path != "Textures/planks.png" {
		t.Errorf("companion path = %q, want Textures/planks.png", got[0].path)
	}
	if got[0].ctype != "image/png" {
		t.Errorf("content-type = %q, want image/png", got[0].ctype)
	}
	// The path must be stored exactly as declared: stageCompanions writes
	// it under the render workdir and the viewer's URL modifier matches on
	// it, so a rewritten path is a 404 either way.
	rc, _, dErr := svc.Download(ctx, got[0].hash, storage.VariantOriginal)
	if dErr != nil {
		t.Fatalf("download companion: %v", dErr)
	}
	_ = rc.Close()

	if solo := listCompanions(t, pool, soloAsset); len(solo) != 0 {
		t.Fatalf("embedded GLB registered %d companions: %+v", len(solo), solo)
	}
}

type companionRow struct {
	path, hash, ctype string
	size              int64
}

func listCompanions(t *testing.T, pool *pgxpool.Pool, assetID pgtype.UUID) []companionRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT companion_path, object_hash, content_type, size_bytes
		   FROM asset_companions WHERE asset_id = $1 ORDER BY companion_path`, assetID)
	if err != nil {
		t.Fatalf("query companions: %v", err)
	}
	defer rows.Close()
	var out []companionRow
	for rows.Next() {
		var c companionRow
		if err := rows.Scan(&c.path, &c.hash, &c.ctype, &c.size); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// newCompanionTestAsset inserts a minimal asset row for the FK and
// registers the same blob/pin cleanup the glTF case documents: storage is
// content-addressed against the SHARED dev DB, so a hash left behind
// points at a deleted temp backend on the next run.
func newCompanionTestAsset(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, asset_type, title) VALUES ($1, 1, 'companion-test')`,
		pgID); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		var hashes []string
		rows, _ := pool.Query(c, `SELECT object_hash FROM asset_companions WHERE asset_id = $1`, pgID)
		for rows.Next() {
			var h string
			if rows.Scan(&h) == nil {
				hashes = append(hashes, h)
			}
		}
		rows.Close()
		_, _ = pool.Exec(c, `DELETE FROM asset_companions WHERE asset_id = $1`, pgID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, pgID)
		for _, h := range hashes {
			_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_companion'`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, h)
		}
	})
	return pgID
}

// mustWriteGLB wraps a glTF JSON document in a spec-shaped GLB container:
// 12-byte header ('glTF', version, total length) + JSON chunk + BIN chunk.
func mustWriteGLB(t *testing.T, path, doc string) {
	t.Helper()
	pad := make([]byte, (4-len(doc)%4)%4)
	for i := range pad {
		pad[i] = ' '
	}
	bin := []byte("bin\x00")
	jsonLen := len(doc) + len(pad)

	var buf bytes.Buffer
	put := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	put(0x46546C67) // 'glTF'
	put(2)
	put(uint32(12 + 8 + jsonLen + 8 + len(bin)))
	put(uint32(jsonLen))
	put(0x4E4F534A) // 'JSON'
	buf.WriteString(doc)
	buf.Write(pad)
	put(uint32(len(bin)))
	put(0x004E4942) // 'BIN\0'
	buf.Write(bin)

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
