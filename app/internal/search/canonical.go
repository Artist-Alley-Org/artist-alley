// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// This file is the two halves of ONE contract, kept side by side so
// neither can be changed without the other being read (#1368):
//
//	SelectionToDSL   facet.Selection → canonical DSL text
//	SelectionFromDSL compiled DSL    → facet.Selection
//
// # THE CONTRACT, stated so it cannot be over-read
//
//	Every constraint present in a savable interactive search is written
//	exactly once in canonical DSL and reconstructs the SAME effective
//	facet.Selection when that DSL is compiled again.
//
// ⛔ IT IS `Selection → DSL → Selection`, AND NOT ARBITRARY BOOLEAN-DSL
// EQUIVALENCE. The compiler flattens filter terms into a single set
// regardless of the AND / OR structure they were written in — `walk`
// returns a tsquery fragment and pushes filter terms into a flat
// [dsl.Filters] as a side effect — so `extension:png OR extension:jpg`
// and `extension:png AND extension:jpg` compile to the same two-element
// slice. That is not a defect being papered over: it is structurally the
// shape a [facet.Selection] holds, which is a flat set of terms whose
// combination rule is a property of the DIMENSION (ADR 0093's 2026-08-20
// amendment) rather than of the syntax. The round trip is exact in the
// direction that is asked of it, and a hand-written boolean expression
// over filters is NOT something this promises to preserve.
//
// The free-text expression is a different matter and is preserved
// EXACTLY, as one opaque parenthesised operand — see [ComposeDSL].

// ErrDimensionNotRepresentable is returned by [SelectionToDSL] for a
// facet dimension the DSL has no spelling for.
//
// ⛔ IT IS AN ERROR AND NOT A DROP, and that is the whole point. The
// defect #1368 exists to fix is a saved search that replays WIDER than
// the search it was saved from, and silently omitting a term the caller
// had ticked reproduces it exactly — a stored query that looks like the
// one on screen and matches more. `ai`, `kind`, `collection` and
// `visibility` are registered dimensions that no savable surface emits
// today; if one ever reaches this function the save fails loudly rather
// than persisting a wider query.
var ErrDimensionNotRepresentable = errors.New("search: facet dimension has no DSL spelling")

// dslFieldForFacet maps a facet dimension to the DSL field that spells
// it. ONE table, read in both directions by the two functions below, so
// the serializer and the compiler bridge cannot come to disagree about
// which name a dimension answers to.
//
// The entries are the SAVABLE SURFACE, traced from producers rather
// than from the facet registry: `/search` reads `filter=` tokens from its
// URL, the advanced page emits `field:` and `type:`, and the rail emits
// [facet.AllFacets]. Nothing else can reach a Save button.
//
// #1173 adds `file_size:`, which is reachable from `/search`'s own URL
// the moment the dimension parses — so it is savable by construction and
// omitting it here would make [ErrDimensionNotRepresentable] fire on a
// query a caller could type, which is a save that fails rather than a
// save that replays wider, but is still a hole in the contract above.
var dslFieldForFacet = map[facet.FacetType]dsl.Field{
	facet.FacetTag:         dsl.FieldTag,
	facet.FacetOwner:       dsl.FieldOwner,
	facet.FacetSensitivity: dsl.FieldSensitivity,
	facet.FacetAssetType:   dsl.FieldType,
	facet.FacetExtension:   dsl.FieldExtension,
	facet.FacetField:       dsl.FieldField,
	facet.FacetFileSize:    dsl.FieldFileSize,

	// #1173 sprint 18c adds `workflow_state:`, reachable from
	// `/search`'s own URL the moment the dimension parses — so it is
	// savable by construction, for the same reason `file_size:` is.
	// ⛔ Omitting it would make [ErrDimensionNotRepresentable] fire on a
	// query a caller could type, and a saved query naming a state an
	// operator later deletes must stay REPRESENTABLE (it returns zero;
	// it does not become unparseable).
	facet.FacetWorkflowState: dsl.FieldWorkflowState,

	// #1173 sprint 25a adds `preview:` and `id:`, both reachable from
	// `/search`'s own URL and from the advanced page's free-text box (as
	// `!nopreviews` / `!list…`, which fold onto these before the bridge
	// sees them), so both are savable by construction. The canonical
	// spelling is ALWAYS the typed one; a verb is never written back.
	facet.FacetPreview: dsl.FieldPreview,
	facet.FacetID:      dsl.FieldID,

	// #1173 sprint 25b adds `last:`, reachable from `/search`'s URL and
	// from the free-text box as `!lastN`, which folds onto it. Savable by
	// construction; the stored spelling is `last:N`, never the verb, and
	// a saved search replays it asset-only under the executor's existing
	// contract (the window is formed over the requested types, and the
	// executor requests assets).
	facet.FacetLast: dsl.FieldLast,
}

