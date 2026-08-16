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
//	factor   := LPAREN expr RPAREN | phrase | fieldMatch | freeText
//	phrase   := STRING           // "quoted phrase"
//	fieldMatch := IDENT COLON (STRING | WORD)
//	freeText := WORD             // any bare token
//	BOOL_OP  := "AND" | "OR"     // Boolean operators; NOT is a
//	                             // prefix modifier on term, not an
//	                             // infix operator
//
// Field whitelist (enforced at parse time; any other field → error):
//
//	title, description, body, tag, owner, type, sensitivity,
//	extension, similar_to
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
