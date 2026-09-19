// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Verbs are the `!word` aliases the advanced search accepts (#1173,
// sprint 25a).
//
// # ⛔ A VERB IS SUGAR OVER THE TYPED GRAMMAR, NOT A SECOND EXECUTOR
//
// ADR 0056 decision 3 refused `!bang` syntax because the project already
// had a typed `field:value` grammar and "a second vocabulary would be a
// second code path to keep honest". That reasoning is kept whole here:
// a verb never reaches the compiler as itself. [parser.parseVerb] folds
// it into exactly the [FieldMatchNode] (or AND-chain of them) that the
// canonical spelling parses to, so everything downstream (placement,
// compilation, the facet bridge, serialization, saved-search replay)
// sees one AST shape and one dimension. `!nopreviews` and
// `preview:missing` are indistinguishable after parseFactor returns;
// so are `!list<a>,<b>` and `(id:<a> AND id:<b>)`.
//
// # The table
//
// A verb is matched by NAME PREFIX on the word after `!`, longest name
// first, and the rest of the word is its PAYLOAD. `!list` carries its
// ids in the payload with no separator, which is why `!list:a`, with a
// colon, is refused as malformed rather than read as a field named
// `!list`. Names fold case; payloads keep their bytes (the list fold
// canonicalises each id itself).
//
// Unknown verbs are an error naming the ones that exist. `!last…` is
// unknown in 25a on purpose: sprint 25b registers it here and the test
// that pins it unknown is the one 25b flips.
type verb struct {
	// name is the verb without its leading `!`, lowercase.
	name string
	// usage is the canonical spelling shown in every error.
	usage string
	// fold turns the payload into the node the canonical spelling
	// parses to, plus the canonical TEXT that spelling has, for
	// [Canonicalize].
	fold func(payload string) (Node, string, error)
}

// verbs is the registry. Ordered longest name first so a name that is
// a prefix of another can never shadow it.
var verbs = []verb{
	{name: "nopreviews", usage: "!nopreviews", fold: foldNoPreviews},
	{name: "list", usage: "!list<uuid>,<uuid>,...", fold: foldList},
}

// verbUsages renders the registry for error messages.
func verbUsages() string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, v.usage)
	}
	return strings.Join(out, ", ")
}

// PreviewMissing is the ONLY legal value of [FieldPreview], spelled
// once here for the fold and once in the facet layer's closed
// vocabulary. `preview:present` is deliberately not a value: the
// dimension exists to find what the pipeline did not produce.
const PreviewMissing = "missing"

// foldNoPreviews is `!nopreviews` → `preview:missing`.
func foldNoPreviews(payload string) (Node, string, error) {
	if payload != "" {
		return nil, "", DSLError{
			Kind:    SyntaxError,
			Message: fmt.Sprintf("!nopreviews takes no value (got %q); write !nopreviews", payload),
		}
	}
	return FieldMatchNode{Field: FieldPreview, Value: PreviewMissing},
		SerializeTerm(FieldPreview, PreviewMissing), nil
}

// foldList is `!list<a>,<b>,...` → `id:<a> AND id:<b> AND ...`, as the
// SAME left-associative AND chain [parser.parseAnd] builds for the
// canonical spelling, so the two parse to one tree and not to two that
// merely flatten alike.
//
// The comma is the delimiter and the ONLY delimiter. An empty entry
// (`a,,b`, a trailing comma) is refused rather than skipped, because a
// dropped entry is a list that looks applied and is narrower than what
// was typed. Each entry is parsed by google/uuid and written back in its
// canonical lowercase hyphenated form, which is what the facet layer's
// CanonicalValue does to the typed spelling, one library call on each
// side, no grammar of our own. Exact duplicates collapse here, in first-
// seen order, so the canonical text is the SET the alias names; the
// selection would have collapsed them anyway, and the cardinality rule
// counts DISTINCT ids wherever they are counted.
//
// N=1 renders bare (`id:<a>`); N>1 renders parenthesised, because the
// alias was ONE factor and the canonical text must stay one factor when
// [Canonicalize] splices it back into whatever surrounded it.
func foldList(payload string) (Node, string, error) {
	if payload == "" {
		return nil, "", DSLError{
			Kind:    SyntaxError,
			Message: "!list needs at least one id; write !list<uuid>,<uuid>,...",
		}
	}
	seen := make(map[string]struct{}, 4)
	ids := make([]string, 0, 4)
	for _, raw := range strings.Split(payload, ",") {
		if strings.TrimSpace(raw) == "" {
			return nil, "", DSLError{
				Kind:    SyntaxError,
				Message: "!list has an empty entry; write !list<uuid>,<uuid>,... with no empty entries",
			}
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, "", DSLError{
				Kind:    SyntaxError,
				Message: fmt.Sprintf("!list: %q is not a UUID; write !list<uuid>,<uuid>,...", raw),
			}
		}
		canonical := id.String()
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		ids = append(ids, canonical)
	}
	var node Node
	terms := make([]string, 0, len(ids))
	for _, id := range ids {
		term := FieldMatchNode{Field: FieldID, Value: id}
		if node == nil {
			node = term
		} else {
			node = AndNode{Left: node, Right: term}
		}
		terms = append(terms, SerializeTerm(FieldID, id))
	}
	text := strings.Join(terms, " AND ")
	if len(terms) > 1 {
		text = Group(text)
	}
	return node, text, nil
}

