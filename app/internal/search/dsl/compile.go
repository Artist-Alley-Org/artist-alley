// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl

import (
	"fmt"
	"strings"
)

// CompiledQuery is what the compiler produces. The Engine consumes it.
//
// Phase 1.16.B-3 adds SimilarToAssetID — populated when the user
// wrote similar_to:<uuid>. The compiler doesn't perform the DB
// embedding-fetch itself (keeps the DSL package pure); the search
// Service resolves the ID → embedding just before Engine.Run.
type CompiledQuery struct {
	// TSQuery is the ts_query STRING the Engine passes to Postgres
	// as `search_text @@ (TSQuery)::tsquery`. Built by the compiler
	// from the AST — user text only ever enters via
	// plainto_tsquery('english', $N) / phraseto_tsquery('english',
	// $N) sub-expressions, where $N is a Postgres $-placeholder
	// bound to the corresponding TSQueryArgs slot.
	//
	// This is a load-bearing invariant: the compiled TSQuery
	// string contains ONLY:
	//   - literal '&', '|', '!', '(', ')'
	//   - the substrings 'plainto_tsquery(' or 'phraseto_tsquery('
	//     with an $N placeholder + closing ')'
	// No user text ever appears verbatim in this string.
	TSQuery string

	// TSQueryArgs is the []any slice bound to the $-placeholders
	// in TSQuery — passed to pgx.Query as parameters. Every user-
	// supplied text value flows through this slice.
	TSQueryArgs []any

	// Filters is the typed post-tsvector filter set the Engine
	// applies as ordinary WHERE clauses. Every text value here is
	// also passed to pgx via $-parameter, never string-interpolated.
	Filters Filters

	// FreeText is the TEXT INTENT of the query, reconstructed from the
	// nodes that contribute one: bare words, phrases, and the values of
	// `title:` / `description:` / `body:`. Joined with spaces, in source
	// order.
	//
	// # ⛔ WHY THIS EXISTS, AND WHY THE RAW DSL STRING CANNOT DO ITS JOB
	//
	// Nothing executes [CompiledQuery.TSQuery] — verified: its only
	// readers test it for emptiness. What actually runs is
	// `plainto_tsquery('english', $1)` over [search.Query.Text], and both
	// DSL callers used to set that to the WHOLE DSL STRING: search's
	// applyDSL when no `q=` accompanied it, and the saved-search executor
	// unconditionally. That was survivable while the only DSL anybody
	// stored was a bare phrase, because English stop-wording eats `and`,
	// `or` and `not` and Postgres's parser eats the punctuation, so
	// `cat OR dog` and `cat dog` produce the same lexemes.
	//
	// It stops being survivable the moment a saved query carries its
	// filters (#1368). `(cat) AND extension:png` fed to plainto_tsquery
	// yields `'cat' & 'extens' & 'png'` — the FILTER TERM BECOMES A TEXT
	// REQUIREMENT — so a replay would return near-nothing and the count
	// equality this issue is measured by could not hold in either
	// direction. The text half of the query has to be recovered from the
	// AST for the same reason the filter half is: the stored DSL is the
	// canonical form, and reconstructing the query from it means
	// reconstructing all of it.
	//
	// ⚠️ For a DSL made only of text this is byte-equivalent in EFFECT to
	// the old behaviour (the lexemes are identical). Where it differs is
	// a DSL that also carries filter terms, and there it is strictly more
	// faithful: `tag:sketch` now narrows by tag alone instead of also
	// demanding the words "tag" and "sketch" appear in the text.
	FreeText string

	// SimilarToAssetID is the asset UUID the caller wrote as
	// similar_to:<uuid>. Empty when no similar_to node appeared.
	// The search Service resolves this to the actual embedding
	// via vector.Fetcher before Engine.Run.
	//
	// Phase 1.16.B-3 addition.
	SimilarToAssetID string

	// HybridWeightSuggestion is a hint the compiler emits based
	// on DSL shape: 1.0 when similar_to is the SOLE non-filter
	// node (pure-vector intent); 0.5 when combined with
	// free-text via implicit AND (mixed intent).
	//
	// The Service can honour it OR override with the operator's
	// sysconfig default. Zero when no similar_to appeared.
	HybridWeightSuggestion float64
}

