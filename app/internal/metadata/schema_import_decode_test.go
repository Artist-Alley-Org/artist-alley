// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The strict schema-import request checks (#1576), without a database.
// The production-router, storage and admission behaviour is tested in
// app/internal/http/fields_import_preview_test.go.

package metadata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// validField is one complete portable field, as the exporter writes it.
func validField(code, typ string, opts string, cond string) string {
	if opts == "" {
		opts = "null"
	}
	if cond == "" {
		cond = "null"
	}
	return fmt.Sprintf(`{"code":%q,"description":"","display_condition":%s,"display_group":"general","display_order":100,`+
		`"edit_tab":null,"label":"Label","open_vocabulary":false,"options":%s,"regexp_filter":null,"required":false,`+
		`"searchable":true,"show_in_advanced_search":true,"show_on_card":false,"show_on_upload":true,"subject_kind":"asset","type":%q}`,
		code, cond, opts, typ)
}

func validBody() string {
	return `{"document":{"fields":[` +
		validField("alpha", "tree", `[{"children":[{"children":[],"label":null,"value":"uk"}],"label":"Europe","value":"europe"}]`, "") + `,` +
		validField("beta", "text", "", `["alpha=europe","missing_ctrl~x"]`) +
		`],"format":"aa-field-schema/1"},"source":{"label":"Studio B","uri":"https://studio-b.example/fields"}}`
}

func decodeStatus(t *testing.T, body string) (int, *openapi.FieldSchemaImportRefusal, decodedPreview) {
	t.Helper()
	d, status, ref, err := decodePreviewRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return status, ref, d
}

func TestDecodePreview_Valid(t *testing.T) {
	status, ref, d := decodeStatus(t, validBody())
	if status != 0 || ref != nil {
		t.Fatalf("valid body refused: %d %+v", status, ref)
	}
	if *d.source.Label != "Studio B" || *d.source.URI != "https://studio-b.example/fields" {
		t.Fatalf("source %+v", d.source)
	}
	// Canonical bytes are exactly what the exporter's encoder writes.
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(d.doc)
	if !bytes.Equal(buf.Bytes(), d.canonical) || !bytes.HasSuffix(d.canonical, []byte("}\n")) {
		t.Fatal("canonical bytes are not the encoder's output with its newline")
	}
	if d.digest != sha256Hex(d.canonical) || !strings.HasPrefix(d.digest, "sha256:") {
		t.Fatalf("digest %s", d.digest)
	}
}

func TestDecodePreview_RoundTripOfExportBytes(t *testing.T) {
	// A document encoded by the exporter's encoder previews with the
	// same bytes and so the same digest.
	_, _, d := decodeStatus(t, validBody())
	body := `{"document":` + strings.TrimSuffix(string(d.canonical), "\n") + `}`
	_, ref, d2 := decodeStatus(t, body)
	if ref != nil || !bytes.Equal(d2.canonical, d.canonical) {
		t.Fatalf("export bytes did not round-trip: %+v", ref)
	}
}

