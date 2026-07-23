// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package featured

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// RailRow is one rendered placement.
type RailRow struct {
	ID                    pgtype.UUID
	SubjectKind           string
	SubjectID             pgtype.UUID
	Position              int32
	Title                 string
	AssetFileHash         *string
	AssetHasImage         bool
	AssetPreviewAvailable bool
}

// ListPublicRail returns the public featured rail for one caller
// (#417, ADR 0065).
//
// FEATURING NEVER WIDENS ACCESS. That is the whole invariant, and it is
// enforced structurally here rather than by a second copy of the
// visibility rule:
//
//   - Each subject table is joined with the ADR 0063 predicate for that
//     entity IN THE JOIN CONDITION, not in WHERE. A subject the caller
//     may not see therefore fails to join and comes back NULL.
//   - The placement row is then dropped unless it resolved to a
//     visible subject.
//
// The naive version of this query — LEFT JOIN both tables, filter only
// on scope — is the one that gets it wrong: it returns the placement
// with a NULL title, which both leaks that a placement exists and
// renders an empty tile. Curation is a SELECTION over what is already
// visible; a placement pointing at content the caller cannot see must
// produce nothing at all.
//
// Hand-built rather than sqlc for the usual reason (ADR 0063):
// Predicate.ToSQL returns a runtime fragment and sqlc queries are
// static strings with fixed placeholders. This is the fourth splice
// site of this shape after #429, #438 and #449 — and note it splices
// the predicate TWICE, once per subject kind, because the subject is
// polymorphic.
//
// Per-asset sensitivity still applies (ADR 0020 via #40): the
// thumbnail hint is suppressed for any asset that is not public-tier,
// so an embargo asset surfaces its title and no pixels. Suppressed in
// SQL rather than in the mapper so the hash never leaves the database
// layer for a gated asset.
//
// Placeholder discipline: the scope argument binds first, then the
// asset predicate's args, then the collection predicate's. Each
// ToSQL call is given the running length, so the two fragments never
// collide.
func ListPublicRail(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	limit int32,
) ([]RailRow, error) {
	args := []any{limit} // $1

	assetPred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, fmt.Errorf("featured: asset visibility filter: %w", err)
	}
	assetFrag, assetArgs := assetPred.ToSQL("a", len(args))
	args = append(args, assetArgs...)

	collPred, err := visibility.Filter(ctx, visibility.EntityCollection, caller)
	if err != nil {
		return nil, fmt.Errorf("featured: collection visibility filter: %w", err)
	}
	collFrag, collArgs := collPred.ToSQL("c", len(args))
	args = append(args, collArgs...)

	// The predicate fragments arrive as " AND (...)", which is exactly
	// what an ON clause wants appended.
	sql := `SELECT f.id, f.subject_kind, f.subject_id, f.position,
       COALESCE(a.title, c.name, '')::text AS title,
       CASE WHEN a.sensitivity = 'public' THEN a.file_hash ELSE NULL END AS asset_file_hash,
       COALESCE(a.sensitivity = 'public' AND a.has_image, false)::boolean AS asset_has_image,
       -- preview_available (#471): public-tier AND a servable col exists.
       -- Gated on public exactly like the file_hash hint above, so it is
       -- suppressed for non-public assets (0064-safe) and the rail fires
       -- no /variants/col request for them.
       COALESCE(a.sensitivity = 'public' AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'col'), false)::boolean AS asset_preview_available
FROM featured_items f
LEFT JOIN assets a
       ON f.subject_kind = 'asset' AND a.id = f.subject_id` + assetFrag + `
LEFT JOIN collections c
       ON f.subject_kind = 'collection' AND c.id = f.subject_id` + collFrag + `
WHERE f.scope = 'public'
  AND (a.id IS NOT NULL OR c.id IS NOT NULL)
ORDER BY f.position ASC, f.created_at ASC
LIMIT $1::INTEGER`

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("featured: list public rail: %w", err)
	}
	defer rows.Close()

	var out []RailRow
	for rows.Next() {
		var r RailRow
		if err := rows.Scan(
			&r.ID, &r.SubjectKind, &r.SubjectID, &r.Position,
			&r.Title, &r.AssetFileHash, &r.AssetHasImage, &r.AssetPreviewAvailable,
		); err != nil {
			return nil, fmt.Errorf("featured: rail scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("featured: rail rows: %w", err)
	}
	return out, nil
}
