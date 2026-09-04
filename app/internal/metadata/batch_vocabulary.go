// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE PREVIEW'S VOCABULARY PASS — resolution WITHOUT mutation (#1173,
// #1119, ADR 0019, ADR 0092).
//
// # Why the shipped entry point cannot be reused here
//
// openOrCheckVocabulary is the whole write policy for one value save,
// and it MUTATES. Its own doc says so: it "MUST be called inside the
// write's transaction: a rejection rolls the created terms back with
// the value", and on an open field it calls EnsureOpenVocabularyTerms,
// which takes a row lock and REWRITES the options document.
//
// A preview writes nothing. Calling it would mean an operator who asked
// "what would this do" had already grown the catalogue by asking.
//
// # But the preview must still bind the EXACT canonical slugs
//
// Not approximately, and not "the apply will sort it out". The preview
// reports a would_change count the operator confirms with a typed
// number, and it binds a value into a token the apply stores verbatim.
// If the preview resolved "Sunset" one way and the apply resolved it
// another, the confirmed count would describe a different operation
// from the committed one.
//
// So this is the SAME resolution the mint path performs, with the
// creation removed: indexVocabulary plus resolveOrMint plus Slugify,
// which between them already handle every case — the alias, the merge
// tombstone, the casing variant, the archived term with no forwarding
// address, and the genuinely new term. Nothing here re-implements
// matching; it declines to write.
//
// # PHANTOM WOULD_CHANGE — the case this exists to prevent
//
// An operator appends the term "Sunset " to a keyword field. It looks
// new. Its canonical slug is `sunset`, which some targets already hold.
// A preview that reported those targets as would_change, and an apply
// that then found nothing to do on them, would have shown the operator
// a number that was never true and confirmed it with a count that was
// never right. Resolving to the canonical slug BEFORE partitioning is
// what makes the two agree.
package metadata

