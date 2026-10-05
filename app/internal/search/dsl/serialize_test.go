// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1368 — the VALUE half of "one grammar, one saved query".
//
// ⛔ THESE ARE NOT FAIL-BEFORE TESTS. They reference [Serialize], which
// does not exist on `dev`, and the escape they exercise is a change to
// the grammar rather than a bug in it. The fail-before regression for
// this issue is a browser user flow (scripts/dogfood/ui/tests/standalone/
// saved-search-filters-1368.spec.ts), which runs unchanged against both.
//
// What is asserted here is one property, stated once and then driven
// over every shape that could break it:
//
//	Lex(Serialize(v)) is a single token whose Value is v.
package dsl

import (
	"strings"
	"testing"
)

// serializerCorpus is every value shape the round trip has to survive.
// The list is grouped by WHY each entry is here, because a table with no
// reasons is a table nobody can extend correctly.
var serializerCorpus = []struct {
	name string
	v    string
}{
	// Bare, and must STAY bare — the N=0 half of "nothing churns".
	{"plain word", "png"},
	{"digits and dashes", "asset-type_2.0"},
	{"unicode", "ねこ"},
	{"uuid", "0192abcd-1234-5678-9abc-def012345678"},

	// The lexer's word-run terminators (lexer.go's Lex loop).
	{"space", "two words"},
	{"tab", "two\twords"},
	{"newline", "two\nwords"},
	{"carriage return", "two\rwords"},
	{"colon", "ns:value"},
	{"open paren", "a(b"},
	{"close paren", "a)b"},
	{"quote", `a"b`},

	// The keyword arm, which no list of punctuation would have caught.
	// ⚠️ The lexer upper-cases before comparing, so a LOWERCASE value is
	// the one that proves the point.
	{"lowercase and", "and"},
	{"lowercase or", "or"},
	{"lowercase not", "not"},
	{"mixed-case Or", "Or"},
	{"uppercase AND", "AND"},

	// Not a token at all.
	{"empty", ""},
	{"only spaces", "   "},

	// #1368's losslessness hole: before this sprint NO quoted spelling
	// existed for any of these.
	{"trailing backslash", `abc\`},
	{"backslash before quote", `abc\"def`},
	{"backslash and quote", `a\b"c`},
	{"lone backslash", `\`},
	{"doubled backslash", `a\\b`},
	{"windows path", `C:\Users\art\ref.png`},
	{"regex-ish", `\d+\s"x"`},

	// Field terms, which travel through this function opaquely.
	{"field eq", "color_space=sRGB"},
	{"field contains", "credit~Blossom & Co"},
	{"field date bound", "licence_expires>=2026-01-31T00:00:00Z"},
	{"field value with colon", "note=see: page 4"},
	{"field value that is a keyword", "status=or"},
}

// TestSerialize_RoundTripsThroughTheLexer is the whole contract.
func TestSerialize_RoundTripsThroughTheLexer(t *testing.T) {
	for _, tc := range serializerCorpus {
		t.Run(tc.name, func(t *testing.T) {
			out := Serialize(tc.v)
			toks, err := Lex(out)
			if err != nil {
				t.Fatalf("Serialize(%q) = %q, which does not lex: %v", tc.v, out, err)
			}
			if len(toks) != 2 || toks[1].Kind != TokEOF {
				t.Fatalf("Serialize(%q) = %q lexed to %d tokens, want exactly one + EOF",
					tc.v, out, len(toks)-1)
			}
			if toks[0].Value != tc.v {
				t.Fatalf("Serialize(%q) = %q lexed back to %q", tc.v, out, toks[0].Value)
			}
			if k := toks[0].Kind; k != TokWord && k != TokString {
				t.Fatalf("Serialize(%q) = %q lexed as %s, want WORD or STRING", tc.v, out, k)
			}
		})
	}
}

// TestSerialize_LeavesSafeValuesBare keeps the canonical form readable
// and, more importantly, keeps the N=0 promise: a value that needs no
// quoting is not given any, so a re-save cannot rewrite a stored query
// into a differently-spelled equivalent.
func TestSerialize_LeavesSafeValuesBare(t *testing.T) {
	for _, v := range []string{"png", "sketch", "0192abcd-1234", "ねこ", "a.b-c_d"} {
		if got := Serialize(v); got != v {
			t.Errorf("Serialize(%q) = %q, want it left bare", v, got)
		}
	}
}

// TestSerialize_QuotesWhatTheLexerWouldNotGiveBack is the same claim from
// the other side: everything in the corpus that is NOT returned bare must
// be a quoted string, and no value may be returned bare unless lexing it
// really does give it back.
func TestSerialize_QuotesWhatTheLexerWouldNotGiveBack(t *testing.T) {
	for _, tc := range serializerCorpus {
		out := Serialize(tc.v)
		bare := out == tc.v
		if bare && !lexesAsOneBareWord(tc.v) {
			t.Errorf("Serialize(%q) left it bare but the lexer does not return it", tc.v)
		}
		if !bare && !strings.HasPrefix(out, `"`) {
			t.Errorf("Serialize(%q) = %q — not bare and not a quoted string", tc.v, out)
		}
	}
}

