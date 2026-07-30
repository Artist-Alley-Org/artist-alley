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
