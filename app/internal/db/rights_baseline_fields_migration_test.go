// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Migration 00097 (#1577 S1): the two seeded rights definitions.
//
//   - On a clean install both rows appear with their fixed ids, codes and
//     receiver-local defaults.
//   - An operator's existing row with either code, or a row already holding
//     either id, is left exactly as it was, and no seeded row is created in
//     its place.
//   - Down removes a seeded row only while nothing depends on it, and an
//     operator's colliding rows are never touched; Up again is harmless.

package db

import (
	"database/sql"
	"testing"
)

const (
	rightsBeforeVersion = 96
	rightsAtVersion     = 97
	seedRightsID        = "56d71791-189d-429c-b7e4-e837baf91ddb"
	seedStatementID     = "6c3e25be-83ed-4762-b636-a1e37cdbb74c"
)

func defRow(t *testing.T, sqlDB *sql.DB, where string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := sqlDB.QueryRowContext(t.Context(), `SELECT to_jsonb(d)::text FROM field_definition d WHERE `+where, args...).Scan(&s); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return s.String
}

func TestMigration00097_RightsBaselineFields_CleanInstall(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, rightsAtVersion); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, code string }{{seedRightsID, "rights_uri"}, {seedStatementID, "required_statement"}} {
		var typ, group, status, subject string
		var readCap, writeCap, mirrors sql.NullString
		var searchable, card, upload, advanced bool
		var applies int
		if err := sqlDB.QueryRowContext(ctx, `SELECT type, display_group, status, subject_kind, read_capability, write_capability,
			    mirrors_column, searchable, show_on_card, show_on_upload, show_in_advanced_search, cardinality(applies_to)
			  FROM field_definition WHERE id = $1 AND code = $2`, c.id, c.code).
			Scan(&typ, &group, &status, &subject, &readCap, &writeCap, &mirrors, &searchable, &card, &upload, &advanced, &applies); err != nil {
			t.Fatalf("%s: %v", c.code, err)
		}
		if typ != "text" || group != "rights" || status != "active" || subject != "asset" || readCap.Valid || writeCap.Valid ||
			mirrors.Valid || !searchable || card || !upload || !advanced || applies != 0 {
			t.Fatalf("%s seeded as %s %s %s %s %v %v %v %v %v %v %v %d", c.code, typ, group, status, subject, readCap, writeCap,
				mirrors, searchable, card, upload, advanced, applies)
		}
	}

	// Down keeps a seeded row something depends on, removes the other.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO field_definition (code, label, type, status, deprecated_replacement_id)
		VALUES ('old_statement', 'Old', 'text', 'deprecated', $1)`, seedStatementID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, rightsBeforeVersion); err != nil {
		t.Fatal(err)
	}
	if defRow(t, sqlDB, `id = $1`, seedRightsID) != "" {
		t.Fatal("Down kept the unused seeded rights_uri")
	}
	if defRow(t, sqlDB, `id = $1`, seedStatementID) == "" {
		t.Fatal("Down removed a seeded row another definition depends on")
	}
	// Up again re-seeds the removed one and changes nothing else.
	kept := defRow(t, sqlDB, `id = $1`, seedStatementID)
	if _, err := p.UpTo(ctx, rightsAtVersion); err != nil {
		t.Fatal(err)
	}
	if defRow(t, sqlDB, `id = $1 AND code = 'rights_uri'`, seedRightsID) == "" || defRow(t, sqlDB, `id = $1`, seedStatementID) != kept {
		t.Fatal("a second Up did not restore exactly the missing row")
	}
}

func TestMigration00097_RightsBaselineFields_OperatorRowsWin(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, rightsBeforeVersion); err != nil {
		t.Fatal(err)
	}
	// An operator already has a rights_uri of their own (gated, longtext),
	// and some row already holds the statement's seeded id under another code.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO field_definition (code, label, type, read_capability, display_group)
		VALUES ('rights_uri', 'Our licence', 'longtext', 'fields.admin', 'legal')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO field_definition (id, code, label, type)
		VALUES ($1, 'operator_note', 'Note', 'text')`, seedStatementID); err != nil {
		t.Fatal(err)
	}
	ownRights := defRow(t, sqlDB, `code = 'rights_uri'`)
	ownNote := defRow(t, sqlDB, `id = $1`, seedStatementID)

	if _, err := p.UpTo(ctx, rightsAtVersion); err != nil {
		t.Fatalf("Up over operator rows: %v", err)
	}
	if defRow(t, sqlDB, `code = 'rights_uri'`) != ownRights || defRow(t, sqlDB, `id = $1`, seedStatementID) != ownNote {
		t.Fatal("Up changed an operator's row")
	}
	if defRow(t, sqlDB, `id = $1`, seedRightsID) != "" {
		t.Fatal("Up created a seeded rights_uri beside the operator's")
	}
	if defRow(t, sqlDB, `code = 'required_statement'`) != "" {
		t.Fatal("Up created a seeded required_statement under another id")
	}
	if _, err := p.DownTo(ctx, rightsBeforeVersion); err != nil {
		t.Fatal(err)
	}
	if defRow(t, sqlDB, `code = 'rights_uri'`) != ownRights || defRow(t, sqlDB, `id = $1`, seedStatementID) != ownNote {
		t.Fatal("Down touched an operator's row")
	}
}

