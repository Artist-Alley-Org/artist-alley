// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
//nolint:dupword // the failure strings quote SQL fragments verbatim.

// #1065 — NOTHING ENFORCED THAT AN ASSET FULL-TEXT MATCH GOES THROUGH
// [AssetSearchMatchSQL]. This does.
//
// # The leak class, so whoever trips this can fix it without reading #902
//
// `a.search_text @@ q` decides IN SQL whether a row comes back. A caller
// who searches a phrase occurring only in a restricted asset's title
// watches the result count move 0→1 and has just recovered a word the
// payload withholds — then walks the rest of the title one token at a
// time. Withholding the payload after the fact cannot close that; only
// the MATCH can. #902 put the rule in one fragment,
// [AssetSearchMatchSQL], which is `search_text @@ … AND
// FieldsReadableSQL(…)`.
//
// That made the rule correct and singular. It did not make it
// mandatory: a future feature can write a naked `search_text @@` against
// `assets` in perfect good faith and silently reintroduce #902 on a new
// surface. The project closes rules this way rather than by convention
// elsewhere — the capability-reachability test (#958/#961), the codegen
// drift check, the *_MatchesGo twins — and this is the same shape.
//
// # Why enforcement has to sit here and not in the engine
//
// The structural answer a mature engine uses is to attach the filter to
// the ROLE so a query cannot omit it (OpenSearch's DLS). The Postgres
// analogue is RLS, and it is the wrong GRANULARITY: ADR 0064
// deliberately keeps an unreadable asset LISTED as a placeholder, so an
// RLS policy strong enough to gate the match would also remove the row
// from the listing and take "request access" (#881) with it. Our
// requirement is sub-row — the row stays, its TEXT stops matching — so
// the gate is a conjunct, and a conjunct only protects the sites that
// use it. This test is the part neither design provided.
//
// # ⚠️ WHAT THIS GUARD CANNOT CATCH — read before trusting it
//
// It is TEXTUAL. It checks that the right rule is NAMED at each site,
// never that it is composed correctly:
//
//   - a site that calls AssetSearchMatchSQL and then ORs the result with
//     something permissive passes;
//   - a facet aggregator that obtains a fragment from
//     buildAssetPopulationSQL and then splices it into the wrong branch
//     of its WHERE clause passes;
//   - an allow-listed site whose stated reason is checked by one
//     assertion here can still be unsafe for a second reason nobody
//     wrote down.
//
// So this is a floor, not a proof. It stops the cheap and by far the
// most likely half — a new `search_text @@` against assets with no gate
// at all — and it stops the specific rot that ate `ListAssetsPage`'s
// safety, which was that its only protection was a fact about the rest
// of the tree (nobody calls it) recorded in a comment. Comments do not
// fail builds. This does.
//
// Needs no database.

package visibility

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// appRoot is app/, reached from app/internal/visibility. The guard lives
// beside the rule it enforces rather than in a lint package, so the
// fragment and its enforcement move together.
const appRoot = "../.."

// searchTextMatch finds `search_text @@`, capturing the table qualifier
// (`a.`, `assets.`, or nothing).
var searchTextMatch = regexp.MustCompile(`(?:([A-Za-z_][A-Za-z0-9_]*)\.)?search_text\s*@@`)

