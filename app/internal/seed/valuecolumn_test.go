// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import "testing"

// ---------------------------------------------------------------------------
// The seeder's half of the storage-column pin (#778)
// ---------------------------------------------------------------------------
//
// fieldValueParams is a FOURTH writer of asset_field_value, living in
// a different package from the three in internal/metadata and so
// outside the pin there. It is the one that matters most in practice:
// a dataset regeneration writes through it in bulk, so a column
// disagreement here mislocates every seeded value at once rather than
// one at a time.
//
// The expectations below must stay identical to valueColumnFor in
// app/internal/metadata/valuecolumn_test.go. They are duplicated
// rather than shared because importing a test-only map across packages
// would mean exporting it from production code — and a pin that
// changes shape to be shareable is a pin that can be quietly widened.
// If you change one table, change the other; the E2E test in
// internal/http asserts the seeder and the API agree in the database
// itself, which is the check that fails if these two ever drift apart.

func TestSeederUsesPinnedColumns(t *testing.T) {
	// One representative raw value per type, in whatever JSON shape a
	// dataset MANIFEST would carry.
	cases := []struct {
		fieldType string
		raw       any
		want      string
	}{
		{"text", "hello", "value_text"},
		{"longtext", "hello", "value_text"},
		{"rich_text", "hello", "value_text"},
		{"select", "srgb", "value_text"},
		// A tree value is ONE option slug — the node — not a
		// "europe/uk/london" path and not an array of slugs along the
		// path. See the 2026-07-31 tree-storage amendment to ADR 0012.
		{"tree", "london", "value_text"},
		{"number", float64(3), "value_num"},
		{"boolean", true, "value_num"},
		// RFC3339 even for `date`: parseTime accepts nothing else, so a
		// bare "2026-07-31" in a MANIFEST is silently dropped rather
		// than stored. Out of scope for #778 (no `date` field has ever
		// been seeded either) but pinned here so the next person to
		// seed one finds out from a test instead of from missing data.
		{"date", "2026-07-31T00:00:00Z", "value_date"},
		{"datetime", "2026-07-31T12:00:00Z", "value_date"},
		{"multi_select", []any{"a", "b"}, "value_options"},
		{"reference", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "value_ref"},
	}

	for _, c := range cases {
		t.Run(c.fieldType, func(t *testing.T) {
			p, ok := fieldValueParams(c.fieldType, c.raw)
			if !ok {
				t.Fatalf("fieldValueParams(%q, %v) refused the value", c.fieldType, c.raw)
			}

			var got []string
			if p.ValueText != nil {
				got = append(got, "value_text")
			}
			if p.ValueNum != nil {
				got = append(got, "value_num")
			}
			if p.ValueDate.Valid {
				got = append(got, "value_date")
			}
			if p.ValueOptions != nil {
				got = append(got, "value_options")
			}
			if p.ValueRef.Valid {
				got = append(got, "value_ref")
			}

			if len(got) != 1 {
				t.Fatalf("field type %q populated %v — must populate exactly one column (%s)",
					c.fieldType, got, c.want)
			}
			if got[0] != c.want {
				t.Errorf("seeder stores %q in %s, want %s. "+
					"The seeder must agree with app/internal/metadata's writers or a "+
					"regenerated dataset lands its values in a column nothing reads (#778).",
					c.fieldType, got[0], c.want)
			}
		})
	}
}

// TestSeederTreeValueRejectsThePathShapes guards the two encodings a
// dataset author might reasonably reach for and that the old ADR text
// invited. Neither is what this column holds, and both would be stored
// verbatim as an unresolvable slug rather than failing loudly — so
// pin the intent while the dataset is still being written.
func TestSeederTreeValueTakesTheLeafSlug(t *testing.T) {
	p, ok := fieldValueParams("tree", "london")
	if !ok || p.ValueText == nil {
		t.Fatalf("tree seed of a leaf slug did not produce value_text: %+v", p)
	}
	if *p.ValueText != "london" {
		t.Errorf("want the slug stored verbatim, got %q", *p.ValueText)
	}
	if p.ValueOptions != nil {
		t.Errorf("tree seed populated value_options (%v) — that was the collection "+
			"path's old shape and must not come back through the seeder", p.ValueOptions)
	}
}
