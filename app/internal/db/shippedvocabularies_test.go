// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #820 — a shipped field whose TYPE requires a vocabulary must ship
// with one.
//
// #812 pinned WHICH definitions a fresh install has and what type each
// is. It did not ask whether any of them could hold a value, and two
// could not: `keywords` (multi_select) and `country` (tree) shipped
// with options = '{}' from the v0.1 baseline onwards. A multi_select
// with no options has nothing to select; a tree with no vocabulary is
// a hierarchical field with no hierarchy. Both had shipped in that
// state to every install, and every consumer of those two fields was
// therefore running against a state production could reach but no
// fixture ever constructed — the shape that produced #778, #791 and
// #807/#808 in turn.
//
// These run against a genuinely fresh, migrated, dropped database for
// the same reason the #812 pins do: the property is "what the
// MIGRATIONS produce", and any other test in the suite is free to
// insert or edit a field definition in the shared database.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset.

package db

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// optionCarryingTypes are the field types whose stored value is a slug
// looked up in options.values. For these, and only these, an empty
// options document makes the field unusable rather than merely bare.
//
// Mirrors metadata/handler.go's validFieldType switch — the three types
// that route through resolveValueOptions. Kept as a literal because the
// question this test asks ("does an empty vocabulary make this field
// dead?") is a property of the TYPE SYSTEM, and hard-coding it here
// means adding a fourth such type is a visible edit rather than a
// silently-widened check.
var optionCarryingTypes = map[string]bool{
	"select":       true,
	"multi_select": true,
	"tree":         true,
}

