// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl

import (
	"errors"
	"fmt"
	"strings"
)

// Field is the whitelisted set of `field:value` prefixes the parser
// accepts. Any other identifier before a colon returns ErrUnknownField
// listing every valid choice. This is the load-bearing check that
// keeps the DSL safe: unknown fields can't smuggle raw text into the
// compiler.
type Field string

const (
	FieldTitle       Field = "title"
	FieldDescription Field = "description"
	FieldBody        Field = "body" // alias for description on posts
	FieldTag         Field = "tag"
	FieldOwner       Field = "owner"
	FieldType        Field = "type"        // asset_type name / id
	FieldSensitivity Field = "sensitivity" // public / team / restricted / embargo
	FieldExtension   Field = "extension"
	FieldSimilarTo   Field = "similar_to" // reserved — compilation returns 501
	// FieldField is the `field:` dimension — the FAMILY of
	// operator-defined metadata fields (ADR 0092 / #1165), carried as one
	// opaque `code<op>value` token exactly as the `filter=field:…` wire
	// form carries it.
	//
	// ⛔ THE VALUE IS NOT PARSED HERE, deliberately. [facet.SplitFieldTerm]
	// and [facet.FacetType.CanonicalValue] are the single authority for
	// what a field code and an operator mean; a second reading in this
	// package is the "two implementations that agree today" shape ADR 0093
	// decision 3 refuses, and it is what would let a date bound canonicalise
	// one way on the rail and another way in a saved query. So the compiler
	// carries the token whole and the facet layer decides what it says.
	FieldField Field = "field"
	// FieldFileSize is the `file_size:` dimension — a BARE BOUND with
	// the operator leading, `file_size:>=12345` (#1173, sprint 18b).
	//
	// ⛔ THE VALUE IS NOT PARSED HERE either, for [FieldField]'s reason.
	// [facet.SplitOrderedBound] and [facet.FacetType.CanonicalValue] are
	// the single authority for what a bound means, and a second reading
	// in this package is the "two implementations that agree today"
	// shape ADR 0093 decision 3 refuses.
	//
	// ⭐ IT NEEDS NO LEXER CHANGE, and that is worth stating because it
	// is the property that made this dimension cheap. `>` and `=` are
	// not delimiters in [Lex]'s word run — it breaks on whitespace and
	// on `:` `(` `)` `"` and nothing else — so `>=12345` lexes as one
	// [TokWord], which [parseFieldMatch] already accepts as a value and
	// which [Serialize] therefore emits UNQUOTED.
	FieldFileSize Field = "file_size"
	// FieldWorkflowState is the `workflow_state:` dimension — an
	// asset's workflow state, spelled by its NATURAL KEY
	// `<domain>/<code>`, or the reserved literal `none` (#1173,
	// sprint 18c).
	//
	// ⛔ THE VALUE IS NOT PARSED HERE either, for [FieldField]'s reason.
	// [facet.FacetType.CanonicalValue] is the single authority for what
	// a workflow-state identity means, including where the domain ends.
	//
	// ⚠️ IT NEEDS NO LEXER CHANGE AND IT IS THE FIRST NON-`field:`
	// DIMENSION WHOSE CANONICAL SPELLING IS QUOTED. An asset domain is
	// `asset:<ref>`, so the value carries a `:` — which DOES terminate
	// [Lex]'s word run. [Serialize] finds that out by lexing its own
	// candidate token rather than by consulting a list of characters,
	// so it emits `workflow_state:"asset:1/published"` with no rule
	// added anywhere. The same machinery covers a code carrying
	// whitespace, a quote or a backslash, which #897 permits.
	FieldWorkflowState Field = "workflow_state"
)

// AllFields is the whitelist. Exposed so error responses can list
// every valid choice.
var AllFields = []Field{
	FieldTitle, FieldDescription, FieldBody, FieldTag, FieldOwner,
	FieldType, FieldSensitivity, FieldExtension, FieldSimilarTo,
	FieldField, FieldFileSize, FieldWorkflowState,
}

