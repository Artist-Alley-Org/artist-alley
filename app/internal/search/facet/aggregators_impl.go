// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// buildAssetVisibilityAppendedSQL composes the two planes an asset
// aggregate must respect and returns the WHERE-fragment suffix + bound
// args so each aggregator adds them cleanly.
//
// THE SENSITIVITY DECISION (#899). Facet counts now include only rows
// the caller could actually OPEN, not every row they can see exists.
// That is a deliberate narrowing, and it is deliberately narrower than
// `total_count` on /search beside it, so it is worth stating why.
//
// The judgement is different for the SENSITIVITY facet than for the
// others, and it is the sensitivity facet that forced the change. ADR
// 0064 keeps a restricted row LISTED, so its existence is already
// disclosed and a count that includes it arguably discloses nothing
// new. But a facet does not report existence — it reports a PROPERTY,
// as a filterable dimension. `sensitivity: restricted 1` states this
// item's tier; `extension: ogg 1` states its file type; `asset_type:
// Audio 1` states its kind. Every one of those is a field #899
// withholds from the payload of that same asset, handed back as an
// aggregate. With a narrow enough query the aggregate is the item.
//
// So all four asset aggregators take the content plane, not just
// sensitivity: fixing one and leaving `extension` to answer the same
// question is a distinction an attacker will not respect. (`tag` used to
// be exempt here on the grounds that it aggregated through POSTS only;
// since #907 it counts `asset_tag` too, and its asset half composes this
// same clause. A tag is a withheld field like the rest.)
//
// The counts are what the caller can open, and so are strictly less
// than or equal to the result total. An owner still sees `restricted N`
// for their own work, which is the case that made "just drop the
// sensitivity facet" wrong.
func buildAssetVisibilityAppendedSQL(ctx context.Context, caller visibility.Caller, caps visibility.ContentCaps, offset int) (string, []any, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return "", nil, err
	}
	frag, args := pred.ToSQL("a", offset)
	// The content plane binds no new placeholder: the caller ref is
	// inlined as a literal because it is an int64 this package produced,
	// never caller-supplied text, and threading another placeholder
	// through four aggregators' arg lists is where an off-by-one lives.
	frag += visibility.ContentReadableSQL("a", strconv.FormatInt(caller.UserRef, 10), caps)
	return frag, args, nil
}

// buildAssetPopulationSQL is [buildAssetVisibilityAppendedSQL] plus the
// caller's ACTIVE facet selection (#907) — the full "which asset rows
// am I counting" clause, in one place, so the four asset aggregators
// cannot each narrow by a slightly different set.
//
// `own` is the aggregator's own facet type; the selection is reduced by
// [Selection.ForFacet] so an OR dimension does not filter itself out of
// existence. See that method for the rule.
//
// satisfiable=false means the selection names a dimension assets do not
// have. No dimension is post-only today so it cannot fire, but the
// caller honours it rather than assuming — the fail-closed direction
// costs one branch and survives the next dimension.
func buildAssetPopulationSQL(
	ctx context.Context,
	req Request,
	own FacetType,
	offset int,
) (string, []any, bool, error) {
	frag, args, err := buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps, offset)
	if err != nil {
		return "", nil, false, err
	}
	selFrag, selArgs, ok := req.Selection.ForFacet(own).SQL(
		visibility.EntityAsset, "a", offset+len(args))
	if !ok {
		return "", nil, false, nil
	}
	return frag + selFrag, append(args, selArgs...), true, nil
}

// assetTypeAgg counts assets grouped by their asset_type ref +
// joined to asset_type.name for display. Visibility gate + q text
// match applied before aggregation (per B-2 decision 6).
type assetTypeAgg struct{}

func (assetTypeAgg) Type() FacetType { return FacetAssetType }

