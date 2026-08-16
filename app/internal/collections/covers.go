// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The composed cover mosaic — /collections and /collections/{id}
// ---------------------------------------------------------------------------
//
// #1026: a collection holding two posts and one asset rendered as an
// empty folder. A collection stores no cover of its own; the tile
// composes one from its members at render time, and the composer only
// ever knew about `collection_resources`. #882 gave collections POSTS —
// "save someone else's post" — and nothing taught the cover composer
// about them, so a post-only collection had no cover source at all.
//
// # Why this is composed on the SERVER
//
// It used to be a per-card fetch of /collections/{id}/resources from a
// client-side store (web/src/lib/stores/collectionCovers.svelte.ts,
// deleted with this change). Three reasons, and only the third is about
// request count:
//
//  1. ORDERING across two member lists cannot be done correctly on the
//     client without fetching BOTH in full. Interleaving by `added_at`
//     needs every candidate, not the first page of one table.
//  2. The CROWDING fix needs to know which members are renderable, and
//     the read predicate lives here. The old card slotted a restricted
//     member as a blank tile: four restricted members at the head of a
//     collection produced four blank quarters and every renderable
//     member behind them never got a chance.
//  3. It removes one HTTP request per card. Real, but a consequence.
//
// # The withholding shape, and what it is NOT
//
// A member this caller may not picture is ABSENT — it does not occupy a
// slot. That is deliberately different from
// /collections/{id}/resources, which returns a restricted asset as a
// VISIBLE placeholder so the reader can see a restriction exists and
// #881's "request access" has something to attach to. A blank quarter of
// a mosaic attaches to nothing; it just costs a picture. The posts
// listing already answers with absence for the same kind of reason
// ("absence is the honest answer here").
//
// Note that the old client store did NOT leak restricted pictures — it
// crowded them out. This change must keep that true while fixing the
// crowding, which is what
// TestCollectionCovers_WithheldMemberContributesNothing pins.

// coverCount is how many tiles a mosaic holds. CollectionCard lays out
// 1 / 2 / 3 / 4 members and ignores anything past the fourth, so asking
// for more would be rows nobody paints.
const coverCount = 4

