// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The card payload (#850).
//
// A search hit used to carry a title, a summary and a thumbhash — enough
// for a text row and nothing else. /search therefore rendered its own
// text cards while every other browse surface rendered AssetCard /
// PostCard / CollectionCard through the shared ContentGrid, and searching
// felt like leaving the app.
//
// The fix is not a lighter card contract; it is a hit that carries what a
// card needs. These builders produce the `extra` bag for each hit type,
// naming every field EXACTLY as the corresponding list endpoint names it
// (`file_extension`, `preview_available`, `members[].asset`, …), so the
// frontend maps a hit onto a card row without translating.
//
// ⛔ THE WITHHOLDING SEAM. Every asset-derived value below is downstream
// of ONE visibility.FieldsReadable decision, and the builders are only
// ever called on its true branch. A restricted hit does not get a
// narrower `extra` — it gets withheldHit, which carries no ExtraJSON at
// all, by construction. Widening this payload is safe precisely because
// it cannot be reached without passing that gate; do not add a field
// here that is read on a row the caller failed it on. See
// hit_withholding_test.go, which asserts the serialized allow-list.
//
// The three availability flags additionally AND with `readable` at their
// assignment sites for the same reason list_page.go does: a true ladder
// flag on gated bytes is a 403 the client walks straight into.

// assetCardColumnsSQL is the SELECT fragment carrying the presentation
// columns a card reads off an asset row, beyond the ones the hit
// projections already select (id / title / description / thumbhash /
// timestamps).
//
// Deliberately the same expressions as assets.ListAssetsPageGated's
// derived columns, and for the same reason ADR 0063 gives for the
// visibility fragment: the browse tile and the search tile must be able
// to disagree about NOTHING. `col`, the configured ladder and
// `sprites.vtt` are the three gates CardThumb consults before it requests
// any bytes.
//
// `table` is the enclosing query's name for the assets table (`assets`
// or an alias); `ladderParam` is the `$n` holding the operator's
// configured ladder as text[]. Contributes ONE placeholder — the ladder —
// and pixeldims contributes none (ADR 0063 placeholder discipline).
func assetCardColumnsSQL(table, ladderParam string) string {
	return table + `.asset_type, ` + table + `.file_hash, ` + table + `.file_extension,
	       ` + pixeldims.SelectColumnsSQL(table+".id") + `,
	       (` + table + `.file_hash IS NOT NULL AND EXISTS (
	            SELECT 1 FROM storage_variants sv
	             WHERE sv.object_hash = ` + table + `.file_hash AND sv.variant_key = 'col')) AS has_col_variant,
	       ` + sysconfig.LadderSatisfiedSQL(table+".file_hash", ladderParam) + ` AS has_full_ladder,
	       (` + table + `.file_hash IS NOT NULL AND EXISTS (
	            SELECT 1 FROM storage_variants sv
	             WHERE sv.object_hash = ` + table + `.file_hash AND sv.variant_key = 'sprites.vtt')) AS has_scrub_variant`
}

// assetCardRow is the scan target for assetCardColumnsSQL. Its field
// order IS the fragment's column order — scanDest() below is what keeps
// the two in step, so a column added to the fragment is a compile-time
// edit here rather than a positional scan bug at runtime.
type assetCardRow struct {
	AssetType     int64
	FileHash      *string
	FileExtension *string
	PixelWidth    *int32
	PixelHeight   *int32
	HasCol        bool
	HasLadder     bool
	HasScrub      bool
}

// scanDest returns the scan destinations for assetCardColumnsSQL, in
// fragment order. Callers append it to their own destinations.
func (r *assetCardRow) scanDest() []any {
	return []any{
		&r.AssetType, &r.FileHash, &r.FileExtension,
		&r.PixelWidth, &r.PixelHeight,
		&r.HasCol, &r.HasLadder, &r.HasScrub,
	}
}