// Filters is the typed post-tsvector filter set. Empty fields
// contribute no WHERE clause.
//
// #907 converts this into a facet.Selection at the HTTP edge, which is
// what the Engine actually renders — so `tag:foo` typed in the DSL and
// `filter=tag:foo` ticked on the rail become the same predicate set and
// cannot mean two different things.
//
// ⛔ EVERY DIMENSION IS A SLICE, AND #1368 IS WHY. Four of them were
// plain strings written by a plain assignment in [compiler.walkFieldMatch],
// so `extension:png AND extension:jpg` compiled to `Extension = "jpg"`
// and the first term vanished with no error and no log line. The rail
// lets a reader tick both values, and the facet layer ORs them, so the
// collapse was reachable from a click — and once a saved search carries
// its selection as DSL it is reachable from every saved search too. Only
// `Tags` was already a slice, which is exactly why `tag:` was the one
// dimension that never lost a term.
//
// ⚠️ THE SLICE'S MEANING IS THE DIMENSION'S, NOT THIS TYPE'S: `Tags` is
// an AND (an asset carries every tag asked for) and the other four are
// ORs (an asset has exactly one extension, one sensitivity, one type and
// one owner, so an AND over two of them returns nothing forever). That
// asymmetry is decided once, in [facet.FacetType.conjunctive], and this
// type only has to avoid destroying the terms before it gets there.
//
// Every field is a STRING (or a slice of them), including the ones that
// name a numeric row. The bucket a caller ticks carries an opaque
// value — `asset_type` is a ref, `owner` is a user ref — and a human
// typing the same filter writes the NAME. One renderer accepts either
// (see facet.dimensionSQL), so nothing here has to guess which it got.
type Filters struct {
	// Tags requires the entity carry EVERY tag in the slice (AND).
	// Empty slice = no tag filter.
	Tags []string
	// Owners are the owners' numeric user_refs or usernames. Any of
	// them matches (OR). Empty = no owner filter.
	//
	// It used to be a *int64 parsed with fmt.Sscanf("%d"), which meant
	// `owner:alice` silently produced NO filter (the username was
	// stashed in an unexported field nothing outside this package could
	// read) and `owner:12abc` silently produced owner 12 — Sscanf stops
	// at the first non-digit and reports success. Neither mattered while
	// the Engine ignored Filters entirely; both are wrong the moment it
	// does not.
	Owners []string
	// Sensitivities are the sensitivity enum values (public / team /
	// restricted / embargo). Any of them matches (OR). Empty = no filter.
	Sensitivities []string
	// AssetTypes are asset_type names or numeric refs. Any of them
	// matches (OR). Empty = no filter.
	AssetTypes []string
	// Extensions are file extensions WITHOUT a leading dot. Any of them
	// matches (OR). Empty = no filter.
	Extensions []string
	// Fields are `field:` terms, carried WHOLE as `code<op>value`
	// (#1368). Opaque here on purpose — see [FieldField].
	Fields []string

	// Below are compiler-internal buckets consumed by the Engine's
	// SQL renderer. Kept exported-lowercase so tests in this
	// package can peek without exposing to the Engine's public
	// API surface.
	titleMatches       []string
	descriptionMatches []string
}

// Compile walks the AST + produces a CompiledQuery. Returns typed
// errors on unsupported nodes (SimilarTo in B-2) so the HTTP layer
// can render the right status (501 for similar_to; 400 for parse).
func Compile(q Query) (CompiledQuery, error) {
	if q.Root == nil {
		return CompiledQuery{}, nil
	}
	c := &compiler{}
	tsQ, err := c.walk(q.Root)
	if err != nil {
		return CompiledQuery{}, err
	}
	// Emit hybrid-weight suggestion when similar_to appeared:
	// pure-vector intent (weight 1.0) if the AST is a lone
	// SimilarTo or only combined with filter nodes; mixed intent
	// (weight 0.5) when the AST also produced a non-empty tsQ.
	var weightHint float64
	if c.similarToAssetID != "" {
		if strings.TrimSpace(tsQ) == "" {
			weightHint = 1.0
		} else {
			weightHint = 0.5
		}
	}
	return CompiledQuery{
		TSQuery:                tsQ,
		TSQueryArgs:            c.args,
		FreeText:               strings.Join(c.freeText, " "),
		Filters:                c.filters,
		SimilarToAssetID:       c.similarToAssetID,
		HybridWeightSuggestion: weightHint,
	}, nil
}

// compiler is the walker + argument accumulator. Stateful across
// one Compile() call.
type compiler struct {
	args    []any
	filters Filters
	// freeText accumulates the source text of every node that
	// contributes a tsquery fragment, in walk order. See
	// [CompiledQuery.FreeText].
	freeText []string
	// paramIndex is the 1-based index the next placeholder will
	// take. Bumped every time a user text value is appended to args.
	paramIndex int
	// similarToAssetID captures the first similar_to:<uuid> node
	// seen while walking the AST. Multiple similar_to's in one
	// query aren't supported; the second one wins the same slot
	// so the compiler doesn't split into two vector passes.
	similarToAssetID string
}

// nextPlaceholder allocates the next $N placeholder + appends the
// value to args. Returns the "$N" string ready for interpolation.
func (c *compiler) nextPlaceholder(v any) string {
	c.paramIndex++
	c.args = append(c.args, v)
	return fmt.Sprintf("$%d", c.paramIndex)
}