// tableBinding finds `FROM|JOIN <table> [AS] <alias>` so an occurrence's
// qualifier can be resolved to a real table. Go's regexp is RE2 and has
// no backreferences, so the optional alias is filtered against
// sqlKeywords below rather than being excluded in the pattern.
var tableBinding = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(assets|posts|collections|asset_tag|post_tags)\b(?:\s+(?:AS\s+)?([A-Za-z_][A-Za-z0-9_]*))?`)

// sqlKeywords are the words that can follow a table name without being
// an alias. Without this, `FROM assets WHERE …` binds the alias "WHERE".
var sqlKeywords = map[string]bool{
	"where": true, "order": true, "group": true, "limit": true, "on": true,
	"left": true, "right": true, "inner": true, "outer": true, "cross": true,
	"join": true, "using": true, "having": true, "set": true, "union": true,
	"as": true, "lateral": true, "offset": true, "returning": true, "values": true,
}

// fragmentProducers are files whose `search_text @@` is inside a
// function that BUILDS a fragment for someone else to splice, rather
// than inside a statement of its own. Such a site has no FROM clause,
// so its table cannot be inferred and is declared instead.
//
// There is exactly one, and there should stay that way: a second entry
// here means a second producer of asset text-match SQL, which is the
// thing #902 consolidated. Adding one is a decision, not a fix.
var fragmentProducers = map[string]string{
	"internal/visibility/fields.go": "assets",
}

// site is one code-line occurrence.
type site struct {
	file      string // path relative to appRoot
	line      int
	qualifier string
	table     string
}

// allowEntry is an exception whose REASON IS DATA, not a comment.
//
// Each entry names the file it exempts, how many occurrences it expects
// (so adding a second unguarded match to an exempt file still fails),
// the justification in words, and — the part that matters — a `verify`
// that asserts the justification is STILL TRUE. An entry that merely
// names a file is a second convention; an entry that checks its own why
// fails when the why stops holding, which is exactly what would have
// happened to ListAssetsPage the moment someone called it.
type allowEntry struct {
	file   string
	count  int
	reason string
	verify func(t *testing.T, tree map[string]string)
}

// allowList is the complete set of asset-bound `search_text @@` sites
// that do not compose AssetSearchMatchSQL.
var allowList = []allowEntry{
	{
		file:  "internal/visibility/fields.go",
		count: 1,
		reason: "this IS AssetSearchMatchSQL — the fragment every other asset " +
			"site must compose. It cannot compose itself.",
		verify: func(t *testing.T, tree map[string]string) {
			src := tree["internal/visibility/fields.go"]
			// The occurrence must be inside the producer, and the
			// producer must still AND the field plane onto it. If that
			// conjunct is ever dropped, every site downstream inherits
			// the leak while this guard keeps passing them.
			if !strings.Contains(src, "func AssetSearchMatchSQL(") {
				t.Error("fields.go no longer defines AssetSearchMatchSQL; this allow-list entry " +
					"exempts a file for being the producer, and it is not the producer any more")
			}
			if !strings.Contains(src, "FieldsReadableSQL(alias, callerArg, caller, caps, mut)") {
				t.Error("AssetSearchMatchSQL no longer ANDs FieldsReadableSQL onto the match. " +
					"That conjunct IS the rule — without it the shared fragment is a naked " +
					"`search_text @@` that every call site now trusts.")
			}
		},
	},
	{
		file:  "internal/assets/queries.sql",
		count: 1,
		reason: "ListAssetsPage is the PARITY ORACLE for " +
			"TestListAssetsPage_AuthenticatedParity. It applies no visibility rule at all, " +
			"deliberately: an oracle that applies the rule under test cannot detect that rule " +
			"being wrong. Its ONLY protection is that no production code calls it.",
		verify: func(t *testing.T, tree map[string]string) {
			// THE ASSERTION THAT WAS RESTING ON NOTHING. The comment
			// above the query says "do NOT promote this to handler
			// code"; a comment cannot notice when somebody does.
			//
			// `ListAssetsPageGated` is the hand-built, gated browse
			// query and shares the prefix, so the pattern requires the
			// open paren.
			call := regexp.MustCompile(`\bListAssetsPage\(`)
			for path, src := range tree {
				switch {
				case strings.HasSuffix(path, "_test.go"):
					// A test cannot serve a request. The oracle exists
					// to be called by one.
					continue
				case path == "internal/assets/queries.sql.go":
					// The generated method declaration itself.
					continue
				}
				if call.MatchString(src) {
					t.Errorf("%s calls ListAssetsPage from production code.\n\n"+
						"That sqlc query has NO visibility rule of any kind — not a weak one, "+
						"none. Its entire safety was that nobody called it, and this allow-list "+
						"entry asserts exactly that, which is why you are reading this instead of "+
						"shipping #902 again on a new surface.\n\n"+
						"Use assets.ListAssetsPageGated, which splices the visibility predicate "+
						"and composes visibility.AssetSearchMatchSQL for its `q` filter.", path)
				}
			}
		},
	},
	{
		file:  "internal/search/facet/aggregators_impl.go",
		count: 5,
		reason: "the four asset aggregators and the tag aggregator's asset half AND the " +
			"content conjunct over the same row, so a row the caller cannot open contributes " +
			"to no bucket whatever the query text says. The count cannot move, so there is " +
			"nothing to probe. Composing the shared fragment on top would render a second " +
			"overlapping OR-tree per row for an answer this clause already decided.",
		verify: func(t *testing.T, tree map[string]string) {
			src := tree["internal/search/facet/aggregators_impl.go"]
			// ⭐ Assert the CONJUNCT, not the site. The issue is
			// explicit about this: naming these five files would be a
			// second convention. What makes them safe is one specific
			// clause, and that clause is what gets checked.
			if !strings.Contains(src, `frag += visibility.FieldsReadableSQL("a"`) {
				t.Error("buildAssetVisibilityAppendedSQL no longer appends " +
					"visibility.FieldsReadableSQL to the asset predicate.\n\n" +
					"The five `a.search_text @@` sites in this file are exempt from " +
					"AssetSearchMatchSQL BECAUSE OF that conjunct and nothing else — its own " +
					"doc calls the dependency out: \"if the FieldsReadableSQL call below is " +
					"ever narrowed away or made conditional the way /search's filter conjunct " +
					"is, these five matches become #902 again\". Either restore it or route " +
					"the matches through visibility.AssetSearchMatchSQL.")
			}
			// And that every aggregator actually takes its fragment
			// from the helper that appends it, rather than building a
			// bare predicate of its own.
			if n := strings.Count(src, "buildAssetPopulationSQL(ctx"); n < 4 {
				t.Errorf("only %d asset aggregators obtain their WHERE clause from "+
					"buildAssetPopulationSQL (want at least 4). An aggregator that builds its "+
					"own predicate does not inherit the conjunct that exempts this file.", n)
			}
		},
	},
}

// TestAssetSearchMatch_EveryTextMatchIsGuarded is the enforcement.
func TestAssetSearchMatch_EveryTextMatchIsGuarded(t *testing.T) {
	tree := loadTree(t)

	allowed := map[string]allowEntry{}
	for _, e := range allowList {
		allowed[e.file] = e
	}

	counts := map[string]int{}
	for _, s := range collectSites(t, tree) {
		// POSTS AND COLLECTIONS ARE OUT OF SCOPE, and not by omission.
		// Their documents are ROW-filtered: a post the caller may not
		// read is not in the result set at all, so a match against it
		// cannot move a count. An asset is different precisely because
		// ADR 0064 keeps the unreadable row LISTED and withholds its
		// FIELDS — which is what makes its text a channel and nothing
		// else's.
		if s.table != "assets" {
			continue
		}
		counts[s.file]++
		if _, ok := allowed[s.file]; ok {
			continue
		}
		t.Errorf(`%s:%d has an unguarded full-text match against assets:

    %ssearch_text @@ …

An asset's indexed text is a DISCLOSURE CHANNEL. `+"`@@`"+` decides in SQL
whether the row comes back, so a caller who guesses a phrase from a
restricted asset's withheld title watches the count move 0→1 and can
walk the rest of it one token at a time. Withholding the payload
afterwards cannot close that — only the match can.

Compose visibility.AssetSearchMatchSQL(alias, tsqueryExpr, callerArg,
caller, caps, mut) instead of writing the operator directly. It is the
one expression of "this asset's text matches this caller's query", and
/search, browse and the suggest source all go through it (#902).

If this site is genuinely safe for a DIFFERENT reason, add an entry to
allowList in this file — and make its verify() assert the reason, not
just name the file.`, s.file, s.line, s.qualifier+".")
	}

	// Every allow-list entry must still be earning its exemption, and
	// must still cover exactly what it claims to cover.
	for _, e := range allowList {
		t.Run("allowlist/"+e.file, func(t *testing.T) {
			got := counts[e.file]
			if got == 0 {
				t.Errorf("allow-list entry for %s exempts a site that no longer exists. "+
					"Remove the entry — a stale exemption is a hole waiting for the next "+
					"`search_text @@` someone adds to that file.", e.file)
				return
			}
			if got != e.count {
				t.Errorf("%s has %d asset text-match sites, the allow-list expects %d.\n\n"+
					"The exemption's reason — %q — was written about the sites that were there. "+
					"A new one does not inherit it. Check the new site and either guard it or "+
					"extend the reason to cover it deliberately.", e.file, got, e.count, e.reason)
				return
			}
			e.verify(t, tree)
		})
	}
}

// loadTree reads every hand-written .go and .sql file under app/.
//
// # What is skipped, and why each exclusion is safe
//
//   - vendor/, node_modules/, .git — not ours.
//   - *.sql.go and *_gen.go / *.gen.go — GENERATED. Their SQL text is
//     copied verbatim from the .sql and .yaml sources, which ARE
//     scanned, so a finding there is not actionable in the .go file and
//     hand-editing it would be undone by the next regeneration. This is
//     the same scoping migration_citations_test.go (#964) uses.
//     ⚠️ It is safe ONLY because the source is scanned: the guard would
//     miss a query that exists solely as generated output. Nothing in
//     this tree does — sqlc generates from .sql — and if a generator
//     that invents SQL is ever added, this exclusion must shrink.
//   - *_test.go — a test file cannot serve a request. The three
//     occurrences in the tree are ASSERTIONS about the composed
//     fragment (fields_sql_test.go, field_plane_agreement_test.go), and
//     requiring those to compose the thing they are checking would be
//     circular. This is a class exemption and is the loosest thing here.
func loadTree(t *testing.T) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.Walk(appRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sql") {
			return nil
		}
		if strings.HasSuffix(name, ".sql.go") ||
			strings.HasSuffix(name, "_gen.go") ||
			strings.HasSuffix(name, ".gen.go") {
			// Still recorded — the ListAssetsPage caller sweep needs to
			// see generated files — but marked so site collection skips
			// them.
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			rel, _ := filepath.Rel(appRoot, path)
			tree[filepath.ToSlash(rel)] = string(b)
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(appRoot, path)
		tree[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", appRoot, err)
	}
	if len(tree) < 100 {
		t.Fatalf("only %d files found under %s — the guard is scanning the wrong tree and "+
			"would pass vacuously", len(tree), appRoot)
	}
	return tree
}

// collectSites finds every `search_text @@` on a CODE line and resolves
// the table it matches against.
//
// Comment lines (`//` or `--` as the first non-space token) are skipped:
// this rule is discussed in prose in eight places and a guard that
// cannot tell a sentence from a statement is a guard nobody keeps. A
// trailing comment after code is NOT skipped, so the conservative
// direction — treat it as code and demand a gate — is the one taken
// when the two are on one line.
func collectSites(t *testing.T, tree map[string]string) []site {
	t.Helper()
	var out []site
	for path, src := range tree {
		if strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, ".sql.go") ||
			strings.HasSuffix(path, "_gen.go") ||
			strings.HasSuffix(path, ".gen.go") {
			continue
		}
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue
			}
			m := searchTextMatch.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			s := site{file: path, line: i + 1, qualifier: m[1]}
			if declared, ok := fragmentProducers[path]; ok {
				// A FRAGMENT BUILDER has no FROM clause to resolve
				// against — it is parameterised by the caller's alias
				// and is spliced into somebody else's statement. The
				// table it concerns is therefore DECLARED here rather
				// than inferred, and the declaration is not a free pass:
				// the file still has to appear on allowList, and that
				// entry's verify() is what checks the fragment is still
				// the guarded one.
				s.table = declared
			} else {
				s.table = resolveTable(lines, i, s.qualifier)
			}
			if s.table == "" {
				// FAIL CLOSED. An occurrence whose table the guard
				// cannot name is one it cannot clear, and guessing
				// "probably posts" is how this stops being enforcement.
				t.Errorf("%s:%d — cannot resolve which table `%ssearch_text @@ …` matches "+
					"against. The guard walks back for a FROM/JOIN binding the qualifier; "+
					"make the binding visible (alias the table where it is selected) or add "+
					"an allow-list entry that states why the site is safe.",
					path, i+1, s.qualifier+".")
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// resolveTable walks BACKWARDS from the occurrence looking for the
// FROM/JOIN that binds its qualifier, then forwards a short way for the
// case where the SELECT list is written above its FROM (which is most
// of them). Returns "" when it cannot decide.
func resolveTable(lines []string, idx int, qualifier string) string {
	const back, fwd = 40, 15

	scan := func(from, to int) string {
		var found string
		for i := from; i <= to && i < len(lines); i++ {
			if i < 0 {
				continue
			}
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue
			}
			for _, m := range tableBinding.FindAllStringSubmatch(lines[i], -1) {
				table, alias := m[1], m[2]
				if sqlKeywords[strings.ToLower(alias)] {
					alias = ""
				}
				switch {
				case qualifier == "" && alias == "":
					// Unqualified column, unaliased table: the nearest
					// such binding owns it.
					found = table
				case qualifier != "" && alias == qualifier:
					found = table
				case qualifier != "" && alias == "" && table == qualifier:
					// `FROM assets` with `assets.search_text`.
					found = table
				}
			}
		}
		return found
	}

	// Backwards first: the nearest preceding binding wins, so scan the
	// window and keep the LAST match before the occurrence.
	if got := scan(idx-back, idx); got != "" {
		return got
	}
	// Then forwards, for `WHERE …` written above `FROM …` — which the
	// hand-built builders in this tree do not do, but the sqlc sources
	// effectively do when the WHERE spans many lines below a distant
	// FROM. Cheap, and the alternative is a false failure.
	return scan(idx, idx+fwd)
}
