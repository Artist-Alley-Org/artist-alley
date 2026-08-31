// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"net/http"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// foldDSL compiles a `dsl=` parameter into the two things a
// POPULATION is made of — a facet selection and a free-text string —
// and folds them into what the `q=` and `filter=` parameters already
// produced.
//
// # ⛔ ONE BRIDGE, NOT THREE
//
// [Handler.applyDSL] does this for `/search`, plus a `similar_to`
// resolution that needs the vector fetcher. `/search/facets` and
// `/search/contributors` need the same fold and nothing else: a
// suggestion list that ignored the query it is being asked about would
// describe the corpus rather than the search, and a SECOND compiler
// would be a second answer to "what does this query mean" — which is
// exactly the drift the one-representation rule exists to stop. So the
// filter half lives here and both suggestion endpoints call it.
//
// # ⚠️ `similar_to:` is compiled and NOT resolved, deliberately
//
// It is a RANKING hint rather than a predicate — `/search` hands it to
// the hybrid scorer, and the facet population has never composed it
// (`buildAssetPopulationSQL` has no vector arm at all). Dropping it here
// keeps the FILTER half of a DSL honest without inventing a vector path
// these endpoints have no use for, and a suggestion list is allowed to
// be a superset of a ranked page in a way a COUNT is not.
//
// Returns the folded selection and the text to search with. `text` is
// only replaced when the caller supplied none: `q=` and `dsl=` compose
// on `/search` the same way.
//
// ⛔ THE COMPILED FREE TEXT, NEVER THE RAW DSL STRING — #1368's lesson,
// which is that `(cat) AND extension:png` reaching `plainto_tsquery`
// makes the filter term a text requirement too and the query returns
// near-nothing.
func foldDSL(
	input string,
	into facet.Selection,
	text string,
) (facet.Selection, string, error) {
	parsed, err := dsl.Parse(input)
	if err != nil {
		return into, text, err
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		return into, text, err
	}
	sel, err := SelectionFromDSL(compiled.Filters, into)
	if err != nil {
		return into, text, err
	}
	if text == "" {
		text = compiled.FreeText
	}
	return sel, text, nil
}

// writeDSLError renders a compiler failure in the SAME shape `/search`
// does, so a client parses one error body wherever a `dsl=` is accepted.
func writeDSLError(w http.ResponseWriter, err error) {
	if de, ok := err.(dsl.DSLError); ok {
		payload := map[string]any{"error": "dsl_error", "kind": int(de.Kind), "message": de.Message}
		if len(de.ValidFields) > 0 {
			payload["valid_fields"] = de.ValidFields
		}
		writeJSON(w, http.StatusBadRequest, payload)
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dsl_parse_error"})
}