// assetCardExtra builds the `extra` bag for a READABLE asset hit.
//
// Only ever called past visibility.FieldsReadable — see the package note
// above. `picture` is threaded in anyway rather than assumed, because
// the three availability flags AND the thumbhash have to AND with it at
// exactly one place and this is that place.
//
// #939 — `picture` is visibility.PreviewReadable, NOT FieldsReadable.
// The two diverge for a caller who reaches this row only through a
// team-scoped `assets.admin`: ADR 0064 gives them the fields and
// withholds the picture, because a thumbhash IS a blur and the
// availability flags are a promise the binary handlers still refuse to
// keep for them. Passing `true` here (which this took literally, from
// both call sites, before #939) hands that caller the blur.
func assetCardExtra(r assetCardRow, thumbhash []byte, picture bool) []byte {
	if !picture {
		thumbhash = nil
	}
	out := map[string]any{
		"asset_type":     r.AssetType,
		"file_hash":      r.FileHash,
		"file_extension": r.FileExtension,
		// `thumbhash`, not `thumbhash_b64`. Every other surface that
		// ships a blur-up placeholder calls the field `thumbhash` and
		// base64-encodes it (assets.rowToAPI, posts' preview enrich), and
		// the card contract in cardAsset.ts asks for that name. The old
		// spelling was unique to this endpoint and read by nothing.
		"thumbhash":         encodeThumbhash(thumbhash),
		"preview_available": r.HasCol && picture,
		"ladder_available":  r.HasLadder && picture,
		"scrub_available":   r.HasScrub && picture,
	}
	// A pair or neither, never a half-populated one the client has to
	// re-validate before dividing (pixeldims.Sane is the one definition).
	if pixeldims.Sane(r.PixelWidth, r.PixelHeight) {
		out["pixel_width"] = *r.PixelWidth
		out["pixel_height"] = *r.PixelHeight
	} else {
		out["pixel_width"] = nil
		out["pixel_height"] = nil
	}
	b, _ := json.Marshal(out)
	return b
}

// encodeThumbhash renders the raw thumbhash bytes as base64, or nil when
// the asset has none. Nil rather than "" so the client's
// `thumbhash: string | null` means what it says.
func encodeThumbhash(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return base64.StdEncoding.EncodeToString(b)
}

// collectionCardExtra is the `extra` bag for a collection hit.
//
// TWO fields: the visibility label CollectionCard badges, and the
// composed mosaic `covers` (#1026), named and shaped EXACTLY as
// GET /collections names them so the frontend maps a hit onto a card row
// without translating.
//
// The covers used to be the card's own business — it fetched
// /collections/{id}/resources per tile from a client-side store. That
// store is gone (it could not see post members at all, and it slotted
// withheld members as blank tiles), so a hit that did not carry covers
// would render every searched collection as an empty folder. They are
// composed by collections.ComposeCovers against the SAME caller triple
// this engine's row predicate used, and every component of that triple
// is already part of the result cache key — so one caller's mosaic can
// never be served to another.
//
// #650's decision stands: no `featured` flag. Featuring is a scoped
// placement in featured_items (ADR 0065), not a boolean on the row.
func collectionCardExtra(visibilityLabel string, covers []openapi.CollectionCover) []byte {
	if covers == nil {
		// An empty array rather than null: "nothing to show" is an
		// answer, and the card's `covers?.length` reads the same either
		// way only by accident.
		covers = []openapi.CollectionCover{}
	}
	b, _ := json.Marshal(map[string]any{
		"visibility": visibilityLabel,
		"covers":     covers,
	})
	return b
}

// postCardMember is one entry of the post hit's `members` array — the
// same shape openapi.PostMember serialises to, so PostCard consumes it
// without translation.
//
// Exactly ONE member is ever sent: the cover. A post hit is a tile, and a
// tile shows one image; shipping the whole membership would turn a
// 25-hit page into an unbounded join for pixels nobody renders. The true
// size of the set travels separately as `member_count`, which is what
// PostCard's multi-asset badge actually needs.
type postCardMember struct {
	AssetID          uuid.UUID
	Card             *assetCardRow
	Thumbhash        []byte
	Readable         bool
	OwnerDisplayName string
}

