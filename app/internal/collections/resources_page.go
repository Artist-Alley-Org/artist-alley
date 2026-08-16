// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ListCollectionResourcesPageGated is the collection-contents query,
// gated per member (#438, #883).
//
// Hand-built rather than sqlc because the readability decision needs
// sensitivity + owner + a team-membership EXISTS joined into the same
// pass, which the static sqlc query cannot express.
//
// Gating here is not optional. A PUBLIC collection may contain
// non-public assets, so gating only the parent collection would hand a
// caller the title, asset_type, status and file_hash of every asset
// pinned inside it that they are not entitled to. The parent gate and
// the member gate answer different questions and both are required.
//
// # Why the EntityAsset predicate is no longer spliced (#883)
//
// It used to be, and it FILTERED non-readable members out of the result
// entirely. That is the wrong shape: a member the caller cannot see must
// come back as a VISIBLE placeholder, not vanish, or the reader cannot
// tell that a restriction exists and #881's "request access" has nothing
// to attach to.
//
// Nothing is given up by dropping the splice, because
// visibility.FieldsReadable is the CONJUNCTION of that predicate's asset
// branch and the content plane — strictly tighter than the fragment it
// replaces (see its doc). The one conjunct it deliberately does not
// carry is soft-delete, which is back INLINE below: a deleted member is
// not a placeholder, it is gone.
//
// The SELECT list, ordering and (sort_order, added_at) cursor are
// otherwise carried over from the sqlc ListCollectionResourcesPage
// unchanged.
type ListCollectionResourcesPageGatedParams struct {
	CollectionID    pgtype.UUID
	CursorSortOrder *int32
	CursorAddedAt   pgtype.Timestamptz
	RowLimit        int32
	// Ladder is the operator's CONFIGURED preview variant keys (#591).
	// Empty means "unknown" and resolves to ladder_available = false.
	Ladder []string
	// MutationCaps is the caller's resolved `assets.admin` scope
	// (#939). Widens the FIELD plane only — never the picture, never
	// the bytes. The zero value denies, so omitting it fails closed.
	MutationCaps visibility.AssetMutationCaps
	// Mature is the caller's resolved mature-content axis (#1147).
	//
	// ⚠️ IT NARROWS ROWS, unlike every other field on this struct. This
	// query's whole shape is "list the member, redact what the caller may
	// not see", and the mature axis is the one rule that does not fit it:
	// a mature member is ABSENT, not placeheld.
	//
	// The argument is assets.ListAssetsPageGated's, verbatim, because it
	// is the same list of assets reached through a different door. A
	// restricted member stays listed because ADR 0064 requires browse to
	// show the corpus and #881's request-access flow hangs off the
	// placeholder. A mature member has no such flow — there is nothing to
	// request, only a preference to change — and #921 measured what the
	// placeholder alternative looks like: a grid of blurred plates nobody
	// asked to be offered.
	//
	// The zero value is the DISQUALIFIED viewer, so a caller that forgets
	// this field shows too little rather than leaking.
	Mature visibility.MatureViewer
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
	// same pass (#640). Nil unless BOTH are present — and always nil on a
	// Restricted row, along with every other asset column.
	PixelWidth  *int32
	PixelHeight *int32
	// Restricted is true when the caller fails
	// visibility.FieldsReadable for this member (#883). The handler
	// serialises such a row as a placeholder: no asset column at all,
	// only the collection_resources columns plus OwnerDisplayName.
	Restricted bool
	// OwnerDisplayName is the asset owner's display name per
	// visibility.OwnerDisplayNameSQL, the ONE asset-derived value a
	// placeholder is permitted to carry. Empty when the asset is
	// unowned, when the owner has no resolvable name, and when the owner
	// opted out of anonymous exposure and this caller is anonymous
	// (#1023) — the handler then omits the field rather than sending "",
	// so those three cases are one answer on the wire.
	OwnerDisplayName string
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

	// #1147 — the mature axis, on the ROW plane. See the field's doc for
	// why this one narrows rows in a query built to redact them instead.
	//
	// It reuses $5, the caller ref already bound above for the
	// team-membership probe, so it adds NO placeholder. That is not a
	// tidiness point: MatureFilterSQL folds to the empty string for a
	// qualified viewer and for an admin, and a NEW placeholder referenced
	// only by a fragment that sometimes vanishes is the 42P18 ("could not
	// determine data type of parameter") that bit posts/list_page.go —
	// an error on every request by exactly the readers who qualify.
	// assets/list_page.go reuses its $8 for the same reason.
	matureFrag := visibility.MatureFilterSQL("a", visibility.MatureOwnerColAsset,
		"$5", p.Mature, visibility.ResolveContentCaps(caps).SystemAdmin)

	// Derived columns join preview_available's inputs in the same pass —
	// no per-asset round-trips (#471). Readability is decided in-Go per
	// row (visibility.FieldsReadable) from a.sensitivity + a.status +
	// a.processing_status + a.owner + membership + caps.
	//
	// The owner's display name is resolved in this pass too. It is the
	// one asset-derived value a placeholder carries, so fetching it
	// per-restricted-row afterwards would be an N+1 on exactly the path
	// that needs it most.
	//
	// #1023 — it comes from visibility.OwnerDisplayNameSQL rather than
	// from LEFT JOINs written here. What used to sit here was
	// `COALESCE(NULLIF(up.display_name,''), u.username, '')` — the same
	// text as posts' preview enrich, and with the same defect: it never
	// consulted `hide_from_anonymous`, so a public collection holding a
	// restricted asset disclosed the username of an owner who had taken
	// ADR 0024's opt-out to an anonymous caller.
	sql := `SELECT cr.collection_id, cr.asset_id, cr.sort_order, cr.pinned,
       cr.expires_at, cr.added_at,
       a.title, a.asset_type, a.status, a.file_hash,
       a.file_extension, a.thumbhash,
       a.created_at AS asset_created_at,
       a.sensitivity, a.processing_status, a.owner_user_ref,
       ` + visibility.OwnerDisplayNameSQL("a.owner_user_ref", caller.IsAnonymous) + ` AS owner_display_name,
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
             WHERE tm.team_id = a.team_id AND tm.user_ref = $5::BIGINT)) AS caller_is_team_member,
       a.team_id
FROM collection_resources cr
JOIN assets a ON a.id = cr.asset_id
WHERE cr.collection_id = $1
  AND cr.pinned = TRUE
  AND a.deleted_at IS NULL` + matureFrag + `
  AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
  AND ($2::INTEGER IS NULL
       OR cr.sort_order > $2::INTEGER
       OR (cr.sort_order = $2::INTEGER AND cr.added_at > $3::TIMESTAMPTZ))
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
			processingStatus   string
			ownerUserRef       *int64
			ownerDisplayName   string
			pixelWidth         *int32
			pixelHeight        *int32
			hasColVariant      bool
			hasFullLadder      bool
			hasScrubVariant    bool
			callerIsTeamMember bool
			assetTeamID        *uuid.UUID
		)
		if err := rows.Scan(
			&i.CollectionID, &i.AssetID, &i.SortOrder, &i.Pinned,
			&i.ExpiresAt, &i.AddedAt,
			&i.Title, &i.AssetType, &i.Status, &i.FileHash,
			&i.FileExtension, &i.Thumbhash,
			&i.AssetCreatedAt,
			&sensitivity, &processingStatus, &ownerUserRef, &ownerDisplayName,
			&pixelWidth, &pixelHeight,
			&hasColVariant, &hasFullLadder, &hasScrubVariant, &callerIsTeamMember,
			&assetTeamID,
		); err != nil {
			return nil, fmt.Errorf("collections: list resources scan: %w", err)
		}
		fr := visibility.FieldsRow{
			Sensitivity:      sensitivity,
			Status:           i.Status,
			ProcessingStatus: processingStatus,
			OwnerUserRef:     ownerUserRef,
			IsTeamMember:     callerIsTeamMember,
			TeamID:           assetTeamID,
		}
		fr.ApplyMutationCaps(p.MutationCaps)
		readable := visibility.FieldsReadable(fr, caller, caps)
		// #939 — the FIELD plane admits a scoped `assets.admin` holder;
		// the PICTURE plane does not. ADR 0064 keeps the thumbhash and
		// the availability flags on the binary side, so a holder gets a
		// richer placeholder rather than a rendered tile.
		picture := visibility.PreviewReadable(fr, caller, caps)
		if !picture {
			i.Thumbhash = nil
		}
		if !readable {
			// Placeholder: keep the collection_resources columns, drop
			// EVERY asset column. Constructed from a ZERO row rather than
			// by clearing fields on `i`, so a column added to
			// ListCollectionResourcesPageRow later is absent by default
			// instead of leaking until someone remembers to clear it.
			out = append(out, ListCollectionResourcesPageGatedRow{
				ListCollectionResourcesPageRow: ListCollectionResourcesPageRow{
					CollectionID: i.CollectionID,
					AssetID:      i.AssetID,
					SortOrder:    i.SortOrder,
					Pinned:       i.Pinned,
					ExpiresAt:    i.ExpiresAt,
					AddedAt:      i.AddedAt,
				},
				Restricted:       true,
				OwnerDisplayName: ownerDisplayName,
			})
			continue
		}
		row := ListCollectionResourcesPageGatedRow{
			ListCollectionResourcesPageRow: i,
			// AND with `picture`, not `readable` (#939): these three are
			// a promise the binary handlers must keep, and they still
			// refuse a mutation holder.
			PreviewAvailable: hasColVariant && picture,
			LadderAvailable:  hasFullLadder && picture,
			ScrubAvailable:   hasScrubVariant && picture,
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
