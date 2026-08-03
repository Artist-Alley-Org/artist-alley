// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

import (
	"context"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	texttemplate "text/template"
)

//go:embed templates
var embeddedTemplates embed.FS

// Template names — stable strings the notification job + admin
// surfaces key off. Adding a new template: drop three files under
// templates/<name>.{subject,txt,html}.tmpl + add the constant
// here. The registry loads at package init from the embed.FS.
const (
	// TemplateAdminTest is the admin "send test email" template —
	// proves SMTP wiring works end-to-end. Variables: site_name,
	// site_url, recipient_name.
	TemplateAdminTest = "admin_test"

	// TemplateNotificationGeneric is the verb-agnostic fallback the
	// notification-email job handler uses when no per-verb template
	// is registered. Variables: site_name, site_url, recipient_name,
	// verb, target_kind. Per-verb templates land incrementally and
	// override via the templateForVerb() lookup.
	TemplateNotificationGeneric = "notification_generic"

	// TemplateRegisterVerify is the email sent by the
	// /auth/register endpoint (Phase 1.19.C) carrying the
	// click-to-verify link. Variables: site_name, site_url,
	// recipient_name, verify_url, expires_in.
	TemplateRegisterVerify = "register_verify"

	// TemplateSavedSearchDigest is the email sent by the
	// saved-search notifier (Phase 1.16.B-4) when a scheduled
	// re-run detects new hits. Named with the notification_
	// prefix so templateForVerb resolves "saved_search.digest"
	// (dots → underscores) to this template automatically.
	// Variables: site_name, site_url, recipient_name,
	// search_name, added_count, results_url, hits
	// ([]{title, summary, url}).
	TemplateSavedSearchDigest = "notification_saved_search_digest"

	// TemplateSavedSearchRemovedDigest is the Phase 1.16.B-5
	// variant sent when a saved-search's previous hit set has
	// been entirely removed (hits either deleted or newly
	// invisible to the owner). Distinct verb (per pre-audit Q4
	// finding: templateForVerb is strictly one-per-verb). Verb:
	// "saved_search.removed_digest". Variables: site_name,
	// site_url, recipient_name, search_name, removed_count,
	// results_url.
	TemplateSavedSearchRemovedDigest = "notification_saved_search_removed_digest"

	// TemplateNotificationDigest is the batched general-activity digest
	// (Phase 1.55.Y) sent by the digest coordinator when a user has one
	// or more topics on a non-immediate cadence. Variables: site_name,
	// site_url, recipient_name, cadence_label, count, unsubscribe_url,
	// items ([]{headline, url, when, summary}).
	TemplateNotificationDigest = "notification_digest"
)

// Render produces a [Message] from a registered template + the
// caller's data map. Subject and bodies are template-rendered
// from the same data so callers don't have to thread the same
// values through three render calls.
//
// Each face prefers an operator override (#795) resolved from the
// process-wide store installed at boot, and falls back to the shipped
// template when there is no override — or when an override fails to
// parse or execute, so a live send never fails for want of a working
// override (ADR 0081 §2 / ADR 0085). ctx flows through so the override
// resolve can read its cache.
func Render(ctx context.Context, name string, to []string, data map[string]any) (Message, error) {
	tpl, ok := registry[name]
	if !ok {
		return Message{}, fmt.Errorf("email.Render: unknown template %q", name)
	}
	var ov map[string]string
	var logger *slog.Logger
	if templateStore != nil {
		ov = templateStore.overridesFor(ctx, name)
		logger = templateStore.logger
	}
	subj, err := renderTextPart(name, PartSubject, ov[PartSubject], tpl.subject, data, logger)
	if err != nil {
		return Message{}, fmt.Errorf("email.Render: subject %q: %w", name, err)
	}
	text, err := renderTextPart(name, PartText, ov[PartText], tpl.text, data, logger)
	if err != nil {
		return Message{}, fmt.Errorf("email.Render: text %q: %w", name, err)
	}
	var html string
	if tpl.html != nil {
		html, err = renderHTMLPart(name, ov[PartHTML], tpl.html, data, logger)
		if err != nil {
			return Message{}, fmt.Errorf("email.Render: html %q: %w", name, err)
		}
	}
	return Message{
		To:       to,
		Subject:  strings.TrimSpace(subj),
		TextBody: text,
		HTMLBody: html,
	}, nil
}

// ShippedParts returns the raw shipped source of every part a template
// has, part → body, for the editor's "ships as" column. Never includes
// an html entry for a text-only template.
func ShippedParts(name string) (map[string]string, bool) {
	tpl, ok := registry[name]
	if !ok {
		return nil, false
	}
	out := map[string]string{
		PartSubject: tpl.subjectSrc,
		PartText:    tpl.textSrc,
	}
	if tpl.html != nil {
		out[PartHTML] = tpl.htmlSrc
	}
	return out, true
}

// TemplateNames returns every shipped template name, sorted, so the
// admin surface and its tests iterate in a stable order.
func TemplateNames() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// loadedTemplate bundles the three faces of one template plus their raw
// sources (kept so the editor can show what ships without a second copy
// on disk — the binary already embeds it).
type loadedTemplate struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template // nil = text-only template

	subjectSrc string
	textSrc    string
	htmlSrc    string
}

var registry = map[string]*loadedTemplate{}

func init() {
	must := func(err error) {
		if err != nil {
			panic("email: template registry: " + err.Error())
		}
	}
	for _, name := range []string{
		TemplateAdminTest,
		TemplateNotificationGeneric,
		TemplateRegisterVerify,
		TemplateSavedSearchDigest,
		TemplateSavedSearchRemovedDigest,
		TemplateNotificationDigest,
	} {
		must(loadInto(registry, name))
	}
}

func loadInto(reg map[string]*loadedTemplate, name string) error {
	subj, err := readFile("templates/" + name + ".subject.tmpl")
	if err != nil {
		return fmt.Errorf("%s.subject: %w", name, err)
	}
	text, err := readFile("templates/" + name + ".txt.tmpl")
	if err != nil {
		return fmt.Errorf("%s.txt: %w", name, err)
	}
	loaded := &loadedTemplate{subjectSrc: subj, textSrc: text}
	loaded.subject, err = texttemplate.New(name + ".subject").Parse(subj)
	if err != nil {
		return fmt.Errorf("%s.subject parse: %w", name, err)
	}
	loaded.text, err = texttemplate.New(name + ".txt").Parse(text)
	if err != nil {
		return fmt.Errorf("%s.txt parse: %w", name, err)
	}
	// HTML is optional — text-only templates skip the file.
	if html, err := readFile("templates/" + name + ".html.tmpl"); err == nil {
		loaded.htmlSrc = html
		loaded.html, err = htmltemplate.New(name + ".html").Parse(html)
		if err != nil {
			return fmt.Errorf("%s.html parse: %w", name, err)
		}
	}
	reg[name] = loaded
	return nil
}

func readFile(path string) (string, error) {
	b, err := fs.ReadFile(embeddedTemplates, path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