// ComposeCovers returns, for each requested collection, either the
// curator's CHOSEN cover as the sole entry (#1027) or — when there is
// none, or this caller may not picture it — the first `coverCount`
// members whose picture this caller may render, in the curator's order,
// interleaved across both membership kinds.
//
// # Why the override lives HERE and not in the callers
//
// Because there are two callers — [Handler.attachCovers], serving both
// the list and the detail, and search's collection hit — and a rule
// expressed twice will eventually disagree with itself. #1023 shipped a
// display-name ladder transcribed four times, three of them wrong. The
// callers ask "what do I paint"; only this function decides.
//
// The fallback is the reason it cannot be a caller's job in the first
// place: choosing between the override and the mosaic needs the mosaic
// already computed under the same predicate, in the same round trip,
// because "the override did not render" is only knowable after running
// the gate. A caller could not do it without a second query and a
// second copy of the rule.
//
// Each asset appears AT MOST ONCE per collection, at its earliest
// position — one asset is reachable by several routes (pinned directly
// and carried as a post's cover, or shared by two posts) and the same
// picture twice is not a summary. See the DISTINCT ON below.
//
// Collections with no renderable member are ABSENT from the map rather
// than mapped to an empty slice; callers emit whatever their surface's
// "no cover" is. A nil/empty `ids` is not an error and does no query.
//
// # Why the whole decision is in SQL
//
// Because the decision determines which rows come back at all. "The
// first four that render" cannot be answered by fetching a prefix and
// filtering in Go without guessing how long the prefix must be — and
// the guess is exactly the crowding bug in a new costume, just with a
// higher threshold. A ROW_NUMBER over the already-filtered set answers
// it exactly, with LIMIT applied after the gate, for every collection on
// a hub page in ONE round trip.
//
// That is the exception [visibility.PreviewReadableSQL] was created
// under, and it is held to [visibility.PreviewReadable] by
// TestPreviewReadableSQL_MatchesGo. Nothing here re-expresses a
// visibility rule: the asset half is that fragment, the post half is
// [visibility.Filter] over EntityPost — the same predicate
// GET /posts/{id}, the browse feed and /collections/{id}/posts all run.
//
// # The caller triple
//
// `caller` and `caps` decide the PICTURE plane over assets; `postCaps`
// decides which post ROWS exist. They are passed in resolved rather than
// read off the context because /search reaches this from an engine that
// holds no *auth.Identity — it resolved the same three at the HTTP edge
// and folds them into its cache key. [CoverCallerFromContext] is the
// adapter for handlers that DO have an identity.
//
// Asset MUTATION scope is deliberately not a parameter. ADR 0064 gives a
// team-scoped `assets.admin` holder the FIELDS of the assets they
// administer and explicitly not the picture, and a mosaic is nothing but
// pictures.
//
// # The mature axis (#1147, ADR 0090)
//
// `mature` is the FOURTH caller-side input and it is a parameter for the
// same reason the other three are: /search reaches this from an engine
// that holds no request context, having resolved the viewer at the HTTP
// edge and folded [visibility.MatureViewer.CacheKey] into its key. A
// function that read the context itself would answer from an empty one
// there — the disqualified viewer, permanently and silently, which is
// the exact defect #1147 also found in saved searches.
//
// It composes on the PICTURE plane, which is the plane this whole
// function operates on, and it is why the leak existed at all:
// `visibility.PreviewReadableSQL` answers "who is ALLOWED" and says
// nothing about "who has OPTED IN" (ADR 0090 §1). Every list surface was
// gated by #1117 while this one — one construction over, deriving a
// picture rather than listing a row — was not, so a mature asset pinned
// into a public collection rendered its real `col` rendition to an
// anonymous visitor, on the same /search response that correctly dropped
// the asset's own hit.
//
// THREE splices, one per table that contributes a picture:
//
//	a (override)   the curator's chosen cover — a mature cover is a
//	               mature picture, and `collections` has no mature column
//	               of its own, so the cover asset is where the rule acts
//	a (renderable) the mosaic's member assets — the tile itself
//	p (members)    the post half's ROW plane. A post's `mature` is DERIVED
//	               by trigger from its members, so a post can be mature
//	               with a non-mature cover; the feed and
//	               /collections/{id}/posts both drop that post for a
//	               disqualified viewer, and a mosaic tile standing in for
//	               a member this viewer cannot see is not a summary of
//	               what they can see. It is the same conjunct
//	               featured.ListPlacements splices on `ip` for the member
//	               COUNT, so the count and the mosaic keep agreeing.
//
// ⚠️ WITHHOLDING HERE IS A FALLBACK, NEVER A BLANK TILE (ADR 0088). The
// override's splice can only send a collection down to the mosaic branch
// — the NOT EXISTS below already makes "the override did not render"
// mean "compose from members" — and the members' splice removes
// candidates before the rank, so the LIMIT stays exact and a mosaic
// simply comes back shorter. A collection whose only renderable members
// are mature is ABSENT from the map for this viewer, which is the same
// answer as a collection with no renderable member at all, and callers
// already paint their surface's "no cover" for that. Nothing anywhere
// gets an empty slot.
func ComposeCovers(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	caps visibility.ContentCaps,
	postCaps visibility.PostCaps,
	mature visibility.MatureViewer,
	ids []uuid.UUID,
) (map[uuid.UUID][]openapi.CollectionCover, error) {
	if len(ids) == 0 {
		return map[uuid.UUID][]openapi.CollectionCover{}, nil
	}

	// $1 = collection ids, $2 = tiles wanted. The post predicate's args
	// are appended LAST and its fragment binds from $3 up (ADR 0063
	// placeholder discipline). It renders textually ABOVE $2 inside the
	// CTE, which is fine — the invariant is an index bound, not textual
	// order.
	args := []any{ids, coverCount}

	pred, err := visibility.Filter(ctx, visibility.EntityPost, caller,
		visibility.WithPostCaps(postCaps))
	if err != nil {
		return nil, fmt.Errorf("collections: covers: post read rule: %w", err)
	}
	// EntityPost's ToSQL renders `deleted_at IS NULL AND (rule)` — the
	// shape visibility.PostReadable runs, IncludeSoftDeleted
	// deliberately NOT passed. A soft-deleted post is not a cover.
	postFrag, postArgs := pred.ToSQL("p", len(args))
	args = append(args, postArgs...)

	// The caller's ref goes in as a LITERAL, not a placeholder, and that
	// is load-bearing rather than a shortcut.
	//
	// PreviewReadableSQL is EMPTY for a system.admin / content.read.all
	// caller — that is its documented short-circuit. A `$n` naming the
	// ref would then be bound and never referenced, and Postgres refuses
	// the statement outright ("could not determine data type of
	// parameter"). So the mosaic would 500 for exactly the two
	// capabilities that are supposed to see everything, on a path no
	// ordinary test caller takes. (Found by deleting this fragment and
	// watching every cover test fail with 42P18 rather than the one
	// assertion under test.)
	//
	// A bound tautology is the other way out — query.go's search COUNT
	// does exactly that — but it costs a placeholder that means nothing
	// and a comment on both halves explaining why deleting either breaks
	// the other. An int64 the auth resolver produced inside this process
	// is not caller text and cannot carry an injection, which is the
	// same argument FieldsReadableSQL makes for rendering the mutation
	// team set as UUID literals.
	previewFrag := visibility.PreviewReadableSQL("a",
		strconv.FormatInt(caller.UserRef, 10), caller, caps)

	// The mature conjuncts (#1147), on the SAME literal-ref discipline as
	// previewFrag above and for a second reason on top of it.
	//
	// Every other splice site in the tree binds this ref as a `$n` and has
	// to bind it CONDITIONALLY, because MatureFilterSQL returns the empty
	// string for a qualified viewer and for an admin, and a parameter no
	// statement mentions is 42P18 on every request by a qualified reader
	// (posts/list_page.go and featured/rail.go both carry that warning).
	// A literal has no such arm to get wrong: the fragment is spliced
	// three times below, and three placeholders that must appear-or-not
	// together, in a query that already renumbers two predicate fragments
	// around them, is exactly the arithmetic ADR 0063's discipline exists
	// to avoid. `caller.UserRef` is an int64 the auth resolver produced
	// inside this process, not caller text.
	//
	// ⚠️ ZERO IS THE ANONYMOUS SENTINEL AND IT IS LOAD-BEARING, not a
	// fallback: visibility.NewCaller(nil) leaves UserRef at 0 and
	// MatureFilterSQL wraps the argument in `NULLIF(…, 0)` precisely so an
	// anonymous caller cannot match a row whose owner column holds 0 as
	// ITS OWNER. Same guard PreviewReadableSQL relies on one line up.
	matureOwnerArg := strconv.FormatInt(caller.UserRef, 10)
	matureAssetFrag := visibility.MatureFilterSQL("a",
		visibility.MatureOwnerColAsset, matureOwnerArg, mature, caps.SystemAdmin)
	maturePostFrag := visibility.MatureFilterSQL("p",
		visibility.MatureOwnerColPost, matureOwnerArg, mature, caps.SystemAdmin)

	// previewFrag is spliced TWICE below — once for the curator's chosen
	// cover, once for the derived mosaic's members. That is safe because
	// it contains no `$n`: its caller ref is a literal (above) and
	// FieldsReadableSQL renders the mutation team set as UUID literals
	// for the same reason. A fragment carrying placeholders could not be
	// reused without renumbering, and the reuse is the point — the
	// override and the mosaic must gate the picture identically or the
	// override becomes a second, weaker door to the same asset.
	sql := `
WITH override AS (
    -- #1027: the curator CHOSE a cover. It wins over the composed
    -- mosaic, and it is the sole tile — a chosen cover is a statement
    -- about what this collection looks like, not a first among four.
    --
    -- It is gated by the SAME picture plane as a member, against the
    -- SAME asset conditions, and that is the whole security argument:
    -- pointing a collection at an asset cannot show anyone a picture
    -- they could not already see. The pointer may name any asset the
    -- CURATOR could picture, so the curator's plane and the viewer's
    -- routinely differ; the viewer's is the one that decides here.
    --
    -- When this CTE comes back EMPTY for a collection — withheld from
    -- this viewer, soft-deleted, or still missing its 'col' rendition —
    -- the mosaic below answers instead. Falling back rather than
    -- yielding nothing is load-bearing: an empty tile is exactly the
    -- crowding defect #1026 fixed, and shipping it back through a new
    -- door would be the same bug with a new cause. It also means the
    -- absence of a cover is indistinguishable from never having set
    -- one, so the tile cannot be read as "something is hidden here".
    --
    -- A hard-deleted asset never reaches this join at all: the FK is
    -- ON DELETE SET NULL, so the pointer is already gone.
    --
    -- #1147: the mature conjunct is spliced here too, and NOT only on the
    -- mosaic. The pointer is a second door to one asset's pixels, and a
    -- door the curator opens — pointing a public collection at a mature
    -- asset would otherwise publish it to every reader, including one who
    -- never opted in, which is the escalation the picture-plane reuse two
    -- paragraphs up exists to make impossible. Withholding sends the
    -- collection down to the mosaic below, exactly as a withheld tier
    -- does.
    SELECT c.id AS collection_id, c.cover_asset_id AS asset_id
      FROM collections c
      JOIN assets a ON a.id = c.cover_asset_id
     WHERE c.id = ANY($1::UUID[])
       AND a.deleted_at IS NULL
       AND a.file_hash IS NOT NULL
       AND EXISTS (SELECT 1 FROM storage_variants sv
                    WHERE sv.object_hash = a.file_hash
                      AND sv.variant_key = 'col')` + previewFrag + matureAssetFrag + `
),
members AS (
    -- The asset half. No EntityAsset predicate spliced, matching
    -- ListCollectionResourcesPageGated: the picture plane below is
    -- strictly tighter than that predicate's asset branch. Soft-delete
    -- is the one conjunct it does not carry, so it stays inline.
    SELECT cr.collection_id, cr.added_at, cr.sort_order, cr.asset_id
      FROM collection_resources cr
     WHERE cr.collection_id = ANY($1::UUID[])
       AND cr.pinned = TRUE
       AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
    UNION ALL
    -- The post half. A post contributes its FEED COVER, and
    -- cover_thumbnail_asset_id wins over cover_asset_id because that is
    -- what the Post schema already specifies for a feed card — the tile
    -- and the card should show the same picture. NULL when a post has
    -- neither; the JOIN below drops it.
    --
    -- #1147's post ROW plane rides the post predicate: posts.mature is
    -- derived by trigger from the members, so a mature post can carry a
    -- non-mature cover and the asset conjunct below would pass it. The
    -- feed hides that post; a tile standing in for it here would put the
    -- hidden member back on screen.
    SELECT cp.collection_id, cp.added_at, cp.sort_order,
           COALESCE(p.cover_thumbnail_asset_id, p.cover_asset_id)
      FROM collection_posts cp
      JOIN posts p ON p.id = cp.post_id
     WHERE cp.collection_id = ANY($1::UUID[])
       AND cp.pinned = TRUE
       AND (cp.expires_at IS NULL OR cp.expires_at > NOW())` + postFrag + maturePostFrag + `
),
renderable AS (
    -- DISTINCT ON collapses one asset reached by several routes to a
    -- single tile, at its EARLIEST position. A collection may pin an
    -- asset directly and also hold a post whose cover is that same
    -- asset, and two posts may share a cover; the mosaic is a visual
    -- summary and the same picture twice summarises nothing. It has to
    -- happen HERE, before the rank, or a duplicate would eat a slot and
    -- the mosaic would come back short.
    SELECT DISTINCT ON (m.collection_id, m.asset_id)
           m.collection_id, m.asset_id, m.added_at, m.sort_order
      FROM members m
      JOIN assets a ON a.id = m.asset_id
     WHERE a.deleted_at IS NULL
       -- Caller-INDEPENDENT: an asset with no 'col' rendition cannot
       -- paint a tile for anyone, so it is not a candidate. Pushing it
       -- down here is what keeps the LIMIT below exact.
       AND a.file_hash IS NOT NULL
       AND EXISTS (SELECT 1 FROM storage_variants sv
                    WHERE sv.object_hash = a.file_hash
                      AND sv.variant_key = 'col')` + previewFrag + matureAssetFrag + `
     ORDER BY m.collection_id, m.asset_id, m.added_at ASC, m.sort_order ASC
),
ranked AS (
    SELECT r.collection_id, r.asset_id,
           ROW_NUMBER() OVER (
               PARTITION BY r.collection_id
               -- added_at first: it is the ONE axis comparable across
               -- the two membership tables, and it is the order the
               -- curator built. sort_order breaks ties within a kind;
               -- asset_id makes the result stable.
               ORDER BY r.added_at ASC, r.sort_order ASC, r.asset_id ASC
           ) AS rn
      FROM renderable r
)
-- The override replaces the mosaic rather than joining it, so a
-- collection contributes rows from exactly one of the two branches.
-- NOT EXISTS rather than a LEFT JOIN ... IS NULL because the mosaic
-- branch must contribute NOTHING when an override rendered, not four
-- rows that a later step filters: the override is the answer, and the
-- mosaic is what the answer falls back to.
--
-- rn 0 for the override keeps ONE ordering expression for both
-- branches; it is never compared against $2 because a single row
-- cannot overflow a four-tile budget.
SELECT collection_id, asset_id
  FROM (
      SELECT o.collection_id, o.asset_id, 0 AS rn
        FROM override o
      UNION ALL
      SELECT r.collection_id, r.asset_id, r.rn
        FROM ranked r
       WHERE r.rn <= $2::INTEGER
         AND NOT EXISTS (SELECT 1 FROM override o
                          WHERE o.collection_id = r.collection_id)
  ) final
 ORDER BY collection_id, rn`

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("collections: covers: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]openapi.CollectionCover, len(ids))
	for rows.Next() {
		var collectionID, assetID uuid.UUID
		if err := rows.Scan(&collectionID, &assetID); err != nil {
			return nil, fmt.Errorf("collections: covers scan: %w", err)
		}
		out[collectionID] = append(out[collectionID], openapi.CollectionCover{
			AssetId: openapi_types.UUID(assetID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collections: covers rows: %w", err)
	}
	return out, nil
}

// CallerMayPictureAsset answers whether this caller may see the given
// asset's PICTURE — the gate the write path runs before storing a
// collection's chosen cover (#1027).
//
// It lives beside [ComposeCovers] and splices the same
// [visibility.PreviewReadableSQL] because it is deliberately the SAME
// question at the other end of the feature: the curator may only point
// at what they could already look at. Writing it as a second expression
// — "readable, roughly" — is how a write gate drifts loose from the read
// gate it is supposed to mirror, and a looser write gate here would let
// a curator name an asset id they cannot see and learn from the response
// whether it exists.
//
// # Why it does NOT require the 'col' rendition
//
// [ComposeCovers] additionally requires a `col` variant, because an
// asset without one cannot paint a tile. This does not, and the
// difference is intentional: renditions are produced asynchronously, so
// an asset uploaded a moment ago has no `col` yet and would be refused
// for a reason that stops being true by itself a minute later. The read
// path already falls back to the mosaic in the meantime, so the cost of
// accepting it is a collection that keeps its old tile briefly, while
// the cost of refusing it is an error the curator cannot act on.
//
// Mutation scope is not a parameter, for the reason on [ComposeCovers]:
// ADR 0064 gives an `assets.admin` holder the FIELDS and explicitly not
// the picture, and a cover is nothing but a picture.
//
// # It takes the mature axis for the SAME reason it takes the rest
//
// The whole argument above is that this is one rule evaluated twice, so
// the moment [ComposeCovers] grew a fourth input (#1147) this had to
// grow it too — a write gate that stayed one conjunct wider would be the
// "readable, roughly" second expression the paragraph above refuses. It
// costs nothing an artist would notice: the owner exemption is inside
// [visibility.MatureFilterSQL], so an artist may always point a
// collection at their own mature asset, opted in or not.
//
// The narrow thing it closes is an existence oracle on the mature axis.
// A disqualified curator who guesses a mature asset's id would otherwise
// learn from the 400-vs-success difference that the id names something
// real — the one fact the read path spends three splices withholding
// from that same curator.
func CallerMayPictureAsset(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	caps visibility.ContentCaps,
	mature visibility.MatureViewer,
	assetID uuid.UUID,
) (bool, error) {
	// Soft-delete is inline for the same reason it is in ComposeCovers:
	// PreviewReadableSQL does not carry it (a deleted asset is gone, not
	// withheld) and every caller's own query drops those rows.
	previewFrag := visibility.PreviewReadableSQL("a",
		strconv.FormatInt(caller.UserRef, 10), caller, caps)
	// Literal owner ref, as above and for the first of the same two
	// reasons: it keeps the conjunct free of a placeholder that would
	// have to be bound conditionally.
	matureFrag := visibility.MatureFilterSQL("a",
		visibility.MatureOwnerColAsset, strconv.FormatInt(caller.UserRef, 10),
		mature, caps.SystemAdmin)
	sql := `SELECT EXISTS (
    SELECT 1 FROM assets a
     WHERE a.id = $1
       AND a.deleted_at IS NULL` + previewFrag + matureFrag + `)`
	var ok bool
	if err := pool.QueryRow(ctx, sql, assetID).Scan(&ok); err != nil {
		return false, fmt.Errorf("collections: cover picture gate: %w", err)
	}
	return ok, nil
}

// CoverCallerFromContext resolves the four caller-side inputs
// [ComposeCovers] takes from a request context.
//
// The mature viewer is returned HERE rather than read at each call site,
// so the read path and the write gate below it cannot pick up different
// answers — "may point at" and "may see painted" are one rule evaluated
// twice, and #1147 is what a fourth input silently defaulting on one of
// the two halves looks like. ⚠️ visibility.MatureFromContext returns the
// DISQUALIFIED viewer when the middleware never ran; see its doc for why
// the visible failure was chosen over the invisible one.
//
// The POST caller maps a nil-or-anonymous identity to the anonymous
// caller, which is what posts.postRuleInputs does and is NARROWER than
// [collectionCaller], which binds the identity's ref. The two agree in
// practice — no user holds ref 0, so the authenticated post predicate's
// author and grant disjuncts cannot match for it — and the narrower one
// is the direction to be wrong in. See the note on
// posts.ListCollectionPosts, which made the same choice for the same
// reason.
func CoverCallerFromContext(ctx context.Context) (
	visibility.Caller,
	visibility.ContentCaps,
	visibility.PostCaps,
	visibility.MatureViewer,
) {
	mature := visibility.MatureFromContext(ctx)
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		// The mature viewer still comes off the context for the
		// anonymous arm rather than being zeroed here: the middleware
		// already resolved an anonymous caller to the disqualified
		// viewer, and re-deriving it would be a second expression of the
		// first conjunct — the one place a "signed in" answer could
		// drift from visibility.QualifiesForMature's.
		return visibility.NewCaller(nil), visibility.ContentCaps{}, visibility.PostCaps{}, mature
	}
	can := func(code string) bool { return id.Can(code) }
	ref := id.UserRef
	return visibility.NewCaller(&ref),
		visibility.ResolveContentCaps(can),
		visibility.ResolvePostCaps(can),
		mature
}

// attachCovers stamps the composed mosaic onto a page of collections,
// in ONE query for the whole page.
//
// Per-caller by definition — a member may be withheld from one reader
// and not the next — so it runs AFTER the by-id cache read and never
// touches the cached Collection. That is ADR 0013's 2026-08-11
// amendment verbatim: "if a value depends on who is asking, it is
// computed after the cache, not stored in it. A shared entry may hold
// only what every caller may see." Baking a cover set into the cached
// row would mean the first reader to warm an entry decides what every
// later reader sees — intermittent, ordering-dependent, and invisible to
// any test that warms the cache from the caller it asserts with.
//
// The pointers are into the caller's own []openapi.Collection, built by
// copying each cached row by value, so assigning `Covers` writes to that
// copy. `Covers` is RESET to nil first: the rows come off a cache that
// must never have carried one, and clearing makes "the query decides"
// true rather than merely expected.
func (h *Handler) attachCovers(ctx context.Context, cs ...*openapi.Collection) error {
	ids := make([]uuid.UUID, 0, len(cs))
	for _, c := range cs {
		if c == nil {
			continue
		}
		c.Covers = nil
		ids = append(ids, uuid.UUID(c.Id))
	}
	if len(ids) == 0 {
		return nil
	}
	caller, caps, postCaps, mature := CoverCallerFromContext(ctx)
	byID, err := ComposeCovers(ctx, h.Pool, caller, caps, postCaps, mature, ids)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c == nil {
			continue
		}
		covers := byID[uuid.UUID(c.Id)]
		if covers == nil {
			// An empty array, not an absent key: "nothing to show" is
			// an answer, and the schema reserves ABSENT for surfaces
			// that did not compose covers at all.
			covers = []openapi.CollectionCover{}
		}
		c.Covers = &covers
	}
	return nil
}
