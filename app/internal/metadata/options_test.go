// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOptionsDocRoundTripsPreLifecycleDocuments is the highest-value
// test here: every options-carrying field in a live instance today
// holds the bare-string form written by the seeder, with no status
// key anywhere. Normalising must not disturb them.
func TestOptionsDocRoundTripsPreLifecycleDocuments(t *testing.T) {
	// Verbatim from field_definition.options on a seeded instance.
	live := []string{
		`{"values":["sRGB","Linear","Raw","N/A"]}`,
		`{"values":["Unreal 5","Unity 2022","Godot 4","All","N/A"]}`,
		`{"values":["Greybox","Pass 1","Polish","Final","Locked"]}`,
		`{"values":["PC","Console","Mobile","All"]}`,
		`{"values":["256x256","512x512","1024x1024","2048x2048","4096x4096"]}`,
		// The object form ADR 0012 documents, also without status.
		`{"values":[{"value":"low","label":"Low"},{"value":"high","label":"High"}]}`,
		// Non-vocabulary documents must pass through untouched.
		`{}`,
		`{"min":0,"max":10}`,
	}
	for _, in := range live {
		out, err := normalizeOptionsDoc([]byte(in))
		if err != nil {
			t.Fatalf("normalize(%s): unexpected error %v", in, err)
		}
		if !sameJSON(t, in, string(out)) {
			t.Errorf("round-trip changed the document\n in: %s\nout: %s", in, out)
		}
		// No status key may be invented for an entry that had none —
		// that is what keeps the five live fields valid.
		if strings.Contains(string(out), `"status"`) {
			t.Errorf("normalize invented a status key: %s", out)
		}
	}
}

// TestOptionEntryAcceptsBothShapes pins the decode side: a bare slug
// string and the full object must both land in the same model.
func TestOptionEntryAcceptsBothShapes(t *testing.T) {
	var bare FieldOption
	if err := json.Unmarshal([]byte(`"sRGB"`), &bare); err != nil {
		t.Fatalf("bare slug: %v", err)
	}
	if bare.Value != "sRGB" || bare.Label != "" || bare.Status != "" {
		t.Errorf("bare slug decoded as %+v", bare)
	}

	var obj FieldOption
	if err := json.Unmarshal(
		[]byte(`{"value":"srgb","label":"sRGB","status":"deprecated","replaced_by":"linear"}`),
		&obj); err != nil {
		t.Fatalf("object: %v", err)
	}
	if obj.Value != "srgb" || obj.Label != "sRGB" ||
		obj.Status != OptionDeprecated || obj.ReplacedBy != "linear" {
		t.Errorf("object decoded as %+v", obj)
	}

	var boom FieldOption
	if err := json.Unmarshal([]byte(`42`), &boom); err == nil {
		t.Error("a number should not decode as an option")
	}
}

