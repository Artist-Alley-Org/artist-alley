// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl

import "strings"

// Serialize renders one filter VALUE as a DSL token that lexes back to
// exactly that value (#1368).
//
// # ⛔ THE RULE IS DERIVED FROM THE LEXER, NOT FROM A LIST OF CHARACTERS
//
// The tempting implementation is "quote when the value contains a space,
// a colon, a paren or a quote". That list is a SECOND copy of [Lex]'s
// word-run grammar, and a copy of a grammar is the shape ADR 0093
// decision 3 refuses: it agrees today, nothing asserts it must keep
// agreeing, and the day a delimiter is added to the lexer the copy
// silently starts emitting tokens that no longer mean what they say.
//
// So the question is asked of the lexer itself: LEX THE CANDIDATE BARE
// TOKEN and keep it only when the answer is one [TokWord] carrying the
// original bytes. Everything the lexer treats specially fails that test
// by construction, without this function having to know what any of it
// is:
//
//   - a space, tab, newline or carriage return splits the run, so more
//     than one token comes back;
//   - `:`, `(`, `)` and `"` terminate the run and emit a token of their
//     own, so the word no longer carries the whole value;
//   - `and`, `or` and `not` come back as [TokAnd] / [TokOr] / [TokNot]
//     rather than [TokWord] — ⚠️ CASE-INSENSITIVELY, so a lowercase tag
//     literally named `or` is caught here too, which a hand-written list
//     of punctuation would have missed entirely;
//   - the empty value produces only [TokEOF].
//
// Whatever the lexer starts treating specially next is covered on the
// day it changes, because this asks it rather than remembering it.
//
// # The quoted form
//
// Anything that fails the bare test is written as a [TokString] — the
// grammar's existing spelling, not a second one — with `\` and `"`
// escaped per [lexString]. Those two escapes are exactly enough for
// losslessness: every other byte is written literally and read back
// literally, and the two that are not are each spelled by a two-byte
// sequence with no other reading.
//
// The contract is `Lex(Serialize(v))` yields one token whose Value is v,
// for EVERY v. It is asserted directly, over the ugly cases and over a
// fuzz corpus, in serialize_test.go.
func Serialize(v string) string {
	if lexesAsOneBareWord(v) {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v) + 2)
	sb.WriteByte('"')
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\', '"':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// lexesAsOneBareWord reports whether v, written unquoted, lexes back to
// a single [TokWord] carrying v.
//
// This is the whole of the quoting rule. See [Serialize].
func lexesAsOneBareWord(v string) bool {
	toks, err := Lex(v)
	if err != nil {
		return false
	}
	// Lex always appends TokEOF, so a single word is exactly two tokens.
	if len(toks) != 2 || toks[1].Kind != TokEOF {
		return false
	}
	return toks[0].Kind == TokWord && toks[0].Value == v
}

// SerializeTerm renders one `field:value` term with the value quoted
// per [Serialize]. The field name is a member of [AllFields] and is
// never caller text, so it needs no quoting of its own.
func SerializeTerm(f Field, value string) string {
	return string(f) + ":" + Serialize(value)
}

// Group wraps expr in parentheses so its internal precedence cannot be
// reinterpreted by whatever it is composed with (#1368).
//
// # ⛔ WHY THIS IS NOT OPTIONAL
//
// [parser.parseAnd]'s own comment states the hazard: AND binds TIGHTER
// than OR. So conjuncting a filter onto a saved expression by appending
// it corrupts every expression whose top level is a disjunction:
//
//	saved:        cat OR dog
//	appended:     cat OR dog AND extension:png
//	which parses: cat OR (dog AND extension:png)   ⛔ WRONG — wider
//	required:     (cat OR dog) AND extension:png   ✅
//
// The expression stays OPAQUE — nothing here parses it, and nothing here
// needs to. The only requirement is that adding the canonical selection
// cannot change what the expression already meant, and a paren pair is
// the whole of that guarantee because [parser.parseFactor] already makes
// a parenthesised group one factor.
//
// An empty (or all-whitespace) expression yields the empty string rather
// than `()`, which is a parse error.
func Group(expr string) string {
	if strings.TrimSpace(expr) == "" {
		return ""
	}
	return "(" + expr + ")"
}