func TestDecodePreview_Refusals(t *testing.T) {
	v := validBody()
	field := func(mut func(string) string) string {
		return `{"document":{"fields":[` + mut(validField("alpha", "text", "", "")) + `],"format":"aa-field-schema/1"}}`
	}
	opt := func(opts string) string {
		return `{"document":{"fields":[` + validField("alpha", "select", opts, "") + `],"format":"aa-field-schema/1"}}`
	}
	deep := func(levels int) string {
		s := `{"children":[],"label":null,"value":"v0"}`
		for i := 1; i < levels; i++ {
			s = fmt.Sprintf(`{"children":[%s],"label":null,"value":"v%d"}`, s, i)
		}
		return `{"document":{"fields":[` + validField("alpha", "tree", "["+s+"]", "") + `],"format":"aa-field-schema/1"}}`
	}
	cases := []struct {
		name, body string
		status     int
		reason     string
		path       string
	}{
		{"not json", `{"document":`, 400, "request_invalid", "$"},
		{"invalid utf8", "{\"document\":\"\xff\"}", 400, "request_invalid", "$"},
		{"lone surrogate", `{"document":{"fields":[` + strings.Replace(validField("alpha", "text", "", ""), `"Label"`, `"\ud800x"`, 1) + `],"format":"aa-field-schema/1"}}`, 400, "request_invalid", "$"},
		{"trailing content", v + `{}`, 400, "request_invalid", "$"},
		{"second value", v + ` 1`, 400, "request_invalid", "$"},
		{"envelope not object", `[]`, 400, "request_invalid", "$"},
		{"missing document", `{"source":{"label":"x"}}`, 400, "request_invalid", "$.document"},
		{"unknown envelope member", strings.Replace(v, `"source":`, `"extra":1,"source":`, 1), 400, "request_invalid", "$.extra"},
		{"case-variant envelope key", strings.Replace(v, `"document":`, `"Document":`, 1), 400, "request_invalid", "$.Document"},
		{"duplicate envelope key", strings.Replace(v, `"source":`, `"source":{},"source":`, 1), 400, "request_invalid", "$"},
		{"source not object", strings.Replace(v, `"source":{"label":"Studio B","uri":"https://studio-b.example/fields"}`, `"source":"x"`, 1), 400, "request_invalid", "$.source"},
		{"source unknown key", strings.Replace(v, `"label":"Studio B"`, `"label":"Studio B","Label":"y"`, 1), 400, "request_invalid", "$.source.Label"},
		{"source duplicate key", strings.Replace(v, `"label":"Studio B"`, `"label":"Studio B","label":"y"`, 1), 400, "request_invalid", "$.source"},
		{"source empty label", strings.Replace(v, `"label":"Studio B"`, `"label":""`, 1), 400, "request_invalid", "$.source.label"},
		{"source label 201 runes", strings.Replace(v, `"label":"Studio B"`, `"label":"`+strings.Repeat("é", 201)+`"`, 1), 400, "request_invalid", "$.source.label"},
		{"source relative uri", strings.Replace(v, `https://studio-b.example/fields`, `/fields`, 1), 400, "request_invalid", "$.source.uri"},
		{"source ftp uri", strings.Replace(v, `https://studio-b.example/fields`, `ftp://x.example/f`, 1), 400, "request_invalid", "$.source.uri"},
		{"source uri 2049 bytes", strings.Replace(v, `https://studio-b.example/fields`, `https://x.example/`+strings.Repeat("a", 2049-18), 1), 400, "request_invalid", "$.source.uri"},
		{"unsupported format", strings.Replace(v, `aa-field-schema/1`, `aa-field-schema/2`, 1), 422, "unsupported_format", "$.document.format"},
		{"format not string", strings.Replace(v, `"aa-field-schema/1"`, `1`, 1), 422, "document_invalid", "$.document.format"},
		{"document unknown member", strings.Replace(v, `"format":"aa-field-schema/1"}`, `"format":"aa-field-schema/1","x":1}`, 1), 422, "document_invalid", "$.document.x"},
		{"document duplicate key", strings.Replace(v, `"format":"aa-field-schema/1"}`, `"format":"aa-field-schema/1","format":"aa-field-schema/1"}`, 1), 422, "document_invalid", "$.document"},
		{"no fields", `{"document":{"fields":[],"format":"aa-field-schema/1"}}`, 422, "document_invalid", "$.document.fields"},
		{"unsorted codes", `{"document":{"fields":[` + validField("beta", "text", "", "") + "," + validField("alpha", "text", "", "") + `],"format":"aa-field-schema/1"}}`, 422, "document_invalid", "$.document.fields[1].code"},
		{"duplicate code", `{"document":{"fields":[` + validField("alpha", "text", "", "") + "," + validField("alpha", "text", "", "") + `],"format":"aa-field-schema/1"}}`, 422, "document_invalid", "$.document.fields[1].code"},
		{"missing false bool", field(func(f string) string { return strings.Replace(f, `"required":false,`, ``, 1) }), 422, "document_invalid", "$.document.fields[0].required"},
		{"missing null member", field(func(f string) string { return strings.Replace(f, `"edit_tab":null,`, ``, 1) }), 422, "document_invalid", "$.document.fields[0].edit_tab"},
		{"field case-variant key", field(func(f string) string { return strings.Replace(f, `"label":`, `"Label":`, 1) }), 422, "document_invalid", "$.document.fields[0].Label"},
		{"field duplicate key", field(func(f string) string {
			return strings.Replace(f, `"label":"Label"`, `"label":"Label","label":"Other"`, 1)
		}), 422, "document_invalid", "$.document.fields[0]"},
		{"sender-local applies_to", field(func(f string) string { return strings.Replace(f, `"code":`, `"applies_to":[1],"code":`, 1) }), 422, "document_invalid", "$.document.fields[0].applies_to"},
		{"sender-local read_capability", field(func(f string) string { return strings.Replace(f, `"code":`, `"read_capability":"x","code":`, 1) }), 422, "document_invalid", "$.document.fields[0].read_capability"},
		{"sender-local status", field(func(f string) string { return strings.Replace(f, `"code":`, `"status":"active","code":`, 1) }), 422, "document_invalid", "$.document.fields[0].status"},
		{"sender-local id", field(func(f string) string { return strings.Replace(f, `"code":`, `"id":"x","code":`, 1) }), 422, "document_invalid", "$.document.fields[0].id"},
		{"bool as string", field(func(f string) string { return strings.Replace(f, `"searchable":true`, `"searchable":"true"`, 1) }), 422, "document_invalid", "$.document.fields[0].searchable"},
		{"null bool", field(func(f string) string { return strings.Replace(f, `"searchable":true`, `"searchable":null`, 1) }), 422, "document_invalid", "$.document.fields[0].searchable"},
		{"float order", field(func(f string) string { return strings.Replace(f, `"display_order":100`, `"display_order":1.5`, 1) }), 422, "document_invalid", "$.document.fields[0].display_order"},
		{"exponent order", field(func(f string) string { return strings.Replace(f, `"display_order":100`, `"display_order":1e2`, 1) }), 422, "document_invalid", "$.document.fields[0].display_order"},
		{"null label", field(func(f string) string { return strings.Replace(f, `"label":"Label"`, `"label":null`, 1) }), 422, "document_invalid", "$.document.fields[0].label"},
		{"padded label", field(func(f string) string { return strings.Replace(f, `"label":"Label"`, `"label":" Label"`, 1) }), 422, "document_invalid", "$.document.fields[0].label"},
		{"bad code", field(func(f string) string { return strings.Replace(f, `"code":"alpha"`, `"code":"Alpha"`, 1) }), 422, "document_invalid", "$.document.fields[0].code"},
		{"bad type", field(func(f string) string { return strings.Replace(f, `"type":"text"`, `"type":"colour"`, 1) }), 422, "document_invalid", "$.document.fields[0].type"},
		{"bad subject", field(func(f string) string { return strings.Replace(f, `"subject_kind":"asset"`, `"subject_kind":"team"`, 1) }), 422, "document_invalid", "$.document.fields[0].subject_kind"},
		{"blank edit tab", field(func(f string) string { return strings.Replace(f, `"edit_tab":null`, `"edit_tab":" "`, 1) }), 422, "document_invalid", "$.document.fields[0].edit_tab"},
		{"empty regexp", field(func(f string) string { return strings.Replace(f, `"regexp_filter":null`, `"regexp_filter":""`, 1) }), 422, "document_invalid", "$.document.fields[0].regexp_filter"},
		{"bad regexp", field(func(f string) string { return strings.Replace(f, `"regexp_filter":null`, `"regexp_filter":"("`, 1) }), 422, "document_invalid", "$.document.fields[0].regexp_filter"},
		{"regexp on boolean", `{"document":{"fields":[` + strings.Replace(validField("alpha", "boolean", "", ""), `"regexp_filter":null`, `"regexp_filter":"^x$"`, 1) + `],"format":"aa-field-schema/1"}}`, 422, "document_invalid", "$.document.fields[0].regexp_filter"},
		{"empty condition", field(func(f string) string {
			return strings.Replace(f, `"display_condition":null`, `"display_condition":[]`, 1)
		}), 422, "document_invalid", "$.document.fields[0].display_condition"},
		{"malformed term", field(func(f string) string {
			return strings.Replace(f, `"display_condition":null`, `"display_condition":["no operator"]`, 1)
		}), 422, "document_invalid", "$.document.fields[0].display_condition[0]"},
		{"self term", field(func(f string) string {
			return strings.Replace(f, `"display_condition":null`, `"display_condition":["alpha=x"]`, 1)
		}), 422, "document_invalid", "$.document.fields[0].display_condition[0]"},
		{"options on text", field(func(f string) string { return strings.Replace(f, `"options":null`, `"options":[]`, 1) }), 422, "document_invalid", "$.document.fields[0].options"},
		{"null options on select", `{"document":{"fields":[` + validField("alpha", "select", "null", "") + `],"format":"aa-field-schema/1"}}`, 422, "document_invalid", "$.document.fields[0].options"},
		{"option unknown key", opt(`[{"children":[],"label":null,"status":"active","value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].status"},
		{"option alias key", opt(`[{"aliases":["b"],"children":[],"label":null,"value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].aliases"},
		{"option missing children", opt(`[{"label":null,"value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].children"},
		{"option missing label", opt(`[{"children":[],"value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].label"},
		{"option empty label", opt(`[{"children":[],"label":"","value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].label"},
		{"option case key", opt(`[{"children":[],"label":null,"Value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].Value"},
		{"option duplicate key", opt(`[{"children":[],"label":null,"value":"a","value":"b"}]`), 422, "document_invalid", "$.document.fields[0].options[0]"},
		{"option duplicate slug", opt(`[{"children":[{"children":[],"label":null,"value":"a"}],"label":null,"value":"a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].children[0].value"},
		{"option padded slug", opt(`[{"children":[],"label":null,"value":" a"}]`), 422, "document_invalid", "$.document.fields[0].options[0].value"},
		{"depth 65", deep(65), 422, "document_invalid", "$.document.fields[0].options"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ref, _ := decodeStatus(t, c.body)
			if status != c.status || ref == nil || ref.Reason != c.reason {
				t.Fatalf("got %d %+v, want %d %s", status, ref, c.status, c.reason)
			}
			found := false
			for _, p := range ref.Problems {
				if strings.HasPrefix(p.Path, c.path) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no problem at %s: %+v", c.path, ref.Problems)
			}
		})
	}
	t.Run("depth 64 accepted", func(t *testing.T) {
		if status, ref, _ := decodeStatus(t, deep(64)); status != 0 {
			t.Fatalf("depth 64 refused: %+v", ref)
		}
	})
	t.Run("101 fields", func(t *testing.T) {
		parts := make([]string, 101)
		for i := range parts {
			parts[i] = validField(fmt.Sprintf("f%03d", i), "text", "", "")
		}
		status, ref, _ := decodeStatus(t, `{"document":{"fields":[`+strings.Join(parts, ",")+`],"format":"aa-field-schema/1"}}`)
		if status != http.StatusUnprocessableEntity || ref.Reason != "document_invalid" {
			t.Fatalf("101 fields: %d %+v", status, ref)
		}
	})
	t.Run("node limit", func(t *testing.T) {
		mk := func(n int) string {
			terms := make([]string, n)
			for i := range terms {
				terms[i] = fmt.Sprintf(`{"children":[],"label":null,"value":"t%d"}`, i)
			}
			return opt("[" + strings.Join(terms, ",") + "]")
		}
		// At about 41 bytes a term, 100,000 terms cannot fit in 2 MiB:
		// the node bound passes and the size bound refuses instead.
		if status, ref, _ := decodeStatus(t, mk(FieldSchemaMaxOptionNodes)); status != http.StatusUnprocessableEntity || ref.Reason != "document_too_large" {
			t.Fatalf("100000 nodes: %d %+v", status, ref)
		}
		status, ref, _ := decodeStatus(t, mk(FieldSchemaMaxOptionNodes+1))
		if status != http.StatusUnprocessableEntity || ref.Reason != "document_invalid" {
			t.Fatalf("100001 nodes: %d %+v", status, ref)
		}
	})
	t.Run("unicode preserved", func(t *testing.T) {
		body := strings.Replace(validBody(), `"label":"Europe"`, `"label":"Ünïcødé 😀 é"`, 1)
		_, ref, d := decodeStatus(t, body)
		if ref != nil || *(*d.doc.Fields[0].Options)[0].Label != "Ünïcødé 😀 é" {
			t.Fatalf("unicode changed: %+v", ref)
		}
	})
	t.Run("display_group may be empty", func(t *testing.T) {
		if status, ref, _ := decodeStatus(t, field(func(f string) string {
			return strings.Replace(f, `"display_group":"general"`, `"display_group":""`, 1)
		})); status != 0 {
			t.Fatalf("empty display_group refused: %+v", ref)
		}
	})
}

// The export enforces the same option limits, on the active projection.
func TestExport_FormatLimits(t *testing.T) {
	chain := func(levels int) []byte {
		s := `{"value":"v0"}`
		for i := 1; i < levels; i++ {
			s = fmt.Sprintf(`{"value":"v%d","children":[%s]}`, i, s)
		}
		return []byte(`{"values":[` + s + `]}`)
	}
	row := func(code string, opts []byte) FieldDefinition {
		r := validRow(code)
		r.Type, r.Options = "tree", opts
		return r
	}
	if _, ref, err := buildFieldSchemaExport([]string{"a"}, []FieldDefinition{row("a", chain(64))}); err != nil || ref != nil {
		t.Fatalf("depth 64 refused: %+v %v", ref, err)
	}
	_, ref, err := buildFieldSchemaExport([]string{"a"}, []FieldDefinition{row("a", chain(65))})
	if err != nil || ref == nil || ref.Reason != openapi.FormatLimitExceeded || len(ref.Codes) != 1 || ref.Codes[0] != "a" {
		t.Fatalf("depth 65: %+v %v", ref, err)
	}
	// A retired subtree does not travel, so it does not count.
	retired := []byte(`{"values":[{"value":"r","status":"archived","children":[` + strings.TrimSuffix(strings.TrimPrefix(string(chain(70)), `{"values":[`), `]}`) + `]}]}`)
	if _, ref, err := buildFieldSchemaExport([]string{"a"}, []FieldDefinition{row("a", retired)}); err != nil || ref != nil {
		t.Fatalf("retired deep subtree counted: %+v %v", ref, err)
	}
	flat := func(n int) []byte {
		terms := make([]string, n)
		for i := range terms {
			terms[i] = fmt.Sprintf(`"t%d"`, i)
		}
		return []byte(`{"values":[` + strings.Join(terms, ",") + `]}`)
	}
	r1, r2 := validRow("a"), validRow("b")
	r1.Type, r1.Options = "select", flat(50_000)
	r2.Type, r2.Options = "select", flat(50_001)
	_, ref, err = buildFieldSchemaExport([]string{"a", "b"}, []FieldDefinition{r1, r2})
	if err != nil || ref == nil || ref.Reason != openapi.FormatLimitExceeded || len(ref.Codes) != 0 {
		t.Fatalf("100001 nodes: %+v %v", ref, err)
	}
	// 100,000 passes the node bound; that many terms exceed 2 MiB, so
	// the size bound is what refuses.
	r2.Options = flat(50_000)
	if _, ref, err := buildFieldSchemaExport([]string{"a", "b"}, []FieldDefinition{r1, r2}); err != nil || ref == nil || ref.Reason != openapi.ExportTooLarge {
		t.Fatalf("100000 nodes: %+v %v", ref, err)
	}
}

// rev699 C1: U+0000 cannot be stored in TEXT or JSONB. The source label
// is a request problem (400); a NUL in any stored document string is a
// document problem (422) at its own path. Nothing is stripped.
func TestDecodePreview_NULRefused(t *testing.T) {
	v := validBody()
	status, ref, _ := decodeStatus(t, strings.Replace(v, `"label":"Studio B"`, `"label":"Studio\u0000B"`, 1))
	if status != 400 || ref.Reason != "request_invalid" || ref.Problems[0].Path != "$.source.label" {
		t.Fatalf("source label NUL: %d %+v", status, ref)
	}
	field := func(mut func(string) string) string {
		return `{"document":{"fields":[` + mut(validField("alpha", "text", "", "")) + `],"format":"aa-field-schema/1"}}`
	}
	opt := func(opts string) string {
		return `{"document":{"fields":[` + validField("alpha", "select", opts, "") + `],"format":"aa-field-schema/1"}}`
	}
	for name, c := range map[string]struct{ body, path string }{
		"description": {field(func(f string) string { return strings.Replace(f, `"description":""`, `"description":"a\u0000b"`, 1) }), "$.document.fields[0].description"},
		"label":       {field(func(f string) string { return strings.Replace(f, `"label":"Label"`, `"label":"La\u0000bel"`, 1) }), "$.document.fields[0].label"},
		"display_group": {field(func(f string) string {
			return strings.Replace(f, `"display_group":"general"`, `"display_group":"gen\u0000"`, 1)
		}), "$.document.fields[0].display_group"},
		"edit_tab": {field(func(f string) string { return strings.Replace(f, `"edit_tab":null`, `"edit_tab":"Ta\u0000b"`, 1) }), "$.document.fields[0].edit_tab"},
		"regexp_filter": {field(func(f string) string {
			return strings.Replace(f, `"regexp_filter":null`, `"regexp_filter":"^a\u0000$"`, 1)
		}), "$.document.fields[0].regexp_filter"},
		"condition": {field(func(f string) string {
			return strings.Replace(f, `"display_condition":null`, `"display_condition":["beta=x\u0000"]`, 1)
		}), "$.document.fields[0].display_condition[0]"},
		"option value": {opt(`[{"children":[],"label":null,"value":"a\u0000"}]`), "$.document.fields[0].options[0].value"},
		"option label": {opt(`[{"children":[],"label":"A\u0000","value":"a"}]`), "$.document.fields[0].options[0].label"},
	} {
		t.Run(name, func(t *testing.T) {
			status, ref, _ := decodeStatus(t, c.body)
			if status != 422 || ref.Reason != "document_invalid" {
				t.Fatalf("got %d %+v", status, ref)
			}
			if !slices.ContainsFunc(ref.Problems, func(p openapi.FieldSchemaImportProblem) bool {
				return p.Path == c.path && strings.Contains(p.Problem, "U+0000")
			}) {
				t.Fatalf("no U+0000 problem at %s: %+v", c.path, ref.Problems)
			}
		})
	}
	// Ordinary Unicode, a paired-surrogate emoji, tabs and newlines stay.
	body := field(func(f string) string {
		return strings.Replace(f, `"description":""`, `"description":"é 😀 tab\there\nline"`, 1)
	})
	status, ref, d := decodeStatus(t, body)
	if status != 0 || d.doc.Fields[0].Description != "é 😀 tab\there\nline" {
		t.Fatalf("ordinary text refused or changed: %d %+v", status, ref)
	}
}

// rev699 C2: only $.document and its descendants are the document.
func TestDecodePreview_DocumentPathBoundary(t *testing.T) {
	v := validBody()
	status, ref, _ := decodeStatus(t, strings.Replace(v, `"source":`, `"documentary":{"x":1,"x":2},"source":`, 1))
	if status != 400 || ref.Reason != "request_invalid" {
		t.Fatalf("duplicate inside an envelope member named like the document: %d %+v", status, ref)
	}
	status, ref, _ = decodeStatus(t, strings.Replace(v, `"format":"aa-field-schema/1"}`, `"format":"aa-field-schema/1","x":{"y":1,"y":2}}`, 1))
	if status != 422 || ref.Reason != "document_invalid" {
		t.Fatalf("duplicate inside the document: %d %+v", status, ref)
	}
	status, ref, _ = decodeStatus(t, strings.Replace(v, `{"document":{"fields":[`, `{"document":{"fields":[{"code":"a","code":"b"},`, 1))
	if status != 422 || ref.Reason != "document_invalid" {
		t.Fatalf("duplicate inside a document field: %d %+v", status, ref)
	}
	if status, ref, _ = decodeStatus(t, strings.Replace(v, `"source":`, `"documentary":{"x":`, 1)); status != 400 {
		t.Fatalf("malformed JSON: %d %+v", status, ref)
	}
}

// rev699 C4: a huge out-of-range field list is refused on its count,
// without walking its elements, and details stay capped.
func TestDecodePreview_CardinalityBeforeWalk(t *testing.T) {
	body := `{"document":{"fields":[` + strings.TrimSuffix(strings.Repeat(`{},`, 200_000), ",") + `],"format":"aa-field-schema/1"}}`
	status, ref, _ := decodeStatus(t, body)
	if status != 422 || ref.Reason != "document_invalid" || len(ref.Problems) != 1 || ref.Problems[0].Path != "$.document.fields" {
		t.Fatalf("got %d %s %d problems", status, ref.Reason, len(ref.Problems))
	}
	// 100 fields, each with many problems: refused, details capped at 100.
	parts := make([]string, 100)
	for i := range parts {
		parts[i] = `{}`
	}
	status, ref, _ = decodeStatus(t, `{"document":{"fields":[`+strings.Join(parts, ",")+`],"format":"aa-field-schema/1"}}`)
	if status != 422 || len(ref.Problems) != importMaxProblems {
		t.Fatalf("100 empty fields: %d %d problems", status, len(ref.Problems))
	}
	// One object with many unknown members: capped while accumulating.
	keys := make([]string, 5000)
	for i := range keys {
		keys[i] = fmt.Sprintf(`"k%d":1`, i)
	}
	status, ref, _ = decodeStatus(t, `{"document":{"fields":[{`+strings.Join(keys, ",")+`}],"format":"aa-field-schema/1"}}`)
	if status != 422 || len(ref.Problems) != importMaxProblems {
		t.Fatalf("5000 unknown members: %d %d problems", status, len(ref.Problems))
	}
}
