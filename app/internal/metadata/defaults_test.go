// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// ---------------------------------------------------------------------------
// Write-time validation (#793, ADR 0081 §3)
// ---------------------------------------------------------------------------
//
// The whole reason a default is declarative rather than an expression
// is that a declaration can be checked at the door. These tests are the
// door. Every case that reaches storage here is a case the apply path
// is then allowed to trust.

func textPtr(s string) *string  { return &s }
func numPtr(f float64) *float64 { return &f }

// A vocabulary with one term of each lifecycle, plus a nested one, so
// the tree case is exercised at depth rather than only at the top level.
var lifecycleOptions = []byte(`{"values":[
	"greybox",
	{"value":"polish","label":"Polish"},
	{"value":"retired","label":"Retired","status":"deprecated","replaced_by":"polish"},
	{"value":"mistake","label":"Mistake","status":"archived"},
	{"value":"europe","label":"Europe","children":[
		{"value":"london","label":"London"},
		{"value":"old-town","label":"Old Town","status":"archived"}
	]}
]}`)

// Acceptance item 4. A default naming a retired term is rejected on
// WRITE — not filtered on read, not warned about. Filtering on read
// would leave the operator with a default that silently does nothing;
// the point of the lifecycle is that a term you retired stops spreading,
// and a default is the fastest way to spread one.
func TestValidateFieldDefault_RejectsRetiredOptions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fieldType string
		def       FieldDefault
		wantIn    string
	}{
		{"select/deprecated", "select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("retired")}, "deprecated"},
		{"select/archived", "select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("mistake")}, "archived"},
		{"tree/archived-leaf", "tree",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("old-town")}, "archived"},
		{"multi_select/one-bad-term", "multi_select",
			FieldDefault{Kind: DefaultKindLiteral, ValueOptions: []string{"greybox", "mistake"}}, "archived"},
		{"select/unknown-slug", "select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("never-existed")}, "not an option"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFieldDefault(tc.fieldType, lifecycleOptions, tc.def)
			if err == nil {
				t.Fatalf("accepted a default naming a term that must not be offered")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not say why (want it to mention %q)", err, tc.wantIn)
			}
		})
	}
}

func TestValidateFieldDefault_AcceptsActiveOptions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fieldType string
		def       FieldDefault
	}{
		{"select/bare-slug-entry", "select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("greybox")}},
		{"select/object-entry", "select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("polish")}},
		{"tree/nested-leaf", "tree",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("london")}},
		{"multi_select/several", "multi_select",
			FieldDefault{Kind: DefaultKindLiteral, ValueOptions: []string{"greybox", "polish"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateFieldDefault(tc.fieldType, lifecycleOptions, tc.def); err != nil {
				t.Errorf("rejected an active term: %v", err)
			}
		})
	}
}

// A literal must populate the member its type uses and no other. Left
// unchecked, buildUpsertParams reads only the member it wants, so a
// default aimed at the wrong column would be stored and then silently
// do nothing — the #778 failure, arriving through a new door.
func TestValidateFieldDefault_RejectsWrongColumn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fieldType string
		def       FieldDefault
	}{
		{"number given text", "number",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("three")}},
		{"text given number", "text",
			FieldDefault{Kind: DefaultKindLiteral, ValueNum: numPtr(3)}},
		{"date given text", "date",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("2026-01-01")}},
		{"multi_select given text", "multi_select",
			FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("greybox")}},
		{"tree given options array", "tree",
			FieldDefault{Kind: DefaultKindLiteral, ValueOptions: []string{"europe", "london"}}},
		{"number carrying BOTH", "number",
			FieldDefault{Kind: DefaultKindLiteral, ValueNum: numPtr(3), ValueText: textPtr("three")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateFieldDefault(tc.fieldType, lifecycleOptions, tc.def); err == nil {
				t.Error("accepted a literal aimed at a column this field type does not read")
			}
		})
	}
}