// ParseField normalises a case-insensitive identifier to its
// canonical Field, or (empty, false) for anything not in the
// whitelist.
func ParseField(s string) (Field, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "title":
		return FieldTitle, true
	case "description", "descr":
		return FieldDescription, true
	case "body":
		return FieldBody, true
	case "tag", "tags":
		return FieldTag, true
	case "owner", "author":
		return FieldOwner, true
	case "type":
		return FieldType, true
	case "sensitivity":
		return FieldSensitivity, true
	case "extension", "ext":
		return FieldExtension, true
	case "similar_to", "similarto":
		return FieldSimilarTo, true
	case "field":
		return FieldField, true
	case "file_size", "filesize":
		return FieldFileSize, true
	case "workflow_state":
		return FieldWorkflowState, true
	}
	return "", false
}

// Node is the AST interface. Every parse tree node implements it.
// Types: [AndNode], [OrNode], [NotNode], [FreeTextNode],
// [PhraseNode], [FieldMatchNode], [SimilarToNode].
type Node interface {
	isNode()
}

// AndNode / OrNode — boolean composition of children.
type AndNode struct{ Left, Right Node }
type OrNode struct{ Left, Right Node }
type NotNode struct{ Inner Node }

// FreeTextNode is a bare word or free-standing string that hasn't
// been scoped to a field. Compiled via plainto_tsquery.
type FreeTextNode struct{ Text string }

// PhraseNode is a "quoted phrase". Compiled via phraseto_tsquery,
// which preserves inter-term ordering.
type PhraseNode struct{ Text string }

// FieldMatchNode is `field:value`. Value is a single word or a
// quoted string.
type FieldMatchNode struct {
	Field Field
	Value string
}

// SimilarToNode reserves the DSL keyword for Phase 1.16.B-3 vector
// search. Compilation returns ErrSimilarToNotImplemented in B-2.
type SimilarToNode struct{ ID string }

func (AndNode) isNode()        {}
func (OrNode) isNode()         {}
func (NotNode) isNode()        {}
func (FreeTextNode) isNode()   {}
func (PhraseNode) isNode()     {}
func (FieldMatchNode) isNode() {}
func (SimilarToNode) isNode()  {}

// Query is the outer AST wrapper. Returned by Parse; consumed by
// Compile.
type Query struct {
	Root Node
	// Raw is the original DSL string. Kept for debug + logging;
	// NEVER passed to Compile as input.
	Raw string
}

// Parse tokenises + parses input into a Query. Empty input returns
// (Query{Root: nil}, nil) — the caller treats this as "no DSL,
// use free-text path". Malformed input returns a typed DSLError so
// the HTTP handler can render the whitelist / position hint.
func Parse(input string) (Query, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return Query{Raw: input}, nil
	}
	tokens, err := Lex(trimmed)
	if err != nil {
		return Query{}, err
	}
	p := &parser{tokens: tokens}
	node, err := p.parseExpr()
	if err != nil {
		return Query{}, err
	}
	if p.peek().Kind != TokEOF {
		return Query{}, fmt.Errorf("%w: unexpected %s at position %d",
			ErrParseUnexpected, p.peek().Kind, p.peek().Start)
	}
	return Query{Root: node, Raw: input}, nil
}

// parser is the tiny hand-rolled top-down parser. Precedence (highest
// first): parens > NOT > (implicit) AND > OR. Matches every DSL a
// human is likely to type without introducing surprises.
type parser struct {
	tokens []Token
	pos    int
}

func (p *parser) peek() Token { return p.tokens[p.pos] }

func (p *parser) advance() Token {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

// parseExpr — OR at the top so it binds loosest.
func (p *parser) parseExpr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TokOr {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = OrNode{Left: left, Right: right}
	}
	return left, nil
}

