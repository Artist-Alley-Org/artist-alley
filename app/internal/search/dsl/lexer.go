// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// MaxInputBytes caps the DSL string length. Any input longer is
// rejected before lexing to keep parse work bounded — no
// pathological Regexp / stack blowup vector.
const MaxInputBytes = 4096

// TokenKind identifies each lexeme the parser consumes.
type TokenKind int

const (
	// TokEOF marks the end of input. Terminal token in every parse.
	TokEOF TokenKind = iota
	// TokWord is a bare identifier / free-text term.
	TokWord
	// TokString is a double-quoted phrase. Inner content is
	// preserved verbatim apart from the two escapes lexString
	// documents: \" for a quote and \\ for a backslash.
	TokString
	// TokAnd / TokOr / TokNot are the boolean operators. Case-
	// insensitive at lex time.
	TokAnd
	TokOr
	TokNot
	// TokColon separates a field identifier from its value.
	TokColon
	// TokLParen / TokRParen scope groupings.
	TokLParen
	TokRParen
)

// String returns the canonical name for logging + error messages.
func (t TokenKind) String() string {
	switch t {
	case TokEOF:
		return "EOF"
	case TokWord:
		return "WORD"
	case TokString:
		return "STRING"
	case TokAnd:
		return "AND"
	case TokOr:
		return "OR"
	case TokNot:
		return "NOT"
	case TokColon:
		return "COLON"
	case TokLParen:
		return "LPAREN"
	case TokRParen:
		return "RPAREN"
	}
	return "UNKNOWN"
}

// Token is one lexeme with the source-offset range for error
// messages that point at the offending byte.
type Token struct {
	Kind  TokenKind
	Value string
	Start int
	End   int
}

// Lex tokenises input into a slice of Tokens. Returns ErrInputTooLong
// for input above the byte cap; ErrUnterminatedString for a phrase
// that never closes.
func Lex(input string) ([]Token, error) {
	if len(input) > MaxInputBytes {
		return nil, fmt.Errorf("%w: %d bytes > cap %d", ErrInputTooLong, len(input), MaxInputBytes)
	}
	tokens := make([]Token, 0, 32)
	i := 0
	for i < len(input) {
		c := input[i]

		// Skip whitespace.
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}

		switch c {
		case '(':
			tokens = append(tokens, Token{Kind: TokLParen, Value: "(", Start: i, End: i + 1})
			i++
			continue
		case ')':
			tokens = append(tokens, Token{Kind: TokRParen, Value: ")", Start: i, End: i + 1})
			i++
			continue
		case ':':
			tokens = append(tokens, Token{Kind: TokColon, Value: ":", Start: i, End: i + 1})
			i++
			continue
		case '"':
			end, val, err := lexString(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, Token{Kind: TokString, Value: val, Start: i, End: end})
			i = end
			continue
		}

		// Words are runs of non-whitespace non-punctuation. We
		// terminate on ':' so field:value doesn't require a
		// separator. Word chars: letters, digits, dash, underscore,
		// period, and any non-ASCII rune (so unicode terms lex as
		// one word). Uppercase AND/OR/NOT become boolean keywords.
		end := i
		for end < len(input) {
			ch := rune(input[end])
			if ch == ':' || ch == '(' || ch == ')' || ch == '"' {
				break
			}
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				break
			}
			end++
		}
		if end == i {
			return nil, fmt.Errorf("%w: unrecognised byte 0x%02x at position %d", ErrLexInvalid, c, i)
		}
		word := input[i:end]
		switch strings.ToUpper(word) {
		case "AND":
			tokens = append(tokens, Token{Kind: TokAnd, Value: word, Start: i, End: end})
		case "OR":
			tokens = append(tokens, Token{Kind: TokOr, Value: word, Start: i, End: end})
		case "NOT":
			tokens = append(tokens, Token{Kind: TokNot, Value: word, Start: i, End: end})
		default:
			tokens = append(tokens, Token{Kind: TokWord, Value: word, Start: i, End: end})
		}
		i = end
	}
	tokens = append(tokens, Token{Kind: TokEOF, Value: "", Start: i, End: i})
	return tokens, nil
}

// lexString reads a "quoted phrase" starting at input[startQuote].
// Returns (endIndex, unquotedValue, err). Two escapes: \" for a literal
// double-quote and \\ for a literal backslash. A backslash before any
// OTHER byte is written literally, which is what keeps a Windows path or
// a regex fragment typed straight into the box readable.
//
// # ⛔ WHY \\ HAD TO BECOME AN ESCAPE (#1368)
//
// While \" was the only escape there was NO valid quoted representation
// for a value ending in a backslash, so the grammar could not express
// every value a canonical serialization has to be able to write down.
// Traced for the value `abc\`:
//
//	"abc\"    the backslash is followed by the closing quote, so the old
//	           reading took it as an escaped quote, wrote a quote, ran past
//	           the delimiter and reached ErrUnterminatedString.
//	"abc\\"   the first backslash was followed by a backslash — not a
//	           quote — so it was written literally; the second then ate the
//	           closing quote exactly as above.
//
// The same hole swallowed any backslash placed immediately before a
// quote. #1368 makes a saved search a LOSSLESS round trip of the
// selection it was saved from ([Serialize] is the other half), and a
// value the grammar cannot spell is a hole in that contract, so the
// smallest extension that closes it lands here rather than as a
// special case in the serializer.
//
// ⚠️ THIS CHANGES THE READING OF EXISTING INPUT CONTAINING `\\`, which
// used to yield two literal backslashes and now yields one. Verified
// before the change: no test, no fixture and no seeded saved_search row
// contained one — the only `\\` in the package was this function's own
// source. Pre-release, so the reading simply changes; there is no shim.
func lexString(input string, startQuote int) (int, string, error) {
	i := startQuote + 1
	var sb strings.Builder
	for i < len(input) {
		c := input[i]
		if c == '\\' && i+1 < len(input) {
			switch input[i+1] {
			case '"':
				sb.WriteByte('"')
				i += 2
				continue
			case '\\':
				sb.WriteByte('\\')
				i += 2
				continue
			}
		}
		if c == '"' {
			return i + 1, sb.String(), nil
		}
		sb.WriteByte(c)
		i++
	}
	return startQuote, "", fmt.Errorf("%w: unterminated string starting at position %d", ErrUnterminatedString, startQuote)
}

// isWordStart returns true for runes that can begin a Word token.
// Currently unused (the lex loop's word-run rules are more
// permissive), kept as a documentation hook.
func isWordStart(r rune) bool { //nolint:unused
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// Sentinels — HTTP layer maps to 400 Bad Request.
var (
	ErrInputTooLong       = errors.New("dsl: input exceeds size cap")
	ErrLexInvalid         = errors.New("dsl: invalid input during lexing")
	ErrUnterminatedString = errors.New("dsl: unterminated quoted string")
)