// A seeded row an operator configured after 00097, with no value, history
// or reference, is still the operator's state: Down keeps it exactly, and a
// later Up leaves it exactly as configured. The other, untouched seed in
// the same database is removed and restored as before.
func TestMigration00097_RightsBaselineFields_CustomizedSeedSurvivesDown(t *testing.T) {
	cases := []struct {
		name, change string
	}{
		{"relabelled and redescribed", `UPDATE field_definition SET label = 'Licence', description = 'Our own wording' WHERE id = $1`},
		{"gated by a read capability", `UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`},
		{"given a default and an edit tab", `UPDATE field_definition SET default_value = '"https://creativecommons.org/licenses/by/4.0/"'::jsonb, edit_tab = 'Legal' WHERE id = $1`},
		{"retired", `UPDATE field_definition SET status = 'archived' WHERE id = $1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := freshDatabase(t)
			sqlDB := openCfg(t, cfg)
			ctx := t.Context()
			p := afvhProvider(t, sqlDB)
			if _, err := p.UpTo(ctx, rightsAtVersion); err != nil {
				t.Fatal(err)
			}
			if _, err := sqlDB.ExecContext(ctx, c.change, seedRightsID); err != nil {
				t.Fatalf("customize the seed: %v", err)
			}
			customized := defRow(t, sqlDB, `id = $1`, seedRightsID)
			if customized == "" {
				t.Fatal("the seeded rights_uri is missing after Up")
			}
			if _, err := p.DownTo(ctx, rightsBeforeVersion); err != nil {
				t.Fatal(err)
			}
			if got := defRow(t, sqlDB, `id = $1`, seedRightsID); got != customized {
				t.Fatalf("Down did not keep the operator's configured row:\n got %s\nwant %s", got, customized)
			}
			if defRow(t, sqlDB, `id = $1`, seedStatementID) != "" {
				t.Fatal("Down kept the untouched, unused required_statement")
			}
			if _, err := p.UpTo(ctx, rightsAtVersion); err != nil {
				t.Fatal(err)
			}
			if got := defRow(t, sqlDB, `id = $1`, seedRightsID); got != customized {
				t.Fatalf("Up changed the operator's configured row:\n got %s\nwant %s", got, customized)
			}
			if defRow(t, sqlDB, `id = $1 AND code = 'required_statement'`, seedStatementID) == "" {
				t.Fatal("Up did not restore the untouched required_statement")
			}
		})
	}
}