// TestOptionEncodesNarrowestForm covers the property that makes the
// round-trip above possible: only entries that carry information
// beyond their slug grow into objects.
func TestOptionEncodesNarrowestForm(t *testing.T) {
	cases := []struct {
		name string
		in   FieldOption
		want string
	}{
		{"slug only", FieldOption{Value: "PC"}, `"PC"`},
		{"label equals slug", FieldOption{Value: "PC", Label: "PC"}, `"PC"`},
		{"explicit active is noise", FieldOption{Value: "PC", Status: OptionActive}, `"PC"`},
		{"real label", FieldOption{Value: "pc", Label: "PC"}, `{"value":"pc","label":"PC"}`},
		{
			"deprecated with successor",
			FieldOption{Value: "PC", Status: OptionDeprecated, ReplacedBy: "Console"},
			`{"value":"PC","status":"deprecated","replaced_by":"Console"}`,
		},
		{"archived", FieldOption{Value: "PC", Status: OptionArchived}, `{"value":"PC","status":"archived"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !sameJSON(t, tc.want, string(got)) {
				t.Errorf("got %s want %s", got, tc.want)
			}
		})
	}
}

// TestOptionsDocRejectsBadDocuments covers the validation the brief
// calls out: an unknown status, and a replaced_by that names nothing.
// A dangling replaced_by is the same orphan class ADR 0012 rejects
// hard deletion for.
func TestOptionsDocRejectsBadDocuments(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{
			"unknown status",
			`{"values":[{"value":"a","status":"retired"}]}`,
			"unknown status",
		},
		{
			"replaced_by names a slug that does not exist",
			`{"values":[{"value":"a","status":"deprecated","replaced_by":"ghost"}]}`,
			"not an option of this field",
		},
		{
			"replaced_by points at itself",
			`{"values":[{"value":"a","status":"deprecated","replaced_by":"a"}]}`,
			"cannot point at itself",
		},
		{
			"empty slug",
			`{"values":[{"value":"  "}]}`,
			"must not be empty",
		},
		{
			"duplicate slug",
			`{"values":["a","a"]}`,
			"duplicate option value",
		},
		{
			"values is not an array",
			`{"values":{"a":1}}`,
			"must be an array",
		},
		{
			"nested replaced_by dangles too",
			`{"values":[{"value":"a","children":[{"value":"b","replaced_by":"ghost"}]}]}`,
			"not an option of this field",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeOptionsDoc([]byte(tc.doc))
			if err == nil {
				t.Fatalf("expected rejection of %s", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestOptionsDocAcceptsCrossReferences: replaced_by may name a
// sibling, a parent or a child — anything in the same field.
func TestOptionsDocAcceptsCrossReferences(t *testing.T) {
	docs := []string{
		`{"values":[{"value":"a","status":"deprecated","replaced_by":"b"},"b"]}`,
		`{"values":["b",{"value":"a","status":"deprecated","replaced_by":"b"}]}`,
		`{"values":[{"value":"a","children":["b"]},{"value":"c","status":"deprecated","replaced_by":"b"}]}`,
	}
	for _, d := range docs {
		if _, err := normalizeOptionsDoc([]byte(d)); err != nil {
			t.Errorf("normalize(%s): %v", d, err)
		}
	}
}

// TestOptionsDocPreservesSiblingKeys — a vocabulary edit must not drop
// whatever else the type keeps in options.
func TestOptionsDocPreservesSiblingKeys(t *testing.T) {
	out, err := normalizeOptionsDoc([]byte(`{"values":["a"],"allow_custom":true}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["allow_custom"] != true {
		t.Errorf("sibling key dropped: %s", out)
	}
}

// TestResolveOptionSlugsHandlesTheBareStringForm is the acceptance
// case that matters most: every options-carrying field on a live
// instance today stores bare slug strings, which carry no label at
// all. Resolution must hand back the slug as the label rather than an
// empty string, or the fix for #775 blanks the whole catalogue it was
// meant to make readable.
func TestResolveOptionSlugsHandlesTheBareStringForm(t *testing.T) {
	doc := []byte(`{"values":["sRGB","Linear","Raw","N/A"]}`)

	got := resolveOptionSlugs(doc, []string{"sRGB", "Raw"})
	if len(got) != 2 {
		t.Fatalf("want 2 resolved, got %d (%v)", len(got), got)
	}
	for _, slug := range []string{"sRGB", "Raw"} {
		o, ok := got[slug]
		if !ok {
			t.Fatalf("slug %q did not resolve", slug)
		}
		if o.Label != slug {
			t.Errorf("slug %q: want label %q (the slug itself), got %q", slug, slug, o.Label)
		}
		if o.Status != OptionActive {
			t.Errorf("slug %q: absent status must mean active, got %q", slug, o.Status)
		}
	}
}

// TestResolveOptionSlugsReadsLabelsAndLifecycle covers the object
// form, which is what an operator's edits in /admin/fields produce.
func TestResolveOptionSlugsReadsLabelsAndLifecycle(t *testing.T) {
	doc := []byte(`{"values":[
		{"value":"srgb","label":"sRGB"},
		{"value":"raw","label":"Raw","status":"deprecated","replaced_by":"srgb"},
		{"value":"gone","label":"Gone","status":"archived"}
	]}`)

	got := resolveOptionSlugs(doc, []string{"srgb", "raw", "gone"})
	if len(got) != 3 {
		t.Fatalf("want 3 resolved, got %d (%v)", len(got), got)
	}
	if got["srgb"].Label != "sRGB" || got["srgb"].Status != OptionActive {
		t.Errorf("srgb: got %+v", got["srgb"])
	}
	if got["raw"].Label != "Raw" || got["raw"].Status != OptionDeprecated {
		t.Errorf("raw: got %+v", got["raw"])
	}
	// Archived still resolves on a read path — the picker's job is to
	// stop offering it, not to blank a value already stored.
	if got["gone"].Label != "Gone" || got["gone"].Status != OptionArchived {
		t.Errorf("gone: got %+v", got["gone"])
	}
}

// TestResolveOptionSlugsOmitsWhatItCannotResolve pins the fallback
// contract: an absent entry means "render the raw slug", so an unknown
// term, a malformed document and a non-vocabulary document must all
// degrade to no entry rather than to an empty label.
func TestResolveOptionSlugsOmitsWhatItCannotResolve(t *testing.T) {
	cases := []struct{ name, doc, slug string }{
		{"unknown slug", `{"values":["sRGB"]}`, "mystery"},
		{"no values key", `{"min":0,"max":10}`, "sRGB"},
		{"empty document", `{}`, "sRGB"},
		{"null", `null`, "sRGB"},
		{"malformed values", `{"values":{"not":"an array"}}`, "sRGB"},
		{"not json at all", `nonsense`, "sRGB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveOptionSlugs([]byte(c.doc), []string{c.slug}); len(got) != 0 {
				t.Errorf("want nothing resolved, got %v", got)
			}
		})
	}
	if got := resolveOptionSlugs([]byte(`{"values":["sRGB"]}`), nil); got != nil {
		t.Errorf("no slugs must resolve to nil, got %v", got)
	}
}