// parseAnd — AND binds tighter than OR. Bare adjacent terms
// (implicit AND) are treated as if separated by AND.
func (p *parser) parseAnd() (Node, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		k := p.peek().Kind
		if k == TokAnd {
			p.advance()
			right, err := p.parseTerm()
			if err != nil {
				return nil, err
			}
			left = AndNode{Left: left, Right: right}
			continue
		}
		// Implicit AND on adjacent factors — but ONLY when the next
		// token can start a factor. Avoids swallowing trailing OR /
		// EOF / RPAREN.
		if k == TokWord || k == TokString || k == TokLParen || k == TokNot {
			right, err := p.parseTerm()
			if err != nil {
				return nil, err
			}
			left = AndNode{Left: left, Right: right}
			continue
		}
		break
	}
	return left, nil
}

// parseTerm — optional NOT prefix + factor.
func (p *parser) parseTerm() (Node, error) {
	if p.peek().Kind == TokNot {
		p.advance()
		inner, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return NotNode{Inner: inner}, nil
	}
	return p.parseFactor()
}

// parseFactor — parens | phrase | field:value | free-text word.
func (p *parser) parseFactor() (Node, error) {
	t := p.peek()
	switch t.Kind {
	case TokLParen:
		p.advance()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != TokRParen {
			return nil, fmt.Errorf("%w: expected ')' at position %d", ErrParseUnexpected, p.peek().Start)
		}
		p.advance()
		return inner, nil
	case TokString:
		p.advance()
		return PhraseNode{Text: t.Value}, nil
	case TokWord:
		// Look ahead for ':' — if present it's a field-match.
		if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].Kind == TokColon {
			return p.parseFieldMatch()
		}
		p.advance()
		return FreeTextNode{Text: t.Value}, nil
	case TokEOF:
		return nil, fmt.Errorf("%w: unexpected end of input", ErrParseUnexpected)
	}
	return nil, fmt.Errorf("%w: unexpected %s at position %d", ErrParseUnexpected, t.Kind, t.Start)
}

// parseFieldMatch — WORD COLON (WORD | STRING). Field must be in
// the whitelist; unknown → ErrUnknownField with the whitelist in
// the error's ValidFields slot so the HTTP handler renders it.
func (p *parser) parseFieldMatch() (Node, error) {
	fieldTok := p.advance()
	if p.peek().Kind != TokColon {
		return nil, fmt.Errorf("%w: expected ':' after field", ErrParseUnexpected)
	}
	p.advance()
	field, ok := ParseField(fieldTok.Value)
	if !ok {
		return nil, DSLError{
			Kind:        UnknownField,
			Message:     fmt.Sprintf("unknown field %q", fieldTok.Value),
			ValidFields: fieldNames(),
		}
	}
	valTok := p.peek()
	if valTok.Kind != TokWord && valTok.Kind != TokString {
		return nil, fmt.Errorf("%w: expected value after ':'", ErrParseUnexpected)
	}
	p.advance()
	if field == FieldSimilarTo {
		return SimilarToNode{ID: valTok.Value}, nil
	}
	return FieldMatchNode{Field: field, Value: valTok.Value}, nil
}

// fieldNames returns AllFields as a []string for error surfaces.
func fieldNames() []string {
	out := make([]string, 0, len(AllFields))
	for _, f := range AllFields {
		out = append(out, string(f))
	}
	return out
}

// DSLErrorKind classifies parse errors so the HTTP handler can
// render different UX per class (unknown-field with whitelist vs
// generic syntax error).
type DSLErrorKind int

const (
	UnknownField DSLErrorKind = iota
	SyntaxError
	SimilarToNotImplemented
)

// DSLError is the typed error for parse + compile stages. Passed
// through to the HTTP layer; caller checks Kind.
type DSLError struct {
	Kind        DSLErrorKind
	Message     string
	ValidFields []string
}

func (e DSLError) Error() string { return e.Message }

// Sentinels wrapped in fmt.Errorf so errors.Is works.
var (
	ErrParseUnexpected         = errors.New("dsl: unexpected token")
	ErrSimilarToNotImplemented = errors.New("dsl: similar_to reserved for Phase 1.16.B-3")
)
