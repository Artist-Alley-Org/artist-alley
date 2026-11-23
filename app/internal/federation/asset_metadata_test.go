// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Profile asset-metadata-1 (spec v1 §18, #1577): the pure member parser,
// the placement check, and regressions proving the running receiver and
// sender are unchanged by it.

package federation_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

const (
	amCC       = "https://creativecommons.org/licenses/by/4.0/"
	amRS       = "http://rightsstatements.org/vocab/InC/1.0/"
	amMarker   = "SECRET-MARKER-7f3a"
	amStmtJSON = `{"label":{"en":["Attribution"]},"value":{"none":["Studio A"]}}`
)

// amMember builds a raw member from the four raw JSON values.
func amMember(title, description, rights, statement string) json.RawMessage {
	return json.RawMessage(`{"title":` + title + `,"description":` + description +
		`,"rights":` + rights + `,"requiredStatement":` + statement + `}`)
}

func amQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func amParse(t *testing.T, raw json.RawMessage) *federation.AssetMetadataSnapshot {
	t.Helper()
	s, err := federation.ParseAssetMetadata(raw)
	if err != nil {
		shown := raw
		if len(shown) > 300 {
			shown = shown[:300]
		}
		t.Fatalf("a valid member was refused: %v\n%s", err, shown)
	}
	return s
}

func amRefused(t *testing.T, name string, raw json.RawMessage) error {
	t.Helper()
	s, err := federation.ParseAssetMetadata(raw)
	if !errors.Is(err, federation.ErrMetadataInvalid) {
		t.Errorf("%s: got %v, want ErrMetadataInvalid", name, err)
		return err
	}
	if s != nil {
		t.Errorf("%s: a refused member returned a snapshot", name)
	}
	// The profile's own reason, never a legacy one.
	if errors.Is(err, federation.ErrArchivalInvalid) || errors.Is(err, federation.ErrInvalidMember) {
		t.Errorf("%s: the error wraps a legacy sentinel: %v", name, err)
	}
	return err
}

func strp(s string) *string { return &s }

// amBS is one backslash. Escaped test inputs are built from it so that no
// editor, generator or shell can turn a JSON escape into the character it
// stands for and silently test a different case.
var amBS = string(rune(92))

// amEsc is the six-byte JSON escape of one UTF-16 code unit: amEsc("0074")
// is a backslash, a u and the four hex digits.
func amEsc(hex4 string) string { return amBS + "u" + hex4 }

// amWireHasEscape fails unless raw holds the escape as literal ASCII bytes.
func amWireHasEscape(t *testing.T, raw []byte, hex4 string) {
	t.Helper()
	want := []byte{92, 'u', hex4[0], hex4[1], hex4[2], hex4[3]}
	if !bytes.Contains(raw, want) {
		t.Fatalf("setup: the wire bytes lack the literal escape %q: % x", want, raw)
	}
}

// amObjectKeys lists the decoded member names of a JSON object, in order
// and with repeats, as a streaming decoder sees them.
func amObjectKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("setup: not an object: %v", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return keys
}