// TestResolveValueOptionsOnlyResolvesVocabularyTypes keeps the map off
// values that hold no slug — a text field's value_text is prose, not a
// term, and must never be looked up in a vocabulary.
func TestResolveValueOptionsOnlyResolvesVocabularyTypes(t *testing.T) {
	doc := []byte(`{"values":["sRGB","Linear"]}`)
	text := "sRGB"

	if got := resolveValueOptions("select", &text, nil, doc); len(got) != 1 || got["sRGB"].Label != "sRGB" {
		t.Errorf("select: want sRGB resolved, got %v", got)
	}
	if got := resolveValueOptions("multi_select", nil, []string{"sRGB", "Linear"}, doc); len(got) != 2 {
		t.Errorf("multi_select: want 2 resolved, got %v", got)
	}
	// `tree` resolves too, out of value_text, exactly like select. This
	// list used to include it — a leftover from when nothing agreed on
	// where a tree value lived, so nothing resolved one.
	if got := resolveValueOptions("tree", &text, nil, doc); len(got) != 1 || got["sRGB"].Label != "sRGB" {
		t.Errorf("tree: want sRGB resolved out of value_text, got %v", got)
	}
	for _, typ := range []string{"text", "longtext", "rich_text", "number", "date", "datetime", "boolean", "reference"} {
		if got := resolveValueOptions(typ, &text, []string{"sRGB"}, doc); got != nil {
			t.Errorf("%s: want no resolution, got %v", typ, got)
		}
	}
	empty := ""
	if got := resolveValueOptions("select", &empty, nil, doc); got != nil {
		t.Errorf("empty select value: want nil, got %v", got)
	}
	if got := resolveValueOptions("select", nil, nil, doc); got != nil {
		t.Errorf("nil select value: want nil, got %v", got)
	}
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("bad json %s: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	x, _ := json.Marshal(av)
	y, _ := json.Marshal(bv)
	return string(x) == string(y)
}