// postCardExtra builds the `extra` bag for a post hit.
//
// `cover` may be nil — a post with no members, or one whose cover asset
// was deleted. PostCard already handles a missing cover (it falls through
// to CardThumb's no-preview plate), so the absence is expressible rather
// than needing a placeholder.
func postCardExtra(coverAssetID *uuid.UUID, cover *postCardMember, likeCount, commentCount, memberCount int64) []byte {
	out := map[string]any{
		"like_count":    likeCount,
		"comment_count": commentCount,
		"member_count":  memberCount,
	}
	if coverAssetID != nil {
		out["cover_asset_id"] = coverAssetID.String()
	} else {
		out["cover_asset_id"] = nil
	}
	members := []any{}
	if cover != nil {
		m := map[string]any{"asset_id": cover.AssetID.String(), "sort_order": 0}
		if !cover.Readable {
			// The #883 member placeholder, byte-for-byte the shape
			// posts.enrichPreview emits: the whole asset object is
			// withheld rather than blanked, so there is no
			// empty-vs-withheld difference for a client to read anything
			// off. PostCard's `coverRestricted` branch renders the
			// restricted plate from exactly this.
			m["restricted"] = true
			if cover.OwnerDisplayName != "" {
				m["owner_display_name"] = cover.OwnerDisplayName
			}
		} else if cover.Card != nil {
			r := *cover.Card
			asset := map[string]any{
				"id":                cover.AssetID.String(),
				"file_hash":         r.FileHash,
				"file_extension":    r.FileExtension,
				"thumbhash":         encodeThumbhash(cover.Thumbhash),
				"preview_available": r.HasCol,
				"ladder_available":  r.HasLadder,
				"scrub_available":   r.HasScrub,
				"pixel_width":       nil,
				"pixel_height":      nil,
			}
			if pixeldims.Sane(r.PixelWidth, r.PixelHeight) {
				asset["pixel_width"] = *r.PixelWidth
				asset["pixel_height"] = *r.PixelHeight
			}
			m["asset"] = asset
		}
		members = append(members, m)
	}
	out["members"] = members
	b, _ := json.Marshal(out)
	return b
}

// loadPostCovers resolves the cover asset for each post hit and returns
// its card payload, one round trip for the whole page.
//
// A second pass rather than a LATERAL join on runPosts' query, for two
// reasons. The join would put asset columns in scope beside the post
// predicate's unqualified column references — `deleted_at`, `status`,
// `visibility` all exist on both tables — and an ambiguous reference in
// the ONE fragment that decides who sees which post is not a risk worth
// taking for a round trip. And this mirrors posts.enrichPreview, which
// solves the identical problem for the browse feed; two expressions of
// "resolve a post's cover for this caller" that could drift is exactly
// what ADR 0063 is about.
//
// Fails CLOSED: an asset row that does not come back stays unreadable and
// renders as the #883 placeholder.
func (e *Engine) loadPostCovers(ctx context.Context, q Query, ids []uuid.UUID) (map[uuid.UUID]*postCardMember, error) {
	out := make(map[uuid.UUID]*postCardMember, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	caller, caps := callerOf(q)
	mut := mutCapsOf(q)
	rows, err := e.Pool.Query(ctx, `
		SELECT assets.id, assets.thumbhash,
		       `+visibility.FieldsColumnsSQL("assets", "$2", caller)+`,
		       `+assetCardColumnsSQL("assets", "$3")+`
		  FROM assets
		 WHERE assets.id = ANY($1::UUID[])
		   AND assets.deleted_at IS NULL
	`, ids, callerRefOf(q), e.ladder(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        uuid.UUID
			thumb     []byte
			fr        visibility.FieldsRow
			ownerName string
			card      assetCardRow
		)
		dest := append([]any{
			&id, &thumb,
			&fr.Sensitivity, &fr.Status, &fr.ProcessingStatus, &fr.OwnerUserRef,
			&fr.TeamID, &fr.IsTeamMember, &ownerName,
		}, card.scanDest()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		fr.ApplyMutationCaps(mut)
		// TWO decisions from one row (#939): the FIELD plane decides the
		// placeholder, the PICTURE plane decides the blur and the three
		// availability flags. They diverge only for an `assets.admin`
		// holder, who is owed the columns and refused the image.
		readable := visibility.FieldsReadable(fr, caller, caps)
		picture := visibility.PreviewReadable(fr, caller, caps)
		m := &postCardMember{
			AssetID:          id,
			Readable:         readable,
			OwnerDisplayName: ownerName,
		}
		if readable {
			m.Card = &card
			m.Card.HasCol = card.HasCol && picture
			m.Card.HasLadder = card.HasLadder && picture
			m.Card.HasScrub = card.HasScrub && picture
			if picture {
				m.Thumbhash = thumb
			}
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
