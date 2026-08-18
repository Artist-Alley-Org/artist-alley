// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package featured

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
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
	CoverAssetID pgtype.UUID
	// CoverFocalX/Y position the tile's 890:500 crop, as fractions of
	// the cover picture (#1207). nil means centre — the CSS default, and
	// what every tile did before the columns existed.
	//
	// SET ONLY WHEN A CHOSEN COVER WON. The derived cover is a picture
	// the curator never saw in the editor, so positioning they chose for
	// a different picture is not carried onto it; see the cover lateral.
	CoverFocalX *float64
	CoverFocalY *float64
	// CoverZoom tightens the tile's crop, as a multiplier on the fitting
	// rectangle (#1212). nil means fit — what every tile did before the
	// column existed. Rides the same rungs as the focal point, and for
	// the same reason: it is a framing chosen against a specific
	// picture.
	CoverZoom             *float64
	AssetFileHash         *string
	AssetPreviewAvailable bool
	// AssetLadderAvailable: every CONFIGURED preview rung exists for the
	// cover asset AND it clears the same public-tier gate as
	// AssetPreviewAvailable (#591). Computed against sysconfig's ladder,
	// never a hardcoded rung list.
	AssetLadderAvailable bool
	// OwnerUserRef is the subject's owner: the asset's for an asset
	// subject, the collection's for a collection. Nil when the subject
	// did not resolve to one (an asset may have no owner).
	//
	// It exists for the promo band's author chip (#1118) and is
	// deliberately a REF rather than a name: resolving a display name is
	// users.ResolveDisplayName's job and its ladder has an anonymous arm
	// (ADR 0024's opt-out). Transcribing it here would be the fifth copy
	// of the rule #1023 was filed for. The band handler hands these refs
	// to users.LookupAuthors — the one home — and the rail asks for
	// nothing, so nothing here decides what a name is.
	OwnerUserRef *int64
}

// PlacementQuery is the input to [ListPlacements] — everything the one
// placement read needs to answer for one caller on one surface.
//
// A struct rather than nine positional parameters because two of the
// nine (Mature, MatureAdmin) arrived after the rest and a positional
// list of that length is how a caller silently passes the wrong bool.
type PlacementQuery struct {
	Caller   visibility.Caller
	PostCaps visibility.PostCaps
	// Mature is the caller's resolved mature-content axis (ADR 0090),
	// read once at the HTTP edge from visibility.MatureFromContext. Its
	// zero value is the DISQUALIFIED viewer, which is the safe default
	// for a caller that forgets to set it.
	Mature visibility.MatureViewer
	// MatureAdmin is the system.admin exemption from the mature rule —
	// separate from the axis itself because ADR 0090 checks it BEFORE
	// qualification, so it survives the instance switch being off.
	MatureAdmin bool
	Limit       int32
	Ladder      []string
	// BandID selects the SURFACE (#1118):
	//
	//   nil        the featured rail — every placement written before
	//              bands existed, and the only surface GET /featured
	//              serves.
	//   a band id  that promo band's cards, in curation order.
	//
	// ⚠️ The two surfaces gate their AUDIENCE differently, and the
	// difference is not a second rule — it is the same rule applied to
	// whichever row carries the audience. A rail placement carries its
	// own `scope`; a band card does not, because the BAND carries one
	// for the whole strip (migration 00053). So the rail arm splices
	// ScopeVisibleSQL over `f` and the band arm does not splice it at
	// all: [GetRenderableBand] has already applied it to the band, and
	// applying it again over a column that is not the audience would be
	// reading a stale copy.
	BandID *uuid.UUID
}

