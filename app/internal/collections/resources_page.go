// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ListCollectionResourcesPageGated is the collection-contents query with
// the asset visibility predicate applied (#438).
//
// Hand-built rather than sqlc for the same reason as
// assets.ListAssetsPageGated (#429): Predicate.ToSQL returns a runtime
// fragment and sqlc queries are static strings with fixed placeholders,
// so a fragment can only be spliced into SQL assembled at runtime.
//
// Row filtering here is not optional. A PUBLIC collection may contain
// non-public assets, so gating only the parent collection would still
// hand an anonymous caller the title, asset_type, status and file_hash
// of every draft asset pinned inside it. The parent gate and the row
// gate answer different questions and both are required.
//
// The SELECT list, ordering and (sort_order, added_at) cursor are
// carried over from the sqlc ListCollectionResourcesPage unchanged. The
// inline `a.deleted_at IS NULL` is deliberately GONE: the predicate
// asserts soft-delete itself, and leaving the inline clause would be a
// second expression of one rule on one path — the defect ADR 0063
// exists to prevent. #429 set that precedent.
//
// Placeholder discipline: every placeholder this builder emits is
// <= argOffset, the predicate's fragment owns everything above it, and
// predicate args are appended LAST. LIMIT binds before the fragment
// renders even though it reads later in the statement — the invariant
// is an index bound, not textual order.
type ListCollectionResourcesPageGatedParams struct {
	CollectionID    pgtype.UUID
	CursorSortOrder *int32
	CursorAddedAt   pgtype.Timestamptz
	RowLimit        int32
}

// ListCollectionResourcesPageGated runs the contents query for one caller.
func ListCollectionResourcesPageGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	p ListCollectionResourcesPageGatedParams,
) ([]ListCollectionResourcesPageRow, error) {
	args := []any{
		p.CollectionID,    // $1
		p.CursorSortOrder, // $2
		p.CursorAddedAt,   // $3
		p.RowLimit,        // $4
	}

	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, fmt.Errorf("collections: visibility filter: %w", err)
	}
	visFrag, visArgs := pred.ToSQL("a", len(args))
	args = append(args, visArgs...) // predicate args LAST

	sql := `SELECT cr.collection_id, cr.asset_id, cr.sort_order, cr.pinned,
       cr.expires_at, cr.added_at,
       a.title, a.asset_type, a.status, a.file_hash, a.created_at AS asset_created_at
FROM collection_resources cr
JOIN assets a ON a.id = cr.asset_id
WHERE cr.collection_id = $1
  AND cr.pinned = TRUE
  AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
  AND ($2::INTEGER IS NULL
       OR cr.sort_order > $2::INTEGER
       OR (cr.sort_order = $2::INTEGER AND cr.added_at > $3::TIMESTAMPTZ))` +
		visFrag + `
ORDER BY cr.sort_order ASC, cr.added_at ASC
LIMIT $4::INTEGER`

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("collections: list resources: %w", err)
	}
	defer rows.Close()

	var out []ListCollectionResourcesPageRow
	for rows.Next() {
		var i ListCollectionResourcesPageRow
		if err := rows.Scan(
			&i.CollectionID, &i.AssetID, &i.SortOrder, &i.Pinned,
			&i.ExpiresAt, &i.AddedAt,
			&i.Title, &i.AssetType, &i.Status, &i.FileHash, &i.AssetCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("collections: list resources scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collections: list resources rows: %w", err)
	}
	return out, nil
}