// SelectionToDSL renders a [facet.Selection] as canonical DSL: every term
// written once, quoted by [dsl.Serialize], joined with `AND`.
//
// # Deterministic, because a re-save must not churn
//
// The terms are sorted by (field name, value) rather than kept in
// insertion order. A selection is a SET — [facet.Selection.With]
// deduplicates and [facet.Selection.CacheKey] already sorts for the same
// reason — so two rails ticked in different orders describe one query and
// must serialize to one string. Without that, loading a saved search and
// saving it again rewrites the stored DSL for no reason, and every
// equality assertion over the stored form becomes order-dependent.
//
// Returns the empty string for an empty selection.
func SelectionToDSL(sel facet.Selection) (string, error) {
	terms := sel.Terms()
	if len(terms) == 0 {
		return "", nil
	}
	rendered := make([]string, 0, len(terms))
	for _, t := range terms {
		f, ok := dslFieldForFacet[t.Type]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrDimensionNotRepresentable, string(t.Type))
		}
		rendered = append(rendered, dsl.SerializeTerm(f, t.Value))
	}
	sort.Strings(rendered)
	return strings.Join(rendered, " AND "), nil
}

// ComposeDSL is the canonical form of "this query expression, narrowed by
// this selection" — the single string a saved search stores (#1368).
//
// # ⛔ THE EXPRESSION IS ONE OPERAND, AND IT IS PARENTHESISED
//
// AND binds tighter than OR in this grammar (see dsl's parseAnd), so
// appending a filter to a saved expression whose top level is a
// disjunction silently re-associates it:
//
//	cat OR dog        + extension:png
//	naive:  cat OR dog AND extension:png
//	parses: cat OR (dog AND extension:png)   ⛔ wider than what was saved
//	here:   (cat OR dog) AND extension:png   ✅
//
// Nothing here parses the expression and nothing here needs to. It is
// carried through opaquely and wrapped, so the only claim being made is
// that ADDING the selection cannot change what the expression already
// meant.
//
// # The N=0 case is byte-for-byte, EXCEPT for verbs
//
// With no selection the expression is returned untouched — not wrapped,
// not normalised. A search with no filters therefore saves and replays
// exactly as it did before this change, which is what makes the
// browser regression's failure attributable to the filters alone.
//
// ⚠️ ONE EXCEPTION SINCE #1173 SPRINT 25a, and it is the whole of the
// exception: a `!nopreviews` or `!list…` VERB inside the expression is
// written in its canonical typed spelling (`preview:missing`,
// `(id:… AND id:…)`) by [dsl.Canonicalize] before anything else
// happens. A verb is input sugar with a defined canonical form, and the
// column stores the canonical form: ADR 0093's 18a amendment says the
// stored query is "the canonical form" and that reconstructing the
// query means reconstructing all of it. Every byte that is NOT a verb
// token is still carried through untouched, Canonicalize splices by
// lexer offset and never enters a quoted phrase, so the opacity
// argument above is unchanged for the expression proper.
func ComposeDSL(expr string, sel facet.Selection) (string, error) {
	filters, err := SelectionToDSL(sel)
	if err != nil {
		return "", err
	}
	expr, err = dsl.Canonicalize(expr)
	if err != nil {
		return "", err
	}
	expr = strings.TrimSpace(expr)
	switch {
	case filters == "":
		return expr, nil
	case expr == "":
		return filters, nil
	default:
		return dsl.Group(expr) + " AND " + filters, nil
	}
}