// boolean inherits the 0/1 contract #791 pinned, because validation
// runs through buildUpsertParams rather than around it.
func TestValidateFieldDefault_BooleanInheritsTheZeroOneContract(t *testing.T) {
	for _, v := range []float64{0, 1} {
		if err := ValidateFieldDefault("boolean", nil,
			FieldDefault{Kind: DefaultKindLiteral, ValueNum: numPtr(v)}); err != nil {
			t.Errorf("boolean default %v rejected: %v", v, err)
		}
	}
	if err := ValidateFieldDefault("boolean", nil,
		FieldDefault{Kind: DefaultKindLiteral, ValueNum: numPtr(2)}); err == nil {
		t.Error("boolean default accepted value_num=2")
	}
	if err := ValidateFieldDefault("boolean", nil,
		FieldDefault{Kind: DefaultKindLiteral, ValueText: textPtr("true")}); err == nil {
		t.Error(`boolean default accepted value_text "true" — the pre-#791 encoding`)
	}
}

// The context set is CLOSED. Anything not in it is rejected with a
// message naming what is, because the alternative — accepting it and
// resolving to nothing — is a setting that appears to work.
func TestValidateFieldDefault_ContextIsAClosedSet(t *testing.T) {
	err := ValidateFieldDefault("text", nil,
		FieldDefault{Kind: DefaultKindContext, Context: "os.Getenv"})
	if err == nil {
		t.Fatal("accepted an unknown context value")
	}
	for _, c := range DefaultContexts() {
		if !strings.Contains(err.Error(), string(c)) {
			t.Errorf("the rejection does not name %q as an option: %v", c, err)
		}
	}

	// There is no expression language, and no macro column. A string
	// that looks like one is just an unknown context name.
	for _, bogus := range []string{"{{ .User }}", "$user", "concat(a,b)", ""} {
		if err := ValidateFieldDefault("text", nil,
			FieldDefault{Kind: DefaultKindContext, Context: DefaultContext(bogus)}); err == nil {
			t.Errorf("accepted %q as a context value", bogus)
		}
	}
}

func TestValidateFieldDefault_ContextMustMatchTheFieldsStorageShape(t *testing.T) {
	// current_date fills a date column; a text field does not have one.
	if err := ValidateFieldDefault("text", nil,
		FieldDefault{Kind: DefaultKindContext, Context: ContextCurrentDate}); err == nil {
		t.Error("accepted current_date on a text field")
	}
	// uploading_user fills a text column; a date field does not.
	if err := ValidateFieldDefault("datetime", nil,
		FieldDefault{Kind: DefaultKindContext, Context: ContextUploadingUser}); err == nil {
		t.Error("accepted uploading_user on a datetime field")
	}
	// A number field can take no context at all, and must say so.
	err := ValidateFieldDefault("number", nil,
		FieldDefault{Kind: DefaultKindContext, Context: ContextUploadingUser})
	if err == nil {
		t.Fatal("accepted a context default on a number field")
	}
	if !strings.Contains(err.Error(), "use a literal") {
		t.Errorf("the rejection does not tell the operator what to do instead: %v", err)
	}

	// And the positives.
	for _, tc := range []struct {
		fieldType string
		ctx       DefaultContext
	}{
		{"text", ContextUploadingUser},
		{"longtext", ContextUploadingTeam},
		{"date", ContextCurrentDate},
		{"datetime", ContextCurrentDate},
	} {
		if err := ValidateFieldDefault(tc.fieldType, nil,
			FieldDefault{Kind: DefaultKindContext, Context: tc.ctx}); err != nil {
			t.Errorf("%s/%s rejected: %v", tc.fieldType, tc.ctx, err)
		}
	}
}