func (assetTypeAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	frag, args, ok, err := buildAssetPopulationSQL(ctx, req, FacetAssetType, 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	sql := `
		SELECT a.asset_type::text AS value,
		       COALESCE(t.name, a.asset_type::text) AS label,
		       COUNT(*)::BIGINT AS n
		  FROM assets a
		  LEFT JOIN asset_types t ON t.ref = a.asset_type
		 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))` + frag + `
		 GROUP BY a.asset_type, t.name
		 ORDER BY n DESC
		 LIMIT 25
	`
	queryArgs := append([]any{req.QueryText}, args...)
	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, 25)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Label, &b.Count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// sensitivityAgg counts assets grouped by sensitivity enum.
type sensitivityAgg struct{}

func (sensitivityAgg) Type() FacetType { return FacetSensitivity }

func (sensitivityAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	frag, args, ok, err := buildAssetPopulationSQL(ctx, req, FacetSensitivity, 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	sql := `
		SELECT a.sensitivity, COUNT(*)::BIGINT
		  FROM assets a
		 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))` + frag + `
		 GROUP BY a.sensitivity
		 ORDER BY 2 DESC
	`
	queryArgs := append([]any{req.QueryText}, args...)
	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, 4)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// ownerAgg groups assets by owner_user_ref → username. Users with no
// display_name fall back to username.
type ownerAgg struct{}

func (ownerAgg) Type() FacetType { return FacetOwner }

func (ownerAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	frag, args, ok, err := buildAssetPopulationSQL(ctx, req, FacetOwner, 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	sql := `
		SELECT a.owner_user_ref::text AS value,
		       COALESCE(u.username, a.owner_user_ref::text) AS label,
		       COUNT(*)::BIGINT AS n
		  FROM assets a
		  LEFT JOIN "user" u ON u.ref = a.owner_user_ref
		 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))
		   AND a.owner_user_ref IS NOT NULL` + frag + `
		 GROUP BY a.owner_user_ref, u.username
		 ORDER BY n DESC
		 LIMIT 25
	`
	queryArgs := append([]any{req.QueryText}, args...)
	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, 25)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Label, &b.Count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// extensionAgg groups assets by file_extension (lowercased).
type extensionAgg struct{}

func (extensionAgg) Type() FacetType { return FacetExtension }

func (extensionAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	frag, args, ok, err := buildAssetPopulationSQL(ctx, req, FacetExtension, 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	sql := `
		SELECT LOWER(a.file_extension), COUNT(*)::BIGINT
		  FROM assets a
		 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))
		   AND a.file_extension IS NOT NULL
		   AND a.file_extension <> ''` + frag + `
		 GROUP BY LOWER(a.file_extension)
		 ORDER BY 2 DESC
		 LIMIT 25
	`
	queryArgs := append([]any{req.QueryText}, args...)
	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, 25)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// tagAgg counts entities carrying each tag, across BOTH tagged
// entities: assets (`asset_tag`) and posts (`post_tags`).
//
// #873 — the post half's count is only right if the predicate is the
// same one the feed runs. It was not: this composed `public OR author`
// while browse composed the full rule, so every tag that appears only on
// org-only or followers posts was counted as though those posts did not
// exist. Under-counting is the failure that looks like nothing at all.
//
// #907 — and the ASSET half was missing entirely, which is the same
// failure with the volume turned up. `asset_tag` is a first-class
// tagging surface in this product: it is what `/assets?tag=` filters on
// (assets.ListAssetsPageGated), what the AI tagger writes, and what the
// seed catalogue populates — 6,500 rows against 3,400 post_tags on the
// reference dataset. Nearly two thirds of the tagging in the corpus was
// invisible to the one facet a user reaches for first.
//
// Counting both halves is not a widening of convenience: this is the
// dimension the FILTER spans (see [Selection.dimensionSQL]), and a count
// computed over a different population than the filter it labels is the
// #907 defect restated. One population, both places.
//
// The two halves are counted, not unioned as rows: an asset and a post
// are different entities and each contributes 1. That matches
// /search's total_count, which likewise sums per-entity totals.
type tagAgg struct{}

func (tagAgg) Type() FacetType { return FacetTag }

func (tagAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	// The asset half — visibility + the content plane + the active
	// selection, exactly as the four asset aggregators above compose it.
	// A tag IS one of the fields visibility.FieldsReadable withholds, so
	// a restricted asset must not contribute its tags to anyone who
	// cannot open it.
	assetFrag, assetArgs, assetOK, err := buildAssetPopulationSQL(ctx, req, FacetTag, 1)
	if err != nil {
		return nil, err
	}

	// The post half. Placeholders continue after the asset half's, so
	// the two branches share $1 (the query text) and nothing else.
	postPred, err := visibility.Filter(ctx, visibility.EntityPost, req.Caller,
		visibility.WithPostCaps(req.PostCaps))
	if err != nil {
		return nil, err
	}
	postOffset := 1 + len(assetArgs)
	postFrag, postArgs := postPred.ToSQL("p", postOffset)
	postSelFrag, postSelArgs, postOK := req.Selection.ForFacet(FacetTag).SQL(
		visibility.EntityPost, "p", postOffset+len(postArgs))
	postFrag += postSelFrag
	postArgs = append(postArgs, postSelArgs...)

	branches := make([]string, 0, 2)
	queryArgs := []any{req.QueryText}
	if assetOK {
		branches = append(branches, `
			SELECT at.tag AS tag, COUNT(DISTINCT at.asset_id)::BIGINT AS n
			  FROM asset_tag at
			  JOIN assets a ON a.id = at.asset_id
			 WHERE ($1::TEXT = '' OR a.search_text @@ plainto_tsquery('english', $1))`+assetFrag+`
			 GROUP BY at.tag`)
		queryArgs = append(queryArgs, assetArgs...)
	}
	if postOK {
		branches = append(branches, `
			SELECT pt.tag AS tag, COUNT(DISTINCT pt.post_id)::BIGINT AS n
			  FROM post_tags pt
			  JOIN posts p ON p.id = pt.post_id
			 WHERE ($1::TEXT = '' OR p.search_text @@ plainto_tsquery('english', $1))`+postFrag+`
			 GROUP BY pt.tag`)
		queryArgs = append(queryArgs, postArgs...)
	}
	if len(branches) == 0 {
		// The selection names a dimension neither tagged entity has, so
		// nothing can carry a tag under it. Empty, not unfiltered.
		return nil, nil
	}
	// pgx rejects a statement bound with more args than it names, so an
	// UNSATISFIABLE branch is DROPPED along with its args rather than
	// rendered as a false tautology — the branch list and queryArgs are
	// appended in lockstep above for exactly that reason (ADR 0063).
	sql := `
		SELECT u.tag, SUM(u.n)::BIGINT
		  FROM (` + strings.Join(branches, "\n\t\t\tUNION ALL\n") + `) u
		 GROUP BY u.tag
		 ORDER BY 2 DESC
		 LIMIT 25
	`
	rows, err := pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]Bucket, 0, 25)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}
