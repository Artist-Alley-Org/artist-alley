// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #812 — what a FRESH INSTALL's field catalogue is, pinned.
//
// The absence of this test is what caused the issue. `aa seed --reset`
// named field_definition in its TRUNCATE list, so every dev, CI and
// demo instance deleted the shipped catalogue and installed the seed's
// studio catalogue instead. Nothing anywhere asserted what the shipped
// one WAS, so the divergence was invisible for the whole life of the
// project: production ran one field catalogue, every instance we tested
// ran a different one, and neither was a subset of the other.
//
// Both tests here run against a genuinely fresh database — created,
// migrated, dropped — rather than the shared test DB, because the
// property is "what the MIGRATIONS produce" and any other test in the
// suite is free to insert a field definition into a shared database.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the
// other integration tests in this package.

package db

import (
	"database/sql"
	"sort"
	"testing"
)

// migratedFieldCodes is the derived half: the codes a fresh migration
// run actually leaves in field_definition.
func migratedFieldCodes(t *testing.T, sqlDB *sql.DB) []string {
	t.Helper()
	rows, err := sqlDB.QueryContext(t.Context(),
		`SELECT code FROM field_definition ORDER BY code`)
	if err != nil {
		t.Fatalf("read field_definition: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

// TestShippedFieldCatalogue_MatchesMigrations is the derived
// enforcement for the hand-written registry in shippedfields.go. It
// fails in BOTH directions, which is the point: an unregistered new
// definition would be swept away by the next `aa seed --reset`, and a
// registered code with no row would make the reset's keep-list a lie.
func TestShippedFieldCatalogue_MatchesMigrations(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	inDB := map[string]bool{}
	for _, code := range migratedFieldCodes(t, sqlDB) {
		inDB[code] = true
	}
	registered := map[string]bool{}
	for _, f := range ShippedFields() {
		if f.Reason == "" {
			t.Errorf("shipped field %q has no Reason — every entry must say why it is "+
				"part of the product, so the next reader does not add one by resemblance", f.Code)
		}
		if registered[f.Code] {
			t.Errorf("shipped field %q is registered twice", f.Code)
		}
		registered[f.Code] = true
	}

	for code := range inDB {
		if !registered[code] {
			t.Errorf("migration-inserted field definition %q is not in shippedFields "+
				"(app/internal/db/shippedfields.go). Classify it: if it is part of the "+
				"product, register it with a reason — otherwise the next `aa seed --reset` "+
				"DELETES it, because Reset sweeps every field_definition row that is not on "+
				"that list. If it is NOT part of the product, it should not be in a migration.",
				code)
		}
	}
	for code := range registered {
		if !inDB[code] {
			t.Errorf("shippedFields registers %q but no migration inserts it. `aa seed "+
				"--reset` is preserving a code that does not exist, and any operator or "+
				"seed row that happens to use it would be preserved by accident.", code)
		}
	}
}

// TestShippedFieldCatalogue_FreshInstallContents pins the catalogue
// itself — the exact codes, and for each the properties a consumer
// depends on. A change to any of it is then a line in a diff with this
// test next to it, rather than something discovered months later on a
// production instance.
//
// The type pin is not decoration. `country` is typed `tree`, which
// #778 was diagnosed on the premise that no tree field had ever
// existed: true of SEEDED instances only, and false of every fresh
// install since v0.1. Changing a shipped field's type is a decision
// with its own migration, and this is where it announces itself.
func TestShippedFieldCatalogue_FreshInstallContents(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	// code -> type | subject_kind | display_group | extraction_source
	want := map[string]string{
		// Wired by 00025 (#813), each to a real metadata.CanonicalField.
		// A name that is not one routes NOTHING and says nothing about
		// it, so these four strings are load-bearing; the constants they
		// must equal are pinned in
		// asset/metadata.TestShippedWiring_NamesRealCanonicalFields.
		"capture_date": "datetime|asset|technical|capture_datetime",
		"copyright":    "text|asset|rights|xmp_rights",
		"credit":       "text|asset|rights|iptc_credit",
		"country":      "tree|asset|general|iptc_country",

		// Wired by 00028 (#830). `keywords` was deliberately unwired
		// through 00025 for a reason that has since been fixed: the
		// applier carried value_text / value_num / value_date and had
		// no path to value_options, so wiring it would have left the
		// field visibly empty while the logs claimed success. The
		// applier now writes a set, and the field is OPEN, so a keyword
		// the vocabulary does not have is created rather than dropped.
		"keywords": "multi_select|asset|core|iptc_keywords",

		// Still deliberately unwired: `title` mirrors assets.title, and
		// which of the two owns the concept is #822's open question,
		// not extraction's.
		"title":       "text|asset|core|",
		"description": "longtext|asset|core|",

		// Unwired by 00020: these two are COMPUTED by the preview
		// pipeline, never extracted (#765). A non-empty
		// extraction_source here would re-open that defect.
		"pixel_width":  "number|asset|technical|",
		"pixel_height": "number|asset|technical|",
	}

	rows, err := sqlDB.QueryContext(t.Context(),
		`SELECT code, type || '|' || subject_kind || '|' || display_group
		        || '|' || extraction_source
		   FROM field_definition ORDER BY code`)
	if err != nil {
		t.Fatalf("read field_definition: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var code, shape string
		if err := rows.Scan(&code, &shape); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[code] = shape
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	codes := make([]string, 0, len(want))
	for c := range want {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, code := range codes {
		switch actual, ok := got[code]; {
		case !ok:
			t.Errorf("shipped field %q is gone from a fresh install. Every operator loses "+
				"it on upgrade; if that is intended, change this pin in the same commit.", code)
		case actual != want[code]:
			t.Errorf("shipped field %q is now %q, want %q (type|subject_kind|display_group|"+
				"extraction_source)", code, actual, want[code])
		}
	}
	for code := range got {
		if _, ok := want[code]; !ok {
			t.Errorf("a fresh install now ships field definition %q, which this pin does "+
				"not know about. Add it here AND to shippedFields, or drop it from the "+
				"migration — a definition every operator gets is a product decision.", code)
		}
	}

	// Which fields a fresh install lets VALUES grow (#830). Pinned
	// separately from the shape above because it is a different kind of
	// decision: the shape says what a field is, this says how strict it
	// is about what may be written into it. Opening a curated
	// vocabulary — `country`, say — would let one misspelled file put a
	// nonsense term in every operator's catalogue, which is exactly the
	// thing #824 closed and is not something to arrive at by accident.
	openRows, err := sqlDB.QueryContext(t.Context(),
		`SELECT code FROM field_definition WHERE open_vocabulary ORDER BY code`)
	if err != nil {
		t.Fatalf("read open_vocabulary: %v", err)
	}
	defer openRows.Close()
	var open []string
	for openRows.Next() {
		var code string
		if err := openRows.Scan(&code); err != nil {
			t.Fatalf("scan: %v", err)
		}
		open = append(open, code)
	}
	if err := openRows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(open) != 1 || open[0] != "keywords" {
		t.Errorf("fields shipping with an open vocabulary = %v, want [keywords]", open)
	}
}

// The six fixtures 00023 removed, named individually so a re-fold of the
// baseline that reintroduces them is caught by name rather than by a
// count. They are federation-guard and metadata-type-validation test
// rows (created_by_user_ref = 420000) that the v0.1 baseline fold
// captured, and they shipped to every operator as "Text Field",
// "Score", "Due", "Tags" and two copies of "Fed Guard".
func TestShippedFieldCatalogue_FoldedFixturesAreGone(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	for _, code := range []string{
		"mcoltest_fedguard", "ctest_fedguard",
		"mtv_text", "mtv_due", "mtv_score", "mtv_tags",
	} {
		var exists bool
		if err := sqlDB.QueryRowContext(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM field_definition WHERE code = $1)`, code).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", code, err)
		}
		if exists {
			t.Errorf("test fixture field definition %q is back in a fresh install "+
				"(migration 00023 removed it, #812). If the baseline was re-folded from a "+
				"dump, scrub field_definition the same way system_config is scrubbed.", code)
		}
	}

	// No install has this ref; its presence is the fingerprint of a
	// re-fold, whatever the code is called.
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM field_definition WHERE created_by_user_ref IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count attributed definitions: %v", err)
	}
	if n != 0 {
		t.Errorf("%d shipped field definition(s) are attributed to a user ref. A definition "+
			"a migration inserts has no author; an attributed one came out of somebody's "+
			"database.", n)
	}
}