// SelectionFromDSL folds the compiler's typed [dsl.Filters] into a
// facet.Selection, on top of whatever the `filter=` parameter already
// contributed.
//
// ONE conversion, in one place, so the typed query and the rail cannot
// drift into meaning different things — and so that adding a dimension
// means a line here beside a line in the DSL whitelist and a case in
// facet.dimensionSQL, and nothing else.
//
// Exported for the SAVED-SEARCH executor (search/saved), which compiles
// the same DSL on a timer and has to reach the same predicate set. It
// did not before #907 because nothing did; leaving it out now would mean
// `tag:foo` narrowed the page a user saved and silently did not narrow
// the digest they get emailed from it.
//
// # ⛔ IT CANONICALISES, AND THAT IS LOAD-BEARING RATHER THAN TIDY
//
// The `filter=` path validates every value through
// [facet.FacetType.CanonicalValue] ([facet.ParseSelection] does it), and
// until #1368 this path did not — which cost nothing while the DSL could
// only name dimensions whose values are opaque text. `field:` is not one
// of those. A date bound arrives as `code>=2026-01-31`, is spliced into
// a `::TIMESTAMPTZ` cast by facet.dimensionSQL, and an unvalidated one
// raises a Postgres 22P02 MID-QUERY — a 500 on a caller's typo, which is
// exactly the failure CanonicalValue was written to prevent on the other
// path. Canonicalising also makes the round trip exact: `<=2026-01-31`
// becomes the last microsecond of that day here and on the rail, so a
// saved query and the search it was saved from name the same instant.
//
// Returns [facet.ErrBadFilter]-shaped rejection as a [dsl.DSLError] so
// the HTTP edge renders it as a 400 beside the parser's own errors.
func SelectionFromDSL(f dsl.Filters, into facet.Selection) (facet.Selection, error) {
	add := func(ft facet.FacetType, values []string) error {
		for _, v := range values {
			canonical, ok := ft.CanonicalValue(v)
			if !ok {
				return dsl.DSLError{
					Kind:    dsl.SyntaxError,
					Message: fmt.Sprintf("%s: %q is not a valid value", string(ft), v),
				}
			}
			into = into.With(ft, canonical)
		}
		return nil
	}
	// Ordered by dimension so the resulting selection's insertion order
	// is a function of the DSL alone. Nothing downstream depends on that
	// order — Selection.SQL groups and CacheKey sorts — but a stable one
	// makes the round-trip tests compare selections directly.
	for _, pair := range []struct {
		ft     facet.FacetType
		values []string
	}{
		{facet.FacetTag, f.Tags},
		{facet.FacetOwner, f.Owners},
		{facet.FacetSensitivity, f.Sensitivities},
		{facet.FacetAssetType, f.AssetTypes},
		{facet.FacetExtension, f.Extensions},
		{facet.FacetField, f.Fields},
		{facet.FacetFileSize, f.FileSizes},
		{facet.FacetWorkflowState, f.WorkflowStates},
		{facet.FacetPreview, f.Previews},
		{facet.FacetID, f.IDs},
		{facet.FacetLast, f.Lasts},
	} {
		if err := add(pair.ft, pair.values); err != nil {
			return facet.Selection{}, err
		}
	}
	// #1173 sprint 25a: the whole-selection rules, asked of the UNION
	// of what `filter=` contributed and what the DSL added, after every
	// duplicate has collapsed. This is the same [facet.Selection.Validate]
	// ParseSelection asks, which is what makes "at most 50 distinct ids"
	// one bound through `!list`, `id:` and `filter=id:` alike, and one
	// bound over a request that splits its ids across two parameters.
	if err := into.Validate(); err != nil {
		return facet.Selection{}, dsl.DSLError{
			Kind:    dsl.SyntaxError,
			Message: err.Error(),
		}
	}
	return into, nil
}
