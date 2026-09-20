// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The RECENT WINDOW: `last:N` as a predicate (#1173, sprint 25b).
//
// # What the predicate says
//
// "This row sorts at or before the N-th newest row of the union of every
// requested entity's eligible rows", under ONE total order:
//
//	recency DESC, id DESC, type ASC
//
// where recency is each entity's clock ([RecentClock]) and type ASC is
// the hit type string order (`asset` < `collection` < `post`), rendered
// in SQL as an integer rank that DESCENDS in that order ([RecentRank])
// so the whole tuple compares in one direction and one row comparison
// spells "at or before". The search engine's keyset, Go merge and Go
// cursor cut are written from the same rank, so the four agree by
// construction rather than by coincidence.
//
// # ⛔ THE ARMS ARE RENDERED BY THE EXECUTION SITE, NOT HERE
//
// The union's arms are the three entities' BASELINE eligibility: the
// row plane, the mature axis and the field plane for assets; the post
// read rule with post capabilities and the post mature axis for posts;
// collection readability and soft-deletion for collections. Those are
// caller-dependent, they bind arguments, and they need a context, none
// of which [Selection.SQL]'s one-placeholder-per-term contract carries.
// Rather than widen that contract, the site renders the arms with the
// renderers it already calls ([RecentArms], which is built from
// buildAssetVisibilityAppendedSQL and the tag aggregator's post half,
// the same fragments the counts are made of), binds their arguments
// BEFORE the selection's, and hands the already-bound fragments in
// through [RenderContext.RecentArms], the way [RenderContext.CallerArg]
// carries an already-bound placeholder. One authority for eligibility;
// the count equals the rows because both statements receive the same
// fragments; and a site that supplies no arms gets an UNSATISFIABLE
// dimension, the fail-closed direction the rest of the file takes.
//
// # Why the cutoff is a scalar subquery and not a per-row count
//
// "Fewer than N eligible rows sort before this one" is the same
// statement, and it costs a scan per candidate row. The N-th key of the
// union is one uncorrelated subquery Postgres evaluates once per
// statement (an InitPlan), ordered by the three clocks' indexes
// (`assets_created_at_idx`, `collections_created_at_idx`, and 25b's
// `posts_recent_idx`), and every row then compares against it. When the
// union holds fewer than N rows the subquery yields its LAST row, so
// every eligible row passes; it can never yield no row when it is
// evaluated, because it is evaluated on a row that is itself in the
// union.
package facet

