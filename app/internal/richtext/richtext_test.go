// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package richtext_test

import (
	"encoding/json"
	"os"
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

// ---------------------------------------------------------------------------
// Semantic emptiness, against the SHARED case list (#1389)
// ---------------------------------------------------------------------------

// The rule for "is this rich_text value empty" has two implementations,
// one per language: IsEmpty here, and isFieldValueEmpty /
// htmlToPlainText in web/src/lib/fieldDisplay.ts. They must agree, or a
// required field renders blank while the server considers it filled —
// and the editor decides between a Set and a Clear on the frontend's
// answer while the server enforces on its own.
//
// So the CASES ARE NOT WRITTEN HERE. Both suites read the same file, in
// the shape #956 established for exactly this problem: a mirror is only
// as good as a test that reads the other side rather than a second copy
// of what it is supposed to say.
//
// `stored` in that file is what Sanitize actually produced for `input`,
// so this asserts three things at once: the sanitiser still behaves the
// way the rule was designed around, the predicate agrees with the
// recorded verdict, and the frontend is testing the same values.
func TestIsEmpty_SharedCaseList(t *testing.T) {
	const path = "../../../web/src/lib/fieldEmptiness.cases.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		// The Go suite runs in a container that mounts only the paths a
		// test actually reads; scripts/test.sh mounts this one. A skip
		// here means the mount was dropped, which is how the catalogue
		// guard #956 went quietly green for months — so say what is
		// wrong rather than reporting a pass.
		t.Skipf("shared case list not readable at %s (scripts/test.sh must mount it): %v", path, err)
	}
	var doc struct {
		RichText []struct {
			Input  string `json:"input"`
			Stored string `json:"stored"`
			Empty  bool   `json:"empty"`
		} `json:"rich_text"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(doc.RichText) == 0 {
		t.Fatal("the shared case list is empty, so this test proves nothing")
	}
	empties := 0
	for _, c := range doc.RichText {
		if got := richtext.Sanitize(c.Input); got != c.Stored {
			t.Errorf("Sanitize(%q) = %q, the case list records %q — the measured behaviour the rule was built on has changed",
				c.Input, got, c.Stored)
		}
		if got := richtext.IsEmpty(c.Stored); got != c.Empty {
			t.Errorf("IsEmpty(%q) = %v, want %v (from input %q)", c.Stored, got, c.Empty, c.Input)
		}
		if c.Empty {
			empties++
		}
	}
	// A list that drifted to all-empty or all-full would still pass
	// every assertion above while testing nothing.
	if empties == 0 || empties == len(doc.RichText) {
		t.Fatalf("the case list must cover BOTH verdicts; %d of %d are empty", empties, len(doc.RichText))
	}
	t.Logf("%d shared cases, %d empty / %d not", len(doc.RichText), empties, len(doc.RichText)-empties)
}
