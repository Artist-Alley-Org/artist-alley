// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// Resolving an extracted value against a field's controlled vocabulary
// ---------------------------------------------------------------------------
//
// A file says "United Kingdom". The field stores `gb`. Nothing in
// between those two facts existed until this file.
//
// IPTC 2:101 is Country-PrimaryLocationName — a LABEL, written by
// whichever human or camera filled the field in, in whatever language
// and casing they used. Our `country` field is a `tree` whose values
// are ISO 3166-1 alpha-2 leaf slugs (migration 00024), because a slug
// is what a federated peer receives and `gb` means the same thing on
// both ends where `United Kingdom` means whatever the peer's catalogue
// happens to call it.
//
// Wiring the two together WITHOUT this step would put the label in the
// slug column: an asset_field_value.value_text of "United Kingdom" on
// a field whose vocabulary contains no such slug. It would not resolve
// (resolveOptionSlugs looks up slugs), so the asset page would render
// the raw string and look approximately right, while the value was
// unaddressable — not equal to `gb`, not filterable with it, not
// meaningful to a peer. Nothing validates it today (#824), so nothing
// would have objected.
//
// So extraction resolves. On a match the SLUG is stored; on no match
// nothing is stored and the operator gets a failure row naming the
// value that did not resolve, which is the fact they need in order to
// either add the term or fix the file.
//
// # Why matching is deliberately narrow
//
// Label or slug, case-insensitive, whitespace-trimmed. Nothing else —
// no fuzzy distance, no "United Kingdom of Great Britain and Northern
// Ireland" alias table, no language mapping. A vocabulary term is an
// operator's decision about what their catalogue means; guessing which
// one a file meant is how you silently file a photograph under the
// wrong country. A near-miss surfaces as a failure row the operator
// resolves ONCE by adding a term, after which every future file with
// that spelling matches exactly. Aliases are #789's problem, where
// they can be a property of the vocabulary rather than a heuristic
// hidden in the extractor.
//
// # Which terms are matchable
//
// Active and deprecated; archived terms are not. That is the same rule
// isOfferable() applies in web/src/lib/fieldOptions.ts, and for the
// same reason: this is a WRITE path creating a new value, and a term
// retired hard enough to be archived should not be acquiring new ones.
// Deprecated stays matchable because it is still offered — it is "stop
// choosing this", not "this is wrong".

// vocabularyEntry is the decode target for one entry in a field's
// options.values. Deliberately a private mirror of
// metadata.FieldOption rather than an import of it: that type lives in
// the API-facing metadata package, which sits ABOVE this one and
// imports it. Same reason SetByDefault is duplicated rather than
// imported.
type vocabularyEntry struct {
	Value    string            `json:"value"`
	Label    string            `json:"label,omitempty"`
	Status   string            `json:"status,omitempty"`
	Children []vocabularyEntry `json:"children,omitempty"`
}

// vocabularyDoc is the shape of field_definition.options for the
// vocabulary-carrying types.
type vocabularyDoc struct {
	Values []json.RawMessage `json:"values"`
}

// resolveVocabularySlug matches one extracted string against a field's
// options document and returns the stored slug.
//
// Accepts both entry shapes the document supports — a bare slug string
// ("sRGB") and an object ({"value":"gb","label":"United Kingdom"}) —
// because both are live: the seeder writes bare slugs and migration
// 00024 writes objects. Descends into children at full depth, since
// every country slug sits one level down under a continent and slugs
// are unique across the whole tree.
//
// Returns ok=false when the document is absent, malformed, empty, or
// simply has no term for this value. The caller does not write in that
// case — see the applier.
func resolveVocabularySlug(optionsJSON []byte, extracted string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(extracted))
	if want == "" || len(optionsJSON) == 0 {
		return "", false
	}
	var doc vocabularyDoc
	if err := json.Unmarshal(optionsJSON, &doc); err != nil {
		return "", false
	}
	return walkVocabulary(doc.Values, want)
}

// walkVocabulary is resolveVocabularySlug's depth-first search. Split
// out so the recursion has one job and the entry point can own the
// normalisation.
func walkVocabulary(raw []json.RawMessage, want string) (string, bool) {
	for _, r := range raw {
		// Bare-slug form: {"values": ["srgb", "linear"]}.
		var bare string
		if err := json.Unmarshal(r, &bare); err == nil {
			if strings.EqualFold(strings.TrimSpace(bare), want) {
				return bare, true
			}
			continue
		}
		var e vocabularyEntry
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		if e.Value != "" && e.Status != "archived" {
			if strings.EqualFold(strings.TrimSpace(e.Value), want) ||
				(e.Label != "" && strings.EqualFold(strings.TrimSpace(e.Label), want)) {
				return e.Value, true
			}
		}
		if len(e.Children) > 0 {
			// Re-marshal the children so the recursion sees the same
			// RawMessage shape and keeps handling both entry forms.
			kids := make([]json.RawMessage, 0, len(e.Children))
			for _, c := range e.Children {
				b, err := json.Marshal(c)
				if err != nil {
					continue
				}
				kids = append(kids, b)
			}
			if slug, ok := walkVocabulary(kids, want); ok {
				return slug, true
			}
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Which field types extraction can write at all
// ---------------------------------------------------------------------------

// Field types the applier is able to write. The list is short because
// [FieldValueSnapshot] and [WriteAssetFieldValueParams] carry
// value_text / value_num / value_date and nothing else — there is no
// value_options and no value_ref anywhere in the extraction path.
//
// This is enforced rather than assumed because the gap is invisible
// from the wiring surface: /admin/fields will happily accept
// `iptc_keywords` as the extraction_source of the multi_select
// `keywords` field, and the pipeline would then run, validate, and
// write... a text value into a field whose reader looks at
// value_options. The result is a field that stays empty while every
// log line says it was set.
//
// So a wired-but-unwritable field is refused loudly, at apply time,
// with a failure row naming the type. `keywords` is exactly this case
// and is the reason the type is checked: its IPTC mapping is real
// (2:25 → iptc_keywords, the mapping ResourceSpace ships too) and it
// stays unwired until #789 gives the applier a multi-value column to
// put it in.
var writableFieldTypes = map[string]bool{
	"text":      true,
	"longtext":  true,
	"rich_text": true,
	"number":    true,
	"boolean":   true,
	"date":      true,
	"datetime":  true,
	// select + tree hold ONE vocabulary slug in value_text, so they
	// are writable — via resolveVocabularySlug, never verbatim.
	"select": true,
	"tree":   true,
}

// vocabularyFieldTypes are the writable types whose value is a
// vocabulary slug rather than free text, and which therefore must be
// resolved before writing.
var vocabularyFieldTypes = map[string]bool{
	"select": true,
	"tree":   true,
}
