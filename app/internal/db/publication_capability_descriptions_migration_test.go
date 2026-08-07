// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #938 — migration 00038 corrects five capability descriptions that
// named states the schema does not have.
//
// A description is not decoration here. `capabilities.description` is
// the text an operator reads in the admin capability list at the moment
// they decide whether to grant something, and all five of these
// described a `draft → pending_review → published → archived` machine
// that was never built. The live constraint is
// `CHECK (status = ANY (ARRAY['draft','active','archived']))`.
//
// This test exists because the correction is data, not schema, and the
// usual safety net does not cover it: a fresh-database migration run
// proves the statements PARSE, and would pass just as happily if every
// UPDATE matched zero rows — the "accepted but empty" failure. So it
// asserts the text at v37, at v38, and after the Down, on the same
// database.
package db

import (
	"database/sql"
	"io/fs"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	pubCapsBeforeVersion = 37 // 00037_asset_admin_capability_and_deleted_by
	pubCapsAtVersion     = 38 // 00038_publication_capability_descriptions
)

func pubCapsProvider(t *testing.T, sqlDB *sql.DB) *goose.Provider {
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

func capDescription(t *testing.T, sqlDB *sql.DB, code string) string {
	t.Helper()
	var d string
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT description FROM capabilities WHERE code = $1`, code,
	).Scan(&d); err != nil {
		t.Fatalf("read description for %s: %v", code, err)
	}
	return d
}

func TestMigration00038_CorrectsDescriptions_AndDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := pubCapsProvider(t, sqlDB)

	// ── The state 00038 has to correct ──────────────────────────────
	if _, err := p.UpTo(ctx, pubCapsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", pubCapsBeforeVersion, err)
	}
	// Asserted rather than assumed: if the baseline text ever changes,
	// this fails here instead of silently making the Down restore
	// something that was never there.
	before := map[string]string{
		"assets.submit":    "Submit an asset for review (draft → pending_review)",
		"assets.review":    "Approve or reject an asset in review (pending_review → published)",
		"assets.publish":   "Publish an asset directly without review (draft → published)",
		"assets.archive":   "Archive a published asset (published → archived)",
		"assets.unarchive": "Restore an archived asset (archived → published)",
	}
	for code, want := range before {
		if got := capDescription(t, sqlDB, code); got != want {
			t.Fatalf("at v%d, %s description = %q, want %q", pubCapsBeforeVersion, code, got, want)
		}
	}
	adminBefore := capDescription(t, sqlDB, "assets.admin")
	if !strings.Contains(adminBefore, "reserved to the owner and system.admin") {
		t.Fatalf("at v%d, assets.admin description = %q, want the 00037 text", pubCapsBeforeVersion, adminBefore)
	}

	// ── Apply 00038 ─────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, pubCapsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", pubCapsAtVersion, err)
	}

	// No description may still name a state the constraint forbids,
	// EXCEPT where it says so to warn the reader off. `pending_review`
	// survives only inside the two unenforced notices, which have to
	// name it to explain why they are unenforced.
	for code := range before {
		got := capDescription(t, sqlDB, code)
		if strings.Contains(got, "published") {
			t.Errorf("%s still names the non-existent `published` state: %q", code, got)
		}
		switch code {
		case "assets.submit", "assets.review":
			if !strings.Contains(got, "UNENFORCED") {
				t.Errorf("%s must say it is unenforced (#951): %q", code, got)
			}
			if !strings.Contains(got, "#951") {
				t.Errorf("%s must point at #951: %q", code, got)
			}
		default:
			if strings.Contains(got, "pending_review") {
				t.Errorf("%s names the non-existent `pending_review` state: %q", code, got)
			}
			if !strings.Contains(got, "active") && !strings.Contains(got, "archived") {
				t.Errorf("%s names neither live state it governs: %q", code, got)
			}
		}
	}
	// Each enforced verb describes the transition the handler actually
	// gates. These substrings are the load-bearing claims, not prose.
	if got := capDescription(t, sqlDB, "assets.publish"); !strings.Contains(got, "EVERY transition INTO active") {
		t.Errorf("assets.publish description does not state the invariant: %q", got)
	}
	if got := capDescription(t, sqlDB, "assets.archive"); !strings.Contains(got, "cannot move any asset into active") {
		t.Errorf("assets.archive description does not disclaim publication: %q", got)
	}
	if got := capDescription(t, sqlDB, "assets.unarchive"); !strings.Contains(got, "requires assets.publish") {
		t.Errorf("assets.unarchive description does not state the conjunction: %q", got)
	}
	if got := capDescription(t, sqlDB, "assets.admin"); strings.Contains(got, "reserved to the owner and system.admin") {
		t.Errorf("assets.admin still claims status is owner-only: %q", got)
	}

	// ── Down restores exactly what v37 left ─────────────────────────
	if _, err := p.DownTo(ctx, pubCapsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", pubCapsBeforeVersion, err)
	}
	for code, want := range before {
		if got := capDescription(t, sqlDB, code); got != want {
			t.Errorf("after Down, %s description = %q, want %q", code, got, want)
		}
	}
	if got := capDescription(t, sqlDB, "assets.admin"); got != adminBefore {
		t.Errorf("after Down, assets.admin description = %q, want %q", got, adminBefore)
	}
}
