// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

// Per-event view-model — #795, ADR 0081 §2 (as amended 2026-07-31).
//
// This file is the SECURITY BOUNDARY, not a convenience. An operator
// template renders against a flat, typed view-model of strings, numbers
// and scalar-valued rows — never a domain object, never anything
// carrying methods. `{{.Thing.Method}}` has nothing to reach because
// nothing in scope has methods: everything below is a map key holding a
// string, an int, or a []map[string]any of the same.
//
// The set is DOCUMENTED and FINITE. It lists exactly the keys the
// send paths already assemble for each template — verified against the
// code that builds each data map, not against what the shipped
// templates happen to reference:
//
//   - admin_test            → sysconfig.Handler.SendSMTPTestEmail
//   - notification_generic  → NotificationJobHandler.Handle base set
//   - notification_saved_search_digest / …_removed_digest
//                           → search/saved.notify base set + payload
//   - notification_digest   → email/digest.Coordinator.sendUserDigest
//   - register_verify       → http.server register-verify closure
//
// A NOTE ON "FLAT". The ADR calls the view-model "flat scalars"; two
// events (saved-search digest, activity digest) in fact carry a
// COLLECTION of scalar rows — `hits` and `items` are `[]map[string]any`
// in the code, each row a flat map of strings. Those are within the
// boundary (a map has no methods), but they are not literally flat, so
// they are declared here as collections with their own row fields
// rather than papered over. A template's `{{range .hits}}{{.title}}{{end}}`
// is safe and supported.
//
// THIS IS A COMPATIBILITY SURFACE. Renaming a field here silently
// breaks every operator template that used the old name, so it gets the
// same care as a public API — which is the argument for keeping it
// small. Add a field only when a send path actually populates it.

// FieldKind is the wire/type tag for one view-model field. Only the two
// scalar shapes the send paths actually produce exist; there is no
// object or list kind because a field is never either of those (a
// repeated group is a [FieldGroup], not a field).
type FieldKind string

const (
	// KindString is a plain string interpolation, e.g. {{.site_name}}.
	KindString FieldKind = "string"
	// KindNumber is an integer, e.g. {{.added_count}} — the count
	// fields the digests compare against literals ({{if ne .count 1}}).
	KindNumber FieldKind = "number"
)

// Field is one documented, in-scope view-model key.
type Field struct {
	Name        string    // the template key, e.g. "site_name"
	Kind        FieldKind // string | number
	Description string    // shown to the operator in the editor
}

// FieldGroup is a repeated group an operator ranges over — a slice of
// scalar-valued rows. `Fields` are the keys available INSIDE the range.
type FieldGroup struct {
	Name        string
	Description string
	Fields      []Field
}

// ViewModel is the finite field set for one template name.
type ViewModel struct {
	// Description is a plain-word summary of when the mail is sent.
	Description string
	Scalars     []Field
	Collections []FieldGroup
}

// Common scalar fields shared by more than one event. Declared once so
// the same key never drifts a description between templates.
var (
	fieldSiteName      = Field{"site_name", KindString, "The name of this site."}
	fieldSiteURL       = Field{"site_url", KindString, "This site's base URL."}
	fieldRecipientName = Field{"recipient_name", KindString, "The display name of the person receiving the mail."}
)

// notificationBase is the set NotificationJobHandler.Handle assembles
// for EVERY verb-routed template before merging the verb's payload.
var notificationBase = []Field{
	fieldRecipientName,
	{"verb", KindString, "The notification verb, e.g. comment.created."},
	{"target_kind", KindString, "The kind of thing the notification is about, e.g. asset."},
	{"target_id", KindString, "The id of the thing the notification is about."},
	fieldSiteName,
	fieldSiteURL,
}