// The two shapes do not mix. A document carrying both is ambiguous
// about which one the resolver should honour, and an ambiguity stored
// is an ambiguity that eventually gets resolved by accident.
func TestValidateFieldDefault_ShapesDoNotMix(t *testing.T) {
	if err := ValidateFieldDefault("text", nil, FieldDefault{
		Kind: DefaultKindContext, Context: ContextUploadingUser, ValueText: textPtr("x"),
	}); err == nil {
		t.Error("accepted a context default carrying a literal")
	}
	if err := ValidateFieldDefault("text", nil, FieldDefault{
		Kind: DefaultKindLiteral, ValueText: textPtr("x"), Context: ContextUploadingUser,
	}); err == nil {
		t.Error("accepted a literal default naming a context")
	}
	if err := ValidateFieldDefault("text", nil, FieldDefault{Kind: DefaultKindLiteral}); err == nil {
		t.Error("accepted a literal default with no value")
	}
	if err := ValidateFieldDefault("text", nil, FieldDefault{}); err == nil {
		t.Error("accepted a default with no kind")
	}
	if err := ValidateFieldDefault("text", nil, FieldDefault{Kind: "macro"}); err == nil {
		t.Error(`accepted kind "macro" — there is no third shape`)
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func TestResolveFieldDefault_Contexts(t *testing.T) {
	now := time.Date(2026, 7, 31, 14, 30, 15, 0, time.UTC)
	rc := DefaultResolveContext{UserDisplay: "Ada Lovelace", TeamName: "Textures", Now: now}

	w, ok := ResolveFieldDefault("text", FieldDefault{Kind: DefaultKindContext, Context: ContextUploadingUser}, rc)
	if !ok || w.ValueText == nil || *w.ValueText != "Ada Lovelace" {
		t.Errorf("uploading_user resolved to %v (ok=%v)", w, ok)
	}

	w, ok = ResolveFieldDefault("text", FieldDefault{Kind: DefaultKindContext, Context: ContextUploadingTeam}, rc)
	if !ok || w.ValueText == nil || *w.ValueText != "Textures" {
		t.Errorf("uploading_team resolved to %v (ok=%v)", w, ok)
	}

	// A `date` field carries a day. Truncating at resolve time rather
	// than at read time means no display surface has to know which of
	// the two date types it is looking at.
	w, ok = ResolveFieldDefault("date", FieldDefault{Kind: DefaultKindContext, Context: ContextCurrentDate}, rc)
	if !ok || w.ValueDate == nil {
		t.Fatalf("current_date did not resolve for a date field")
	}
	if h, m := w.ValueDate.Hour(), w.ValueDate.Minute(); h != 0 || m != 0 {
		t.Errorf("a `date` default kept a time-of-day (%02d:%02d)", h, m)
	}

	w, ok = ResolveFieldDefault("datetime", FieldDefault{Kind: DefaultKindContext, Context: ContextCurrentDate}, rc)
	if !ok || w.ValueDate == nil || !w.ValueDate.Equal(now) {
		t.Errorf("a `datetime` default lost its instant: %v", w)
	}
}

// An unresolvable context applies NOTHING. Not an empty string, not a
// zero date — a blank the artist can fill in, rather than a value they
// have to notice is wrong.
func TestResolveFieldDefault_UnresolvableContextAppliesNothing(t *testing.T) {
	empty := DefaultResolveContext{}
	for _, c := range DefaultContexts() {
		fieldType := "text"
		if contextTargetType[c] == kindDate {
			fieldType = "datetime"
		}
		if _, ok := ResolveFieldDefault(fieldType, FieldDefault{Kind: DefaultKindContext, Context: c}, empty); ok {
			t.Errorf("context %q resolved against an empty context — "+
				"a default nobody can compute must not be written as a blank or a zero", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Team-override selection
// ---------------------------------------------------------------------------

func teamUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("bad uuid: %v", err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

// Acceptance item 5. A team's override beats the field default, and
// removing it falls back cleanly — which at this layer is simply "the
// group has no override rows any more".
func TestChooseDefault_TeamOverrideWinsAndFallsBack(t *testing.T) {
	fieldDefault := []byte(`{"kind":"literal","value_text":"greybox"}`)
	teamValue := []byte(`{"kind":"literal","value_text":"polish"}`)
	team := teamUUID(t, "11111111-1111-1111-1111-111111111111")

	g := candidateGroup{
		Default:   fieldDefault,
		Overrides: []candidateOverride{{TeamID: team, Value: teamValue}},
	}
	got, from := chooseDefault(g)
	if string(got) != string(teamValue) {
		t.Errorf("the field default won over a team override: %s", got)
	}
	if !from.Valid || from != team {
		t.Errorf("the applied default is not attributed to the team that supplied it")
	}

	// Override removed — the field default applies again, with nothing
	// left over from the override.
	g.Overrides = nil
	got, from = chooseDefault(g)
	if string(got) != string(fieldDefault) {
		t.Errorf("removing the override did not fall back to the field default: %s", got)
	}
	if from.Valid {
		t.Error("a field default was attributed to a team")
	}
}

// Two of the uploader's teams overriding the same field has no correct
// answer, so it gets none: both are discarded and the field default
// applies. Deliberately not resolved by an ORDER BY — a rule that picks
// confidently and unpredictably is worse than one that steps back to
// the value everyone agreed on.
func TestChooseDefault_AmbiguousOverridesFallBack(t *testing.T) {
	fieldDefault := []byte(`{"kind":"literal","value_text":"greybox"}`)
	g := candidateGroup{
		Default: fieldDefault,
		Overrides: []candidateOverride{
			{TeamID: teamUUID(t, "11111111-1111-1111-1111-111111111111"), Value: []byte(`{"kind":"literal","value_text":"a"}`)},
			{TeamID: teamUUID(t, "22222222-2222-2222-2222-222222222222"), Value: []byte(`{"kind":"literal","value_text":"b"}`)},
		},
	}
	got, from := chooseDefault(g)
	if string(got) != string(fieldDefault) {
		t.Errorf("two competing overrides produced a winner (%s) — there is no basis to pick one", got)
	}
	if from.Valid {
		t.Error("an ambiguous choice was attributed to a team anyway")
	}

	// And with no field default to fall back to, nothing applies.
	g.Default = nil
	if got, _ := chooseDefault(g); len(got) != 0 {
		t.Errorf("ambiguous overrides with no field default still produced %s", got)
	}
}

// A team may carry an override for a field that has no default of its
// own — the query's WHERE accepts either side, so this shape reaches
// chooseDefault and must work.
func TestChooseDefault_OverrideWithNoFieldDefault(t *testing.T) {
	teamValue := []byte(`{"kind":"literal","value_text":"polish"}`)
	g := candidateGroup{
		Overrides: []candidateOverride{{TeamID: teamUUID(t, "11111111-1111-1111-1111-111111111111"), Value: teamValue}},
	}
	if got, _ := chooseDefault(g); string(got) != string(teamValue) {
		t.Errorf("a team override on a field with no default did not apply: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Round-tripping
// ---------------------------------------------------------------------------

// What an operator sends is what gets stored is what comes back. A
// document that mutates across a save is a document an operator cannot
// reason about.
func TestFieldDefault_RoundTrips(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"literal","value_text":"greybox"}`,
		`{"kind":"literal","value_num":4096}`,
		`{"kind":"literal","value_options":["greybox","polish"]}`,
		`{"kind":"context","context":"uploading_user"}`,
	} {
		d, ok, err := ParseFieldDefault([]byte(raw))
		if err != nil || !ok {
			t.Fatalf("%s: parse: %v", raw, err)
		}
		out, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("%s: marshal: %v", raw, err)
		}
		var a, b map[string]any
		_ = json.Unmarshal([]byte(raw), &a)
		_ = json.Unmarshal(out, &b)
		if len(a) != len(b) {
			t.Errorf("%s round-tripped to %s — a member was added or lost", raw, out)
		}
	}

	// "No default" is a distinct state from "a default that is empty".
	for _, raw := range [][]byte{nil, {}, []byte("null")} {
		if _, ok, err := ParseFieldDefault(raw); ok || err != nil {
			t.Errorf("%q parsed as a present default (ok=%v, err=%v)", raw, ok, err)
		}
	}
}

// kindForFieldType and valueColumnFor are two statements of the same
// fact. This is the guard that keeps them one fact.
func TestKindForFieldTypeAgreesWithTheColumnPin(t *testing.T) {
	want := map[valueColumn]valueKind{
		colText: kindText, colNum: kindNum, colDate: kindDate,
		colOptions: kindOptions, colRef: kindRef,
	}
	for _, typ := range allFieldTypes {
		got, ok := kindForFieldType(typ)
		if !ok {
			t.Errorf("kindForFieldType has no entry for %q", typ)
			continue
		}
		if got != want[valueColumnFor[typ]] {
			t.Errorf("field type %q: kindForFieldType says %v, valueColumnFor says %s — "+
				"a type was added to one and not the other", typ, got, valueColumnFor[typ])
		}
	}
}
