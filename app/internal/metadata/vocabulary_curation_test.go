// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit cover for the three pieces of ADR 0092 that decide what a term
// RESOLVES TO, independently of any transaction: aliases, merge
// tombstones, and the bounded search over a vocabulary.
//
// The write path and the merge itself are covered end-to-end against a
// real database (vocabulary_curation_e2e_test.go) — the interesting
// parts there are the row lock, the row rewrite and the rollback, none
// of which a unit test can see. What a unit test CAN see, and what
// these assert, is the precedence rule: which key wins when a slug, a
// label, an alias and a tombstone all want the same word.
package metadata

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// curationFixture is a vocabulary carrying every state the resolver has
// to tell apart: a plain term, a term with aliases, a merge tombstone
// (archived WITH replaced_by), a hard archive (archived WITHOUT one),
// and a two-hop tombstone chain.
func curationFixture() []FieldOption {
	return []FieldOption{
		{Value: "gb", Label: "United Kingdom", Aliases: []string{"uk", "britain"}},
		{Value: "fr", Label: "France"},
		// Merged away into `gb`: a forwarding address, not a retirement.
		{Value: "great-britain", Label: "Great Britain", Status: OptionArchived, ReplacedBy: "gb"},
		// A second hop: `gbr` → `great-britain` → `gb`.
		{Value: "gbr", Label: "GBR", Status: OptionArchived, ReplacedBy: "great-britain"},
		// Hard-retired: archived with nowhere to forward to.
		{Value: "atlantis", Label: "Atlantis", Status: OptionArchived},
		{Value: "de", Label: "Germany", Status: OptionDeprecated},
	}
}

// An alias is a match key: writing it stores the term's own slug.
func TestVocabularyIndex_AliasResolvesToItsTerm(t *testing.T) {
	idx := indexVocabulary(curationFixture())
	for _, in := range []string{"uk", "UK", "  Britain  ", "britain"} {
		slug, matched, rej := idx.resolveOrMint(in, true)
		if rej != nil {
			t.Errorf("resolveOrMint(%q) refused: %v", in, rej)
			continue
		}
		if !matched {
			t.Errorf("resolveOrMint(%q) MINTED %q — an alias must resolve, never create", in, slug)
			continue
		}
		if slug != "gb" {
			t.Errorf("resolveOrMint(%q) = %q, want gb", in, slug)
		}
	}
}

// The slugified form goes through the same key space, so a term that
// only reaches an alias after slugification still resolves. This is the
// branch that used to consult `taken` and could not see an alias at all.
func TestVocabularyIndex_AliasIsReachedThroughSlugification(t *testing.T) {
	idx := indexVocabulary(curationFixture())
	slug, matched, rej := idx.resolveOrMint("UK!", true)
	if rej != nil || !matched || slug != "gb" {
		t.Fatalf("resolveOrMint(\"UK!\") = (%q, %v, %v), want (gb, true, nil)", slug, matched, rej)
	}
}

// The precedence rule, stated as a test: a real term always beats an
// alias. Without it, adding an alias could silently hijack an existing
// term and every future write of that word would land somewhere else.
func TestVocabularyIndex_RealTermBeatsAlias(t *testing.T) {
	values := []FieldOption{
		{Value: "fr", Label: "France"},
		// A hostile (or careless) alias claiming a slug that exists.
		{Value: "gb", Label: "United Kingdom", Aliases: []string{"fr"}},
	}
	idx := indexVocabulary(values)
	slug, matched, rej := idx.resolveOrMint("fr", true)
	if rej != nil || !matched {
		t.Fatalf("resolveOrMint(fr) = (%q, %v, %v)", slug, matched, rej)
	}
	if slug != "fr" {
		t.Fatalf("an alias shadowed a real term: fr resolved to %q", slug)
	}
}

// A tombstone forwards. This is the property federation needs: a peer
// that saw `great-britain` before the merge sends it, and it lands on
// something real instead of 422ing forever.
func TestVocabularyIndex_TombstoneForwards(t *testing.T) {
	idx := indexVocabulary(curationFixture())
	for _, in := range []string{"great-britain", "Great Britain"} {
		slug, matched, rej := idx.resolveOrMint(in, true)
		if rej != nil {
			t.Errorf("resolveOrMint(%q) refused: %v — a merged-away term must forward", in, rej)
			continue
		}
		if !matched || slug != "gb" {
			t.Errorf("resolveOrMint(%q) = (%q, %v), want (gb, true)", in, slug, matched)
		}
	}
}

