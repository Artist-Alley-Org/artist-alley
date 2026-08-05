// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"strconv"

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
// question is a distinction an attacker will not respect. `tag` is
// unaffected — it aggregates through POSTS, under the post predicate.
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

// assetTypeAgg counts assets grouped by their asset_type ref +
// joined to asset_type.name for display. Visibility gate + q text
// match applied before aggregation (per B-2 decision 6).
type assetTypeAgg struct{}

func (assetTypeAgg) Type() FacetType { return FacetAssetType }

func (assetTypeAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	frag, args, err := buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps, 1)
	if err != nil {
		return nil, err
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
	frag, args, err := buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps, 1)
	if err != nil {
		return nil, err
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
	frag, args, err := buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps, 1)
	if err != nil {
		return nil, err
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
	frag, args, err := buildAssetVisibilityAppendedSQL(ctx, req.Caller, req.Caps, 1)
	if err != nil {
		return nil, err
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

// tagAgg counts post_tags grouped by tag string. Visibility is
// applied via posts (join through post_tags → posts) so restricted
// posts' tags never leak into counts.
//
// #873 — the count is only right if the predicate is the same one the
// feed runs. It was not: this composed `public OR author` while browse
// composed the full rule, so every tag that appears only on org-only or
// followers posts was counted as though those posts did not exist. Under-
// counting is the failure that looks like nothing at all.
type tagAgg struct{}

func (tagAgg) Type() FacetType { return FacetTag }

func (tagAgg) Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityPost, req.Caller,
		visibility.WithPostCaps(req.PostCaps))
	if err != nil {
		return nil, err
	}
	frag, args := pred.ToSQL("p", 1)

	sql := `
		SELECT pt.tag, COUNT(DISTINCT pt.post_id)::BIGINT
		  FROM post_tags pt
		  JOIN posts p ON p.id = pt.post_id
		 WHERE ($1::TEXT = '' OR p.search_text @@ plainto_tsquery('english', $1))` + frag + `
		 GROUP BY pt.tag
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

// _ prevents the strconv import from being culled when future
// aggregators need it — several already do implicitly.
var _ = strconv.Itoa
