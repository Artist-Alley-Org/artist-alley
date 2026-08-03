// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
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
	// Ladder is the operator's CONFIGURED preview variant keys (#591).
	// Empty means "unknown" and resolves to ladder_available = false.
	Ladder []string
}

// ListCollectionResourcesPageGatedRow is a contents row plus the derived
// preview_available flag (#471).
type ListCollectionResourcesPageGatedRow struct {
	ListCollectionResourcesPageRow
	PreviewAvailable bool
	// LadderAvailable: every CONFIGURED rung exists AND the caller
	// passes the content plane (#591). Same 0064 contract as
	// PreviewAvailable, from the same readability decision.
	LadderAvailable bool
	// ScrubAvailable: a `sprites.vtt` hover-scrub cue file exists AND
	// the caller passes the content plane (#835). A collection member
	// renders through the same CardThumb as every other surface.
	ScrubAvailable bool
	// PixelWidth / PixelHeight: recorded source dimensions, joined in the
	// same pass (#640). Not readability-gated — metadata about a row the
	// caller can already see — and nil unless BOTH are present.
	PixelWidth  *int32
	PixelHeight *int32
}

// ListCollectionResourcesPageGated runs the contents query for one caller.
// `caps` (nil for anonymous) only short-circuits preview_available for
// SystemAdmin / content.read.all; it never affects which rows return.
func ListCollectionResourcesPageGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	caps visibility.CapabilityChecker,
	p ListCollectionResourcesPageGatedParams,
) ([]ListCollectionResourcesPageGatedRow, error) {
	args := []any{
		p.CollectionID,    // $1
		p.CursorSortOrder, // $2
		p.CursorAddedAt,   // $3
		p.RowLimit,        // $4
		caller.UserRef,    // $5 — team-membership probe; anon = ref 0, no match
		p.Ladder,          // $6 — configured preview ladder (#591)
	}

	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, fmt.Errorf("collections: visibility filter: %w", err)
	}
	visFrag, visArgs := pred.ToSQL("a", len(args))
	args = append(args, visArgs...) // predicate args LAST

	// Derived columns join preview_available's inputs in the same pass —
	// no per-asset round-trips (#471). Readability is decided in-Go per
	// row (visibility.ContentReadable) from a.sensitivity + a.owner +
	// membership + caps.
	sql := `SELECT cr.collection_id, cr.asset_id, cr.sort_order, cr.pinned,
       cr.expires_at, cr.added_at,
       a.title, a.asset_type, a.status, a.file_hash,
       a.file_extension, a.thumbhash,
       a.created_at AS asset_created_at,
       a.sensitivity, a.owner_user_ref,
       ` + pixeldims.SelectColumnsSQL("a.id") + `,
       (a.file_hash IS NOT NULL AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'col')) AS has_col_variant,
       ` + sysconfig.LadderSatisfiedSQL("a.file_hash", "$6") + ` AS has_full_ladder,
       (a.file_hash IS NOT NULL AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'sprites.vtt')) AS has_scrub_variant,
       (a.team_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM team_memberships tm
             WHERE tm.team_id = a.team_id AND tm.user_ref = $5::BIGINT)) AS caller_is_team_member
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

	var out []ListCollectionResourcesPageGatedRow
	for rows.Next() {
		var i ListCollectionResourcesPageRow
		var (
			sensitivity        string
			ownerUserRef       *int64
			pixelWidth         *int32
			pixelHeight        *int32
			hasColVariant      bool
			hasFullLadder      bool
			hasScrubVariant    bool
			callerIsTeamMember bool
		)
		if err := rows.Scan(
			&i.CollectionID, &i.AssetID, &i.SortOrder, &i.Pinned,
			&i.ExpiresAt, &i.AddedAt,
			&i.Title, &i.AssetType, &i.Status, &i.FileHash,
			&i.FileExtension, &i.Thumbhash,
			&i.AssetCreatedAt,
			&sensitivity, &ownerUserRef, &pixelWidth, &pixelHeight,
			&hasColVariant, &hasFullLadder, &hasScrubVariant, &callerIsTeamMember,
		); err != nil {
			return nil, fmt.Errorf("collections: list resources scan: %w", err)
		}
		readable := visibility.ContentReadable(sensitivity, ownerUserRef, caller, caps, callerIsTeamMember)
		row := ListCollectionResourcesPageGatedRow{
			ListCollectionResourcesPageRow: i,
			PreviewAvailable:               hasColVariant && readable,
			LadderAvailable:                hasFullLadder && readable,
			ScrubAvailable:                 hasScrubVariant && readable,
		}
		if pixeldims.Sane(pixelWidth, pixelHeight) {
			row.PixelWidth, row.PixelHeight = pixelWidth, pixelHeight
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collections: list resources rows: %w", err)
	}
	return out, nil
}
