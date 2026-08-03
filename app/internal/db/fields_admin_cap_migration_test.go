// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #804 — migration 00032 makes `fields.admin` a real, grantable
// capability. This proves the Up inserts the capability row and grants
// it to the built-in Admin role, and that the Down removes both cleanly
// and a re-Up restores them (up + down + up is idempotent-clean).

package db

import (
	"database/sql"
	"testing"
)

const (
	fieldsAdminBeforeVersion = 31 // 00031_asset_field_value_history_fks
	fieldsAdminAtVersion     = 32 // 00032_fields_admin_capability
)

// adminRoleID is the built-in Admin role seeded by the baseline; it is
// the role the migration grants fields.admin to.
const adminRoleID = "aa6b632d-5bef-4924-93d4-aba070dfe503"

func capExists(t *testing.T, sqlDB *sql.DB, code string) bool {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM capabilities WHERE code = $1`, code).Scan(&n); err != nil {
		t.Fatalf("count capability %s: %v", code, err)
	}
	return n > 0
}

func roleHasCap(t *testing.T, sqlDB *sql.DB, roleID, code string) bool {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM role_capabilities WHERE role_id = $1 AND capability_code = $2`,
		roleID, code).Scan(&n); err != nil {
		t.Fatalf("count role_capability %s/%s: %v", roleID, code, err)
	}
	return n > 0
}

func TestMigration00032_FieldsAdminCapability_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Just before 00032: no fields.admin capability yet ────────────
	if _, err := p.UpTo(ctx, fieldsAdminBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", fieldsAdminBeforeVersion, err)
	}
	if capExists(t, sqlDB, "fields.admin") {
		t.Fatalf("fields.admin capability exists before 00032")
	}

	// ── Apply 00032: capability row + Admin grant appear ─────────────
	if _, err := p.UpTo(ctx, fieldsAdminAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", fieldsAdminAtVersion, err)
	}
	if !capExists(t, sqlDB, "fields.admin") {
		t.Errorf("fields.admin capability missing after 00032 Up")
	}
	if !roleHasCap(t, sqlDB, adminRoleID, "fields.admin") {
		t.Errorf("Admin role missing fields.admin grant after 00032 Up")
	}

	// ── Down removes the grant AND the capability cleanly ────────────
	if _, err := p.DownTo(ctx, fieldsAdminBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", fieldsAdminBeforeVersion, err)
	}
	if roleHasCap(t, sqlDB, adminRoleID, "fields.admin") {
		t.Errorf("Admin role still holds fields.admin after 00032 Down")
	}
	if capExists(t, sqlDB, "fields.admin") {
		t.Errorf("fields.admin capability still present after 00032 Down")
	}

	// ── Up again is clean (re-inserts row + grant) ───────────────────
	if _, err := p.UpTo(ctx, fieldsAdminAtVersion); err != nil {
		t.Fatalf("re-apply 00032 after down: %v", err)
	}
	if !capExists(t, sqlDB, "fields.admin") || !roleHasCap(t, sqlDB, adminRoleID, "fields.admin") {
		t.Errorf("fields.admin not restored after re-Up")
	}
}