func TestParseAssetMetadata_ValidSnapshots(t *testing.T) {
	stmt := &federation.ArchivalLabelValue{Label: map[string][]string{"en": {"Attribution"}}, Value: map[string][]string{"none": {"Studio A"}}}
	cases := []struct {
		name string
		raw  json.RawMessage
		want federation.AssetMetadataSnapshot
	}{
		{"four claims, markup kept as text, https Creative Commons",
			amMember(`"<b>Harbour</b> & dusk"`, `"<script>alert(1)</script>"`, amQuote(amCC), amStmtJSON),
			federation.AssetMetadataSnapshot{Title: strp("<b>Harbour</b> & dusk"), Description: strp("<script>alert(1)</script>"), Rights: strp(amCC), RequiredStatement: stmt}},
		{"the §18.2 example", json.RawMessage(`{
  "title": "Harbour at dusk",
  "description": null,
  "rights": "https://creativecommons.org/licenses/by/4.0/",
  "requiredStatement": {
    "label": {"en": ["Attribution"]},
    "value": {"none": ["Studio A"]}
  }
}`),
			federation.AssetMetadataSnapshot{Title: strp("Harbour at dusk"), Rights: strp(amCC), RequiredStatement: stmt}},
		{"mixed nulls, http RightsStatements.org", amMember(`null`, `"Oil on board."`, amQuote(amRS), `null`),
			federation.AssetMetadataSnapshot{Description: strp("Oil on board."), Rights: strp(amRS)}},
		{"public domain mark", amMember(`null`, `null`, `"https://creativecommons.org/publicdomain/mark/1.0/"`, `null`),
			federation.AssetMetadataSnapshot{Rights: strp("https://creativecommons.org/publicdomain/mark/1.0/")}},
		{"four nulls clear every claim", amMember(`null`, `null`, `null`, `null`), federation.AssetMetadataSnapshot{}},
		{"keys in another order", json.RawMessage(`{"requiredStatement":null,"rights":null,"description":null,"title":"t"}`),
			federation.AssetMetadataSnapshot{Title: strp("t")}},
		{"an uppercase language tag is a valid tag, not a schema key",
			amMember(`null`, `null`, `null`, `{"label":{"EN-GB":["Credit"]},"value":{"none":["A"]}}`),
			federation.AssetMetadataSnapshot{RequiredStatement: &federation.ArchivalLabelValue{Label: map[string][]string{"EN-GB": {"Credit"}}, Value: map[string][]string{"none": {"A"}}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := amParse(t, c.raw)
			if !reflect.DeepEqual(*got, c.want) {
				t.Fatalf("snapshot\n got %+v\nwant %+v", *got, c.want)
			}
			// Round trip: every key is marshalled, nulls included, and the
			// reparsed snapshot is identical.
			out, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(out, &keys); err != nil {
				t.Fatal(err)
			}
			if len(keys) != 4 {
				t.Fatalf("marshalled %d keys, want 4: %s", len(keys), out)
			}
			for _, k := range []string{"title", "description", "rights", "requiredStatement"} {
				if _, ok := keys[k]; !ok {
					t.Fatalf("marshalled snapshot lacks %q: %s", k, out)
				}
			}
			if back := amParse(t, out); !reflect.DeepEqual(back, got) {
				t.Fatalf("round trip changed the snapshot\n got %+v\nwant %+v", *back, *got)
			}
		})
	}
}

func TestParseAssetMetadata_KeepsValuesExactly(t *testing.T) {
	title := "  leading and trailing spaces  "
	desc := "line one\n\tindented line two\n"
	s := amParse(t, amMember(amQuote(title), amQuote(desc), `null`, `{"label":{"en":[" A "]},"value":{"none":["<i>x</i>"]}}`))
	if *s.Title != title || *s.Description != desc {
		t.Fatalf("a value was changed: %q %q", *s.Title, *s.Description)
	}
	if s.RequiredStatement.Label["en"][0] != " A " || s.RequiredStatement.Value["none"][0] != "<i>x</i>" {
		t.Fatalf("a statement value was changed: %+v", s.RequiredStatement)
	}
	// A valid surrogate-pair escape (the high then the low code unit, as
	// escapes on the wire) decodes to its one code point, not a refusal.
	pairRaw := amMember(`"`+amEsc("d83d")+amEsc("de00")+` smile"`, `null`, `null`, `null`)
	amWireHasEscape(t, pairRaw, "d83d")
	amWireHasEscape(t, pairRaw, "de00")
	if bytes.ContainsRune(pairRaw, 0x1F600) {
		t.Fatal("setup: the wire bytes hold the literal code point, not the escapes")
	}
	s = amParse(t, pairRaw)
	if *s.Title != string(rune(0x1F600))+" smile" {
		t.Fatalf("surrogate pair decoded as %q", *s.Title)
	}
	// U+007F is not a C0 control character: the archival-1 rule allows it.
	amParse(t, amMember(`"a\u007fb"`, `null`, `null`, `null`))
}

func TestParseAssetMetadata_EveryKeyIsRequired(t *testing.T) {
	all := map[string]string{"title": `"t"`, "description": `null`, "rights": `null`, "requiredStatement": `null`}
	for missing := range all {
		parts := []string{}
		for k, v := range all {
			if k != missing {
				parts = append(parts, amQuote(k)+":"+v)
			}
		}
		amRefused(t, "without "+missing, json.RawMessage("{"+strings.Join(parts, ",")+"}"))
	}
	amRefused(t, "empty object", json.RawMessage(`{}`))
	amRefused(t, "requiredStatement without value", amMember(`null`, `null`, `null`, `{"label":{"en":["A"]}}`))
	amRefused(t, "requiredStatement without label", amMember(`null`, `null`, `null`, `{"value":{"en":["A"]}}`))
}

func TestParseAssetMetadata_RefusesShapes(t *testing.T) {
	cases := map[string]string{
		"whole member null":    `null`,
		"an array":             `[]`,
		"a string":             `"title"`,
		"a number":             `1`,
		"a boolean":            `true`,
		"empty input":          ``,
		"only whitespace":      "  \n ",
		"title number":         string(amMember(`42`, `null`, `null`, `null`)),
		"title boolean":        string(amMember(`false`, `null`, `null`, `null`)),
		"title object":         string(amMember(`{}`, `null`, `null`, `null`)),
		"title array":          string(amMember(`["t"]`, `null`, `null`, `null`)),
		"description number":   string(amMember(`null`, `1`, `null`, `null`)),
		"rights object":        string(amMember(`null`, `null`, `{"uri":"x"}`, `null`)),
		"rights array":         string(amMember(`null`, `null`, `[`+amQuote(amCC)+`]`, `null`)),
		"statement string":     string(amMember(`null`, `null`, `null`, `"Studio A"`)),
		"statement array":      string(amMember(`null`, `null`, `null`, `[]`)),
		"statement number":     string(amMember(`null`, `null`, `null`, `7`)),
		"label null":           string(amMember(`null`, `null`, `null`, `{"label":null,"value":{"none":["A"]}}`)),
		"label array":          string(amMember(`null`, `null`, `null`, `{"label":["A"],"value":{"none":["A"]}}`)),
		"label string":         string(amMember(`null`, `null`, `null`, `{"label":"A","value":{"none":["A"]}}`)),
		"tag list null":        string(amMember(`null`, `null`, `null`, `{"label":{"en":null},"value":{"none":["A"]}}`)),
		"tag list string":      string(amMember(`null`, `null`, `null`, `{"label":{"en":"A"},"value":{"none":["A"]}}`)),
		"tag list entry null":  string(amMember(`null`, `null`, `null`, `{"label":{"en":[null]},"value":{"none":["A"]}}`)),
		"tag list entry num":   string(amMember(`null`, `null`, `null`, `{"label":{"en":[1]},"value":{"none":["A"]}}`)),
		"tag list entry obj":   string(amMember(`null`, `null`, `null`, `{"label":{"en":[{}]},"value":{"none":["A"]}}`)),
		"empty title":          string(amMember(`""`, `null`, `null`, `null`)),
		"empty description":    string(amMember(`null`, `""`, `null`, `null`)),
		"empty rights":         string(amMember(`null`, `null`, `""`, `null`)),
		"empty statement text": string(amMember(`null`, `null`, `null`, `{"label":{"en":[""]},"value":{"none":["A"]}}`)),
		"empty language map":   string(amMember(`null`, `null`, `null`, `{"label":{},"value":{"none":["A"]}}`)),
		"empty tag list":       string(amMember(`null`, `null`, `null`, `{"label":{"en":[]},"value":{"none":["A"]}}`)),
		"invalid tag":          string(amMember(`null`, `null`, `null`, `{"label":{"en_GB":["A"]},"value":{"none":["A"]}}`)),
		"rights elsewhere":     string(amMember(`null`, `null`, `"https://example.com/licence"`, `null`)),
		"rights javascript":    string(amMember(`null`, `null`, `"javascript:alert(1)"`, `null`)),
		"rights with a query":  string(amMember(`null`, `null`, amQuote(amCC+"?x=1"), `null`)),
		"rights ftp":           string(amMember(`null`, `null`, `"ftp://creativecommons.org/licenses/by/4.0/"`, `null`)),
	}
	for name, raw := range cases {
		amRefused(t, name, json.RawMessage(raw))
	}
}

func TestParseAssetMetadata_RefusesUnknownAndCaseVariantKeys(t *testing.T) {
	cases := map[string]string{
		"an extra top-level key": `{"title":null,"description":null,"rights":null,"requiredStatement":null,"creator":"A"}`,
		"Title instead of title": `{"Title":null,"description":null,"rights":null,"requiredStatement":null}`,
		"TITLE beside title":     `{"title":null,"TITLE":"x","description":null,"rights":null,"requiredStatement":null}`,
		"RequiredStatement":      `{"title":null,"description":null,"rights":null,"RequiredStatement":null}`,
		"Label in the statement": string(amMember(`null`, `null`, `null`, `{"Label":{"en":["A"]},"value":{"none":["A"]}}`)),
		"an extra statement key": string(amMember(`null`, `null`, `null`, `{"label":{"en":["A"]},"value":{"none":["A"]},"lang":"en"}`)),
		"a nested unknown key":   string(amMember(`null`, `null`, `null`, `{"label":{"en":["A"]},"value":{"none":["A"]},"extra":{"deep":1}}`)),
	}
	for name, raw := range cases {
		amRefused(t, name, json.RawMessage(raw))
	}
}

func TestParseAssetMetadata_RefusesRepeatedNamesAtEveryDepth(t *testing.T) {
	cases := map[string]string{
		"title twice":                    `{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`,
		"label twice":                    string(amMember(`null`, `null`, `null`, `{"label":{"en":["A"]},"label":{"en":["B"]},"value":{"none":["A"]}}`)),
		"a language tag twice":           string(amMember(`null`, `null`, `null`, `{"label":{"en":["A"],"en":["B"]},"value":{"none":["A"]}}`)),
		"requiredStatement twice (null)": `{"title":null,"description":null,"rights":null,"requiredStatement":null,"requiredStatement":null}`,
	}
	// The same names spelled once with a JSON escape: one escaped and one
	// plain key that decode to the same name at that depth.
	titleAlias := `{"` + amEsc("0074") + `itle":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`
	tagLabel := `{"en":["A"],"` + amEsc("0065") + `n":["B"]}`
	tagAlias := string(amMember(`null`, `null`, `null`, `{"label":`+tagLabel+`,"value":{"none":["A"]}}`))
	amWireHasEscape(t, []byte(titleAlias), "0074")
	amWireHasEscape(t, []byte(tagAlias), "0065")
	if k := amObjectKeys(t, titleAlias); len(k) != 5 || k[0] != "title" || k[1] != "title" {
		t.Fatalf("setup: the title alias decodes to keys %q", k)
	}
	if k := amObjectKeys(t, tagLabel); len(k) != 2 || k[0] != "en" || k[1] != "en" {
		t.Fatalf("setup: the tag alias decodes to keys %q", k)
	}
	cases["an escaped alias of title"] = titleAlias
	cases["an escaped alias of a tag"] = tagAlias
	for name, raw := range cases {
		amRefused(t, name, json.RawMessage(raw))
	}
	// Go's decoder alone would keep the last value of a repeated name:
	// the refusal above is the strict guard, not a decoding accident.
	var m map[string]any
	if err := json.Unmarshal([]byte(cases["title twice"]), &m); err != nil || m["title"] != "b" {
		t.Fatalf("encoding/json no longer keeps the last value (%v, %v); revisit this test", err, m["title"])
	}
}

func TestParseAssetMetadata_TextBounds(t *testing.T) {
	euro := "€" // three UTF-8 bytes
	ok := map[string]json.RawMessage{
		"title of 1024 ASCII bytes":       amMember(amQuote(strings.Repeat("a", 1024)), `null`, `null`, `null`),
		"title of 1024 multibyte bytes":   amMember(amQuote(strings.Repeat(euro, 341)+"a"), `null`, `null`, `null`),
		"description of 4096 bytes":       amMember(`null`, amQuote(strings.Repeat("d", 4096)), `null`, `null`),
		"rights URI of exactly 2048":      amMember(`null`, `null`, amQuote(amLongCC(2048)), `null`),
		"statement text of 4096 bytes":    amMember(`null`, `null`, `null`, `{"label":{"en":["A"]},"value":{"none":[`+amQuote(strings.Repeat("v", 4096))+`]}}`),
		"64 language tags and 64 entries": amMember(`null`, `null`, `null`, amStatement(64, 64)),
	}
	for name, raw := range ok {
		t.Run(name, func(t *testing.T) { amParse(t, raw) })
	}
	bad := map[string]json.RawMessage{
		"title of 1025 ASCII bytes":       amMember(amQuote(strings.Repeat("a", 1025)), `null`, `null`, `null`),
		"title of 1026 multibyte bytes":   amMember(amQuote(strings.Repeat(euro, 342)), `null`, `null`, `null`),
		"description of 4097 bytes":       amMember(`null`, amQuote(strings.Repeat("d", 4097)), `null`, `null`),
		"rights URI of 2049 bytes":        amMember(`null`, `null`, amQuote(amLongCC(2049)), `null`),
		"statement text of 4097 bytes":    amMember(`null`, `null`, `null`, `{"label":{"en":["A"]},"value":{"none":[`+amQuote(strings.Repeat("v", 4097))+`]}}`),
		"65 language tags":                amMember(`null`, `null`, `null`, amStatement(65, 1)),
		"65 entries for one language tag": amMember(`null`, `null`, `null`, amStatement(1, 65)),
	}
	for name, raw := range bad {
		amRefused(t, name, raw)
	}
	// Each rights URI built for the boundary is one the §17.2.5 pattern
	// accepts, so the refusal at 2049 is the byte bound itself.
	if !federation.ArchivalRightsURIValid(amLongCC(2049)) || len(amLongCC(2049)) != 2049 {
		t.Fatal("amLongCC no longer builds a pattern-valid URI of the asked length")
	}
}

// amLongCC is a Creative Commons licence-shaped URI of exactly n bytes.
func amLongCC(n int) string {
	const pre, post = "https://creativecommons.org/licenses/", "/4.0/"
	return pre + strings.Repeat("b", n-len(pre)-len(post)) + post
}

// amStatement is a requiredStatement with tags language tags in label
// (each listing entries short strings) and a one-entry value.
func amStatement(tags, entries int) string {
	var b strings.Builder
	b.WriteString(`{"label":{`)
	for i := 0; i < tags; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(amQuote("en-" + string(rune('a'+i%26)) + string(rune('a'+i/26))))
		b.WriteString(":[")
		for j := 0; j < entries; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"e"`)
		}
		b.WriteString("]")
	}
	b.WriteString(`},"value":{"none":["A"]}}`)
	return b.String()
}

// amPadded is an all-null member padded with whitespace INSIDE the object
// to exactly n bytes from { through }.
func amPadded(n int) json.RawMessage {
	base := string(amMember(`"t"`, `null`, `null`, `null`))
	return json.RawMessage("{" + strings.Repeat(" ", n-len(base)) + base[1:])
}

func TestParseAssetMetadata_MemberBound(t *testing.T) {
	if got := len(amPadded(federation.AssetMetadataMaxMemberBytes)); got != 32768 {
		t.Fatalf("padded member is %d bytes", got)
	}
	amParse(t, amPadded(32768))
	err := amRefused(t, "32769 bytes of which most is inside whitespace", amPadded(32769))
	// Its canonical form is tiny: the bound is measured on the raw bytes.
	canon, cerr := federation.Canonicalize(amPadded(32769))
	if cerr != nil || len(canon) > 200 {
		t.Fatalf("canonical form %d bytes (%v)", len(canon), cerr)
	}
	if err != nil && !strings.Contains(err.Error(), "32768") {
		t.Errorf("the refusal does not name the bound: %v", err)
	}
	// JSON whitespace OUTSIDE the object is not part of the span.
	outer := append(append([]byte(" \n\t\r"), amPadded(32768)...), []byte("\r\n\t ")...)
	amParse(t, outer)
	// Individually valid 4,000-byte texts that together exceed the bound.
	list := make([]string, 9)
	for i := range list {
		list[i] = amQuote(strings.Repeat("y", 4000))
	}
	amRefused(t, "nine valid 4,000-byte statement texts", amMember(`null`, `null`, `null`,
		`{"label":{"en":["Attribution"]},"value":{"none":[`+strings.Join(list, ",")+`]}}`))
}

func TestParseAssetMetadata_StrictBytesAndUnicode(t *testing.T) {
	cases := map[string]json.RawMessage{
		"a literal invalid UTF-8 byte": json.RawMessage("{\"title\":\"a\xffb\",\"description\":null,\"rights\":null,\"requiredStatement\":null}"),
		"a lone high surrogate":        amMember(`"a\ud800b"`, `null`, `null`, `null`),
		"a lone low surrogate":         amMember(`"a\udc00b"`, `null`, `null`, `null`),
		"a high surrogate then a BMP":  amMember(`"`+amEsc("d800")+amEsc("0041")+`"`, `null`, `null`, `null`),
		"a carriage return":            amMember(`"a\rb"`, `null`, `null`, `null`),
		"a NUL":                        amMember(`"a\u0000b"`, `null`, `null`, `null`),
		"a bell in a statement":        amMember(`null`, `null`, `null`, `{"label":{"en":["a\u0007"]},"value":{"none":["A"]}}`),
		"a unit separator":             amMember(`null`, `"x\u001fy"`, `null`, `null`),
		"a second JSON value":          json.RawMessage(string(amMember(`null`, `null`, `null`, `null`)) + ` {}`),
		"trailing garbage":             json.RawMessage(string(amMember(`null`, `null`, `null`, `null`)) + `x`),
		"a trailing comma":             json.RawMessage(string(amMember(`null`, `null`, `null`, `null`)) + `,`),
		"a leading non-JSON space":     json.RawMessage(" " + string(amMember(`null`, `null`, `null`, `null`))),
		"a trailing vertical tab":      json.RawMessage(string(amMember(`null`, `null`, `null`, `null`)) + "\v"),
		"an invalid escape":            amMember(`"a\qb"`, `null`, `null`, `null`),
		"an unterminated object":       json.RawMessage(`{"title":null,"description":null,"rights":null,"requiredStatement":null`),
	}
	for name, raw := range cases {
		amRefused(t, name, raw)
	}
	// Tab and line feed are allowed.
	amParse(t, amMember(`"a\tb\nc"`, `null`, `null`, `null`))
}

func TestParseAssetMetadata_NeitherMutatesNorLeaks(t *testing.T) {
	cases := map[string]json.RawMessage{
		"over-long title":   amMember(amQuote(strings.Repeat(amMarker, 80)), `null`, `null`, `null`),
		"rights elsewhere":  amMember(`null`, `null`, amQuote("https://"+amMarker+".example/"), `null`),
		"unknown key":       json.RawMessage(`{"title":null,"description":null,"rights":null,"requiredStatement":null,"` + amMarker + `":1}`),
		"control character": amMember(amQuote(amMarker+"\x01"), `null`, `null`, `null`),
		"repeated key":      json.RawMessage(`{"title":"` + amMarker + `","title":"b","description":null,"rights":null,"requiredStatement":null}`),
		"statement tag":     amMember(`null`, `null`, `null`, `{"label":{"`+amMarker+`_x":["A"]},"value":{"none":["A"]}}`),
	}
	for name, raw := range cases {
		before := bytes.Clone(raw)
		err := amRefused(t, name, raw)
		if err != nil && strings.Contains(err.Error(), amMarker) {
			t.Errorf("%s: the error carries the received value or key: %v", name, err)
		}
		if !bytes.Equal(before, raw) {
			t.Errorf("%s: the input was changed", name)
		}
	}
	raw := amMember(`"t"`, `null`, amQuote(amCC), amStmtJSON)
	before := bytes.Clone(raw)
	amParse(t, raw)
	if !bytes.Equal(before, raw) {
		t.Error("a valid input was changed")
	}
}

// --- placement on an envelope (§18.1) -------------------------------------

var amAllProfiles = federation.MemberContext{
	Negotiated: func(tok string) bool {
		return tok == federation.ProfileAssetMetadata1 || tok == federation.ProfileArchival1 || tok == federation.ProfileOriginalTransfer1
	},
	Applied: func(federation.ActivityType) bool { return true },
}

// amUpdate is an asset Update carrying aa:asset and, when member is not
// nil, aa:metadata.
func amUpdate(t *testing.T, member json.RawMessage) *federation.Envelope {
	t.Helper()
	extra := map[string]json.RawMessage{federation.MemberArchivalAsset: avMember(t)}
	if member != nil {
		extra[federation.MemberAssetMetadata] = member
	}
	return &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate,
		ID: "https://a.example/activities/m1", Actor: "https://a.example/users/alice", Object: avAsset, Extra: extra}
}

// amCompanion is avMember with one top-level key set to a raw value.
func amCompanion(t *testing.T, key, value string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(avMember(t), &m); err != nil {
		t.Fatal(err)
	}
	m[key] = json.RawMessage(value)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func amValid() json.RawMessage {
	return amMember(`"Harbour at dusk"`, `null`, amQuote(amCC), amStmtJSON)
}

func TestValidateAssetMetadataMembers_AcceptsTheProfile(t *testing.T) {
	if err := federation.ValidateAssetMetadataMembers(amUpdate(t, amValid()), amAllProfiles); err != nil {
		t.Fatalf("a valid metadata Update: %v", err)
	}
	if err := federation.ValidateAssetMetadataMembers(amUpdate(t, amMember(`null`, `null`, `null`, `null`)), amAllProfiles); err != nil {
		t.Fatalf("a clear-all metadata Update: %v", err)
	}
}

func TestValidateAssetMetadataMembers_NeedsAllThreeProfiles(t *testing.T) {
	tokens := []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1}
	for _, missing := range tokens {
		mc := federation.MemberContext{Negotiated: func(tok string) bool { return tok != missing && slices.Contains(tokens, tok) }}
		for name, member := range map[string]json.RawMessage{"valid": amValid(), "malformed": json.RawMessage(`null`)} {
			err := federation.ValidateAssetMetadataMembers(amUpdate(t, member), mc)
			if !errors.Is(err, federation.ErrProfileNotNegotiated) || !strings.Contains(err.Error(), missing) {
				t.Errorf("without %s (%s member): %v", missing, name, err)
			}
		}
	}
	if err := federation.ValidateAssetMetadataMembers(amUpdate(t, amValid()), federation.MemberContext{}); !errors.Is(err, federation.ErrProfileNotNegotiated) {
		t.Errorf("with no stored intersection: %v", err)
	}
	// Applied or an offer alone never stands in for the stored intersection.
	applied := federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}
	if err := federation.ValidateAssetMetadataMembers(amUpdate(t, amValid()), applied); !errors.Is(err, federation.ErrProfileNotNegotiated) {
		t.Errorf("with Applied only: %v", err)
	}
}

func TestValidateAssetMetadataMembers_NoMemberNeedsNoNewToken(t *testing.T) {
	archivalOnly := federation.MemberContext{Negotiated: func(tok string) bool { return tok == federation.ProfileArchival1 }}
	flags := amUpdate(t, nil)
	if err := federation.ValidateAssetMetadataMembers(flags, archivalOnly); err != nil {
		t.Errorf("a flags-only Update: %v", err)
	}
	if err := federation.ValidateAssetMetadataMembers(flags, federation.MemberContext{}); err != nil {
		t.Errorf("a flags-only Update with no profiles at all: %v", err)
	}
	av := amUpdate(t, nil)
	av.Type = federation.ActivityAAAssetVersion
	if err := federation.ValidateAssetMetadataMembers(av, federation.MemberContext{}); err != nil {
		t.Errorf("an aa:AssetVersion: %v", err)
	}
}

func TestValidateAssetMetadataMembers_Placement(t *testing.T) {
	is := func(name string, env *federation.Envelope, want error) {
		t.Helper()
		err := federation.ValidateAssetMetadataMembers(env, amAllProfiles)
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
		if want != federation.ErrMetadataInvalid && errors.Is(err, federation.ErrMetadataInvalid) {
			t.Errorf("%s: reported as metadata_invalid: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), amMarker) {
			t.Errorf("%s: the error carries a received name or value: %v", name, err)
		}
	}
	for _, typ := range []federation.ActivityType{federation.ActivityCreate, federation.ActivityAAAssetVersion, federation.ActivityDelete, federation.ActivityLike} {
		env := amUpdate(t, amValid())
		env.Type = typ
		is(string(typ), env, federation.ErrMetadataInvalid)
	}
	comment := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate,
		ID: "https://a.example/activities/m2", Actor: "https://a.example/users/alice", Object: "https://a.example/activities/c1",
		Extra: map[string]json.RawMessage{federation.MemberObjectType: json.RawMessage(`"Note"`),
			federation.MemberContent: json.RawMessage(`"edited"`), federation.MemberAssetMetadata: amValid()}}
	is("a comment Update", comment, federation.ErrMetadataInvalid)

	noAsset := amUpdate(t, amValid())
	delete(noAsset.Extra, federation.MemberArchivalAsset)
	is("no aa:asset", noAsset, federation.ErrMetadataInvalid)

	for _, extra := range []string{federation.MemberObjectType, federation.MemberContent, "aa:" + amMarker} {
		env := amUpdate(t, amValid())
		env.Extra[extra] = json.RawMessage(`"Note"`)
		is(extra+" beside the members", env, federation.ErrUnknownField)
	}

	sealed := amUpdate(t, amValid())
	sealed.Encryption = &federation.EncryptionBlock{}
	is("in clear beside an encryption block", sealed, federation.ErrInvalidMember)

	broken := amUpdate(t, amValid())
	broken.Extra[federation.MemberArchivalAsset] = json.RawMessage(`{"id":"` + avAsset + `"}`)
	is("a malformed aa:asset", broken, federation.ErrArchivalInvalid)

	other := amUpdate(t, amValid())
	other.Object = avAsset + "x"
	is("aa:asset naming another object", other, federation.ErrArchivalInvalid)

	foreign := "https://b.example/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11"
	wrongOrigin := amUpdate(t, amValid())
	wrongOrigin.Object = foreign
	wrongOrigin.Extra[federation.MemberArchivalAsset] = amCompanion(t, "id", amQuote(foreign))
	is("an asset on another origin than the actor's", wrongOrigin, federation.ErrInvalidMember)

	bad := amUpdate(t, amMember(`""`, `null`, `null`, `null`))
	is("a malformed member, correctly placed", bad, federation.ErrMetadataInvalid)
}

func TestValidateAssetMetadataMembers_CompanionCarriesNoDescription(t *testing.T) {
	values := map[string][]string{
		"description":       {`null`, `{}`, `{"title":"t"}`},
		"rights":            {`null`, `""`, amQuote(amCC)},
		"requiredStatement": {`null`, amStmtJSON},
		"identifiers":       {`null`, `[]`, `[{"scheme":"local","value":"x"}]`},
		"accessionNumber":   {`null`, `""`, `"A-1"`},
		"iiifManifest":      {`null`, `""`, `"https://a.example/iiif/m.json"`},
	}
	for key, vals := range values {
		for _, v := range vals {
			env := amUpdate(t, amValid())
			env.Extra[federation.MemberArchivalAsset] = amCompanion(t, key, v)
			err := federation.ValidateAssetMetadataMembers(env, amAllProfiles)
			if !errors.Is(err, federation.ErrMetadataInvalid) {
				t.Errorf("companion %s=%s: got %v, want metadata_invalid", key, v, err)
			}
		}
	}
	// A case-folded alias must not slip through Go's case-insensitive
	// struct matching: it is an invalid archival member.
	for _, alias := range []string{"Description", "RIGHTS", "requiredstatement", "IiifManifest"} {
		env := amUpdate(t, amValid())
		env.Extra[federation.MemberArchivalAsset] = amCompanion(t, alias, `null`)
		err := federation.ValidateAssetMetadataMembers(env, amAllProfiles)
		if !errors.Is(err, federation.ErrArchivalInvalid) || errors.Is(err, federation.ErrMetadataInvalid) {
			t.Errorf("companion alias %s: got %v, want archival_invalid", alias, err)
		}
	}
	// The restriction belongs to the new profile: a flags-only Update's
	// companion is still judged by archival-1 alone.
	legacy := amUpdate(t, nil)
	legacy.Extra[federation.MemberArchivalAsset] = amCompanion(t, "rights", amQuote(amCC))
	if err := federation.ValidateAssetMetadataMembers(legacy, amAllProfiles); err != nil {
		t.Errorf("a flags-only Update's companion: %v", err)
	}
}

// amDeepCopy copies an envelope with nothing shared: slices, pointers and
// every Extra value's bytes are cloned, so a change through env cannot also
// change the copy.
func amDeepCopy(env *federation.Envelope) federation.Envelope {
	c := *env
	c.To = slices.Clone(env.To)
	c.CC = slices.Clone(env.CC)
	if env.Signature != nil {
		sig := *env.Signature
		c.Signature = &sig
	}
	if env.Extra != nil {
		c.Extra = make(map[string]json.RawMessage, len(env.Extra))
		for k, v := range env.Extra {
			c.Extra[k] = bytes.Clone(v)
		}
	}
	return c
}

func TestValidateAssetMetadataMembers_DoesNotChangeTheEnvelope(t *testing.T) {
	malformed := amMember(`1`, `null`, `null`, `null`)
	_, parseErr := federation.ParseAssetMetadata(malformed)
	if !errors.Is(parseErr, federation.ErrMetadataInvalid) {
		t.Fatalf("setup: the malformed member parses: %v", parseErr)
	}
	cases := []struct {
		name      string
		member    json.RawMessage
		companion json.RawMessage
		check     func(t *testing.T, err error)
	}{
		{"a valid member and a valid companion: the whole check succeeds", amValid(), nil,
			func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("got %v, want success", err)
				}
			}},
		{"a malformed member and a valid companion: refused by the member parser", malformed, nil,
			func(t *testing.T, err error) {
				// The same error ParseAssetMetadata gives proves the check
				// got past every placement rule to the member itself.
				if !errors.Is(err, federation.ErrMetadataInvalid) || err.Error() != parseErr.Error() {
					t.Fatalf("got %v, want the member parser's %v", err, parseErr)
				}
			}},
		{"a forbidden companion member: refused before the member is parsed", amValid(), amCompanion(t, "rights", `null`),
			func(t *testing.T, err error) {
				if !errors.Is(err, federation.ErrMetadataInvalid) || err.Error() == parseErr.Error() {
					t.Fatalf("got %v, want the companion refusal", err)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := amUpdate(t, c.member)
			if c.companion != nil {
				env.Extra[federation.MemberArchivalAsset] = c.companion
			}
			// Every base field set, so the comparison covers all of them.
			env.Published = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
			env.To = []string{"https://b.example/users/bob"}
			env.CC = []string{"https://b.example/followers"}
			env.Signature = &federation.Signature{Type: federation.SignatureAlgEd25519,
				PublicKey: "https://a.example/users/alice#main-key", Value: "c2ln"}
			before := amDeepCopy(env)
			c.check(t, federation.ValidateAssetMetadataMembers(env, amAllProfiles))
			if !reflect.DeepEqual(before, *env) {
				t.Fatalf("the envelope was changed\nbefore %+v\n after %+v", before, *env)
			}
		})
	}
}

// --- the running receiver and sender are unchanged -------------------------

func TestAssetMetadata_RunningReceiverStillRefusesTheMember(t *testing.T) {
	env := amUpdate(t, amValid())
	if err := federation.ValidateAssetMetadataMembers(env, amAllProfiles); err != nil {
		t.Fatalf("precondition: the pure check accepts it: %v", err)
	}
	// The inbox's member check, with every profile negotiated and Update
	// applied, still refuses the member: nothing admits it yet.
	err := federation.ValidateMembers(env, amAllProfiles)
	if !errors.Is(err, federation.ErrUnknownField) || errors.Is(err, federation.ErrMetadataInvalid) {
		t.Fatalf("ValidateMembers: got %v, want unknown_field", err)
	}
	// The same activity without the member is the unchanged flags-only Update.
	if err := federation.ValidateMembers(amUpdate(t, nil), amAllProfiles); err != nil {
		t.Fatalf("a flags-only Update: %v", err)
	}
	// No running reason is metadata_invalid.
	if _, ok := federation.KnownInboxStatuses[federation.InboxStatus("metadata_invalid")]; ok {
		t.Fatal("metadata_invalid became a running inbox status; this foundation must not add one")
	}
}

func TestAssetMetadata_WholeEnvelopeDuplicateKeepsItsLegacyReason(t *testing.T) {
	raw := `{"@context":"` + federation.ContextV1 + `","type":"Update","id":"https://a.example/activities/m3",` +
		`"actor":"https://a.example/users/alice","published":"2026-11-23T10:00:00Z","object":"` + avAsset + `",` +
		`"aa:asset":` + string(avMember(t)) + `,` +
		`"aa:metadata":{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}}`
	_, err := federation.Unmarshal([]byte(raw))
	if !errors.Is(err, federation.ErrInvalidMember) || errors.Is(err, federation.ErrMetadataInvalid) {
		t.Fatalf("Unmarshal of a repeated name inside aa:metadata: got %v, want the legacy invalid_member", err)
	}
}

func TestAssetMetadata_NotAdvertisedAndNeverSent(t *testing.T) {
	for _, c := range peer.KnownCapabilities {
		switch string(c) {
		case federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1:
			t.Errorf("KnownCapabilities advertises %s", c)
		}
	}
	all := func(string) bool { return true }
	wire, err := federation.WireMembers(federation.ActivityUpdate, "asset", avPayload(t, avMember(t)), all)
	if err != nil {
		t.Fatalf("a flags-only asset Update: %v", err)
	}
	if _, ok := wire[federation.MemberAssetMetadata]; ok {
		t.Fatal("the sender built an aa:metadata member")
	}
}
