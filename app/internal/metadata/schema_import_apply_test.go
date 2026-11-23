// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Strict apply envelope (#1576 S2) and the structural document context
// of the shared parser, without a database. The apply flow itself is
// tested through the production router in
// app/internal/http/fields_import_apply_test.go.

package metadata

import (
	"strings"
	"testing"
)

func TestDecodeApplyRequest(t *testing.T) {
	valid := `{"preview_id":"6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10","document_digest":"sha256:` + strings.Repeat("ab", 32) + `","decisions":{"alpha":"create","beta":"skip"}}`
	req, ref := decodeApplyRequest([]byte(valid))
	if ref != nil || req.decisions["alpha"] != "create" || len(req.decisions) != 2 {
		t.Fatalf("valid: %+v %+v", req, ref)
	}
	if decisionsDigest(map[string]string{"beta": "skip", "alpha": "create"}) != decisionsDigest(req.decisions) {
		t.Fatal("decisions digest depends on order")
	}
	for name, body := range map[string]string{
		"not json":            `{"preview_id":`,
		"trailing":            valid + `{}`,
		"not object":          `[]`,
		"missing preview_id":  strings.Replace(valid, `"preview_id":"6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10",`, ``, 1),
		"missing digest":      strings.Replace(valid, `"document_digest":"sha256:`+strings.Repeat("ab", 32)+`",`, ``, 1),
		"missing decisions":   strings.Replace(valid, `,"decisions":{"alpha":"create","beta":"skip"}`, ``, 1),
		"unknown member":      strings.Replace(valid, `"decisions":`, `"extra":1,"decisions":`, 1),
		"case member":         strings.Replace(valid, `"decisions":`, `"Decisions":`, 1),
		"duplicate member":    strings.Replace(valid, `"decisions":`, `"decisions":{},"decisions":`, 1),
		"duplicate decision":  strings.Replace(valid, `"beta":"skip"`, `"beta":"skip","beta":"create"`, 1),
		"upper-case uuid":     strings.Replace(valid, `6f6c9a7e`, `6F6C9A7E`, 1),
		"braced uuid":         strings.Replace(valid, `"6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10"`, `"{6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10}"`, 1),
		"bad digest":          strings.Replace(valid, `sha256:`, `sha512:`, 1),
		"upper digest":        strings.Replace(valid, strings.Repeat("ab", 32), strings.Repeat("AB", 32), 1),
		"decisions not obj":   strings.Replace(valid, `{"alpha":"create","beta":"skip"}`, `["alpha"]`, 1),
		"decision not string": strings.Replace(valid, `"beta":"skip"`, `"beta":1`, 1),
		"nul surrogate":       strings.Replace(valid, `"skip"`, `"\ud800"`, 1),
	} {
		if _, ref := decodeApplyRequest([]byte(body)); ref == nil || ref.Reason != "request_invalid" {
			t.Errorf("%s: %+v", name, ref)
		}
	}
}

// Envelope keys that merely look like the document stay envelope (400)
// problems; a real strict-rule break inside the document stays 422.
func TestParserDocumentContext(t *testing.T) {
	v := validBody()
	for name, body := range map[string]string{
		"document.foo key":   strings.Replace(v, `"source":`, `"document.foo":{"a":1,"a":2},"source":`, 1),
		"document[ key":      strings.Replace(v, `"source":`, `"document[0]":{"a":1,"a":2},"source":`, 1),
		"documentary key":    strings.Replace(v, `"source":`, `"documentary":{"a":1,"a":2},"source":`, 1),
		"nested document":    strings.Replace(v, `"source":`, `"x":{"document":{"a":1,"a":2}},"source":`, 1),
		"duplicate document": strings.Replace(v, `"source":`, `"document":{},"source":`, 1),
	} {
		if status, ref, _ := decodeStatus(t, body); status != 400 || ref.Reason != "request_invalid" {
			t.Errorf("%s: %d %+v", name, status, ref)
		}
	}
	inside := strings.Replace(v, `"format":"aa-field-schema/1"}`, `"format":"aa-field-schema/1","x":{"a":1,"a":2}}`, 1)
	if status, ref, _ := decodeStatus(t, inside); status != 422 || ref.Reason != "document_invalid" {
		t.Errorf("inside the document: %d %+v", status, ref)
	}
}
