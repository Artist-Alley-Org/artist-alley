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
	// Subtitle is the wide card's second line (#1110) — a COLLECTION
	// subject's own description, "" when it has none and "" for every
	// other subject kind. See the SQL for why it is read from the same
	// join as Title and why assets deliberately do not contribute one.
	Subtitle string
	// ItemCount is the subtitle's fallback: how many members of a
	// collection subject THIS caller can see. nil for any other subject
	// kind, which is not the same answer as 0 — an asset has no
	// membership, an empty collection has one and it is empty.
	ItemCount *int32
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

// ListPublicRail returns the featured rail for one caller (#417,
// ADR 0065). "Public" names the ENDPOINT — GET /featured is the
// unauthenticated route — not the audience: since #1104 the audience is
// chosen per viewer by ScopeVisibleSQL, so an anonymous request sees
// exactly the `public` placements it always saw and a signed-in one
// also sees `org`.
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
//
// # The subtitle and its count (#1110)
//
// The wide card prints a second line under the name. Its source is the
// collection's own `description`, and its fallback is how many members
// the caller can see. Both are computed HERE rather than by a second
// request, and both are subject to one rule stated twice below because
// they arrive by different routes:
//
//   - The DESCRIPTION rides the `c` join. That join already carries the
//     collection predicate in its ON clause, so a collection this
//     caller cannot see contributes no name AND no description, and the
//     row is dropped by the same WHERE that has always dropped it. The
//     subtitle is therefore withheld exactly when the title is, by
//     construction rather than by a matching pair of conditions that a
//     later edit could unmatch.
//
//   - The COUNT is a fresh traversal into two membership tables, so it
//     gets no such protection for free and each half is gated
//     explicitly. Counting the whole membership would publish the size
//     of the part this caller is not allowed to look at — a public
//     collection of 400 posts of which a stranger may open 3 would
//     announce "400 items". That is the derived-copy defect class:
//     #902 (search text), #1066 (embeddings), and Elastic documents the
//     same leak as a known limit of document-level security. The
//     membership counted is the one ComposeCovers draws its mosaic from
//     — pinned, unexpired, posts + resources — so the tile's picture
//     and the tile's number describe the same set.
//
// postCaps is the caller's resolved post capabilities, needed by the
// post half of that count and by nothing else in this query. Omitting
// it yields the NARROWER answer (see visibility.WithPostCaps), so the
// failure direction is a moderator's count reading low, never a
// stranger's reading high.
func ListPublicRail(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	postCaps visibility.PostCaps,
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

	// Fourth and fifth splices, for the two halves of the member count
	// (#1110). The resource half is the SAME asset predicate a third
	// time, aliased for the count's own join; the post half is the post
	// read rule, which this query did not need before — a collection's
	// cover comes from a post but the cover lateral gates the ASSET, and
	// counting members means asking whether the caller may see each
	// MEMBER.
	countAssetFrag, countAssetArgs := assetPred.ToSQL("ia", len(args))
	args = append(args, countAssetArgs...)

	postPred, err := visibility.Filter(ctx, visibility.EntityPost, caller,
		visibility.WithPostCaps(postCaps))
	if err != nil {
		return nil, fmt.Errorf("featured: post visibility filter: %w", err)
	}
	// Soft-delete NOT waived: a deleted post is not a member. Same shape
	// collections.ComposeCovers splices for the same membership.
	countPostFrag, countPostArgs := postPred.ToSQL("ip", len(args))
	args = append(args, countPostArgs...)

	// The predicate fragments arrive as " AND (...)", which is exactly
	// what an ON clause wants appended.
	sql := `SELECT f.id, f.subject_kind, f.subject_id, f.position,
       COALESCE(a.title, c.name, '')::text AS title,
       -- The subtitle, from the SAME join as the name above (#1110).
       -- Read the header note before adding an ` + "`a.description`" + ` arm here:
       -- an embargo asset is title-only on this surface by ADR 0020, and
       -- its own join is the one that surfaces it, so an asset arm would
       -- print the withheld half of a tile whose pixels are suppressed
       -- three lines down.
       COALESCE(c.description, '')::text AS subtitle,
       -- The subtitle's fallback: members THIS caller can see. NULL —
       -- not 0 — for an asset subject, because "no membership" and "an
       -- empty membership" are different tiles.
       CASE WHEN c.id IS NOT NULL THEN (
              (SELECT count(*)
                 FROM collection_posts icp
                 JOIN posts ip ON ip.id = icp.post_id` + countPostFrag + `
                WHERE icp.collection_id = c.id
                  AND icp.pinned = TRUE
                  AND (icp.expires_at IS NULL OR icp.expires_at > NOW()))
            + (SELECT count(*)
                 FROM collection_resources icr
                 JOIN assets ia ON ia.id = icr.asset_id` + countAssetFrag + `
                WHERE icr.collection_id = c.id
                  AND icr.pinned = TRUE
                  AND (icr.expires_at IS NULL OR icr.expires_at > NOW()))
       ) END::INTEGER AS item_count,
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
WHERE ` + ScopeVisibleSQL("f", caller) + `
  -- A 'team' placement (#1084) resolves neither join and is therefore
  -- dropped here, which is the correct outcome and is stated so nobody
  -- reads it as an oversight: a team tile belongs to the signed-in
  -- teams rail (GET /featured/teams). Adding team subjects to this
  -- query would put studio names on a surface that has never shown them
  -- to logged-out readers — a visibility decision, not a rendering one,
  -- and not one this change makes. ScopeVisibleSQL never admits the
  -- 'team' scope either, so this holds for both arms.
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
			&r.Title, &r.Subtitle, &r.ItemCount, &r.CoverAssetID,
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
