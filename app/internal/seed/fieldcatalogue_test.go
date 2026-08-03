// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// ---------------------------------------------------------------------------
// The shipped field catalogue (#808)
// ---------------------------------------------------------------------------
//
// seed/profiles/dataset.field_definitions.json is hand-edited and is
// the ONLY input to applyFields. Every check applyFields performs at
// run time is performed here against the file on disk instead, so a
// bad edit fails the unit suite rather than a seed nobody runs until
// the demo box redeploys.
//
// This needs no database and no NAS mount: the catalogue is in-repo.

const fieldCataloguePath = "../../../seed/profiles/dataset.field_definitions.json"

func loadFieldCatalogue(t *testing.T) []catField {
	t.Helper()
	b, err := os.ReadFile(fieldCataloguePath)
	if err != nil {
		t.Fatalf("read %s: %v", fieldCataloguePath, err)
	}
	var fields []catField
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("parse %s: %v", fieldCataloguePath, err)
	}
	if len(fields) == 0 {
		t.Fatal("field catalogue is empty")
	}
	return fields
}

// The catalogue must survive the same validator the admin API runs.
// It is one function, not a seed-flavoured copy: tree-wide slug
// uniqueness is what makes a stored leaf slug a complete address (ADR
// 0012's tree amendment), so a duplicate introduced by copy-pasting a
// branch would resolve values to the wrong node with no error anywhere.
func TestFieldCatalogue_OptionsPassAPIValidation(t *testing.T) {
	for _, f := range loadFieldCatalogue(t) {
		if len(f.Options) == 0 {
			continue
		}
		doc, err := json.Marshal(map[string]any{"values": f.Options})
		if err != nil {
			t.Fatalf("%s: marshal options: %v", f.Name, err)
		}
		if _, err := metadata.NormalizeOptionsDoc(doc); err != nil {
			t.Errorf("%s: options rejected by the API validator: %v", f.Name, err)
		}
	}
}

// Swapping catField.Options from []string to []metadata.FieldOption
// must not rewrite the fourteen definitions that predate it. A bare
// slug unmarshals to FieldOption{Value: s}, bare() reports true, and
// MarshalJSON emits the plain string again — assert that rather than
// trusting the reasoning, because the failure mode is a silent rewrite
// of every option document in the database on the next seed.
func TestFieldCatalogue_BareSlugsRoundTripUnchanged(t *testing.T) {
	b, err := os.ReadFile(fieldCataloguePath)
	if err != nil {
		t.Fatalf("read catalogue: %v", err)
	}
	// Decode to the raw JSON per field so the comparison is against
	// what the FILE says, not against a second copy of our own model.
	var raw []struct {
		Name    string          `json:"name"`
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parse catalogue: %v", err)
	}
	for _, r := range raw {
		if len(r.Options) == 0 {
			continue
		}
		var bare []string
		if err := json.Unmarshal(r.Options, &bare); err != nil {
			continue // an object-form entry (tree); covered below
		}
		var typed []metadata.FieldOption
		if err := json.Unmarshal(r.Options, &typed); err != nil {
			t.Fatalf("%s: typed decode of a bare-slug list failed: %v", r.Name, err)
		}
		out, err := json.Marshal(typed)
		if err != nil {
			t.Fatalf("%s: re-encode: %v", r.Name, err)
		}
		want, err := json.Marshal(bare)
		if err != nil {
			t.Fatalf("%s: encode reference: %v", r.Name, err)
		}
		if string(out) != string(want) {
			t.Errorf("%s: bare slugs did not round-trip\n got %s\nwant %s", r.Name, out, want)
		}
	}
}

// The six codes both dataset manifests carry values for. Absent a
// definition, applyAssetFieldValues drops every one of them as an
// unknown code — 4,146 values on site_a, silently until #807.
func TestFieldCatalogue_CoversManifestFieldCodes(t *testing.T) {
	want := map[string]string{
		"production_notes": "longtext",
		"usage_rights":     "rich_text",
		// NOT capture_date, which the baseline migration already ships
		// as a `datetime`. SeedInsertField is ON CONFLICT (code) DO
		// NOTHING and applyFields recovers by fetching the existing
		// row, so a capture_date entry here would have bound to the
		// baseline's datetime row while being tracked as `date` —
		// leaving no row of type `date` anywhere while the count still
		// looked right.
		"license_expires": "date",
		"ingested_at":     "datetime",
		"production_area": "tree",
		"derived_from":    "reference",
	}
	got := map[string]string{}
	for _, f := range loadFieldCatalogue(t) {
		got[f.Name] = f.Type
	}
	for code, typ := range want {
		switch actual, ok := got[code]; {
		case !ok:
			t.Errorf("field %q is missing from the catalogue; every manifest value for it is dropped", code)
		case actual != typ:
			t.Errorf("field %q is typed %q, want %q", code, actual, typ)
		}
	}
}

// The tree's slugs are the seeded values. They are written into both
// MANIFEST.json files on the NAS, which no test can reach, so renaming
// one here silently empties the field for every asset that used it.
// Pinning the set makes that a deliberate edit in two places.
func TestFieldCatalogue_ProductionAreaSlugs(t *testing.T) {
	var tree *catField
	for _, f := range loadFieldCatalogue(t) {
		if f.Name == "production_area" {
			cp := f
			tree = &cp
			break
		}
	}
	if tree == nil {
		t.Fatal("production_area is not in the catalogue")
	}

	// slug -> depth, root = 1. The depth pin is what proves the branch
	// is genuinely three levels deep: a flattened rewrite would still
	// carry every slug.
	want := map[string]int{
		"art": 1, "environment": 2, "environment-architecture": 3,
		"environment-props": 3, "environment-terrain": 3,
		"character": 2, "character-models": 3, "character-textures": 3,
		"ui": 2, "ui-icons": 3, "ui-type": 3,
		"audio": 1, "audio-music": 2, "audio-sfx": 2, "audio-voice": 2,
		"film": 1, "film-footage": 2, "film-documentary": 2,
		"publication": 1, "publication-comics": 2,
	}

	got := map[string]int{}
	var walk func(opts []metadata.FieldOption, depth int)
	walk = func(opts []metadata.FieldOption, depth int) {
		for _, o := range opts {
			got[o.Value] = depth
			if o.Label == "" {
				t.Errorf("option %q has no label; the UI would render the raw slug", o.Value)
			}
			walk(o.Children, depth+1)
		}
	}
	walk(tree.Options, 1)

	for slug, depth := range want {
		if d, ok := got[slug]; !ok {
			t.Errorf("slug %q is gone; every manifest value using it would vanish", slug)
		} else if d != depth {
			t.Errorf("slug %q is at depth %d, want %d", slug, d, depth)
		}
	}
	for slug := range got {
		if _, ok := want[slug]; !ok {
			t.Errorf("unexpected slug %q — add it to the manifests, or to this pin", slug)
		}
	}
}