// resolveVerb matches a `!word` token against the registry and folds
// it. `followedByColon` is the parser's and the canonicaliser's shared
// look-ahead: a verb followed by `:` is the one malformation the fold
// itself cannot see, because the lexer has already cut the word there.
func resolveVerb(word string, followedByColon bool) (Node, string, error) {
	if followedByColon {
		return nil, "", DSLError{
			Kind:    SyntaxError,
			Message: fmt.Sprintf("%s: a verb takes no ':'; the verbs are %s", word, verbUsages()),
		}
	}
	name := strings.ToLower(strings.TrimPrefix(word, "!"))
	for _, v := range verbs {
		if !strings.HasPrefix(name, v.name) {
			continue
		}
		// The payload keeps the ORIGINAL bytes: only the name folds case.
		return v.fold(word[1+len(v.name):])
	}
	return nil, "", DSLError{
		Kind:    UnknownVerb,
		Message: fmt.Sprintf("unknown verb %q; the verbs are %s", word, verbUsages()),
	}
}

// parseVerb consumes one `!word` token and returns the node its
// canonical spelling parses to. See [verbs].
func (p *parser) parseVerb() (Node, error) {
	t := p.advance()
	node, _, err := resolveVerb(t.Value, p.peek().Kind == TokColon)
	return node, err
}

// Canonicalize rewrites every verb in input to its canonical typed
// spelling and leaves EVERY OTHER BYTE untouched (#1173, sprint 25a).
//
// # Why this exists, and why it is token-level
//
// A saved search stores ONE canonical DSL string, and the composer
// ([search.ComposeDSL]) carries the caller's expression through OPAQUELY
// (it wraps, it does not parse), and ADR 0093's 18a amendment says the
// expression half is preserved exactly. An alias inside that expression
// would therefore be stored as typed, and the stored form would be the
// sugar rather than the dimension. That breaks the round-trip contract
// in the one place it is meant to hold: the column.
//
// Re-serialising the whole expression from its AST would fix it at the
// cost of the opacity, free text, phrases and boolean structure would
// all be rewritten by a serializer that has to be right about every
// node. This does less. It lexes the input, replaces each verb token
// with the text its fold produces, and splices by source offset, so
// free text keeps its bytes, quoted phrases are never entered (a
// `"!nopreviews"` phrase is a phrase and stays one), and the result
// parses to the SAME tree the aliased input did: the fold returns the
// node AND the text, from one function, so they cannot disagree.
//
// Input with no verbs comes back byte-for-byte. A malformed verb is the
// same error the parser raises for it.
func Canonicalize(input string) (string, error) {
	tokens, err := Lex(input)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	last := 0
	for i, t := range tokens {
		if t.Kind != TokWord || !strings.HasPrefix(t.Value, "!") {
			continue
		}
		_, text, err := resolveVerb(t.Value, tokens[i+1].Kind == TokColon)
		if err != nil {
			return "", err
		}
		sb.WriteString(input[last:t.Start])
		sb.WriteString(text)
		last = t.End
	}
	if last == 0 && sb.Len() == 0 {
		return input, nil
	}
	sb.WriteString(input[last:])
	return sb.String(), nil
}