// Merges compose, so tombstone chains do. Stopping at the first hop
// would store a tombstone as if it were a value — the exact state the
// merge existed to remove.
func TestVocabularyIndex_TombstoneChainReachesTheLiveTerm(t *testing.T) {
	idx := indexVocabulary(curationFixture())
	slug, matched, rej := idx.resolveOrMint("gbr", true)
	if rej != nil {
		t.Fatalf("refused: %v", rej)
	}
	if !matched || slug != "gb" {
		t.Fatalf("gbr resolved to (%q, %v), want (gb, true) — the chain gbr→great-britain→gb", slug, matched)
	}
}

// A cycle is possible in a hand-edited document and must not hang or
// resolve to a tombstone. It degrades to a plain archive: refused.
func TestVocabularyIndex_TombstoneCycleIsRefusedNotFollowed(t *testing.T) {
	values := []FieldOption{
		{Value: "a", Status: OptionArchived, ReplacedBy: "b"},
		{Value: "b", Status: OptionArchived, ReplacedBy: "a"},
	}
	idx := indexVocabulary(values)
	if _, _, rej := idx.resolveOrMint("a", true); rej == nil {
		t.Fatal("a tombstone cycle resolved; want the archived refusal")
	}
}

// A hard archive — no replaced_by — is unchanged by this sprint. It
// still refuses, and it still refuses with the ARCHIVED status rather
// than as an unknown slug, so the operator is told the term exists.
func TestVocabularyIndex_HardArchiveStillRefuses(t *testing.T) {
	idx := indexVocabulary(curationFixture())
	_, _, rej := idx.resolveOrMint("Atlantis", true)
	if rej == nil {
		t.Fatal("a hard-archived term was matched or minted over")
	}
	if rej.Status != OptionArchived {
		t.Errorf("status = %q, want archived", rej.Status)
	}
}

// canonicaliseVocabulary is the closed-field path. It must redirect
// aliases and tombstones and leave everything else EXACTLY as it
// arrived, because checkVocabulary is the membership rule and knows
// things this does not (chiefly the grandfathering set).
func TestCanonicaliseVocabulary_RedirectsOnlyCuration(t *testing.T) {
	doc, err := json.Marshal(map[string]any{"values": curationFixture()})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	got := canonicaliseVocabulary(doc, []string{"uk", "great-britain", "fr", "atlantis", "nonsense"})
	want := []string{"gb", "fr", "atlantis", "nonsense"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("canonicaliseVocabulary = %v, want %v\n"+
			"  uk → gb (alias), great-britain → gb (tombstone, deduped against it),\n"+
			"  fr/atlantis/nonsense untouched so checkVocabulary can answer for them", got, want)
	}
}