import (
	"errors"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// batchVocabulary is one batch's settled vocabulary verdict — the
// BATCH-WIDE half of the rule.
//
// The lifecycle half SPLITS, and this is the part that does not depend
// on any target: canonicalisation, unknown slugs and mintability are
// properties of the field's document and the caller's grants, so they
// are settled once. The deprecated-or-archived grandfather test needs
// the target's own held set — the same slug is grandfathered on a
// target holding it and refused on a sibling that does not — so that
// half lives in retiredNotHeld and produces a target-level `refused`.
type batchVocabulary struct {
	// Slugs is the canonical, deduped set in arrival order, exactly as
	// the row will store it.
	Slugs []string

	// Mintable are slugs that do not yet exist and would be created.
	// Non-empty means the apply needs `fields.vocabulary.extend`, which
	// it re-checks against CURRENT effective permission.
	Mintable []string

	// Status is every incoming slug's lifecycle state in the document,
	// so the per-target grandfather test needs no second read. A slug
	// absent from the map is a mintable one, which has no status yet.
	Status map[string]OptionStatus
}

// resolveBatchVocabulary canonicalises a batch's incoming terms without
// touching the options document.
//
// `canExtend` is the caller's `fields.vocabulary.extend`, and it is
// SEPARATE from whether the FIELD is open. Told apart because the two
// produce different answers to the same symptom: a term that will not
// be created because the field is closed is a property of the field
// (422 unknown_slug, and the operator picks another word), and one that
// will not be created because this caller may not extend is a property
// of the caller (403 vocabulary_extend_required, and the fix is a
// grant). A client that could not tell them apart could not say which.
//
// ⚠️ open_vocabulary is honoured for `multi_select` ONLY —
// openVocabularyApplies narrows it, and `select` and `tree` therefore
// ALWAYS take the closed branch however the flag is set.
func resolveBatchVocabulary(
	f FieldDefinition,
	incoming []string,
	canExtend bool,
) (batchVocabulary, error) {
	out := batchVocabulary{Status: map[string]OptionStatus{}}
	if len(incoming) == 0 {
		// vocabularySlugs returns nil for `select` and `tree` when the
		// value is nil or the EMPTY STRING, and that is the whole
		// reason "" and "   " behave differently on those types: ""
		// never enters this pipeline at all, while "   " enters it as
		// the slug "   ", matches nothing, and is refused as unknown.
		// The single-target writer has exactly this shape and the batch
		// reproduces it rather than tidying it.
		return out, nil
	}
	switch f.Type {
	case "select", "multi_select", "tree":
	default:
		out.Slugs = incoming
		return out, nil
	}

	values, _, err := decodeOptionValues(f.Options)
	if err != nil && !errors.Is(err, errNoValues) {
		return out, err
	}
	idx := indexVocabulary(values)

	openHere := openVocabularyApplies(f.Type, f.OpenVocabulary)

	seen := make(map[string]struct{}, len(incoming))
	for _, raw := range incoming {
		// A whitespace-only term is DROPPED, not refused — the shipped
		// behaviour, and it is the right one: a trailing comma in a
		// pasted IPTC keyword list must not fail a whole batch.
		// Something that is not empty but has no alphanumerics at all
		// is different and IS refused, because it has no addressable
		// form and never will.
		if trimmedIsEmpty(raw) {
			continue
		}

		// mint=openHere, NOT mint=(openHere && canExtend). Resolution
		// and permission are separated so a refusal can say which one
		// applied: asking with mint=false on an open field would report
		// every new term as unknown even for a caller who may create
		// them.
		slug, matched, rej := idx.resolveOrMint(raw, openHere)
		if rej != nil {
			switch {
			case rej.Status == OptionArchived:
				return out, refuse(422, openapi.BatchArchivedSlug,
					"%s: %q names an archived term with no replacement; un-archive it or choose another",
					f.Code, rej.Slug).withField(f.Code).withOption(rej.Slug)
			default:
				return out, refuse(422, openapi.BatchUnknownSlug,
					"%s: %q is not a term in this field's vocabulary",
					f.Code, rej.Slug).withField(f.Code).withOption(rej.Slug)
			}
		}

		// `matched` is resolveOrMint's own answer to "did this term
		// already exist", and it is the authoritative one: it is true
		// for a direct hit, for a casing or whitespace variant, for an
		// alias and for a merge tombstone, and false ONLY where the
		// term is genuinely new. Re-deriving existence from a slug
		// lookup would get the alias and tombstone cases wrong.
		if !matched {
			if !canExtend {
				// The field IS open and this term WOULD be created, and
				// this caller may not create it. A different answer
				// from the closed field's, because the fix is a grant
				// rather than a correction.
				//
				// Re-checked at apply against CURRENT effective
				// permission — this verdict is a report, never an
				// authority.
				return out, refuse(403, openapi.BatchVocabularyExtendRequired,
					"%s: %q would create a new term, and you do not hold %s",
					f.Code, raw, CapVocabularyExtend).withField(f.Code).withOption(slug)
			}
			out.Mintable = appendUnique(out.Mintable, slug)
		}

		// DEDUPE ON THE CANONICAL SLUG, order preserved. "Sunset,
		// sunset" is one term twice, and an alias beside its own target
		// is one term twice too — which is exactly the case a dedupe on
		// the RAW term would miss.
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		out.Slugs = append(out.Slugs, slug)
	}

	// Every resolved slug's lifecycle status, for the PER-TARGET
	// grandfather test. Through resolveOptionSlugs rather than a walk
	// of the top level, because it descends the WHOLE option tree —
	// slugs are unique tree-wide and a `tree` value legitimately names
	// a node at any depth — and because it normalises a bare-string
	// entry's absent status to active, which a hand-rolled read would
	// get backwards and refuse every un-statused term as retired.
	for slug, resolved := range resolveOptionSlugs(f.Options, out.Slugs) {
		out.Status[slug] = resolved.Status
	}

	if len(out.Slugs) == 0 {
		// Every term was whitespace. buildBatchValue has already
		// refused an empty option set, so arriving here means the
		// client sent something that looked like a value and was not
		// one; writing it would leave a multi_select row holding NULL.
		return out, refuse(422, openapi.BatchUnknownSlug,
			"%s: the value carries no usable terms", f.Code).withField(f.Code)
	}
	return out, nil
}

func trimmedIsEmpty(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		default:
			return false
		}
	}
	return true
}

func appendUnique(dst []string, s string) []string {
	for _, existing := range dst {
		if existing == s {
			return dst
		}
	}
	return append(dst, s)
}
