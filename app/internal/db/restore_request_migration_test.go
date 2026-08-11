// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #931 — migration 00042 gives restoration appeals a marker capability
// and widens `resource_request` from asset-only to three kinds.
//
// What is worth pinning here is not "a column appeared". It is:
//
//   - the capability does not exist before 00042, so the migration is
//     not a no-op some earlier file already did;
//   - it is granted to NO role and NO user — a marker nobody holds is
//     the whole basis for the decide gate being safe (00035's argument,
//     reapplied), and a role seed that quietly picked it up would turn
//     an inert code into a held one;
//   - the CHECK really refuses a fourth kind, proven by trying to write
//     one rather than by reading pg_constraint and believing it;
//   - the uniqueness rule now separates two kinds sharing a uuid, which
//     is the property the coalesce read depends on;
//   - the rename is a RENAME — the old name is gone, so nothing can
//     still be reading it;
//   - Down really reverses it, including the rows it cannot represent,
//     and a re-Up is clean.
//
// The Down data-loss case (non-asset targets are deleted rather than
// left pointing at the wrong table) is asserted explicitly, because a
// rollback that silently retyped a post id as an asset id would be
// worse than one that dropped the row: every reader downstream joins
// that column to `assets`.

package db

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

const (
	restoreReqBeforeVersion = 41 // 00041_team_follows
	restoreReqAtVersion     = 42 // 00042_restore_request_capability
)

const capRestoreRequest = "content.restore.request"

// columnExists answers whether a column is present on a table.
func columnExists(t *testing.T, sqlDB *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		      WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	return exists
}

// indexDef returns an index's definition, or "" when it does not exist.
func indexDef(t *testing.T, sqlDB *sql.DB, name string) string {
	t.Helper()
	var def string
	err := sqlDB.QueryRowContext(t.Context(),
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
		name).Scan(&def)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return def
}

func capabilityExists(t *testing.T, sqlDB *sql.DB, code string) bool {
	t.Helper()
	var exists bool
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM capabilities WHERE code = $1)`, code).Scan(&exists); err != nil {
		t.Fatalf("check capability %s: %v", code, err)
	}
	return exists
}

// seedRequester makes a user that resource_request rows can name, since
// requester_user_ref is FK-constrained.
func seedRequester(t *testing.T, sqlDB *sql.DB, username string) int64 {
	t.Helper()
	var ref int64
	if err := sqlDB.QueryRowContext(t.Context(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'U', 1) RETURNING ref`,
		username).Scan(&ref); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	return ref
}

