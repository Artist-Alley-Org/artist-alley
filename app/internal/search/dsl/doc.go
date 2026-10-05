// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package dsl implements the artist-alley advanced-search query
// language. Phase 1.16.B-2.
//
// The DSL is a small, security-first grammar:
//
//	query    := expr
//	expr     := term (BOOL_OP term)*
//	term     := NOT? factor
//	factor   := LPAREN expr RPAREN | phrase | fieldMatch | verb | freeText
//	phrase   := STRING           // "quoted phrase"
//	fieldMatch := IDENT COLON (STRING | WORD)
//	verb     := "!" WORD         // an alias that FOLDS to a fieldMatch
//	                             // (or an AND chain of them) at parse
//	                             // time; see verbs.go
//	freeText := WORD             // any bare token not beginning with "!"
//	BOOL_OP  := "AND" | "OR"     // Boolean operators; NOT is a
//	                             // prefix modifier on term, not an
//	                             // infix operator
//
// Field whitelist (enforced at parse time; any other field → error):
//
//	title, description, body, tag, owner, type, sensitivity,
//	extension, similar_to, field, file_size, workflow_state,
//	preview, id, last
//
// Verbs (#1173, sprints 25a and 25b): `!nopreviews` is `preview:missing`,
// `!list<uuid>,<uuid>,...` is `id:<uuid> AND id:<uuid> AND ...`, and
// `!last<N>` is `last:N`. A verb is sugar over the typed grammar and
// nothing else: the parser folds it into the node the canonical spelling
// produces, the compiler never sees a verb, and [Canonicalize] writes the
// canonical spelling back into a stored query. `preview`, `id` and `last`
// are legal only as top-level AND terms; `NOT` or `OR` over any of them
// is a compile-time error on both spellings. `last` beside `similar_to`
// is refused as [ErrLastWithSimilarity]. The remaining dimensions keep
// the flattening described in [Filters].
//
// Compilation produces two things:
//
//  1. A ts_query STRING that is safe to hand to Postgres. The
//     string is BUILT by the compiler from the AST — user input
//     only ever reaches SQL through plainto_tsquery(...) or
//     phraseto_tsquery(...) sub-expressions, which are themselves
//     safe wrappers Postgres provides for arbitrary text.
//
//     THE COMPILER NEVER CONCATENATES USER TEXT INTO A to_tsquery
//     STRING DIRECTLY. That's the injection floor for this whole
//     subsystem — see [Compile].
//
//  2. A typed [Filters] struct carrying per-field constraints
//     (owner, sensitivity, type, extension, tag). Every user text value
//     in Filters is passed to Postgres as a $-parameter — never
//     string-interpolated.
//
// # A correction (#907)
//
// The sentence above used to end "…that the search Engine layers on the
// tsvector match via ordinary WHERE clauses". That was FALSE, and had
// been for the five releases since this package shipped. Nothing
// consumed Filters: the Engine's Query carried an `Advanced` placeholder
// documented as "nil in B-1; the engine ignores it", and search/http.go
// said in as many words that there was no Filters plumbing at the Engine
// layer. `tag:foo` parsed, compiled, and changed nothing.
//
// It is true now. The HTTP edge converts [Filters] into a
// facet.Selection — the same type the `filter=` query parameter parses
// into and the same type the facet aggregators count with — and the
// Engine renders it into both the hits and the count statements. The
// two entry points share one renderer so a typed `tag:foo` and a ticked
// `tag` bucket cannot come to mean different things.
//
// Recording the correction rather than quietly editing the line, per ADR
// 0068: a comment asserting a structural guarantee is worse than no
// comment, because it stops the next person looking. This one stopped
// several, roughly 100 lines from a comment in the same subsystem that
// said the opposite.
//
// The [SimilarToNode] AST node is parsed here and resolved to an
// embedding by the search Service (Phase 1.16.B-3); it no longer returns
// 501.
package dsl
