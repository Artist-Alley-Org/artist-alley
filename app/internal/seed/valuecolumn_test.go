// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

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
	// dataset MANIFEST would carry, plus the exact stored form.
	//
	// The stored form is pinned, not just the column (#791). The
	// seeder is the writer that TRANSLATES — a MANIFEST carries JSON
	// `true`, and what reaches the column is 1 — so "which column" is
	// the smaller half of what can go wrong here. A seeder that agreed
	// on value_num and wrote -1, or agreed on value_text and wrote
	// "1", would still produce a dataset nothing can read.
	cases := []struct {
		fieldType string
		raw       any
		want      string
		wantValue string
	}{
		{"text", "hello", "value_text", `"hello"`},
		{"longtext", "hello", "value_text", `"hello"`},
		{"rich_text", "hello", "value_text", `"hello"`},
		{"select", "srgb", "value_text", `"srgb"`},
		// A tree value is ONE option slug — the node — not a
		// "europe/uk/london" path and not an array of slugs along the
		// path. See the 2026-07-31 tree-storage amendment to ADR 0012.
		{"tree", "london", "value_text", `"london"`},
		{"number", float64(3), "value_num", "3"},
		// JSON true/false becomes 1/0 in value_num — ADR 0012's
		// encoding, and (since #791) every other writer's. The
		// collection writer and the display read "true"/"false" out of
		// value_text until then, so a seeded boolean was invisible.
		{"boolean", true, "value_num", "1"},
		{"boolean", false, "value_num", "0"},
		// RFC3339 even for `date`: parseTime accepts nothing else, so a
		// bare "2026-07-31" in a MANIFEST is silently dropped rather
		// than stored. Out of scope for #778 (no `date` field has ever
		// been seeded either) but pinned here so the next person to
		// seed one finds out from a test instead of from missing data.
		{"date", "2026-07-31T00:00:00Z", "value_date", "2026-07-31T00:00:00Z"},
		{"datetime", "2026-07-31T12:00:00Z", "value_date", "2026-07-31T12:00:00Z"},
		{"multi_select", []any{"a", "b"}, "value_options", `["a" "b"]`},
		{"reference", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "value_ref",
			"6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
	}

	for _, c := range cases {
		t.Run(c.fieldType+"/"+c.wantValue, func(t *testing.T) {
			p, ok := fieldValueParams(c.fieldType, c.raw)
			if !ok {
				t.Fatalf("fieldValueParams(%q, %v) refused the value", c.fieldType, c.raw)
			}

			var got, gotValue []string
			if p.ValueText != nil {
				got = append(got, "value_text")
				gotValue = append(gotValue, strconv.Quote(*p.ValueText))
			}
			if p.ValueNum != nil {
				got = append(got, "value_num")
				gotValue = append(gotValue, strconv.FormatFloat(*p.ValueNum, 'g', -1, 64))
			}
			if p.ValueDate.Valid {
				got = append(got, "value_date")
				gotValue = append(gotValue, p.ValueDate.Time.UTC().Format(time.RFC3339))
			}
			if p.ValueOptions != nil {
				got = append(got, "value_options")
				gotValue = append(gotValue, fmt.Sprintf("%q", p.ValueOptions))
			}
			if p.ValueRef.Valid {
				got = append(got, "value_ref")
				gotValue = append(gotValue, uuid.UUID(p.ValueRef.Bytes).String())
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
			if gotValue[0] != c.wantValue {
				t.Errorf("seeder encodes %q(%v) as %s, want %s. "+
					"Same column, different representation — still a value the display "+
					"cannot read (#791).", c.fieldType, c.raw, gotValue[0], c.wantValue)
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
