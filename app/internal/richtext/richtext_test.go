// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package richtext_test

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/richtext"
)

// TestSanitizeStripsScriptTagsAndTheirContents is one of the four
// named canaries for #816. If the sanitiser is removed or weakened,
// these fail by name and the name says what was lost.
func TestSanitizeStripsScriptTagsAndTheirContents(t *testing.T) {
	got := richtext.Sanitize(`<p>hi</p><script>window.__xss && window.__xss()</script>`)
	if strings.Contains(got, "script") || strings.Contains(got, "__xss") {
		t.Fatalf("script survived sanitisation: %q", got)
	}
	if got != "<p>hi</p>" {
		t.Fatalf("want %q, got %q", "<p>hi</p>", got)
	}
}

func TestSanitizeStripsEventHandlerAttributes(t *testing.T) {
	for _, in := range []string{
		`<img src=x onerror="window.__xss()">`,
		`<p onclick="window.__xss()">click</p>`,
		`<a href="https://example.com" onmouseover="window.__xss()">link</a>`,
	} {
		got := richtext.Sanitize(in)
		if strings.Contains(strings.ToLower(got), "onerror") ||
			strings.Contains(strings.ToLower(got), "onclick") ||
			strings.Contains(strings.ToLower(got), "onmouseover") ||
			strings.Contains(got, "__xss") {
			t.Fatalf("event handler survived for %q: %q", in, got)
		}
	}
}

func TestSanitizeStripsJavascriptHrefs(t *testing.T) {
	for _, in := range []string{
		`<a href="javascript:window.__xss()">x</a>`,
		`<a href="JaVaScRiPt:alert(1)">x</a>`,
		`<a href="data:text/html;base64,PHNjcmlwdD4=">x</a>`,
		`<a href="vbscript:msgbox(1)">x</a>`,
		// A relative href is not a scheme hole, but it is an in-app
		// navigation surface we deliberately do not hand out.
		`<a href="/admin/users">x</a>`,
	} {
		got := richtext.Sanitize(in)
		if strings.Contains(got, "href") {
			t.Fatalf("disallowed href survived for %q: %q", in, got)
		}
	}
}

func TestSanitizeKeepsHTTPLinksAndForcesRel(t *testing.T) {
	cases := map[string]string{
		`<a href="https://example.com/a">e</a>`: `<a href="https://example.com/a" rel="noopener noreferrer">e</a>`,
		`<a href="http://example.com/">e</a>`:   `<a href="http://example.com/" rel="noopener noreferrer">e</a>`,
		`<a href="mailto:a@b.test">e</a>`:       `<a href="mailto:a@b.test" rel="noopener noreferrer">e</a>`,
		// An author-supplied rel is replaced, not merged — one policy,
		// not "whatever they wrote plus ours".
		`<a href="https://example.com/" rel="opener">e</a>`: `<a href="https://example.com/" rel="noopener noreferrer">e</a>`,
	}
	for in, want := range cases {
		if got := richtext.Sanitize(in); got != want {
			t.Errorf("Sanitize(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestSanitizeKeepsTheAllowedFormattingSet(t *testing.T) {
	in := `<p>Cleared for <strong>internal</strong> and <em>review</em> use.</p>` +
		`<h3>Terms</h3><h4>Scope</h4>` +
		`<ul><li>one</li><li>two</li></ul><ol><li>first</li></ol>` +
		`<blockquote>quoted</blockquote><p>line<br>break</p>`
	got := richtext.Sanitize(in)
	if got != in {
		t.Fatalf("allowed set was altered\n got  %q\n want %q", got, in)
	}
}

func TestSanitizeStripsDisallowedTagsButKeepsTheirText(t *testing.T) {
	// Stripped, not escaped: the prose survives, the markup does not.
	got := richtext.Sanitize(`<div class="x">plain <span>words</span></div><h1>big</h1>`)
	if strings.ContainsAny(got, "<>") {
		t.Fatalf("markup survived: %q", got)
	}
	for _, want := range []string{"plain", "words", "big"} {
		if !strings.Contains(got, want) {
			t.Fatalf("text %q was dropped: %q", want, got)
		}
	}
}

// TestSanitizeIsIdempotent is what makes sanitising on BOTH the write
// and the read side safe. A value that has already been through the
// policy must come out of it identical, or every read would erode the
// stored value a little further.
func TestSanitizeIsIdempotent(t *testing.T) {
	for _, in := range []string{
		`<p>Cleared for <strong>internal</strong> use.</p>`,
		`<a href="https://example.com/">e</a>`,
		`<p>a &amp; b &lt; c</p>`,
		`<script>alert(1)</script><p>x</p>`,
		`a < b & c`,
		``,
	} {
		once := richtext.Sanitize(in)
		twice := richtext.Sanitize(once)
		if once != twice {
			t.Errorf("not idempotent for %q:\n once  %q\n twice %q", in, once, twice)
		}
	}
}

func TestSanitizeValueTextOnlyTouchesRichText(t *testing.T) {
	dirty := `<script>alert(1)</script><p>hi</p>`

	for _, ft := range []string{"text", "longtext", "select", "tree", "multi_select", "number"} {
		v := dirty
		got := richtext.SanitizeValueText(ft, &v)
		if got == nil || *got != dirty {
			t.Errorf("field type %q was rewritten: %v", ft, got)
		}
	}

	v := dirty
	got := richtext.SanitizeValueText(richtext.FieldType, &v)
	if got == nil || *got != "<p>hi</p>" {
		t.Fatalf("rich_text not sanitised: %v", got)
	}
	// The caller's own string must not be mutated underneath it.
	if v != dirty {
		t.Fatalf("input string was mutated: %q", v)
	}
}

func TestSanitizeValueTextPassesNilThrough(t *testing.T) {
	if got := richtext.SanitizeValueText(richtext.FieldType, nil); got != nil {
		t.Fatalf("nil became %v — an unset value is not an empty one", got)
	}
}
