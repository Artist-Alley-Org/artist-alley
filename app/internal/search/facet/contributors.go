// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The CONTRIBUTOR LOOKUP — "who made the work this search is looking
// at" (#1173, sprint 18d).
//
// The advanced page needs to offer a person a list of contributors to
// narrow by. `ownerAgg` beside this file already computes exactly the
// right POPULATION — owners of the assets this caller can see, under
// this caller's active filters — and then throws most of it away: it
// stops at `LIMIT 25`, orders by count, and labels each bucket with a
// bare `COALESCE(u.username, ref)`. That is a rail's job. A picker has
// two more:
//
//  1. it has to be able to reach EVERY qualifying contributor, not the
//     busiest 25, which means continuation; and
//  2. it has to render each one the way the rest of the product does,
//     which is [users.ResolveDisplayName] and nothing else.
//
// # ⛔ WHY THE POPULATION IS SHARED RATHER THAN RE-DERIVED
//
// [buildAssetPopulationSQL] is the visibility gate, the caller's
// capabilities, the mature axis and the active facet selection, in one
// expression, reduced by [Selection.ForFacet] so the dimension does not
// filter itself out of existence. Writing a second "which assets can
// this caller see" clause here would be a second copy of the read rule,
// which is the failure ADR 0093 and ADR 0070 both exist to prevent. So
// this file calls it with `own = FacetOwner` — the SAME call `ownerAgg`
// makes — and adds only the parts a picker needs that a bucket list
// does not: a name predicate, a keyset, and a page size.
//
// # ⛔ THE LABEL IS RESOLVED IN GO, THE MATCH IS DECIDED IN SQL
//
// ADR 0070 §3 and its 2026-08-13 amendment: the display-name ladder had
// been copied into SQL three times before anyone noticed, and the fix
// was ONE Go expression plus ONE SQL builder held together by a twin
// test. This file adds neither. It returns the three STORED identity
// columns verbatim and lets the caller resolve them, because:
//
//   - Precedence answers WHICH NAME TO SHOW. It does not answer whether
//     a row matches. A person typing "tan" is looking for the human
//     whose name contains it wherever it is stored, so the predicate is
//     the three columns ORed INDEPENDENTLY — not a prefix match against
//     a reconstructed `COALESCE(display_name, fullname, username)`,
//     which would be the ladder's fourth copy and would also make a
//     match depend on which rung happened to win.
//   - Rung 4 (`user <ref>`) is INVENTED rather than stored, so no SQL
//     predicate can reach it. A contributor whose three name columns are
//     all empty is therefore unreachable by ANY prefix, which is why
//     [ContributorQuery.Prefix] is optional — see its doc.
package facet

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ContributorRow is one qualifying contributor, as the three STORED
// identity columns plus the ref.
//
// ⛔ There is deliberately no `DisplayName` here and there must never be
// one. Resolving it needs the caller's anonymity, which is a property of
// the REQUEST rather than of the row, and putting the resolution in this
// package would put a second [users.ResolveDisplayName] in the tree —
// the exact shape ADR 0070's 2026-08-13 amendment found three copies of.
// `facet` also cannot import `users` without a cycle through `openapi`,
// which is the compiler agreeing.
//
// WHITELIST BY CONSTRUCTION, the same discipline [users.LookupAuthors]
// states: the SELECT names four columns and this struct is built field
// by field from them, so an email or an account state added to `user`
// later is withheld because it was never fetched.
type ContributorRow struct {
	// Ref is the contributor's `user.ref` — the wire identity, and the
	// value an `owner:` filter term carries.
	//
	// ⛔ NOT the username. `user_username_uniq_idx` is unique
	// CASE-SENSITIVELY while [Selection.dimensionSQL]'s FacetOwner arm
	// matches `LOWER(fu.username) = LOWER($n)`, so two rows can satisfy
	// one term; `username` is nullable; and a numeric username collides
	// with the same predicate's `owner_user_ref::TEXT = $n` arm.
	Ref int64
	// Username, Fullname and ProfileDisplayName are rungs 3, 2 and 1 of
	// the ADR 0070 ladder, EXACTLY as stored. Nil and "" are both normal:
	// all three columns are nullable and the profile row is a LEFT JOIN.
	Username           *string
	Fullname           *string
	ProfileDisplayName string
	// Assets is how many rows of the population this contributor owns.
	// It is the sort key, and it is NOT part of the wire contract — see
	// [ContributorQuery] for why the order is this one.
	Assets int64
}

