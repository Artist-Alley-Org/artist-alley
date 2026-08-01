// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #738 — `field_definition.field_set_id` is removed, and stays removed.
//
// The column shipped in the baseline as "for bundling (export/import)"
// and never acquired a producer, a consumer, a foreign key, an index or
// a referent — there was never a `field_set` table for it to point at.
// ADR 0012's amendment (2026-07-31) records why it is dropped rather
// than completed, and what an actual schema-exchange envelope should
// carry if one is ever built.
//
// This test exists because the column's mere presence is what caused
// the mistake: #738 was filed to BUILD the missing table, on the
// strength of the dangling column reading as intent. Asserting its
// absence against a genuinely fresh migration run is a stronger
// guarantee than a comment asking nobody to re-add it — the same
// reasoning migration 00016 gives for `assets.has_image` (#579).
//
// It also pins the two columns that actually do the grouping the
// removed one is routinely mistaken for, so a future cleanup pass
// cannot quietly delete the real mechanism and leave this test green.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the
// other integration tests in this package.

package db

import (
	"testing"
)

func TestMigrate_FieldSetIDIsGone(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()

	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	has := func(table, column string) bool {
		t.Helper()
		var exists bool
		err := sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                WHERE table_schema='public'
			                  AND table_name=$1 AND column_name=$2)`,
			table, column).Scan(&exists)
		if err != nil {
			t.Fatalf("check column %s.%s: %v", table, column, err)
		}
		return exists
	}

	// Sanity: we are looking at a migrated field_definition, so a
	// false negative below means "column absent", not "table absent".
	if !tableExists(t, sqlDB, "field_definition") {
		t.Fatal("precondition failed: field_definition missing after Migrate")
	}

	if has("field_definition", "field_set_id") {
		t.Error("field_definition.field_set_id is back. It has no producer, no consumer " +
			"and no referent table; see ADR 0012's 2026-07-31 amendment before re-adding it. " +
			"If you are building schema exchange, the export unit is a list of field CODES " +
			"passed at export time, not a persisted set.")
	}

	if tableExists(t, sqlDB, "field_set") {
		t.Error("a `field_set` table exists. ADR 0012's amendment withdraws the persisted-set " +
			"design; a schema-exchange envelope needs no entity.")
	}

	// The mechanisms that actually group fields. If either of these
	// disappears, "field_set_id was redundant" stops being true and
	// this test's premise needs revisiting.
	for _, col := range []string{"display_group", "applies_to"} {
		if !has("field_definition", col) {
			t.Errorf("field_definition.%s is missing — it is the grouping mechanism "+
				"field_set_id was dropped in favour of (ADR 0012 amendment)", col)
		}
	}
}
