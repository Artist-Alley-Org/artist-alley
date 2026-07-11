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
//     (owner, sensitivity, type, extension, tag) that the search
//     Engine layers on the tsvector match via ordinary WHERE
//     clauses. Every user text value in Filters is passed to the
//     Engine as a $-parameter — never string-interpolated.
//
// The [SimilarTo] AST node is parsed but compilation returns 501
// not_implemented. Reserved for Phase 1.16.B-3 (vector search).
package dsl