// ContributorCursor is the keyset a continuation resumes from: the last
// row's sort key plus its `user_ref`.
//
// The order is `(Assets DESC, Ref ASC)` and `Ref` is UNIQUE, so the pair
// is a total order over the candidate set. That is the whole property a
// keyset needs: with a unique final tiebreak no two rows compare equal,
// so the predicate below can never re-emit a row it has passed (a
// duplicate) nor step over one it has not (a skip). An order on `Assets`
// alone would do both, because ties are the common case — most
// contributors own one asset.
type ContributorCursor struct {
	Assets int64 `json:"n"`
	Ref    int64 `json:"r"`
}

// ErrContributorLimit is returned for a non-positive or oversized page
// size. The handler maps it to 400.
var ErrContributorLimit = errors.New("facet: contributor limit out of range")

// MaxContributorLimit caps one page. A picker asks for a screenful; a
// caller asking for the whole population in one response is asking for
// the continuation to be optional, and it is not.
const MaxContributorLimit = 100

// DefaultContributorLimit is the page size when the caller names none.
//
// Deliberately EQUAL to the 25 `ownerAgg` returns, so the first page of
// this endpoint under an empty prefix is the same SET as the owner
// facet's buckets for the same query — one population with two
// renderings rather than two populations that can disagree. Measured on
// the dev corpus: 25 of 25 identical.
//
// ⚠️ The ORDER can differ, and only where `ownerAgg`'s is undefined:
// its `ORDER BY n DESC` has no tiebreak, so two contributors owning the
// same number of assets come back in whatever order the plan produced.
// This query adds `ref ASC` because a keyset REQUIRES a total order —
// see [ContributorCursor]. Asserting set equality is therefore the
// honest claim; asserting sequence equality would be asserting something
// about the other query that the other query does not promise.
const DefaultContributorLimit = 25

// ContributorQuery is one page of the contributor lookup.
type ContributorQuery struct {
	// Request is the POPULATION: query text, active selection, caller,
	// capabilities and the mature axis. Identical in meaning to the one
	// `/search/facets` builds, and consumed through the same
	// [buildAssetPopulationSQL].
	Request Request

	// Prefix narrows by NAME, and it is OPTIONAL.
	//
	// ⛔ AN EMPTY PREFIX IS A VALID QUERY, NOT A MISSING ARGUMENT, and
	// this is the load-bearing part of the contract rather than a
	// convenience. All three stored name columns are nullable and none
	// carries a NOT NULL or a CHECK, so a contributor can exist with no
	// stored human-readable name at all. [users.ResolveDisplayName]
	// renders that row `user <ref>` — rung 4, which is INVENTED and
	// therefore appears in no column any predicate can read. A REQUIRED
	// prefix would make such a contributor unreachable forever, and
	// continuation cannot repair it because the row never enters the
	// candidate set in the first place. Reproducing rung 4 in SQL to
	// make it matchable would rebuild the ADR 0070 ladder in a fourth
	// place, so the answer is that browsing works with no prefix at all.
	//
	// A NON-EMPTY prefix matches the three columns INDEPENDENTLY,
	// case-insensitively, anchored at the start of each column.
	Prefix string

	// After resumes a previous page. nil is the first page.
	After *ContributorCursor

	// Limit is the page size; 0 means [DefaultContributorLimit].
	Limit int
}

// ContributorPage is one response: the rows, and whether more follow.
type ContributorPage struct {
	Rows []ContributorRow
	// Next is the cursor for the following page, or nil when this page
	// is the last one.
	//
	// ⛔ nil is a TERMINAL claim and it is computed by over-fetching one
	// row rather than by comparing `len(Rows)` to the limit. A full final
	// page is indistinguishable from a full non-final page, so the
	// `len == limit` shortcut reports "more available" on a set that has
	// none — a picker then renders a Load more that returns nothing,
	// which is the same class of lie as a filter that looks applied and
	// is not.
	Next *ContributorCursor
}