// TestSerialize_IsDeterministic — the same value serialises the same way
// every time, which is what stops a reload-and-re-save from churning the
// stored DSL.
func TestSerialize_IsDeterministic(t *testing.T) {
	for _, tc := range serializerCorpus {
		first := Serialize(tc.v)
		for i := 0; i < 5; i++ {
			if got := Serialize(tc.v); got != first {
				t.Fatalf("Serialize(%q) returned %q then %q", tc.v, first, got)
			}
		}
	}
}

// TestLexString_BackslashEscape pins the grammar change itself, in both
// directions, so the reason for it survives the sprint that made it.
//
// ⚠️ The middle case is the one whose READING CHANGED: `"a\\b"` used to
// yield `a\\b` (two literal backslashes) and now yields `a\b`. Verified
// before the change that nothing in the repository or in either stack's
// saved_search table contained a doubled backslash — see lexString.
func TestLexString_BackslashEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"plain"`, "plain"},
		{`"a\"b"`, `a"b`},             // unchanged: the escaped quote
		{`"a\\b"`, `a\b`},             // CHANGED: doubled backslash is now one
		{`"a\b"`, `a\b`},              // unchanged: a lone backslash stays literal
		{`"trailing\\"`, `trailing\`}, // NEW: previously unrepresentable
		{`"C:\Users\art"`, `C:\Users\art`},
	}
	for _, tc := range cases {
		toks, err := Lex(tc.in)
		if err != nil {
			t.Errorf("Lex(%s): %v", tc.in, err)
			continue
		}
		if toks[0].Kind != TokString || toks[0].Value != tc.want {
			t.Errorf("Lex(%s) = %s %q, want STRING %q", tc.in, toks[0].Kind, toks[0].Value, tc.want)
		}
	}
	// The two spellings that had NO reading at all before this change.
	for _, bad := range []string{`"abc\`, `"abc\\`} {
		if _, err := Lex(bad); err == nil {
			t.Errorf("Lex(%s) should still be unterminated", bad)
		}
	}
}

// TestGroup_ProtectsPrecedence is the composition hazard as a parse-tree
// assertion rather than as a string comparison. ⛔ The naive arm is
// asserted too: a test that only shows the correct form working would
// pass against a serializer that appends without wrapping, since both
// produce a tree.
func TestGroup_ProtectsPrecedence(t *testing.T) {
	const naive = "cat OR dog AND extension:png"
	q, err := Parse(naive)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Root.(OrNode); !ok {
		t.Fatalf("premise failed: %q parsed as %T, so AND no longer binds tighter than OR "+
			"and Group's reason needs re-deriving", naive, q.Root)
	}

	wrapped := Group("cat OR dog") + " AND extension:png"
	q2, err := Parse(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	and, ok := q2.Root.(AndNode)
	if !ok {
		t.Fatalf("%q parsed as %T, want AndNode", wrapped, q2.Root)
	}
	if _, ok := and.Left.(OrNode); !ok {
		t.Errorf("left operand = %T, want the saved expression as one OrNode", and.Left)
	}
	if fm, ok := and.Right.(FieldMatchNode); !ok || fm.Field != FieldExtension {
		t.Errorf("right operand = %#v, want extension:png", and.Right)
	}

	if got := Group("  "); got != "" {
		t.Errorf("Group(whitespace) = %q, want empty rather than an unparseable ()", got)
	}
}

// FuzzSerialize_RoundTrips widens the corpus above to whatever the fuzzer
// finds. The property is the same one; only the inputs are open-ended.
func FuzzSerialize_RoundTrips(f *testing.F) {
	for _, tc := range serializerCorpus {
		f.Add(tc.v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if len(v) > MaxInputBytes/2 {
			t.Skip("beyond the input cap; nothing here is about the cap")
		}
		out := Serialize(v)
		toks, err := Lex(out)
		if err != nil {
			t.Fatalf("Serialize(%q) = %q does not lex: %v", v, out, err)
		}
		if len(toks) != 2 || toks[0].Value != v {
			t.Fatalf("Serialize(%q) = %q lexed to %d tokens, first %q", v, out, len(toks)-1, toks[0].Value)
		}
	})
}