// ListPlacements returns the resolved placements for one surface and one
// caller (#417, #1118, ADR 0065).
//
// ONE QUERY SERVES THE RAIL AND EVERY BAND, and that is the point rather
// than an economy. What follows this comment is five splices of the
// ADR 0063 predicate, a per-asset sensitivity gate, the ADR 0090 mature
// conjunct, a cover lateral and a member count — and a band that
// resolved its cards through a second copy of all that would be a second
// chance to get one of them wrong. It is the same argument migration
// 00010 makes about two sources of truth for "what is featured", one
// layer up.
//
// The former entry point was ListPublicRail. "Public" named the
// ENDPOINT — GET /featured is the
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
// SEVEN fragments: one per subject kind (the subject is polymorphic),
// plus the asset predicate five more times — once per cover rung in
// the preference lateral (#1207) and once per half of the member count
// — because each of those renders or counts an asset and so must clear
// the asset bar.
//
// Collection covers (#559, ADR 0027): a collection subject used to
// contribute only its name, because every image hint came from the
// asset join, which cannot match a collection row. The result was a
// row of blank tiles at the top of the landing page. The cover lateral
// below fills them — since #1207 from the curator's choice when there
// is one, and from the collection's own content when there is not.
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
//
// # The mature axis (#1118, ADR 0090)
//
// ⚠️ THIS QUERY DID NOT COMPOSE IT UNTIL #1118, and the omission was
// live: an operator who featured a mature asset published it to every
// reader, including one who had never opted in and one on an install
// whose operator had switched mature content off. `visibility.Filter`
// answers "who is ALLOWED" and says nothing about "who has OPTED IN" —
// the two axes are independent by ADR 0090 §1 — so composing the tier
// predicate was never enough, and the rail is not exempt from a rule
// every other list surface applies.
//
// It is spliced on the ROW plane (the placement is dropped) rather than
// the picture plane (the cover is blurred), matching what #921 chose for
// the feed. A curated strip whose tiles blur out is an operator's
// curation rendered as a wall of frosted glass; a curated strip that is
// simply shorter is the same strip.
//
// SIX SPLICES, one per table this query renders or counts from:
//
//	a   the asset subject          — the tile itself
//	fa  the featured cover  (#1207)┐ a mature cover is a mature picture,
//	ra  the regular cover   (#1207)│ and `collections` carries no mature
//	ca  the derived cover          ┘ column of its own, so the cover is
//	                                 where the rule can act — on EVERY
//	                                 rung of the preference order, since
//	                                 choosing a picture is not a licence
//	                                 to publish it to a reader who never
//	                                 opted in
//	ia  the counted resources      — so the number matches the set
//	ip  the counted posts          — likewise, with the POST owner column
//
// All six take the same viewer, so they are all empty or all present
// together, which is why one bound argument serves them and why it is
// bound conditionally (see the note at the binding site).
func ListPlacements(
	ctx context.Context,
	pool *pgxpool.Pool,
	q PlacementQuery,
) ([]RailRow, error) {
	caller := q.Caller
	// $2 must be bound BEFORE the predicate fragments splice, so their
	// placeholders start above it (the ADR 0063 discipline this file
	// already follows for $1).
	args := []any{q.Limit, q.Ladder} // $1, $2

	// The mature conjunct's owner placeholder. ⚠️ BOUND ONLY WHEN THE
	// FRAGMENT REFERENCES IT — MatureFilterSQL returns the empty string
	// for a qualified viewer and for an admin, and a parameter no
	// statement mentions is 42P18 ("could not determine data type of
	// parameter"), on every request by a QUALIFIED reader. That is the
	// exact bug posts/list_page.go records at its own splice site; the
	// note is repeated rather than referenced because the failure only
	// shows on the arm a single-viewer test does not exercise.
	//
	// 0 is the anonymous sentinel and it is load-bearing: MatureFilterSQL
	// wraps this in NULLIF(…, 0) so an anonymous caller cannot match a
	// row whose owner column holds 0 as ITS OWNER.
	matureOwner := int64(0)
	if !caller.IsAnonymous {
		matureOwner = caller.UserRef
	}
	matureArg := "$" + strconv.Itoa(len(args)+1)
	matureAsset := visibility.MatureFilterSQL(
		"a", visibility.MatureOwnerColAsset, matureArg, q.Mature, q.MatureAdmin)
	matureCover := visibility.MatureFilterSQL(
		"ca", visibility.MatureOwnerColAsset, matureArg, q.Mature, q.MatureAdmin)
	// #1207's two extra cover rungs. A curator-CHOSEN cover is not
	// exempt from ADR 0090 — choosing a picture is not a licence to
	// publish it to a reader who never opted in — so each rung carries
	// the same conjunct the derived one has carried since #1118.
	matureFeaturedCover := visibility.MatureFilterSQL(
		"fa", visibility.MatureOwnerColAsset, matureArg, q.Mature, q.MatureAdmin)
	matureRegularCover := visibility.MatureFilterSQL(
		"ra", visibility.MatureOwnerColAsset, matureArg, q.Mature, q.MatureAdmin)
	matureCountAsset := visibility.MatureFilterSQL(
		"ia", visibility.MatureOwnerColAsset, matureArg, q.Mature, q.MatureAdmin)
	matureCountPost := visibility.MatureFilterSQL(
		"ip", visibility.MatureOwnerColPost, matureArg, q.Mature, q.MatureAdmin)
	if matureAsset != "" {
		args = append(args, matureOwner)
	}

	// The surface. See PlacementQuery.BandID for why the two arms gate
	// their audience on different rows and why that is one rule rather
	// than two.
	surfaceFrag := "f.band_id IS NULL AND " + ScopeVisibleSQL("f", caller)
	if q.BandID != nil {
		args = append(args, *q.BandID)
		surfaceFrag = "f.band_id = $" + strconv.Itoa(len(args)) + "::uuid"
	}

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

	// Sixth and seventh splices of the SAME asset predicate, one per
	// CHOSEN cover rung (#1207). This is what makes "the curator's
	// choice reaches the rail" incapable of widening access: a chosen
	// cover is an asset being painted, so it clears the identical bar
	// the derived one does, and a cover this caller may not see simply
	// fails to produce a row — the next rung answers instead. There is
	// no "chosen covers are trusted" arm anywhere, because the person
	// who chose it and the person reading the rail are not the same
	// person and never were.
	featuredCoverFrag, featuredCoverArgs := assetPred.ToSQL("fa", len(args))
	args = append(args, featuredCoverArgs...)
	regularCoverFrag, regularCoverArgs := assetPred.ToSQL("ra", len(args))
	args = append(args, regularCoverArgs...)

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
		visibility.WithPostCaps(q.PostCaps))
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
       -- The subject's owner, for the promo band's author chip (#1118).
       -- A REF, never a name — see RailRow.OwnerUserRef. It rides the
       -- same predicate-gated joins as the title, so a subject this
       -- caller cannot see contributes no owner either; there is no
       -- separate gate to keep in step.
       COALESCE(a.owner_user_ref, c.owner_user_ref) AS owner_user_ref,
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
                 JOIN posts ip ON ip.id = icp.post_id` + countPostFrag + matureCountPost + `
                WHERE icp.collection_id = c.id
                  AND icp.pinned = TRUE
                  AND (icp.expires_at IS NULL OR icp.expires_at > NOW()))
            + (SELECT count(*)
                 FROM collection_resources icr
                 JOIN assets ia ON ia.id = icr.asset_id` + countAssetFrag + matureCountAsset + `
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
       -- The chosen crop position (#1207), NULL for centre. The lateral
       -- emits it only on the rungs the curator actually looked at, so
       -- there is no CASE on subject_kind here: an asset subject never
       -- resolves the lateral at all.
       cover.focal_x AS cover_focal_x,
       cover.focal_y AS cover_focal_y,
       -- The crop's tightness (#1212), NULL for the fit. Rides the same
       -- rungs as the focal point for the same reason.
       cover.zoom AS cover_zoom,
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
       ON f.subject_kind = 'asset' AND a.id = f.subject_id` + assetFrag + matureAsset + `
LEFT JOIN collections c
       ON f.subject_kind = 'collection' AND c.id = f.subject_id` + collFrag + `
-- The collection tile's picture, in PREFERENCE ORDER (#1207, which
-- absorbed #1200):
--
--   0  featured_cover_asset_id  — chosen FOR this strip
--   1  cover_asset_id           — the collection's ordinary cover
--   2  the derived hero-card cover of ADR 0027
--
-- Until #1207 this lateral was rung 2 alone, under a comment saying the
-- explicit curator override "is not in the schema yet" — untrue since
-- migration 00046 added cover_asset_id, and the reason the strip showed
-- a derived picture while every other collection surface showed the
-- chosen one. That comment is gone with the defect it described.
--
-- ⚠️ EACH RUNG IS INDEPENDENTLY GATED, AND THAT IS WHAT MAKES THE
-- FALLBACK SAFE RATHER THAN MERELY TIDY. The three gates the derived
-- rung has always carried apply to all three:
--
--   * the caller's own asset predicate (spliced per alias) — visibility
--   * sensitivity = 'public' — per-asset tier (ADR 0020/0064), same bar
--     as the asset-subject hint, so an embargo cover yields no pixels
--   * a servable col variant — so the tile never fires a 404
--
-- plus the ADR 0090 mature conjunct. A rung the caller may not see
-- produces NO ROW, so the next rung answers — the tile falls back
-- rather than blanking, and it never names the withheld asset. That is
-- the same direction ComposeCovers takes for the collection card, and
-- it is why "the curator chose it" appears nowhere as a reason to
-- widen: the chooser and the reader are different people.
--
-- The derived rung keeps its own ORDER BY inside parentheses. "Most-
-- recent ELIGIBLE post cover" rather than "most-recent post, then
-- check": picking the newest post first and rejecting it would blank
-- the whole tile whenever the latest post happens to be team-tier,
-- which is the common case in a studio dataset. Restricting the
-- ordering to what the caller may already see keeps the ADR's intent
-- (newest wins) without letting one gated post hide an otherwise-
-- showable collection.
--
-- (The removed fourth gate was has_image, a column with no writer that
-- would have made every cover blank had it been required. It is gone
-- from the schema entirely as of #579; this note stays as the record of
-- why the gate list is three and not four.)
--
-- THE FOCAL POINT RIDES ONLY THE CHOSEN RUNGS. The curator positioned
-- the marquee over a picture they were shown in the editor, which is
-- the featured cover or, when they have not set one, the regular cover.
-- The derived cover is neither — it is whatever the newest eligible
-- post happens to carry today — so a crop chosen for a different
-- picture is not transplanted onto it, and rung 2 emits NULL, which the
-- client reads as centre.
LEFT JOIN LATERAL (
       SELECT cand.id, cand.file_hash, cand.focal_x, cand.focal_y, cand.zoom
         FROM (
              SELECT fa.id, fa.file_hash, 0 AS pref,
                     c.featured_cover_focal_x AS focal_x,
                     c.featured_cover_focal_y AS focal_y,
                     c.featured_cover_zoom AS zoom
                FROM assets fa
               WHERE fa.id = c.featured_cover_asset_id` + featuredCoverFrag + matureFeaturedCover + `
                 AND fa.sensitivity = 'public'
                 AND EXISTS (SELECT 1 FROM storage_variants sv
                              WHERE sv.object_hash = fa.file_hash AND sv.variant_key = 'col')
              UNION ALL
              SELECT ra.id, ra.file_hash, 1 AS pref,
                     c.featured_cover_focal_x, c.featured_cover_focal_y,
                     c.featured_cover_zoom
                FROM assets ra
               WHERE ra.id = c.cover_asset_id` + regularCoverFrag + matureRegularCover + `
                 AND ra.sensitivity = 'public'
                 AND EXISTS (SELECT 1 FROM storage_variants sv
                              WHERE sv.object_hash = ra.file_hash AND sv.variant_key = 'col')
              UNION ALL
              (SELECT ca.id, ca.file_hash, 2 AS pref,
                      NULL::DOUBLE PRECISION, NULL::DOUBLE PRECISION,
                      NULL::DOUBLE PRECISION
                 FROM collection_posts cp
                 JOIN posts p   ON p.id = cp.post_id
                 JOIN assets ca ON ca.id = p.cover_asset_id` + coverFrag + matureCover + `
                WHERE cp.collection_id = c.id
                  AND ca.sensitivity = 'public'
                  AND EXISTS (SELECT 1 FROM storage_variants sv
                               WHERE sv.object_hash = ca.file_hash AND sv.variant_key = 'col')
                ORDER BY p.created_at DESC, p.id DESC
                LIMIT 1)
         ) cand
        ORDER BY cand.pref
        LIMIT 1
) cover ON true
-- The SURFACE, and for the rail arm its audience too (#1118). See
-- PlacementQuery.BandID: the rail's own rows are the ones with no band,
-- and leaving the band_id IS NULL conjunct off would have published every
-- band card on the anonymous landing page the moment a band was
-- curated — band rows carry the table's default scope, which the
-- signed-in arm of ScopeVisibleSQL admits.
WHERE ` + surfaceFrag + `
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
			&r.Title, &r.OwnerUserRef, &r.Subtitle, &r.ItemCount, &r.CoverAssetID,
			&r.CoverFocalX, &r.CoverFocalY, &r.CoverZoom,
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