// Case is NOT canonicalised on the closed path. Widening what a closed
// vocabulary accepts is a separate decision nobody made — this sprint
// added curation, not case-insensitive membership.
func TestCanonicaliseVocabulary_LeavesCasingAlone(t *testing.T) {
	doc, _ := json.Marshal(map[string]any{"values": curationFixture()})
	got := canonicaliseVocabulary(doc, []string{"FR"})
	if len(got) != 1 || got[0] != "FR" {
		t.Fatalf("canonicaliseVocabulary([FR]) = %v; a closed field's membership rule is unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// Alias validation
// ---------------------------------------------------------------------------

func TestNormalizeOptionsDoc_AliasesAreLowercasedAndDeduped(t *testing.T) {
	raw := []byte(`{"values":[{"value":"gb","label":"United Kingdom","aliases":["UK","  uk  ","Britain",""]}]}`)
	out, err := NormalizeOptionsDoc(raw)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	values, _, err := decodeOptionValues(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("values = %d, want 1", len(values))
	}
	got := strings.Join(values[0].Aliases, ",")
	if got != "uk,britain" {
		t.Fatalf("aliases = %q, want %q — lowercased, trimmed, deduped, order kept", got, "uk,britain")
	}
}

func TestNormalizeOptionsDoc_AmbiguousAliasIsRefused(t *testing.T) {
	cases := map[string]string{
		"alias collides with another term's slug":  `{"values":["fr",{"value":"gb","aliases":["fr"]}]}`,
		"alias collides with another term's label": `{"values":[{"value":"fr","label":"France"},{"value":"gb","aliases":["france"]}]}`,
		"two terms claim the same alias":           `{"values":[{"value":"fr","aliases":["eu"]},{"value":"gb","aliases":["eu"]}]}`,
		"alias repeats its own slug":               `{"values":[{"value":"gb","aliases":["gb"]}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeOptionsDoc([]byte(raw)); err == nil {
				t.Fatal("an ambiguous match key was accepted; there is no correct resolution for it")
			}
		})
	}
}

// An option carrying only a slug still serialises as a bare string —
// the round-trip property #737 relies on. Adding a field to the struct
// is exactly how that quietly breaks.
func TestFieldOption_BareRoundTripSurvivesAliases(t *testing.T) {
	raw := []byte(`{"values":["one","two"]}`)
	out, err := NormalizeOptionsDoc(raw)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if !strings.Contains(string(out), `["one","two"]`) {
		t.Fatalf("bare entries grew into objects: %s", out)
	}
}

// ---------------------------------------------------------------------------
// applyMergeToOptions
// ---------------------------------------------------------------------------

func TestApplyMergeToOptions_TombstonesTheSource(t *testing.T) {
	values := curationFixture()
	if rej := applyMergeToOptions(values, "fr", "gb"); rej != nil {
		t.Fatalf("refused: %v", rej)
	}
	var src *FieldOption
	walkOptionsPtr(values, func(o *FieldOption) {
		if o.Value == "fr" {
			src = o
		}
	})
	if src == nil {
		t.Fatal("the source option was deleted — a merge must leave a tombstone")
	}
	if src.Status != OptionArchived {
		t.Errorf("status = %q, want archived", src.Status)
	}
	if src.ReplacedBy != "gb" {
		t.Errorf("replaced_by = %q, want gb", src.ReplacedBy)
	}
	if src.Label != "France" {
		t.Errorf("label = %q, want France — a tombstone keeps its identity or it is "+
			"indistinguishable from a term that never existed", src.Label)
	}
}

func TestApplyMergeToOptions_Refusals(t *testing.T) {
	cases := []struct {
		name, source, target string
		wantStatus           OptionStatus
	}{
		{"source is not a term", "nowhere", "gb", ""},
		{"target is not a term", "fr", "nowhere", ""},
		{"target is archived", "fr", "atlantis", OptionArchived},
		{"source is already a tombstone", "great-britain", "fr", OptionArchived},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rej := applyMergeToOptions(curationFixture(), c.source, c.target)
			if rej == nil {
				t.Fatal("accepted")
			}
			if rej.Status != c.wantStatus {
				t.Errorf("status = %q, want %q", rej.Status, c.wantStatus)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// searchVocabulary
// ---------------------------------------------------------------------------

func searchFixture(n int) []FieldOption {
	out := make([]FieldOption, 0, n)
	for i := 0; i < n; i++ {
		slug := "term-" + itoa(i)
		out = append(out, FieldOption{Value: slug, Label: "Term " + itoa(i)})
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The bound is the whole point. A prefix matching hundreds returns
// `limit`, and `matched` still reports the truth.
func TestSearchVocabulary_BoundHolds(t *testing.T) {
	page := searchVocabulary(searchFixture(500), "term-", false,
		openapi.SearchFieldValuesParamsStatusActive, 25)
	if page.Returned != 25 {
		t.Errorf("returned = %d, want 25", page.Returned)
	}
	if len(page.Values) != 25 {
		t.Errorf("len(values) = %d, want 25", len(page.Values))
	}
	if page.Matched != 500 {
		t.Errorf("matched = %d, want 500 — the cap must not distort the total", page.Matched)
	}
	if !page.Truncated {
		t.Error("truncated = false with 500 matches and a limit of 25")
	}
	if page.VocabularySize != 500 {
		t.Errorf("vocabulary_size = %d, want 500", page.VocabularySize)
	}
}

// Ordering is total and stable: raising the limit extends the list, it
// does not reshuffle it. A caller paging by limit depends on this.
func TestSearchVocabulary_OrderingIsStableUnderAWiderLimit(t *testing.T) {
	fixture := searchFixture(200)
	narrow := searchVocabulary(fixture, "term-1", false, openapi.SearchFieldValuesParamsStatusActive, 10)
	wide := searchVocabulary(fixture, "term-1", false, openapi.SearchFieldValuesParamsStatusActive, 40)
	if len(narrow.Values) != 10 || len(wide.Values) < 10 {
		t.Fatalf("narrow=%d wide=%d", len(narrow.Values), len(wide.Values))
	}
	for i := range narrow.Values {
		if narrow.Values[i].Value != wide.Values[i].Value {
			t.Fatalf("row %d moved when the limit widened: %q vs %q",
				i, narrow.Values[i].Value, wide.Values[i].Value)
		}
	}
}

// Rank order: exact, then prefix, then substring. "type it and press
// Enter" has to land on the term the person meant.
func TestSearchVocabulary_RanksExactThenPrefixThenSubstring(t *testing.T) {
	values := []FieldOption{
		{Value: "zzz-sun", Label: "Zzz Sun"},     // substring only
		{Value: "sunset", Label: "Sunset"},       // prefix
		{Value: "sun", Label: "Sun"},             // exact
		{Value: "aaa-sunny", Label: "Aaa Sunny"}, // substring only
	}
	page := searchVocabulary(values, "sun", true, openapi.SearchFieldValuesParamsStatusActive, 10)
	got := make([]string, 0, len(page.Values))
	for _, v := range page.Values {
		got = append(got, v.Value)
	}
	want := "sun,sunset,aaa-sunny,zzz-sun"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

// prefix mode is the default and must NOT admit substring matches —
// otherwise the two modes are one mode and the parameter is a lie.
func TestSearchVocabulary_PrefixModeExcludesSubstrings(t *testing.T) {
	values := []FieldOption{
		{Value: "sunset", Label: "Sunset"},
		{Value: "zzz-sun", Label: "Zzz Sun"},
	}
	page := searchVocabulary(values, "sun", false, openapi.SearchFieldValuesParamsStatusActive, 10)
	if page.Matched != 1 || page.Values[0].Value != "sunset" {
		t.Fatalf("prefix mode matched %d (%v); want only sunset", page.Matched, page.Values)
	}
}

// A term is findable by its alias. A picker that could not offer a term
// its own vocabulary says is addressable by that word would send people
// to the create row for something that already exists.
func TestSearchVocabulary_MatchesAliases(t *testing.T) {
	page := searchVocabulary(curationFixture(), "brit", false,
		openapi.SearchFieldValuesParamsStatusActive, 10)
	if page.Matched != 1 || page.Values[0].Value != "gb" {
		t.Fatalf("alias search matched %d (%v); want gb via `britain`", page.Matched, page.Values)
	}
	if page.Values[0].Aliases == nil || len(*page.Values[0].Aliases) != 2 {
		t.Errorf("aliases were not returned: %+v", page.Values[0])
	}
}

// The default status filter offers only what a picker MAY offer, and
// `any` is the curation view where a tombstone is visible — with its
// replaced_by, which is what makes it distinguishable from a term that
// never existed.
func TestSearchVocabulary_StatusFilter(t *testing.T) {
	active := searchVocabulary(curationFixture(), "", false,
		openapi.SearchFieldValuesParamsStatusActive, 50)
	if active.Matched != 2 {
		t.Errorf("active matched = %d, want 2 (gb, fr)", active.Matched)
	}

	all := searchVocabulary(curationFixture(), "", false,
		openapi.SearchFieldValuesParamsStatusAny, 50)
	if all.Matched != len(curationFixture()) {
		t.Errorf("any matched = %d, want %d", all.Matched, len(curationFixture()))
	}
	var tombstone *openapi.VocabularyValue
	for i := range all.Values {
		if all.Values[i].Value == "great-britain" {
			tombstone = &all.Values[i]
		}
	}
	if tombstone == nil {
		t.Fatal("the merge tombstone is invisible even under status=any")
	}
	if tombstone.ReplacedBy == nil || *tombstone.ReplacedBy != "gb" {
		t.Errorf("tombstone.replaced_by = %v, want gb — without it, a merged-away term "+
			"is indistinguishable from one that never existed", tombstone.ReplacedBy)
	}
	if tombstone.Status != openapi.VocabularyValueStatus(OptionArchived) {
		t.Errorf("tombstone.status = %q, want archived", tombstone.Status)
	}
}

// A tree field's terms carry the ancestor path, so a picker can print
// "Europe / United Kingdom / London" from one response.
func TestSearchVocabulary_CarriesTheAncestorPath(t *testing.T) {
	values := []FieldOption{{
		Value: "europe", Label: "Europe",
		Children: []FieldOption{{
			Value: "uk", Label: "United Kingdom",
			Children: []FieldOption{{Value: "london", Label: "London"}},
		}},
	}}
	page := searchVocabulary(values, "london", false,
		openapi.SearchFieldValuesParamsStatusActive, 10)
	if page.Matched != 1 {
		t.Fatalf("matched = %d", page.Matched)
	}
	if got := strings.Join(page.Values[0].Path, " / "); got != "Europe / United Kingdom / London" {
		t.Fatalf("path = %q", got)
	}
	if page.VocabularySize != 3 {
		t.Errorf("vocabulary_size = %d, want 3 — every term at every depth", page.VocabularySize)
	}
}