func TestMigration00042_RestoreRequest_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Before 00042 ─────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, restoreReqBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", restoreReqBeforeVersion, err)
	}
	if capabilityExists(t, sqlDB, capRestoreRequest) {
		t.Fatalf("%s already exists at v%d — 00042 is not the migration that introduces it",
			capRestoreRequest, restoreReqBeforeVersion)
	}
	if !columnExists(t, sqlDB, "resource_request", "target_asset_id") {
		t.Fatalf("resource_request.target_asset_id missing at v%d; the rename has nothing to rename",
			restoreReqBeforeVersion)
	}
	if columnExists(t, sqlDB, "resource_request", "target_kind") {
		t.Fatalf("resource_request.target_kind already exists at v%d", restoreReqBeforeVersion)
	}

	// ── Apply 00042 ──────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, restoreReqAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", restoreReqAtVersion, err)
	}

	if !capabilityExists(t, sqlDB, capRestoreRequest) {
		t.Errorf("%s missing after 00042 Up", capRestoreRequest)
	}
	// A marker NOBODY holds. The decide gate's safety argument (00035,
	// restated by 00042) rests on this code conferring nothing AND on
	// nobody having it — a role seed that picked it up would make the
	// first half irrelevant.
	var roleGrants, userGrants int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM role_capabilities WHERE capability_code = $1),
		        (SELECT count(*) FROM user_capability_grants WHERE capability_code = $1)`,
		capRestoreRequest).Scan(&roleGrants, &userGrants); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if roleGrants != 0 || userGrants != 0 {
		t.Errorf("%s is held by %d role(s) and %d user(s); it must be held by nobody",
			capRestoreRequest, roleGrants, userGrants)
	}

	// The rename is a rename, not an addition.
	if columnExists(t, sqlDB, "resource_request", "target_asset_id") {
		t.Errorf("resource_request.target_asset_id still exists after 00042; " +
			"a leftover old column means some reader can still be pointed at it")
	}
	if !columnExists(t, sqlDB, "resource_request", "target_id") {
		t.Errorf("resource_request.target_id missing after 00042")
	}
	if !columnExists(t, sqlDB, "resource_request", "target_kind") {
		t.Errorf("resource_request.target_kind missing after 00042")
	}

	requester := seedRequester(t, sqlDB, "mig42_requester")

	// The default backfills existing rows as 'asset', which is what
	// every pre-00042 row was.
	targetA := uuid.New()
	var gotKind string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO resource_request (requester_user_ref, target_id, requested_capability)
		 VALUES ($1, $2, $3) RETURNING target_kind`,
		requester, targetA, capRestoreRequest).Scan(&gotKind); err != nil {
		t.Fatalf("insert with defaulted kind: %v", err)
	}
	if gotKind != "asset" {
		t.Errorf("target_kind defaulted to %q, want \"asset\"", gotKind)
	}

	// The CHECK refuses a fourth kind. Proven by writing, not by
	// reading the constraint definition.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO resource_request (requester_user_ref, target_kind, target_id, requested_capability)
		 VALUES ($1, 'workspace', $2, $3)`,
		requester, uuid.New(), capRestoreRequest); err == nil {
		t.Error("target_kind = 'workspace' was accepted; the CHECK does not constrain the column")
	}

	// Uniqueness now separates kinds. Two pending appeals from one
	// requester against the SAME uuid in two different tables are two
	// different asks, and the coalesce read (which matches this index)
	// must be able to tell them apart.
	shared := uuid.New()
	for _, k := range []string{"asset", "post"} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO resource_request (requester_user_ref, target_kind, target_id, requested_capability)
			 VALUES ($1, $2, $3, $4)`,
			requester, k, shared, capRestoreRequest); err != nil {
			t.Fatalf("insert pending appeal for kind %s on a shared uuid: %v", k, err)
		}
	}
	// ...but the same (requester, kind, id, capability) twice is still
	// one ask.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO resource_request (requester_user_ref, target_kind, target_id, requested_capability)
		 VALUES ($1, 'asset', $2, $3)`,
		requester, shared, capRestoreRequest); err == nil {
		t.Error("a duplicate pending ask was accepted; resource_request_one_pending_per_ask is not enforcing")
	}

	if def := indexDef(t, sqlDB, "resource_request_one_pending_per_ask"); def == "" {
		t.Error("resource_request_one_pending_per_ask missing after 00042")
	}
	if def := indexDef(t, sqlDB, "idx_resource_request_by_target"); def == "" {
		t.Error("idx_resource_request_by_target missing after 00042")
	}
	if def := indexDef(t, sqlDB, "idx_resource_request_by_asset"); def != "" {
		t.Errorf("idx_resource_request_by_asset survived 00042: %s", def)
	}

	// ── Down ─────────────────────────────────────────────────────────
	//
	// Plant one row of each disposition first, so Down's data handling
	// is observed rather than assumed:
	//
	//   an ACCESS request on an asset  → must survive
	//   an APPEAL (names the capability, about to be deleted) → must go
	//   a POST-kind row (unrepresentable at v41)              → must go
	survivor := uuid.New()
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO resource_request (requester_user_ref, target_kind, target_id, requested_capability)
		 VALUES ($1, 'asset', $2, 'content.access.request')`,
		requester, survivor); err != nil {
		t.Fatalf("plant access request: %v", err)
	}

	if _, err := p.DownTo(ctx, restoreReqBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", restoreReqBeforeVersion, err)
	}

	if capabilityExists(t, sqlDB, capRestoreRequest) {
		t.Error("capability survived Down")
	}
	if columnExists(t, sqlDB, "resource_request", "target_kind") {
		t.Error("target_kind survived Down")
	}
	if !columnExists(t, sqlDB, "resource_request", "target_asset_id") {
		t.Error("target_asset_id was not restored by Down")
	}
	if columnExists(t, sqlDB, "resource_request", "target_id") {
		t.Error("target_id survived Down")
	}

	// The access request is still there; the appeal rows are not, and
	// none of the surviving rows is a post id sitting in a column that
	// means "an asset".
	var total int
	var survivorPresent bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM resource_request),
		        EXISTS (SELECT 1 FROM resource_request WHERE target_asset_id = $1)`,
		survivor).Scan(&total, &survivorPresent); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if !survivorPresent {
		t.Error("Down deleted an ordinary access request; only appeal + non-asset rows should go")
	}
	if total != 1 {
		t.Errorf("resource_request holds %d rows after Down, want 1 (the access request); "+
			"appeal rows and non-asset targets cannot be represented at v%d",
			total, restoreReqBeforeVersion)
	}

	if def := indexDef(t, sqlDB, "resource_request_one_pending_per_ask"); def == "" {
		t.Error("Down did not put back resource_request_one_pending_per_ask")
	}
	if def := indexDef(t, sqlDB, "idx_resource_request_by_asset"); def == "" {
		t.Error("Down did not put back idx_resource_request_by_asset")
	}

	// ── Re-Up ────────────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, restoreReqAtVersion); err != nil {
		t.Fatalf("re-migrate up to %d after down: %v", restoreReqAtVersion, err)
	}
	if !capabilityExists(t, sqlDB, capRestoreRequest) {
		t.Error("capability missing after re-Up")
	}
	if !columnExists(t, sqlDB, "resource_request", "target_kind") {
		t.Error("target_kind missing after re-Up")
	}
}
