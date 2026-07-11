// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"fmt"
	"strings"
	"sync"
)

// Prompt templates are code-versioned per the brief: every template
// has a stable version string ("tag_v1.0", "caption_v1.0") so an
// operator's stored override doesn't accidentally bind to a moving
// target. Audit rows record the version used so a future-you can
// answer "which prompt produced this caption?" months later.
//
// Operator overrides live in system_config under
// `ai.prompts.<concern>.<version>` (e.g. `ai.prompts.tag.v1.0`).
// The registry checks for an override on Lookup; if present, the
// override body replaces the built-in body for THAT version. The
// version key stays stable — the audit row still records "v1.0",
// meaning "the v1.0 template, as customised by operator at time T".

// Template is one named, versioned prompt body.
type Template struct {
	Concern Concern // which surface uses it
	Version string  // e.g. "v1.0"; bumped when the SHIPPED body changes
	Body    string  // raw body; supports {{var}} interpolation via Render
}

// Render substitutes {{var}} placeholders with the supplied values.
// Unknown variables are left as-is (so a stale template body never
// crashes — the operator sees the placeholder text in the prompt and
// notices). Multi-line is fine; the body is opaque text.
func (t Template) Render(vars map[string]string) string {
	out := t.Body
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// PromptRegistry hands out templates by (concern, version), with
// per-instance operator overrides applied on the way out. The
// builtin map seeds the shipped versions; SetOverride installs an
// operator's customised body (from system_config) without changing
// the version identity.
//
// Concurrent-safe: a single registry instance is shared across
// every handler + worker. Reads dominate writes (operator overrides
// land via the admin UI on demand); the RWMutex covers both paths.
type PromptRegistry struct {
	mu        sync.RWMutex
	builtins  map[promptKey]Template
	overrides map[promptKey]string // body-only; identity stays the registered template's
}

type promptKey struct {
	Concern Concern
	Version string
}

// NewPromptRegistry seeds the built-in templates shipped with the
// binary. Add new templates here when a new prompt version goes out;
// the audit trail will reference the version string forever.
func NewPromptRegistry() *PromptRegistry {
	r := &PromptRegistry{
		builtins:  map[promptKey]Template{},
		overrides: map[promptKey]string{},
	}
	for _, t := range builtinTemplates() {
		r.builtins[promptKey{t.Concern, t.Version}] = t
	}
	return r
}

// Lookup returns the template body for (concern, version) with
// operator overrides applied. ok=false when no template (built-in
// OR override) exists for that key — the caller should fall back to
// the default version for the concern.
func (r *PromptRegistry) Lookup(concern Concern, version string) (Template, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k := promptKey{concern, version}
	t, ok := r.builtins[k]
	if !ok {
		return Template{}, false
	}
	if body, has := r.overrides[k]; has {
		t.Body = body
	}
	return t, true
}

// DefaultVersion returns the highest-numbered version shipped for
// the concern. Used when a caller omits PromptVersion in a request;
// it picks the freshest built-in.
func (r *PromptRegistry) DefaultVersion(concern Concern) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best string
	for k := range r.builtins {
		if k.Concern != concern {
			continue
		}
		// Lexicographic "v1.0" > "v0.9" is acceptable for the
		// "vM.N" shapes we ship; if we ever go to double-digit
		// minor versions we'll need a semver comparator.
		if best == "" || k.Version > best {
			best = k.Version
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// SetOverride installs an operator's customised body for an existing
// template version. Returns an error if no template is registered
// for (concern, version) — operators can't fabricate versions; they
// override what's shipped.
func (r *PromptRegistry) SetOverride(concern Concern, version, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := promptKey{concern, version}
	if _, ok := r.builtins[k]; !ok {
		return fmt.Errorf("ai: no built-in template for %s/%s", concern, version)
	}
	r.overrides[k] = body
	return nil
}

// ClearOverride removes a previously-installed override. The
// built-in body becomes effective again on next Lookup.
func (r *PromptRegistry) ClearOverride(concern Concern, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.overrides, promptKey{concern, version})
}

// builtinTemplates is the single source of truth for shipped prompt
// versions. Audit rows reference the version string forever, so:
//
//   - Never delete a version that has shipped — bump to a new one.
//   - Editing the body of a shipped version is fine (small typo,
//     wording cleanup); the audit-recorded version string stays
//     valid (it identifies "v1.0 as compiled into the binary").
//   - The version naming convention is "v<major>.<minor>"; bump
//     minor for tweaks, major for incompatible shape changes (e.g.
//     adding tool-call instructions, changing output format).
func builtinTemplates() []Template {
	return []Template{
		{
			Concern: ConcernTag,
			Version: "v1.0",
			Body: `Analyse this image and return up to {{max_tags}} concise tags ` +
				`describing its content. Return one tag per line, lowercase, no ` +
				`punctuation. Focus on subjects, environments, mediums, and ` +
				`distinctive styles — skip generic words like "image" or "photo".`,
		},
		{
			Concern: ConcernCaption,
			Version: "v1.0",
			Body: `Write a single-sentence caption for this image, between 80 and ` +
				`{{max_length}} characters. Style: {{style_hint}}. ` +
				`Skip preamble; return only the caption text.`,
		},
	}
}