// Contributors returns one page of the contributors qualifying under the
// request's population.
//
// The caller is responsible for [Selection.Authorize] — the same
// division [Dispatcher.Run] uses, where authorization is a whole-query
// yes/no answered once at the chokepoint rather than per aggregator.
func Contributors(
	ctx context.Context,
	pool *pgxpool.Pool,
	q ContributorQuery,
) (ContributorPage, error) {
	limit := q.Limit
	if limit == 0 {
		limit = DefaultContributorLimit
	}
	if limit < 1 || limit > MaxContributorLimit {
		return ContributorPage{}, ErrContributorLimit
	}

	// $1 is the query text, exactly as every asset aggregator binds it,
	// so the population fragment's placeholders start at $2.
	frag, args, ok, err := buildAssetPopulationSQL(ctx, q.Request, FacetOwner, 1)
	if err != nil {
		return ContributorPage{}, err
	}
	if !ok {
		// The selection names a dimension assets do not have, so no
		// asset qualifies and therefore no contributor does. The
		// fail-closed direction every call site of buildAssetPopulationSQL
		// already honours.
		return ContributorPage{}, nil
	}

	queryArgs := append([]any{q.Request.QueryText}, args...)
	// bind appends one argument and returns the placeholder that names
	// it, so the numbering below cannot drift from the slice.
	bind := func(v any) string {
		queryArgs = append(queryArgs, v)
		return "$" + strconv.Itoa(len(queryArgs))
	}

	// The NAME predicate. Empty prefix ⇒ no predicate at all, which is
	// what makes the complete population browsable.
	nameFrag := ""
	if p := strings.TrimSpace(q.Prefix); p != "" {
		// LIKE is the anchored form, so the pattern is built here and the
		// caller's bytes never reach the SQL text. `%` and `_` are
		// wildcards in a LIKE pattern and a person typing them means the
		// characters, so they are escaped along with the escape character
		// itself. `lower()` on both sides because the three lower(...)
		// indexes on `user` are spelled that way and because a person
		// typing a name is not typing its capitalisation.
		pat := bind(likePrefixPattern(p))
		nameFrag = `
		   AND (LOWER(COALESCE(p.display_name, '')) LIKE ` + pat + ` ESCAPE '\'
		     OR LOWER(COALESCE(u.fullname, ''))     LIKE ` + pat + ` ESCAPE '\'
		     OR LOWER(COALESCE(u.username, ''))     LIKE ` + pat + ` ESCAPE '\')`
	}

	// The KEYSET. Spelled out rather than as a row comparison because
	// the two columns run in OPPOSITE directions, and `(n, -ref) < (…)`
	// is the same predicate with an arithmetic trap in it.
	cursorFrag := ""
	if c := q.After; c != nil {
		n := bind(c.Assets)
		r := bind(c.Ref)
		cursorFrag = `
		   AND (c.n < ` + n + `::BIGINT
		     OR (c.n = ` + n + `::BIGINT AND c.ref > ` + r + `::BIGINT))`
	}

	// One row more than asked for, so "is there a next page" is OBSERVED
	// rather than inferred from a full page. See [ContributorPage.Next].
	lim := bind(int64(limit) + 1)

	sql := `
		WITH pop AS (
			SELECT a.owner_user_ref AS ref, COUNT(*)::BIGINT AS n
			  FROM assets a
			 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))
			   AND a.owner_user_ref IS NOT NULL` + frag + `
			 GROUP BY a.owner_user_ref
		)
		SELECT c.ref,
		       u.username,
		       u.fullname,
		       COALESCE(p.display_name, '') AS display_name,
		       c.n
		  FROM pop c
		  JOIN "user" u ON u.ref = c.ref
		  LEFT JOIN user_profiles p ON p.user_ref = u.ref
		 WHERE TRUE` + nameFrag + cursorFrag + `
		 ORDER BY c.n DESC, c.ref ASC
		 LIMIT ` + lim

	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return ContributorPage{}, err
	}
	defer rows.Close()

	out := make([]ContributorRow, 0, limit+1)
	for rows.Next() {
		var r ContributorRow
		if err := rows.Scan(&r.Ref, &r.Username, &r.Fullname, &r.ProfileDisplayName, &r.Assets); err != nil {
			return ContributorPage{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ContributorPage{}, err
	}

	page := ContributorPage{}
	if len(out) > limit {
		last := out[limit-1]
		page.Next = &ContributorCursor{Assets: last.Assets, Ref: last.Ref}
		out = out[:limit]
	}
	page.Rows = out
	return page, nil
}

// likePrefixPattern renders a literal prefix as an anchored,
// lower-cased LIKE pattern with `\` as the escape character.
//
// The escape character is escaped FIRST; doing it last would re-escape
// the backslashes this function just introduced.
func likePrefixPattern(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(strings.ToLower(prefix)) + "%"
}
