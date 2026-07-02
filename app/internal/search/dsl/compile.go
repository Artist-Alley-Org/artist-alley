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
type Filters struct {
	// Tags requires the entity carry EVERY tag in the slice (AND).
	// Empty slice = no tag filter.
	Tags []string
	// Owner requires the entity's owner_user_ref match the exact
	// value. nil = no owner filter.
	Owner *int64
	// Sensitivity is the sensitivity enum value (public / team /
	// restricted / embargo). Empty = no filter.
	Sensitivity string
	// AssetType is the asset_type name or numeric ref. Empty = no
	// filter. Engine handles the name→ref lookup.
	AssetType string
	// Extension is the file extension WITHOUT leading dot. Empty =
	// no filter.
	Extension string
	// Negations lists the terms to exclude from the match. The
	// Engine renders these as `NOT search_text @@ plainto_tsquery`
	// sub-clauses.
	Negations []string

	// Below are compiler-internal buckets consumed by the Engine's
	// SQL renderer. Kept exported-lowercase so tests in this
	// package can peek without exposing to the Engine's public
	// API surface.
	titleMatches       []string
	descriptionMatches []string
	ownerUsername      string
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
		return "plainto_tsquery('english', " + c.nextPlaceholder(x.Text) + ")", nil
	case PhraseNode:
		if strings.TrimSpace(x.Text) == "" {
			return "", nil
		}
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
		return "plainto_tsquery('english', " + c.nextPlaceholder(m.Value) + ")", nil
	case FieldDescription, FieldBody:
		c.filters.descriptionMatches = append(c.filters.descriptionMatches, m.Value)
		return "plainto_tsquery('english', " + c.nextPlaceholder(m.Value) + ")", nil
	case FieldTag:
		c.filters.Tags = append(c.filters.Tags, m.Value)
		return "", nil
	case FieldOwner:
		// owner:N — parse as int64 if numeric; else store as
		// username for the Engine to look up.
		var uid int64
		_, err := fmt.Sscanf(m.Value, "%d", &uid)
		if err == nil {
			c.filters.Owner = &uid
		}
		c.filters.ownerUsername = m.Value
		return "", nil
	case FieldSensitivity:
		c.filters.Sensitivity = m.Value
		return "", nil
	case FieldType:
		c.filters.AssetType = m.Value
		return "", nil
	case FieldExtension:
		c.filters.Extension = m.Value
		return "", nil
	}
	// Unreachable — parser's whitelist gate ensures every Field is
	// one of the above.
	return "", fmt.Errorf("dsl: compiler missing case for field %q", m.Field)
}