// viewModels is the source of truth. Keys are the same stable template
// names as templates.go's constants + the shipped registry.
var viewModels = map[string]ViewModel{
	TemplateAdminTest: {
		Description: "Sent when an operator uses “send a test email” to check the mail relay.",
		Scalars: []Field{
			fieldSiteName,
			fieldSiteURL,
			fieldRecipientName,
			{"triggered_by", KindString, "Who sent the test."},
			{"triggered_at", KindString, "When the test was sent (RFC 3339 timestamp)."},
		},
	},
	TemplateNotificationGeneric: {
		Description: "The fallback notification email, used for any activity that has no template of its own.",
		Scalars:     notificationBase,
	},
	TemplateSavedSearchDigest: {
		Description: "Sent when a saved search finds new results.",
		Scalars: append(append([]Field{}, notificationBase...),
			Field{"search_name", KindString, "The name of the saved search."},
			Field{"added_count", KindNumber, "How many new results were found."},
			Field{"results_url", KindString, "A link to re-run the search."},
		),
		Collections: []FieldGroup{{
			Name:        "hits",
			Description: "The new results. Loop over these with {{range .hits}}…{{end}}.",
			Fields: []Field{
				{"title", KindString, "The result's title."},
				{"summary", KindString, "A short summary of the result."},
				{"url", KindString, "A link to the result."},
			},
		}},
	},
	TemplateSavedSearchRemovedDigest: {
		Description: "Sent when everything a saved search used to match has gone away.",
		Scalars: append(append([]Field{}, notificationBase...),
			Field{"search_name", KindString, "The name of the saved search."},
			Field{"removed_count", KindNumber, "How many results were removed."},
			Field{"results_url", KindString, "A link to re-run the search."},
		),
	},
	TemplateNotificationDigest: {
		Description: "The batched digest of recent activity, sent on the reader's chosen cadence.",
		Scalars: []Field{
			fieldRecipientName,
			fieldSiteName,
			fieldSiteURL,
			{"cadence_label", KindString, "How often this digest is sent, e.g. daily."},
			{"count", KindNumber, "How many items are in this digest."},
			{"unsubscribe_url", KindString, "A one-click link to stop these emails."},
		},
		Collections: []FieldGroup{{
			Name:        "items",
			Description: "The activity in this digest. Loop over these with {{range .items}}…{{end}}.",
			Fields: []Field{
				{"headline", KindString, "A one-line description of the activity."},
				{"url", KindString, "A link to the thing that happened."},
				{"when", KindString, "When it happened, in words."},
				{"summary", KindString, "A short summary, if any."},
			},
		}},
	},
	TemplateRegisterVerify: {
		Description: "Sent to a new account to confirm their email address.",
		Scalars: []Field{
			fieldSiteName,
			fieldSiteURL,
			fieldRecipientName,
			{"verify_url", KindString, "The click-to-confirm link."},
			{"expires_in", KindString, "How long the link is valid, in words."},
		},
	},
}

// ViewModelFor returns the documented field set for a template name.
func ViewModelFor(name string) (ViewModel, bool) {
	vm, ok := viewModels[name]
	return vm, ok
}

// sampleValues supplies readable placeholder text per known string
// field so the editor preview reads like a real email rather than
// "{{site_name}}". Any string field without an entry falls back to its
// own name; number fields always sample as 2 (so {{if ne .x 1}} takes
// its plural branch, exercising more of the template).
var sampleValues = map[string]string{
	"site_name":       "Your Site",
	"site_url":        "https://example.org",
	"recipient_name":  "Alex",
	"triggered_by":    "an operator",
	"triggered_at":    "2026-08-02T12:00:00Z",
	"verb":            "comment.created",
	"target_kind":     "asset",
	"target_id":       "a1b2c3d4",
	"search_name":     "Blue skies",
	"results_url":     "https://example.org/search",
	"cadence_label":   "daily",
	"unsubscribe_url": "https://example.org/unsubscribe",
	"verify_url":      "https://example.org/verify?token=sample",
	"expires_in":      "24 hours",
	"title":           "A sample result",
	"summary":         "A short summary of the result.",
	"url":             "https://example.org/item",
	"headline":        "Alex commented on your asset",
	"when":            "2 hours ago",
}

// sampleFieldValue returns the placeholder for one field.
func sampleFieldValue(f Field) any {
	if f.Kind == KindNumber {
		return 2
	}
	if v, ok := sampleValues[f.Name]; ok {
		return v
	}
	return f.Name
}

// SampleContext builds a data map populated with EVERY declared field
// for one template. Two jobs, one map:
//
//   - validation: executed with Option("missingkey=error") so a
//     template referencing anything OUTSIDE this set fails at save,
//     naming the field (ADR 0081 §2's fail-loud rule);
//   - preview: rendered in the admin editor's sandboxed iframe so an
//     operator sees roughly what the mail will look like.
//
// Collections carry exactly one fully-populated row: enough for a
// {{range}} to iterate and for {{.field}} inside it to resolve every
// documented key, which is what keeps a valid shipped template from
// tripping the missing-key check.
func SampleContext(name string) map[string]any {
	vm, ok := viewModels[name]
	if !ok {
		return map[string]any{}
	}
	data := make(map[string]any, len(vm.Scalars)+len(vm.Collections))
	for _, f := range vm.Scalars {
		data[f.Name] = sampleFieldValue(f)
	}
	for _, c := range vm.Collections {
		row := make(map[string]any, len(c.Fields))
		for _, f := range c.Fields {
			row[f.Name] = sampleFieldValue(f)
		}
		data[c.Name] = []map[string]any{row}
	}
	return data
}