import (
	"context"
	"strconv"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// RecentArm is one entity's eligibility for the recent window, already
// rendered and already bound by the execution site (#1173, sprint 25b).
type RecentArm struct {
	// Entity is which table this arm ranks.
	Entity visibility.EntityType
	// Alias is the table alias the fragment was rendered against, and
	// the alias the union arm declares. It MUST NOT be empty: an
	// un-aliased inner FROM would let the fragment's column references
	// bind to an outer table of the same name.
	Alias string
	// Where is the WHERE-clause suffix for this entity's baseline: it
	// starts with " AND " (or is empty when nothing narrows), and every
	// placeholder it names is bound by the statement that splices the
	// selection.
	Where string
}

// RecentClock is the column an entity's recency is read from.
//
// Assets and collections rank on `created_at`. Posts rank on `posted_at`,
// the column the browse feed orders and keysets by (posts/list_page.go),
// which an author may set apart from the row's `created_at`. A post
// hit's public `created_at` stays `posts.created_at`; only the ORDER
// reads this column.
func RecentClock(e visibility.EntityType) string {
	if e == visibility.EntityPost {
		return "posted_at"
	}
	return "created_at"
}

// RecentRank renders the type tie-break as an integer that DESCENDS in
// hit-type string order, so `type ASC` and `rank DESC` are one fact:
// `asset` (2) before `collection` (1) before `post` (0).
//
// The engine sorts its merged page and cuts its cursor on the same
// function, and its keyset compares the same integer, which is what
// makes "the same ordering fact drives every consumer" a property of
// the code rather than a sentence in a document.
func RecentRank(e visibility.EntityType) int {
	switch e {
	case visibility.EntityAsset:
		return 2
	case visibility.EntityCollection:
		return 1
	case visibility.EntityPost:
		return 0
	}
	return -1
}

// RecentWindow reports the `last:` window this selection carries, if
// any. ok is false for a selection without one; n is the canonical
// value, already validated.
func (s Selection) RecentWindow() (n int, ok bool) {
	for _, t := range s.terms {
		if t.Type != FacetLast {
			continue
		}
		v, err := strconv.Atoi(t.Value)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// RecentArms renders the baseline eligibility of each requested entity
// for a recent window, from the SAME renderers the aggregators count
// with, binding arguments from offset+1 (#1173, sprint 25b).
//
// `req` is the population (caller, capabilities, mature axis); its
// QueryText, Facets and Selection are not read: a baseline is the
// readability alone, and the text and every other term narrow inside
// the window that these arms define.
//
// Aliases are `a`, `c` and `p`, the aggregators' own. They are declared
// by the union arm that splices each fragment, so the fragment's column
// references resolve to the inner table even when the outer statement
// reads the same table under the same alias.
func RecentArms(
	ctx context.Context,
	req Request,
	types []visibility.EntityType,
	offset int,
) ([]RecentArm, []any, error) {
	arms := make([]RecentArm, 0, len(types))
	var args []any
	for _, e := range types {
		var (
			frag  string
			fargs []any
			err   error
		)
		switch e {
		case visibility.EntityAsset:
			frag, fargs, err = buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps,
				req.MutationCaps, req.Mature, offset+len(args))
		case visibility.EntityPost:
			frag, fargs, err = buildPostVisibilityAppendedSQL(ctx, req.Caller, req.PostCaps,
				req.Caps.SystemAdmin, req.Mature, "p", offset+len(args))
		case visibility.EntityCollection:
			frag, fargs, err = buildCollectionVisibilitySQL(ctx, req.Caller, req.Caps.Checker(),
				"c", offset+len(args))
		default:
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		arms = append(arms, RecentArm{Entity: e, Alias: recentAlias(e), Where: frag})
		args = append(args, fargs...)
	}
	return arms, args, nil
}

// recentAlias is the alias each entity's arm is rendered against.
func recentAlias(e visibility.EntityType) string {
	switch e {
	case visibility.EntityAsset:
		return "a"
	case visibility.EntityCollection:
		return "c"
	case visibility.EntityPost:
		return "p"
	}
	return ""
}

// recentTable is the table each entity's arm reads.
func recentTable(e visibility.EntityType) string {
	switch e {
	case visibility.EntityAsset:
		return "assets"
	case visibility.EntityCollection:
		return "collections"
	case visibility.EntityPost:
		return "posts"
	}
	return ""
}

// recentWindowSQL renders the [FacetLast] predicate for entity e, whose
// clock and id are read through alias prefix `a` (`"assets."`, `"c."`,
// `""`), against the window formed from `arms`, with placeholder `p`
// holding N.
//
// Returns ok=false when e is not among the arms: an entity the window
// was not formed over cannot be inside it, which is the same answer as
// "not requested".
func recentWindowSQL(e visibility.EntityType, a, p string, arms []RecentArm) (string, bool) {
	inWindow := false
	unions := make([]string, 0, len(arms))
	for _, arm := range arms {
		if arm.Alias == "" || recentTable(arm.Entity) == "" {
			return "", false
		}
		if arm.Entity == e {
			inWindow = true
		}
		al := arm.Alias
		unions = append(unions, "SELECT "+al+"."+RecentClock(arm.Entity)+" AS ts, "+al+".id AS id, "+
			strconv.Itoa(RecentRank(arm.Entity))+" AS rank FROM "+recentTable(arm.Entity)+" "+al+
			" WHERE TRUE"+arm.Where)
	}
	if !inWindow {
		return "", false
	}
	// `p` holds the canonical digits as TEXT (every term binds one text
	// argument); the cast to BIGINT is safe because CanonicalValue has
	// already proven the bytes are digits in range.
	return `ROW(` + a + RecentClock(e) + `, ` + a + `id, ` + strconv.Itoa(RecentRank(e)) + `) >= (
			SELECT w.ts, w.id, w.rank FROM (
				SELECT u.ts, u.id, u.rank FROM (
					` + strings.Join(unions, "\n\t\t\t\t\tUNION ALL\n\t\t\t\t\t") + `
				) u
				ORDER BY u.ts DESC, u.id DESC, u.rank DESC
				LIMIT (` + p + `::TEXT)::BIGINT
			) w
			ORDER BY w.ts ASC, w.id ASC, w.rank ASC
			LIMIT 1)`, true
}
