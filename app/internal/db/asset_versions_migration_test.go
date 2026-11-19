// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: migration 00084 versions every existing local asset from the
// bytes it has NOW. Pinned against a real, fresh database:
//
//   - a local asset with bytes, soft-deleted or not, gets one draft v1
//     with origin backfill_current, created at migration time (never the
//     asset's own creation time: the version did not exist then), its
//     bytes pinned to the version and no longer GC-scheduled;
//   - an asset without bytes, and a remote asset, get no version;
//   - Down releases every version pin (scheduling unpinned bytes for GC)
//     and removes the tables, the column and the storage_pins guards; Up
//     again re-creates them;
//   - TRUNCATE (seed reset) releases the pins of the versions it removed.

package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	assetVersionsBeforeVersion = 83 // 00083_asset_preservation_events
	assetVersionsAtVersion     = 84 // 00084_asset_versions
)

func TestMigration00084_AssetVersions_BackfillsCurrentBytesOnly(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, assetVersionsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", assetVersionsBeforeVersion, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	live, gone, remoteHash := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	for _, h := range []string{live, gone, remoteHash} {
		// Scheduled for GC before the migration, as an object whose last
		// pin went with a soft delete would be.
		exec(`INSERT INTO storage_objects (hash, size_bytes, content_type, backend, gc_eligible_at) VALUES ($1, 10, 'image/png', 'fs', now() + interval '1 hour')`, h)
	}
	liveAsset, softDeleted, noBytes, remote := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO assets (id, title, asset_type, file_hash, file_extension, created_at)
		VALUES ($1, 'live', (SELECT MIN(ref) FROM asset_types), $2, 'png', now() - interval '400 days')`, liveAsset, live)
	exec(`INSERT INTO assets (id, title, asset_type, file_hash, deleted_at, created_at)
		VALUES ($1, 'soft-deleted', (SELECT MIN(ref) FROM asset_types), $2, now() - interval '2 days', now() - interval '90 days')`, softDeleted, gone)
	exec(`INSERT INTO assets (id, title, asset_type) VALUES ($1, 'no bytes', (SELECT MIN(ref) FROM asset_types))`, noBytes)
	exec(`INSERT INTO assets (id, title, asset_type, file_hash, origin_server_id)
		VALUES ($1, 'remote', (SELECT MIN(ref) FROM asset_types), $2, $3)`, remote, remoteHash, uuid.New())

	migrated := time.Now().Add(-time.Minute)
	if _, err := p.UpTo(ctx, assetVersionsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", assetVersionsAtVersion, err)
	}
	verify(t, sqlDB, "after Up", []uuidHash{{liveAsset, live}, {softDeleted, gone}}, []uuid.UUID{noBytes, remote}, migrated)
	if n := count(`SELECT count(*) FROM asset_preservation_events`); n != 0 {
		t.Fatalf("the backfill invented %d preservation events", n)
	}

	if _, err := p.DownTo(ctx, assetVersionsBeforeVersion); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('asset_versions', 'asset_version_files')`); n != 0 {
		t.Fatalf("after Down: %d version tables remain", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'assets' AND column_name = 'current_version_id'`); n != 0 {
		t.Fatal("after Down: current_version_id remains")
	}
	if n := count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'asset_version'`); n != 0 {
		t.Fatalf("after Down: %d version pins remain", n)
	}
	// storage_pins survives Down; its two 00084 guards must not.
	if n := count(`SELECT count(*) FROM pg_trigger WHERE tgrelid = 'storage_pins'::regclass
		AND tgname IN ('storage_pins_version_commit_check_trg', 'storage_pins_truncate_trg')`); n != 0 {
		t.Fatalf("after Down: %d storage_pins version guards remain", n)
	}
	if n := count(`SELECT count(*) FROM storage_objects WHERE hash IN ($1, $2) AND gc_eligible_at IS NOT NULL`, live, gone); n != 2 {
		t.Fatalf("after Down: %d of 2 unpinned objects scheduled for GC", n)
	}

	migrated = time.Now().Add(-time.Minute)
	if _, err := p.UpTo(ctx, assetVersionsAtVersion); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	verify(t, sqlDB, "after Up again", []uuidHash{{liveAsset, live}, {softDeleted, gone}}, []uuid.UUID{noBytes, remote}, migrated)

	// TRUNCATE fires no row triggers; the statement trigger releases the
	// removed versions' pins and schedules the unpinned bytes.
	exec(`TRUNCATE assets CASCADE`)
	if n := count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'asset_version'`); n != 0 {
		t.Fatalf("after TRUNCATE: %d version pins dangle", n)
	}
	if n := count(`SELECT count(*) FROM storage_objects WHERE hash IN ($1, $2) AND gc_eligible_at IS NOT NULL`, live, gone); n != 2 {
		t.Fatalf("after TRUNCATE: %d of 2 unpinned objects scheduled for GC", n)
	}
}

type uuidHash struct {
	asset uuid.UUID
	hash  string
}

// verify checks each versioned asset's single draft backfill_current v1
// (created at migration time, its bytes pinned to it and not scheduled
// for GC) and that the unversioned assets have none.
func verify(t *testing.T, sqlDB *sql.DB, stage string, versioned []uuidHash, unversioned []uuid.UUID, migrated time.Time) {
	t.Helper()
	ctx := t.Context()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %s: %v", stage, q, err)
		}
		return n
	}
	for _, c := range versioned {
		var (
			vid    uuid.UUID
			origin string
			num    int64
			pred   *uuid.UUID
			hash   string
			sealed *time.Time
			made   time.Time
		)
		if err := sqlDB.QueryRowContext(ctx, `SELECT v.id, v.origin, v.number, v.predecessor_id, v.original_hash, v.sealed_at, v.created_at
			FROM asset_versions v JOIN assets a ON a.current_version_id = v.id WHERE a.id = $1`, c.asset).
			Scan(&vid, &origin, &num, &pred, &hash, &sealed, &made); err != nil {
			t.Fatalf("%s: version of %s: %v", stage, c.asset, err)
		}
		if origin != "backfill_current" || num != 1 || pred != nil || hash != c.hash || sealed != nil || made.Before(migrated) {
			t.Errorf("%s: %s v1 = origin %s number %d pred %v hash %s sealed %v created %v; want a draft backfill_current v1 created at migration time",
				stage, c.asset, origin, num, pred, hash[:8], sealed, made)
		}
		if n := count(`SELECT count(*) FROM asset_versions WHERE asset_id = $1`, c.asset); n != 1 {
			t.Errorf("%s: %s has %d versions, want 1", stage, c.asset, n)
		}
		if n := count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_version' AND pin_subject_id = $2`, c.hash, vid.String()); n != 1 {
			t.Errorf("%s: %s bytes pinned to the version %d times, want 1", stage, c.asset, n)
		}
		if n := count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NOT NULL`, c.hash); n != 0 {
			t.Errorf("%s: %s bytes still scheduled for GC", stage, c.asset)
		}
	}
	for _, id := range unversioned {
		if n := count(`SELECT count(*) FROM asset_versions WHERE asset_id = $1`, id); n != 0 {
			t.Errorf("%s: %s has %d versions, want none", stage, id, n)
		}
		if n := count(`SELECT count(*) FROM assets WHERE id = $1 AND current_version_id IS NOT NULL`, id); n != 0 {
			t.Errorf("%s: %s has a current version", stage, id)
		}
	}
}