// walk renders one AST node to a ts_query fragment. Recursive.
func (c *compiler) walk(n Node) (string, error) {
	switch x := n.(type) {
	case AndNode:
		l, err := c.walk(x.Left)
		if err != nil {
			return "", err
		}
		r, err := c.walk(x.Right)
		if err != nil {
			return "", err
		}
		if l == "" && r == "" {
			return "", nil
		}
		if l == "" {
			return r, nil
		}
		if r == "" {
			return l, nil
		}
		return "(" + l + ") && (" + r + ")", nil
	case OrNode:
		l, err := c.walk(x.Left)
		if err != nil {
			return "", err
		}
		r, err := c.walk(x.Right)
		if err != nil {
			return "", err
		}
		if l == "" && r == "" {
			return "", nil
		}
		if l == "" {
			return r, nil
		}
		if r == "" {
			return l, nil
		}
		return "(" + l + ") || (" + r + ")", nil
	case NotNode:
		inner, err := c.walk(x.Inner)
		if err != nil {
			return "", err
		}
		if inner == "" {
			return "", nil
		}
		return "!!(" + inner + ")", nil
	case FreeTextNode:
		// User text ONLY enters here via plainto_tsquery($N). The
		// placeholder value is x.Text; the ts_query string contains
		// no user characters.
		if strings.TrimSpace(x.Text) == "" {
			return "", nil
		}
		c.freeText = append(c.freeText, x.Text)
		return "plainto_tsquery('english', " + c.nextPlaceholder(x.Text) + ")", nil
	case PhraseNode:
		if strings.TrimSpace(x.Text) == "" {
			return "", nil
		}
		c.freeText = append(c.freeText, x.Text)
		return "phraseto_tsquery('english', " + c.nextPlaceholder(x.Text) + ")", nil
	case FieldMatchNode:
		return c.walkFieldMatch(x)
	case SimilarToNode:
		// Phase 1.16.B-3 — the compiler records the ID; the
		// Service resolves it to an embedding + populates
		// Query.SimilarityHint. The tsQuery contribution is
		// empty (vector search runs separately from BM25).
		id := strings.TrimSpace(x.ID)
		if id == "" {
			return "", DSLError{
				Kind:    SyntaxError,
				Message: "similar_to: value must be a UUID",
			}
		}
		c.similarToAssetID = id
		return "", nil
	}
	return "", fmt.Errorf("dsl: unhandled node type %T", n)
}

// walkFieldMatch renders a `field:value` node as either a
// weight-restricted tsquery sub-expression (title / description /
// body) or a Filter set-side effect (tag / owner / etc.).
func (c *compiler) walkFieldMatch(m FieldMatchNode) (string, error) {
	switch m.Field {
	case FieldTitle:
		// Weighted-tsvector match against class A only. ts_query
		// weight restriction: term:A limits the match to positions
		// carrying weight A in the target vector. We wrap
		// plainto_tsquery + append weight-restrict via a helper
		// tsquery: `plainto_tsquery($N) && to_tsquery('_:A')` won't
		// work; instead we generate:
		//   (plainto_tsquery('english',$N) @@ setweight...) — but
		// that's the wrong direction. Simplest safe rendering:
		// treat title:X as a plain match with post-filter on title
		// column ILIKE '%X%' to guarantee the intent. Deferring
		// the perfect setweight-aware form to a future revision;
		// today's rendering matches AT LEAST what the operator
		// wanted (title contains value) without any injection risk.
		c.filters.titleMatches = append(c.filters.titleMatches, m.Value)
		c.freeText = append(c.freeText, m.Value)
		return "plainto_tsquery('english', " + c.nextPlaceholder(m.Value) + ")", nil
	case FieldDescription, FieldBody:
		c.filters.descriptionMatches = append(c.filters.descriptionMatches, m.Value)
		c.freeText = append(c.freeText, m.Value)
		return "plainto_tsquery('english', " + c.nextPlaceholder(m.Value) + ")", nil
	case FieldTag:
		c.filters.Tags = append(c.filters.Tags, m.Value)
		return "", nil
	case FieldOwner:
		// owner:<ref-or-username>, verbatim. The renderer accepts both
		// forms in one expression, so there is nothing to classify here
		// and no half-parsed number to get wrong.
		c.filters.Owners = append(c.filters.Owners, m.Value)
		return "", nil
	case FieldSensitivity:
		c.filters.Sensitivities = append(c.filters.Sensitivities, m.Value)
		return "", nil
	case FieldType:
		c.filters.AssetTypes = append(c.filters.AssetTypes, m.Value)
		return "", nil
	case FieldExtension:
		c.filters.Extensions = append(c.filters.Extensions, m.Value)
		return "", nil
	case FieldField:
		// Opaque `code<op>value`, straight through. See [FieldField].
		c.filters.Fields = append(c.filters.Fields, m.Value)
		return "", nil
	}
	// Unreachable — parser's whitelist gate ensures every Field is
	// one of the above.
	return "", fmt.Errorf("dsl: compiler missing case for field %q", m.Field)
}
