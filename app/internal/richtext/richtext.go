// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package richtext is the single source of truth for what HTML a
// `rich_text` field value is allowed to contain.
//
// # The boundary decision (#816)
//
// A rich_text value is the ONLY thing this application stores that a
// client renders as markup rather than as text. Every other field type
// is interpolated, so its contents can never be anything but characters
// on a screen. That makes rich_text the one place where a stored string
// is also a program, and it needs a boundary that is written down once
// rather than re-derived per surface.
//
// It is sanitised at BOTH boundaries, with ONE implementation:
//
//   - On WRITE, so what lands in the database is already safe and a
//     future consumer that forgets to sanitise is not a vulnerability.
//     Hooked into buildUpsertParams / buildCollectionUpsertParams (the
//     one place each handler maps an API body to sqlc params, which
//     also covers the defaults pass), the extraction writer, and the
//     seed runner.
//
//   - On READ, so a value that never went through a handler is safe
//     anyway. This is the layer that covers the seed's direct inserts,
//     imports, hand-edits against psql, rows written before this
//     package existed, and — the reason it is not optional — a value
//     that arrives from a federated peer whose sanitiser we do not
//     control. Hooked into buildAssetValue / buildCollectionValue.
//
// Neither one alone is enough. Write-only trusts the database; the seed
// path alone (SeedInsertAssetFieldValue) bypasses every handler gate.
// Read-only leaves a live payload sitting in a table for whatever queries
// it next — search indexing, exports, a report — none of which are
// obliged to route through the DTO builder.
//
// # The client contract
//
// Because the read side sanitises, the API's guarantee to every client
// is: rich_text HTML arrives pre-sanitised and may be rendered as
// markup. web/src/lib/fieldDisplay.ts relies on exactly that and ships
// no client-side sanitiser — there is one policy, and it is this file.
// If that guarantee is ever weakened, the frontend's `html` descriptor
// has to go with it.
//
// # The allowed set
//
// Block and inline structure a person would type into a rights or
// notes field, and nothing else:
//
//	p br strong em ul ol li blockquote h3 h4 a[href]
//
// Links are restricted to http, https and mailto — which is what makes
// `javascript:` a strip and not a hole — and every surviving link is
// given rel="noopener noreferrer" whether or not the author wrote one.
//
// Headings start at h3 because these values render inside a panel that
// already owns h1/h2; a value that could emit an h1 would outrank the
// page it sits on.
//
// Everything not on the list is STRIPPED rather than escaped. Escaping
// would turn a stray `<div>` into visible `&lt;div&gt;` noise in the
// middle of someone's prose, which is the bug this sprint set out to
// fix wearing a different hat.
package richtext

import (
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// FieldType is the one field type whose value is markup. Everything
// that asks "should I sanitise this?" asks it by comparing against
// this constant rather than by spelling the string again.
const FieldType = "rich_text"

// linkRel is forced onto every surviving link. bluemonday can add
// `noreferrer` on its own but has no switch for `noopener` outside of
// its target="_blank" handling, and we do not want target rewriting —
// so the rel is applied in one place, by forceLinkRel, for all links.
const linkRel = "noopener noreferrer"

// policy is built once. bluemonday policies are safe for concurrent
// use once constructed and are not cheap to build.
var policy = buildPolicy()

func buildPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	// Structure. AllowElements permits the tag with no attributes;
	// bluemonday's default "elements allowed without attrs" set already
	// covers these, so nothing here needs AllowNoAttrs.
	p.AllowElements(
		"p", "br",
		"strong", "em",
		"ul", "ol", "li",
		"blockquote",
		"h3", "h4",
	)

	// Links. href only — a `rel` the author supplied is dropped here
	// and re-added by forceLinkRel, so there is exactly one way for a
	// rel to end up on the wire.
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	// Relative hrefs would be an in-app link written by whoever last
	// edited the value, which is a navigation surface we did not mean
	// to hand out. AllowURLSchemes already turns on parseable-URL
	// checking; this states the other half explicitly.
	p.AllowRelativeURLs(false)

	return p
}

// Sanitize returns s with everything outside the allowed set removed.
//
// It is idempotent: sanitising an already-sanitised value returns it
// unchanged, which is what makes sanitising on both the write and the
// read side safe rather than lossy.
func Sanitize(s string) string {
	if s == "" {
		return ""
	}
	return forceLinkRel(policy.Sanitize(s))
}

// SanitizeValueText is the call site every writer and reader uses.
//
// It gates on the field type itself so that no caller has to remember
// which types carry markup — the same shape as the vocabulary helpers
// in the metadata package, and for the same reason: a caller that
// answers "is this one of the HTML types?" for itself is a caller that
// will answer it differently one day.
//
// A nil pointer passes through as nil ("no value" is not the same as
// an empty value, and the column is nullable).
func SanitizeValueText(fieldType string, v *string) *string {
	if v == nil || fieldType != FieldType {
		return v
	}
	clean := Sanitize(*v)
	return &clean
}