// shippedFieldOptions reads code → (type, raw options) from a freshly
// migrated database.
func shippedFieldOptions(t *testing.T, sqlDB *sql.DB) map[string]struct {
	Type    string
	Options []byte
} {
	t.Helper()
	rows, err := sqlDB.QueryContext(t.Context(),
		`SELECT code, type, COALESCE(options, '{}'::jsonb)::text
		   FROM field_definition ORDER BY code`)
	if err != nil {
		t.Fatalf("read field_definition: %v", err)
	}
	defer rows.Close()

	out := map[string]struct {
		Type    string
		Options []byte
	}{}
	for rows.Next() {
		var code, typ, opts string
		if err := rows.Scan(&code, &typ, &opts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[code] = struct {
			Type    string
			Options []byte
		}{Type: typ, Options: []byte(opts)}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// countVocabulary counts the options in a document, at every depth.
func countVocabulary(t *testing.T, raw []byte) int {
	t.Helper()
	var doc struct {
		Values []metadata.FieldOption `json:"values"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("options is not a decodable document: %v (%s)", err, raw)
	}
	var n int
	var walk func([]metadata.FieldOption)
	walk = func(opts []metadata.FieldOption) {
		for _, o := range opts {
			n++
			walk(o.Children)
		}
	}
	walk(doc.Values)
	return n
}

// TestShippedFields_OptionCarryingTypesHaveVocabularies is the check
// whose absence let `country` ship unusable. It is deliberately stated
// over the TYPE rather than over the two known codes: a shipped
// `select` added in a future migration gets the same guarantee without
// anybody remembering to extend a list.
func TestShippedFields_OptionCarryingTypesHaveVocabularies(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	fields := shippedFieldOptions(t, sqlDB)
	codes := make([]string, 0, len(fields))
	for c := range fields {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	var checked int
	for _, code := range codes {
		f := fields[code]
		if !optionCarryingTypes[f.Type] {
			continue
		}
		checked++
		if n := countVocabulary(t, f.Options); n == 0 {
			t.Errorf("shipped field %q is typed %q, which resolves a stored value by "+
				"looking its slug up in options.values — and it ships with an EMPTY "+
				"vocabulary. There is nothing to select, so the field cannot hold a value "+
				"even in principle, and it is in that state on every install. Give it a "+
				"starting vocabulary in a migration (#820), or change its type.",
				code, f.Type)
		}
	}
	if checked == 0 {
		t.Error("no shipped field is of an option-carrying type — either the catalogue " +
			"lost `keywords`/`country`, or optionCarryingTypes no longer matches the " +
			"type names metadata uses. Both make this test pass while checking nothing.")
	}
}

// TestShippedFields_OptionsPassAPIValidator is the note in the #820
// brief made into a test. A migration writes jsonb DIRECTLY — it never
// goes through metadata.NormalizeOptionsDoc, which is the validator
// every admin edit and every seed catalogue entry does go through. So
// a migration is perfectly able to write a document the admin UI would
// refuse to save, and the operator only finds out when they open the
// field, change a label and get an error about a document they never
// wrote.
//
// Round-trip, not merely "accepted": Normalize re-encodes canonically,
// so a document it rewrites is one the next admin save would silently
// rewrite too. Comparison is semantic (decoded), because jsonb and
// encoding/json order object keys by their own rules and a byte
// comparison would fail on nothing.
func TestShippedFields_OptionsPassAPIValidator(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	fields := shippedFieldOptions(t, sqlDB)
	codes := make([]string, 0, len(fields))
	for c := range fields {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	for _, code := range codes {
		normalized, err := metadata.NormalizeOptionsDoc(fields[code].Options)
		if err != nil {
			t.Errorf("shipped field %q has an options document the admin write path "+
				"REJECTS: %v\ndocument: %s\nA migration writes jsonb directly and skips "+
				"this validator, so the operator meets the error on their first edit.",
				code, err, fields[code].Options)
			continue
		}
		var before, after any
		if err := json.Unmarshal(fields[code].Options, &before); err != nil {
			t.Fatalf("%s: decode stored options: %v", code, err)
		}
		if err := json.Unmarshal(normalized, &after); err != nil {
			t.Fatalf("%s: decode normalized options: %v", code, err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("shipped field %q's options are not canonical — the validator rewrites "+
				"them.\nstored:     %s\nnormalized: %s\nThe next admin save would apply that "+
				"rewrite to every install at once.", code, fields[code].Options, normalized)
		}
	}
}

// TestShippedFields_CountryVocabularyIsISO pins the country tree.
//
// The pin is not decoration and it is not "the labels are nice". ADR
// 0012's tree amendment stores a tree value as ONE LEAF SLUG, so the
// slug IS the value — it is what a federated peer receives and what
// the dataset MANIFESTs on the NAS (which no test can reach) are
// written against. Renaming a slug here empties the field for every
// asset that used it, on every install, with no error anywhere.
//
// ISO 3166-1 alpha-2 is why the slugs are portable at all: `gb` means
// the United Kingdom on a peer that never saw our catalogue, where a
// hand-rolled `united-kingdom` would mean whatever that peer happened
// to call it. The two-letter shape is also what makes tree-wide slug
// uniqueness free — no word-shaped branch slug can collide with one.
func TestShippedFields_CountryVocabularyIsISO(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	fields := shippedFieldOptions(t, sqlDB)
	country, ok := fields["country"]
	if !ok {
		t.Fatal("no `country` field definition on a fresh install")
	}
	if country.Type != "tree" {
		t.Fatalf("country is typed %q, not `tree` — if that was deliberate (#789 converts "+
			"it to a keyword list) this pin converts with it", country.Type)
	}

	var doc struct {
		Values []metadata.FieldOption `json:"values"`
	}
	if err := json.Unmarshal(country.Options, &doc); err != nil {
		t.Fatalf("decode country options: %v", err)
	}

	// slug -> depth, root = 1. The depth pin is what proves the tree is
	// genuinely nested: a flattened rewrite would still carry every slug.
	wantBranches := map[string]bool{
		"africa": true, "americas": true, "asia": true,
		"europe": true, "oceania": true,
	}
	wantLeaves := map[string]bool{
		"eg": true, "ke": true, "ma": true, "ng": true, "za": true,
		"ar": true, "br": true, "ca": true, "mx": true, "us": true,
		"cn": true, "in": true, "jp": true, "kr": true, "sg": true,
		"fr": true, "de": true, "it": true, "nl": true, "es": true,
		"se": true, "gb": true,
		"au": true, "nz": true,
	}

	gotDepth := map[string]int{}
	var walk func(opts []metadata.FieldOption, depth int)
	walk = func(opts []metadata.FieldOption, depth int) {
		for _, o := range opts {
			if _, dup := gotDepth[o.Value]; dup {
				t.Errorf("slug %q appears twice in the country tree. A stored value is a "+
					"bare leaf slug, so a duplicate makes it an ambiguous address and it "+
					"resolves to whichever node the walk hits first (ADR 0012).", o.Value)
			}
			gotDepth[o.Value] = depth
			if strings.TrimSpace(o.Label) == "" {
				t.Errorf("option %q has no label; the picker would offer the raw code", o.Value)
			}
			if o.Label == o.Value {
				t.Errorf("option %q's label is its own slug — the point of an ISO code as "+
					"the slug is that the LABEL stays human", o.Value)
			}
			if depth > 2 {
				t.Errorf("option %q is at depth %d. The tree is two levels — continent, "+
					"then country — because level 2 is the level ISO 3166-1 alpha-2 names, "+
					"and a value is only portable at a level a standard covers.", o.Value, depth)
			}
			walk(o.Children, depth+1)
		}
	}
	walk(doc.Values, 1)

	for slug := range wantBranches {
		switch d, ok := gotDepth[slug]; {
		case !ok:
			t.Errorf("continent branch %q is gone from the country tree", slug)
		case d != 1:
			t.Errorf("continent branch %q is at depth %d, want 1", slug, d)
		}
		if len(slug) == 2 {
			t.Errorf("branch slug %q is two characters, the shape of an ISO alpha-2 code. "+
				"Branch/leaf collision is what tree-wide slug uniqueness forbids, and the "+
				"whole reason the branches are words.", slug)
		}
	}
	for slug := range wantLeaves {
		switch d, ok := gotDepth[slug]; {
		case !ok:
			t.Errorf("country slug %q is gone. It is stored as-is in asset_field_value and "+
				"is written into the dataset MANIFESTs on the NAS, which no test can reach "+
				"— every asset using it renders blank.", slug)
		case d != 2:
			t.Errorf("country slug %q is at depth %d, want 2", slug, d)
		}
	}
	for slug, depth := range gotDepth {
		switch depth {
		case 1:
			if !wantBranches[slug] {
				t.Errorf("unexpected continent branch %q — add it to this pin deliberately", slug)
			}
		case 2:
			if !wantLeaves[slug] {
				t.Errorf("unexpected country slug %q — add it to this pin deliberately", slug)
			}
			if len(slug) != 2 || strings.ToLower(slug) != slug ||
				strings.Trim(slug, "abcdefghijklmnopqrstuvwxyz") != "" {
				t.Errorf("country slug %q is not a lowercase ISO 3166-1 alpha-2 code. The "+
					"slug is the whole federated identity of the value; a hand-invented one "+
					"means nothing on a peer.", slug)
			}
		}
	}
}

// TestShippedFields_KeywordsVocabularyIsFlat pins the other half. A
// multi_select has no hierarchy, so a nested entry here would be a
// vocabulary the picker cannot render and a slug no value could reach.
// The count is pinned loosely — this is explicitly a STARTING set an
// operator curates, so the test guards the shape and the floor, not
// the exact terms.
func TestShippedFields_KeywordsVocabularyIsFlat(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	if err := Migrate(t.Context(), cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	fields := shippedFieldOptions(t, sqlDB)
	kw, ok := fields["keywords"]
	if !ok {
		t.Fatal("no `keywords` field definition on a fresh install")
	}
	if kw.Type != "multi_select" {
		t.Fatalf("keywords is typed %q, not `multi_select`", kw.Type)
	}

	var doc struct {
		Values []metadata.FieldOption `json:"values"`
	}
	if err := json.Unmarshal(kw.Options, &doc); err != nil {
		t.Fatalf("decode keywords options: %v", err)
	}
	if len(doc.Values) < 5 {
		t.Errorf("keywords ships %d terms. A multi_select with a near-empty vocabulary is "+
			"the state #820 exists to end.", len(doc.Values))
	}
	seen := map[string]bool{}
	for _, o := range doc.Values {
		if len(o.Children) > 0 {
			t.Errorf("keyword %q has children. multi_select is flat — the picker has no "+
				"way to reach a nested term, so it would be a slug no value can hold.", o.Value)
		}
		if seen[o.Value] {
			t.Errorf("keyword slug %q appears twice", o.Value)
		}
		seen[o.Value] = true
		if strings.TrimSpace(o.Value) == "" {
			t.Error("a keyword option has an empty slug")
		}
		if strings.ToLower(o.Value) != o.Value {
			t.Errorf("keyword slug %q is not lowercase; the slug is the stored value and "+
				"the label is what displays", o.Value)
		}
		if strings.TrimSpace(o.Label) == "" {
			t.Errorf("keyword %q has no label", o.Value)
		}
	}
}
