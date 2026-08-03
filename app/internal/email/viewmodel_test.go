// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

import (
	"errors"
	"strings"
	"testing"
)

// TestShippedTemplatesValidateAgainstViewModel is the load-bearing
// guard on the view-model: every face of every shipped template must
// pass the exact save-time validation an operator override goes through
// — parse, then execute against the event's sample context with
// missingkey=error. If a shipped template references a field the
// view-model does not declare, this fails; if the view-model declares a
// field the template can range over but the sample context omits it,
// this fails. It pins the documented field set (viewmodel.go) to what
// the send paths actually assemble.
func TestShippedTemplatesValidateAgainstViewModel(t *testing.T) {
	for _, name := range TemplateNames() {
		if _, ok := ViewModelFor(name); !ok {
			t.Errorf("shipped template %q has no declared view-model", name)
			continue
		}
		parts, ok := ShippedParts(name)
		if !ok {
			t.Fatalf("ShippedParts(%q) missing", name)
		}
		for part, body := range parts {
			if err := ValidateOverride(name, part, body); err != nil {
				t.Errorf("shipped %s/%s does not validate against its view-model: %v", name, part, err)
			}
		}
	}
}

func TestValidateOverride_UnknownFieldNamesIt(t *testing.T) {
	err := ValidateOverride(TemplateAdminTest, PartSubject, "Hello {{.definitely_not_a_field}}")
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want ErrUnknownField, got %v", err)
	}
	if !strings.Contains(err.Error(), "definitely_not_a_field") {
		t.Errorf("error must NAME the field, got %q", err.Error())
	}
}

// A field valid for one event but not another must be rejected for the
// event that does not carry it — proof the boundary is per-event, not a
// global union of every key.
func TestValidateOverride_FieldFromAnotherEventRejected(t *testing.T) {
	// verify_url exists for register_verify but not for admin_test.
	err := ValidateOverride(TemplateAdminTest, PartText, "Go to {{.verify_url}}")
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want ErrUnknownField for cross-event field, got %v", err)
	}
	if !strings.Contains(err.Error(), "verify_url") {
		t.Errorf("error must name verify_url, got %q", err.Error())
	}
	// …and it IS accepted for the event that supplies it.
	if err := ValidateOverride(TemplateRegisterVerify, PartText, "Go to {{.verify_url}}"); err != nil {
		t.Errorf("verify_url should be valid for register_verify: %v", err)
	}
}

func TestValidateOverride_ParseErrorRejected(t *testing.T) {
	err := ValidateOverride(TemplateAdminTest, PartSubject, "unterminated {{ .site_name")
	if !errors.Is(err, ErrTemplateParse) {
		t.Fatalf("want ErrTemplateParse, got %v", err)
	}
}

func TestValidateOverride_CollectionFieldsInScope(t *testing.T) {
	// A range over hits + its row fields is legal for the saved-search
	// digest — the collection and its sub-fields are declared.
	body := "{{range .hits}}{{.title}} — {{.summary}} ({{.url}})\n{{end}}"
	if err := ValidateOverride(TemplateSavedSearchDigest, PartText, body); err != nil {
		t.Errorf("hits collection fields should validate: %v", err)
	}
	// A sub-field the row does not carry is still refused.
	bad := "{{range .hits}}{{.author}}{{end}}"
	if err := ValidateOverride(TemplateSavedSearchDigest, PartText, bad); !errors.Is(err, ErrUnknownField) {
		t.Errorf("unknown row field should be ErrUnknownField, got %v", err)
	}
}

func TestValidateOverride_UnknownTemplateAndPart(t *testing.T) {
	if err := ValidateOverride("no_such_template", PartSubject, "x"); !errors.Is(err, ErrUnknownTemplate) {
		t.Errorf("want ErrUnknownTemplate, got %v", err)
	}
	if err := ValidateOverride(TemplateAdminTest, "footer", "x"); !errors.Is(err, ErrUnknownPart) {
		t.Errorf("want ErrUnknownPart, got %v", err)
	}
}

// TestRenderPart_FallsBackToShipped proves the send-time belt-and-braces
// rule: an override that errors at execute time does NOT fail the send —
// the shipped template renders instead (ADR 0081 §2 / ADR 0085).
func TestRenderPart_FallsBackToShipped(t *testing.T) {
	tpl := registry[TemplateAdminTest]
	data := map[string]any{
		"site_name":      "Studio Alpha",
		"site_url":       "https://art.example.com",
		"recipient_name": "Pat",
		"triggered_by":   "admin",
		"triggered_at":   "2026-08-02T00:00:00Z",
	}
	// ".site_name.nope" parses (a field chain) but errors at execute
	// because site_name is a string, not a map — the exact "stored but
	// broken" shape the fallback exists for.
	got, err := renderTextPart(TemplateAdminTest, PartSubject, "{{.site_name.nope}}", tpl.subject, data, nil)
	if err != nil {
		t.Fatalf("renderTextPart returned error instead of falling back: %v", err)
	}
	var want strings.Builder
	if e := tpl.subject.Execute(&want, data); e != nil {
		t.Fatalf("shipped execute: %v", e)
	}
	if got != want.String() {
		t.Errorf("fallback = %q, want shipped %q", got, want.String())
	}
}