// forceLinkRel puts rel="noopener noreferrer" on every <a> in an
// already-sanitised fragment, replacing any rel that survived.
//
// This runs over bluemonday's OUTPUT, not over untrusted input: by the
// time it sees the document, the only tags left are ours and the only
// attribute on an anchor is a scheme-checked href. It rewrites anchors
// and re-emits every other token byte-for-byte, so it cannot change
// what the sanitiser decided — only what rel the link carries.
func forceLinkRel(s string) string {
	if !strings.Contains(s, "<a ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 32)
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			// io.EOF, or a parse error on a fragment bluemonday just
			// produced. Either way there is nothing more to rewrite.
			return b.String()
		case html.StartTagToken:
			// Raw() is only valid until the next Next(), and reading it
			// before Token() keeps the pass-through branch honest.
			raw := append([]byte(nil), z.Raw()...)
			t := z.Token()
			if t.Data != "a" {
				b.Write(raw)
				continue
			}
			attrs := t.Attr[:0]
			for _, a := range t.Attr {
				if a.Key != "rel" {
					attrs = append(attrs, a)
				}
			}
			t.Attr = append(attrs, html.Attribute{Key: "rel", Val: linkRel})
			b.WriteString(t.String())
		default:
			b.Write(z.Raw())
		}
	}
}

// ---------------------------------------------------------------------------
// Semantic emptiness (#1389)
// ---------------------------------------------------------------------------

// The sanitiser removes what a value may not CONTAIN. It says nothing
// about whether a value carries anything a reader can see, and nothing
// in it strips empty elements — measured against the shipped policy,
// `<p></p>`, `<p><br></p>`, `<p>   </p>`, `<br>`, `<ul><li></li></ul>`
// and `<blockquote></blockquote>` all survive Sanitize unchanged, and
// `<p>&nbsp;</p>` survives with the entity decoded to a literal U+00A0.
//
// So `strings.TrimSpace` on a stored rich_text value ACCEPTS a value
// that renders as nothing at all. Any rule about whether a rich_text
// field "has a value" has to read the value's WORDS, not its bytes.
//
// This is the server-authoritative twin of the rule the frontend
// already ships: web/src/lib/fieldDisplay.ts's htmlToPlainText, whose
// output feeds the "is this set" test and the field count. There is ONE
// rule and it is this one; a second implementation of it is two rules
// that will disagree about a value a person is looking at.
//
// It lives here, beside Sanitize, rather than in `metadata`, for the
// same reason Sanitize does: this package is where what a rich_text
// value IS gets decided, and callers outside `metadata` need the
// predicate too.

// tagRun matches one HTML tag, exactly as the frontend's
// /<[^>]*>/g does — from a `<` up to the first following `>`, with an
// unterminated `<` left alone.
var tagRun = regexp.MustCompile(`<[^>]*>`)

// plainEntities are decoded in the order the frontend decodes them,
// and the order is load-bearing: `&amp;` is last, so `&amp;lt;` reads
// as the literal `&lt;` its author typed rather than being decoded
// twice into a `<`.
var plainEntities = []struct{ from, to string }{
	{"&nbsp;", " "},
	{"&lt;", "<"},
	{"&gt;", ">"},
	{"&quot;", `"`},
	{"&#34;", `"`},
	{"&apos;", "'"},
	{"&#39;", "'"},
	{"&amp;", "&"},
}

// isJSSpace reports whether r is whitespace to JavaScript's `\s`.
//
// Spelled out rather than delegating to unicode.IsSpace, because the
// two sets are NOT the same and this function's whole job is to agree
// with a JavaScript regexp: unicode.IsSpace counts U+0085 (NEL), which
// `\s` does not, and `\s` counts U+FEFF, which unicode.IsSpace does
// not. Neither divergence can be reached through the sanitiser today,
// which is exactly why it would go unnoticed if the predicate drifted.
//
// Go's regexp `\s` is no help here either: it is [\t\n\f\r ] only, so
// it would miss the U+00A0 that `&nbsp;` decodes to — the single most
// likely character to be sitting alone inside an "empty" paragraph.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		'\u00a0', // NBSP — what `&nbsp;` decodes to
		'\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000',
		'\ufeff': // BOM — in `\s`, NOT in unicode.IsSpace
		return true
	}
	// U+2000..U+200A, the en/em/thin quad space run.
	return r >= '\u2000' && r <= '\u200a'
}

// PlainText returns the visible words of a rich_text value with its
// markup taken out: the Go twin of fieldDisplay.ts's htmlToPlainText.
//
// Tags collapse to a SPACE rather than to nothing, so
// `<li>one</li><li>two</li>` reads as "one two" and not "onetwo".
//
// This is NOT a sanitiser and must never be used as one. Its output is
// never rendered; it exists so a rule can ask what a value SAYS.
func PlainText(s string) string {
	s = tagRun.ReplaceAllString(s, " ")
	for _, e := range plainEntities {
		s = strings.ReplaceAll(s, e.from, e.to)
	}
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	wrote := false
	for _, r := range s {
		if isJSSpace(r) {
			pendingSpace = wrote
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
		wrote = true
	}
	return b.String()
}

// IsEmpty reports whether a rich_text value carries no content a reader
// would see — the SEMANTIC emptiness #1389 defines, as against the raw
// TrimSpace that accepts `<p><br></p>`.
func IsEmpty(s string) bool {
	return PlainText(s) == ""
}
