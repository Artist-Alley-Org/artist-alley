// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package featured

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// RailRow is one rendered placement.
type RailRow struct {
	ID          pgtype.UUID
	SubjectKind string
	SubjectID   pgtype.UUID
	Position    int32
	Title       string
	// CoverAssetID is the asset whose col variant renders the tile: the
	// subject itself for an asset, ADR 0027's hero-card fallback for a
	// collection. Invalid (NULL) when nothing is servable to the caller.
	// Needed separately because SubjectID is the collection, while the
	// variant endpoint is keyed by asset (#559).
	CoverAssetID          pgtype.UUID
	AssetFileHash         *string
	AssetPreviewAvailable bool
	// AssetLadderAvailable: every CONFIGURED preview rung exists for the
	// cover asset AND it clears the same public-tier gate as
	// AssetPreviewAvailable (#591). Computed against sysconfig's ladder,
	// never a hardcoded rung list.
	AssetLadderAvailable bool
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
// THREE fragments: one per subject kind (the subject is polymorphic),
// plus the asset predicate a second time for the collection cover
// lateral, which renders an asset and so must clear the asset bar.
//
// Collection covers (#559, ADR 0027): a collection subject used to
// contribute only its name, because every image hint came from the
// asset join, which cannot match a collection row. The result was a
// row of blank tiles at the top of the landing page. The hero-card
// fallback below fills them from the collection's own content.
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
	ladder []string,
) ([]RailRow, error) {
	// $2 must be bound BEFORE the predicate fragments splice, so their
	// placeholders start above it (the ADR 0063 discipline this file
	// already follows for $1).
	args := []any{limit, ladder} // $1, $2

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

	// Third splice of the SAME asset predicate, aliased for the cover
	// lateral (#559). A collection cover is an ASSET being rendered, so
	// it must clear the identical bar the asset-subject hint does —
	// reusing the predicate rather than restating the rule is the whole
	// reason featuring can't widen access.
	coverFrag, coverArgs := assetPred.ToSQL("ca", len(args))
	args = append(args, coverArgs...)

	// The predicate fragments arrive as " AND (...)", which is exactly
	// what an ON clause wants appended.
	sql := `SELECT f.id, f.subject_kind, f.subject_id, f.position,
       COALESCE(a.title, c.name, '')::text AS title,
       -- Kept in lockstep with asset_file_hash: an id without a servable
       -- hash would have the client build a URL that 404s.
       CASE f.subject_kind
            WHEN 'asset'      THEN CASE WHEN a.sensitivity = 'public' THEN a.id END
            WHEN 'collection' THEN cover.id
       END AS cover_asset_id,
       CASE f.subject_kind
            WHEN 'asset'      THEN CASE WHEN a.sensitivity = 'public' THEN a.file_hash END
            WHEN 'collection' THEN cover.file_hash
       END AS asset_file_hash,
       -- preview_available (#471): public-tier AND a servable col exists.
       -- Gated on public exactly like the file_hash hint above, so it is
       -- suppressed for non-public assets (0064-safe) and the rail fires
       -- no /variants/col request for them. For a collection the cover
       -- lateral already REQUIRES a servable col, so its presence is the
       -- answer.
       COALESCE(CASE f.subject_kind
            WHEN 'asset' THEN a.sensitivity = 'public' AND EXISTS (
                 SELECT 1 FROM storage_variants sv
                  WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'col')
            WHEN 'collection' THEN cover.file_hash IS NOT NULL
       END, false)::boolean AS asset_preview_available,
       -- ladder_available (#591): the same public-tier gate, but asking
       -- whether EVERY configured rung exists rather than just col. The
       -- rung list arrives as $2 from sysconfig — hardcoding the four
       -- defaults here would make the flag lie on any install that
       -- tuned its ladder.
       COALESCE(CASE f.subject_kind
            WHEN 'asset'      THEN a.sensitivity = 'public' AND ` + sysconfig.LadderSatisfiedSQL("a.file_hash", "$2") + `
            WHEN 'collection' THEN ` + sysconfig.LadderSatisfiedSQL("cover.file_hash", "$2") + `
       END, false)::boolean AS asset_ladder_available
FROM featured_items f
LEFT JOIN assets a
       ON f.subject_kind = 'asset' AND a.id = f.subject_id` + assetFrag + `
LEFT JOIN collections c
       ON f.subject_kind = 'collection' AND c.id = f.subject_id` + collFrag + `
-- Hero-card fallback (ADR 0027): "falls back to the most-recent post in
-- the collection". hero_asset_id — the explicit curator override — is
-- not in the schema yet, so the fallback is the whole rule for now.
--
-- "Most-recent ELIGIBLE post cover" rather than "most-recent post, then
-- check": picking the newest post first and rejecting it would blank the
-- whole tile whenever the latest post happens to be team-tier, which is
-- the common case in a studio dataset. Restricting the ordering to what
-- the caller may already see keeps the ADR's intent (newest wins) without
-- letting one gated post hide an otherwise-showable collection.
--
-- Three gates, all mandatory:
--   * the caller's own asset predicate (spliced above) — visibility
--   * sensitivity = 'public' — per-asset tier (ADR 0020/0064), same bar
--     as the asset-subject hint, so an embargo cover yields no pixels
--   * a servable col variant — so the tile never fires a 404
-- The removed third gate was has_image, a column with no writer that
-- would have made every cover blank had it been required. It is gone
-- from the schema entirely as of #579; this note stays as the record of
-- why the gate list is three and not four.
LEFT JOIN LATERAL (
       SELECT ca.id, ca.file_hash
         FROM collection_posts cp
         JOIN posts p   ON p.id = cp.post_id
         JOIN assets ca ON ca.id = p.cover_asset_id` + coverFrag + `
        WHERE cp.collection_id = c.id
          AND ca.sensitivity = 'public'
          AND EXISTS (SELECT 1 FROM storage_variants sv
                       WHERE sv.object_hash = ca.file_hash AND sv.variant_key = 'col')
        ORDER BY p.created_at DESC, p.id DESC
        LIMIT 1
) cover ON true
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
			&r.Title, &r.CoverAssetID,
			&r.AssetFileHash, &r.AssetPreviewAvailable,
			&r.AssetLadderAvailable,
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
