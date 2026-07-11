// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"strings"
	"testing"
)

func TestPromptRegistry_LookupShippedTemplate(t *testing.T) {
	r := NewPromptRegistry()
	tag, ok := r.Lookup(ConcernTag, "v1.0")
	if !ok {
		t.Fatal("expected tag v1.0 to be registered")
	}
	if tag.Concern != ConcernTag || tag.Version != "v1.0" {
		t.Errorf("wrong identity: %+v", tag)
	}
	if tag.Body == "" {
		t.Error("body is empty")
	}
}

func TestPromptRegistry_LookupUnknownVersion(t *testing.T) {
	r := NewPromptRegistry()
	if _, ok := r.Lookup(ConcernTag, "v999.0"); ok {
		t.Error("unknown version should miss")
	}
}

func TestPromptRegistry_DefaultVersion_PicksHighest(t *testing.T) {
	r := NewPromptRegistry()
	got, ok := r.DefaultVersion(ConcernTag)
	if !ok || got != "v1.0" {
		t.Errorf("DefaultVersion(tag) = (%q, %t), want (v1.0, true)", got, ok)
	}
}

func TestPromptRegistry_DefaultVersion_UnknownConcernMisses(t *testing.T) {
	r := NewPromptRegistry()
	// ConcernTranscribe has no built-in templates in 1.14.A (transcription
	// providers form their prompts at the wire layer, not via this registry).
	if _, ok := r.DefaultVersion(ConcernTranscribe); ok {
		t.Error("transcribe should have no built-in templates yet")
	}
}

func TestPromptRegistry_SetOverride_ReplacesBody(t *testing.T) {
	r := NewPromptRegistry()
	original, _ := r.Lookup(ConcernTag, "v1.0")
	if err := r.SetOverride(ConcernTag, "v1.0", "operator's body"); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	after, _ := r.Lookup(ConcernTag, "v1.0")
	if after.Body != "operator's body" {
		t.Errorf("body = %q, want operator's body", after.Body)
	}
	// Identity unchanged — audit rows referencing v1.0 still resolve.
	if after.Version != original.Version {
		t.Errorf("version drifted: %q vs %q", after.Version, original.Version)
	}
}

func TestPromptRegistry_SetOverride_UnknownTemplate_Errors(t *testing.T) {
	r := NewPromptRegistry()
	if err := r.SetOverride(ConcernTag, "v999.0", "body"); err == nil {
		t.Error("expected error for unknown template")
	}
}

func TestPromptRegistry_ClearOverride_RestoresBuiltin(t *testing.T) {
	r := NewPromptRegistry()
	builtin, _ := r.Lookup(ConcernTag, "v1.0")
	_ = r.SetOverride(ConcernTag, "v1.0", "override")
	r.ClearOverride(ConcernTag, "v1.0")
	after, _ := r.Lookup(ConcernTag, "v1.0")
	if after.Body != builtin.Body {
		t.Errorf("after clear: body = %q, want builtin %q", after.Body, builtin.Body)
	}
}

func TestTemplate_RenderSubstitutes(t *testing.T) {
	tpl := Template{Body: "tag {{max_tags}} please"}
	got := tpl.Render(map[string]string{"max_tags": "10"})
	if got != "tag 10 please" {
		t.Errorf("Render = %q", got)
	}
}

func TestTemplate_Render_LeavesUnknownPlaceholders(t *testing.T) {
	// Stale template body that referenced a var we no longer pass.
	// Render leaves it visible so the operator notices on next call.
	tpl := Template{Body: "x={{missing}} y={{present}}"}
	got := tpl.Render(map[string]string{"present": "OK"})
	if !strings.Contains(got, "{{missing}}") {
		t.Errorf("Render = %q, expected unsubstituted placeholder", got)
	}
	if !strings.Contains(got, "y=OK") {
		t.Errorf("Render = %q, expected present substitution", got)
	}
}
