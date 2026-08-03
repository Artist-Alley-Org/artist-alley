// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #821 — asset_field_value_history gains the two ON DELETE CASCADE
// foreign keys its collection sibling always had (migration 00031).
//
// The failure mode this migration has to survive is a table that
// ALREADY holds orphans — history rows whose asset or field is gone,
// which is the very bug the FKs close and which every pre-00031 install
// has been free to accumulate. Adding a FK to a table with a violating
// row aborts the ALTER, so 00031's Up deletes orphans first. A FRESH
// database has no orphans and so cannot exercise that path — which is
// why this test migrates to 00030, PLANTS an orphan, and only then
// applies 00031: without the orphan delete the ADD CONSTRAINT would
// fail here and the test would catch it.
//
// It then proves the FKs actually cascade (the point of the change) and
// that the Down cleanly removes them.

package db

import (
	"database/sql"
	"io/fs"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

const (
	afvhBeforeVersion = 30 // 00030_email_template_overrides
	afvhAtVersion     = 31 // 00031_asset_field_value_history_fks
)

func afvhProvider(t *testing.T, sqlDB *sql.DB) *goose.Provider {
	t.Helper()
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

func fkExists(t *testing.T, sqlDB *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1 AND contype = 'f')`,
		name).Scan(&exists); err != nil {
		t.Fatalf("check constraint %s: %v", name, err)
	}
	return exists
}

func TestMigration00031_OrphanPath_Cascade_AndDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Up to the state just before 00031 ────────────────────────────
	if _, err := p.UpTo(ctx, afvhBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", afvhBeforeVersion, err)
	}

	// Plant an ORPHAN: a history row naming an asset and field that do
	// not exist. Legal at v30 (no FK), and the exact row a real install
	// accumulates. If 00031's Up did not delete it first, the ADD
	// CONSTRAINT below would abort.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO asset_field_value_history (asset_id, field_id, new_value)
		 VALUES ($1, $2, '"orphan"'::jsonb)`,
		uuid.New(), uuid.New()); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}

	// ── Apply 00031 — the orphan-delete + FK-add path ────────────────
	if _, err := p.UpTo(ctx, afvhAtVersion); err != nil {
		t.Fatalf("migrate up to %d (the orphan-delete path): %v", afvhAtVersion, err)
	}
	for _, name := range []string{
		"asset_field_value_history_asset_id_fkey",
		"asset_field_value_history_field_id_fkey",
	} {
		if !fkExists(t, sqlDB, name) {
			t.Errorf("FK %s missing after 00031 Up", name)
		}
	}
	// The orphan is gone (it had to be, or the ALTER would have failed —
	// this just makes the intent explicit).
	var orphans int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM asset_field_value_history`).Scan(&orphans); err != nil {
		t.Fatalf("count after up: %v", err)
	}
	if orphans != 0 {
		t.Errorf("asset_field_value_history has %d row(s) after the orphan sweep, want 0", orphans)
	}

	// ── Both FKs cascade ─────────────────────────────────────────────
	var userRef int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ('mig31_user', 'U', 1) RETURNING ref`).
		Scan(&userRef); err != nil {
		t.Fatalf("user: %v", err)
	}
	var assetType int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT ref FROM asset_types ORDER BY ref LIMIT 1`).Scan(&assetType); err != nil {
		t.Fatalf("asset_types: %v", err)
	}

	// field_id cascade: delete the field_definition, its history rows go.
	fieldID := plantFieldAssetHistory(t, sqlDB, userRef, assetType)
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM field_definition WHERE id = $1`, fieldID); err != nil {
		t.Fatalf("delete field: %v", err)
	}
	if n := countHistoryForField(t, sqlDB, fieldID); n != 0 {
		t.Errorf("deleting a field_definition left %d history row(s); the field_id FK is not cascading", n)
	}

	// asset_id cascade: delete the asset, its history rows go.
	fieldID2, assetID2 := plantFieldAssetHistoryReturningAsset(t, sqlDB, userRef, assetType)
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM assets WHERE id = $1`, assetID2); err != nil {
		t.Fatalf("delete asset: %v", err)
	}
	var afterAsset int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM asset_field_value_history WHERE asset_id = $1`, assetID2).Scan(&afterAsset); err != nil {
		t.Fatalf("count history after asset delete: %v", err)
	}
	if afterAsset != 0 {
		t.Errorf("deleting an asset left %d history row(s); the asset_id FK is not cascading", afterAsset)
	}
	_, _ = sqlDB.ExecContext(ctx, `DELETE FROM field_definition WHERE id = $1`, fieldID2)

	// ── Down removes both FKs cleanly ────────────────────────────────
	if _, err := p.DownTo(ctx, afvhBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", afvhBeforeVersion, err)
	}
	for _, name := range []string{
		"asset_field_value_history_asset_id_fkey",
		"asset_field_value_history_field_id_fkey",
	} {
		if fkExists(t, sqlDB, name) {
			t.Errorf("FK %s still present after 00031 Down", name)
		}
	}

	// ── Up again is clean (re-adds; table is empty so no orphan path) ─
	if _, err := p.UpTo(ctx, afvhAtVersion); err != nil {
		t.Fatalf("re-apply 00031 after down: %v", err)
	}
}

// plantFieldAssetHistory creates a field, an asset and a history row
// tying them together, and returns the field id.
func plantFieldAssetHistory(t *testing.T, sqlDB *sql.DB, userRef, assetType int64) uuid.UUID {
	t.Helper()
	fieldID, _ := plantFieldAssetHistoryReturningAsset(t, sqlDB, userRef, assetType)
	return fieldID
}

func plantFieldAssetHistoryReturningAsset(t *testing.T, sqlDB *sql.DB, userRef, assetType int64) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	code := "mig31_field_" + uuid.NewString()[:8]
	var fieldID uuid.UUID
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO field_definition (code, label, type, subject_kind)
		 VALUES ($1, 'Mig31', 'text', 'asset') RETURNING id`, code).Scan(&fieldID); err != nil {
		t.Fatalf("field_definition: %v", err)
	}
	assetID := uuid.New()
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO assets (id, owner_user_ref, title, asset_type) VALUES ($1, $2, 'Mig31 asset', $3)`,
		assetID, userRef, assetType); err != nil {
		t.Fatalf("asset: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO asset_field_value_history (asset_id, field_id, new_value)
		 VALUES ($1, $2, '"v"'::jsonb)`, assetID, fieldID); err != nil {
		t.Fatalf("history: %v", err)
	}
	return fieldID, assetID
}

func countHistoryForField(t *testing.T, sqlDB *sql.DB, fieldID uuid.UUID) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM asset_field_value_history WHERE field_id = $1`, fieldID).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	return n
}
