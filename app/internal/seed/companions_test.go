// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// White-box integration test for multi-file companion auto-registration
// (#486). Drives Runner.applyAssetCompanions against the real dev
// Postgres + an fs storage backend, proving a seeded .gltf's declared
// siblings land as asset_companions rows with downloadable bytes.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the other
// seed integration tests.

package seed

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/logging"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
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
		" dbname=" + envOr("AA_DB_NAME", "artist_alley") +
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

	// Minimal asset row for the FK. asset_type=1 (Image) is baseline-seeded.
	assetID := uuid.New()
	pgAsset := pgtype.UUID{Bytes: assetID, Valid: true}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, asset_type, title) VALUES ($1, 1, 'companion-test')`,
		pgAsset); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		// Drop the content-addressed blobs + pins we created so reruns
		// don't dedup against stale rows (see the nonce comment below).
		rows, _ := pool.Query(c, `SELECT object_hash FROM asset_companions WHERE asset_id = $1`, pgAsset)
		var hashes []string
		for rows.Next() {
			var h string
			if rows.Scan(&h) == nil {
				hashes = append(hashes, h)
			}
		}
		rows.Close()
		_, _ = pool.Exec(c, `DELETE FROM asset_companions WHERE asset_id = $1`, pgAsset)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, pgAsset)
		for _, h := range hashes {
			_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_companion'`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, h)
		}
	})

	r.applyAssetCompanions(ctx, pgAsset, assetID.String(), filepath.Join(dir, "model.gltf"), "gltf")

	// Exactly the two on-disk siblings register; the data: image is skipped.
	rows, err := pool.Query(ctx,
		`SELECT companion_path, object_hash, content_type, size_bytes
		   FROM asset_companions WHERE asset_id = $1 ORDER BY companion_path`, pgAsset)
	if err != nil {
		t.Fatalf("query companions: %v", err)
	}
	defer rows.Close()

	type comp struct {
		path, hash, ctype string
		size              int64
	}
	var got []comp
	for rows.Next() {
		var c comp
		if err := rows.Scan(&c.path, &c.hash, &c.ctype, &c.size); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, c)
	}
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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
