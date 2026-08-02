// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"strconv"
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

// resolveTermList splits one joined multi-value string and resolves
// each term against the field's vocabulary.
//
// Returns the terms in the form the writer needs — the CANONICAL SLUG
// where a term matched, the ORIGINAL TEXT where it did not — and,
// separately, the ones that did not match.
//
// Passing unmatched text through rather than dropping it is what lets
// the writer create the term with a usable label: `sunset` is the slug
// but "Sunset" is what the photographer wrote, and a vocabulary of
// hyphenated lowercase slugs with no labels is a vocabulary nobody
// wants to read. The writer re-resolves every entry under the row lock
// anyway (a canonical slug matches itself), so nothing here has to be
// right — only useful.
//
// Duplicates are collapsed on the resolved form, so "Sunset, sunset"
// is one term.
func resolveTermList(optionsJSON []byte, joined string) (terms, unresolved []string) {
	seen := make(map[string]struct{})
	for _, raw := range splitTermList(joined) {
		entry := raw
		if slug, ok := resolveVocabularySlug(optionsJSON, raw); ok {
			entry = slug
		} else {
			unresolved = append(unresolved, raw)
		}
		key := slugifyTerm(entry)
		if key == "" {
			// No addressable form — a term of "!!!" is not a term.
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		terms = append(terms, entry)
	}
	return terms, unresolved
}

// quoteTerms renders a term list for a failure message. The operator
// reading /admin/extraction-failures needs to see WHICH terms did not
// resolve, not merely that some did not.
func quoteTerms(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted = append(quoted, strconv.Quote(t))
	}
	return strings.Join(quoted, ", ")
}

// ---------------------------------------------------------------------------
// Which field types extraction can write at all
// ---------------------------------------------------------------------------

// Field types the applier is able to write. Everything except
// `reference`, whose value is another asset's UUID — a thing no file's
// metadata can name, since the id is ours and not the file's.
//
// This is enforced rather than assumed because the gap is invisible
// from the wiring surface: /admin/fields will happily accept a
// canonical source on a field the applier cannot write, and the
// pipeline would then run, validate, and write... a text value into a
// column the field's reader never looks at. The result is a field that
// stays empty while every log line says it was set.
//
// So a wired-but-unwritable field is refused loudly, at apply time,
// with a failure row naming the type.
//
// `multi_select` was on the wrong side of this line until #830.
// `keywords` is the case that mattered — its IPTC mapping is real
// (2:25 → iptc_keywords, the mapping ResourceSpace ships too) — and it
// stayed unwired because [FieldValueSnapshot] and
// [WriteAssetFieldValueParams] carried value_text / value_num /
// value_date and there was no path from extraction to value_options at
// all. There is now: ValueKindTextList carries the set, the applier
// splits IPTC's comma-joined string into it, and each term resolves
// against the field's vocabulary exactly as a select's would.
var writableFieldTypes = map[string]bool{
	"text":      true,
	"longtext":  true,
	"rich_text": true,
	"number":    true,
	"boolean":   true,
	"date":      true,
	"datetime":  true,
	// select + tree hold ONE vocabulary slug in value_text;
	// multi_select holds a SET of them in value_options. All three go
	// through resolveVocabularySlug, never verbatim.
	"select":       true,
	"tree":         true,
	"multi_select": true,
}

// vocabularyFieldTypes are the writable types whose value is a
// vocabulary slug rather than free text, and which therefore must be
// resolved before writing.
var vocabularyFieldTypes = map[string]bool{
	"select":       true,
	"tree":         true,
	"multi_select": true,
}

// keywordSeparator is what a repeatable IPTC dataset arrives joined
// on. FieldIPTCKeywords is DEFINED as 2:25's repeats joined with ", "
// (app/internal/asset/metadata/iptc/iptc.go), which makes splitting it
// back apart the applier's job rather than a guess — the extractor
// chose the separator and this is the other end of that choice.
//
// Split on the comma alone rather than the comma-space, so a file
// written "a,b" by some other tool splits the same way. The trim
// afterwards absorbs the space either way.
const keywordSeparator = ","

// splitTermList turns one joined multi-value string into its terms:
// split, trimmed, empties dropped. Order is preserved; duplicates are
// NOT removed here (resolution collapses them, since two spellings of
// one term resolve to one slug).
func splitTermList(s string) []string {
	parts := strings.Split(s, keywordSeparator)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// slugifyTerm is metadata.Slugify, duplicated for the same reason
// SetByDefault and vocabularyEntry are: the exported original lives in
// the API-facing metadata package, which sits ABOVE this one.
//
// It is used ONLY to predict what slug an unresolved term will become,
// so the equal-value short-circuit can compare a term the vocabulary
// does not have yet against the slug a previous pass minted for it.
// The authoritative mint is metadata.EnsureOpenVocabularyTerms, under
// the row lock; if the two ever disagreed the cost would be one
// redundant write per extraction pass, not a wrong value.
func slugifyTerm(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	const slugMaxLen = 80
	if len(out) > slugMaxLen {
		out = strings.Trim(out[:slugMaxLen], "-")
	}
	return out
}
