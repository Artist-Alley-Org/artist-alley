// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit cases for the portable schema export (#1576) that the database
// cannot hold: its CHECK constraints refuse a bad type, subject kind or
// display_condition shape, so the HTTP test in app/internal/http cannot
// store them. The whitelist, ordering and every storable case are
// asserted there, through the production router.

package metadata

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// The generated DTOs must carry exactly the ADR 0083 members, so a
// property added to the OpenAPI schema without classification fails
// here as well as on the wire.
func TestPortableSchemaDTO_ExactMembers(t *testing.T) {
	tags := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			tag := rt.Field(i).Tag.Get("json")
			if strings.Contains(tag, "omitempty") {
				t.Errorf("%s.%s is omitempty; every portable member is always present", rt.Name(), rt.Field(i).Name)
			}
			out = append(out, strings.Split(tag, ",")[0])
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		v    any
		want []string
	}{
		{openapi.FieldSchemaExport{}, []string{"fields", "format"}},
		{openapi.FieldSchemaExportField{}, []string{
			"code", "description", "display_condition", "display_group", "display_order",
			"edit_tab", "label", "open_vocabulary", "options", "regexp_filter", "required",
			"searchable", "show_in_advanced_search", "show_on_card", "show_on_upload",
			"subject_kind", "type",
		}},
		{openapi.FieldSchemaExportOption{}, []string{"children", "label", "value"}},
	} {
		if got := tags(c.v); !slices.Equal(got, c.want) {
			t.Errorf("%T members %v, want %v", c.v, got, c.want)
		}
	}
}

func validRow(code string) FieldDefinition {
	return FieldDefinition{Code: code, Label: code, Type: "text", SubjectKind: "asset", Status: "active",
		DisplayGroup: "general", Options: []byte(`{}`)}
}

func TestPortableField_RefusesUnstorableShapes(t *testing.T) {
	for name, mutate := range map[string]func(*FieldDefinition){
		"unknown type":                 func(r *FieldDefinition) { r.Type = "colour" },
		"unknown subject kind":         func(r *FieldDefinition) { r.SubjectKind = "team" },
		"condition is not an array":    func(r *FieldDefinition) { r.DisplayCondition = []byte(`{"a":1}`) },
		"condition holds a non-string": func(r *FieldDefinition) { r.DisplayCondition = []byte(`["a=b", 3]`) },
		"condition holds a blank term": func(r *FieldDefinition) { r.DisplayCondition = []byte(`["a=b", "  "]`) },
		"options is not an object":     func(r *FieldDefinition) { r.Type = "select"; r.Options = []byte(`"srgb"`) },
		"option status unknown": func(r *FieldDefinition) {
			r.Type = "select"
			r.Options = []byte(`{"values":[{"value":"a","status":"hidden"}]}`)
		},
		"replaced_by points nowhere": func(r *FieldDefinition) {
			r.Type = "select"
			r.Options = []byte(`{"values":[{"value":"a","status":"archived","replaced_by":"z"}]}`)
		},
		"blank slug": func(r *FieldDefinition) {
			r.Type = "multi_select"
			r.Options = []byte(`{"values":[" "]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := validRow("f")
			mutate(&r)
			if _, err := portableField(r); !errors.Is(err, errMalformedDefinition) {
				t.Fatalf("err %v, want errMalformedDefinition", err)
			}
			_, refusal, err := buildFieldSchemaExport([]string{"f"}, []FieldDefinition{r})
			if err != nil || refusal == nil || refusal.Reason != openapi.MalformedDefinition ||
				!slices.Equal(refusal.Codes, []string{"f"}) {
				t.Fatalf("refusal %+v err %v, want malformed_definition [f]", refusal, err)
			}
		})
	}
}

func TestNormalizeExportCodes(t *testing.T) {
	got, msg := normalizeExportCodes([]string{"b", "a", "b", "c_1"})
	if msg != "" || !slices.Equal(got, []string{"a", "b", "c_1"}) {
		t.Fatalf("got %v %q", got, msg)
	}
	many := make([]string, FieldSchemaExportMaxCodes+1)
	for i := range many {
		many[i] = "a"
	}
	for name, in := range map[string][]string{
		"nil": nil, "empty": {}, "over the bound": many,
		"upper case": {"A"}, "hyphen": {"a-b"}, "blank": {""}, "padded": {"a "},
	} {
		if _, msg := normalizeExportCodes(in); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
