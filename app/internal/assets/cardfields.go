// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// ---------------------------------------------------------------------------
// At-a-glance card decoration (#552)
// ---------------------------------------------------------------------------
//
// Two things a card needs that the asset row does not carry: the values of the
// fields an operator marked `show_on_card`, and — for content that came from a
// peer — who it belongs to.
//
// # One pass for a whole page
//
// Both are BATCHED across the page. A card is a browse surface: a per-row
// query is 50 round trips per scroll, and the per-row ListAssetTags call this
// handler already makes is the precedent not to follow. Two queries decorate
// the whole page regardless of its size.
//
// # The flag decides PRESENTATION and nothing else
//
// ADR 0012 puts `show_on_card` in `display_order`'s class: nothing may gate
// access, filtering or correctness on it. Nothing here does. This pass runs
// AFTER visibility has already chosen the rows and after #899 has replaced
// every unreadable one with a placeholder; it is handed only readable assets
// and it never adds one, removes one, or reorders the page. A client that
// throws the whole `card_fields` array away renders a plainer card and a
// correct one.
//
// # And it cannot become a side door
//
// Gated fields are unreachable from here by construction rather than by a
// filter someone has to remember: migration 00045's CHECK constraint refuses
// `show_on_card` on a field carrying a `read_capability`, so the query has no
// capability argument to get wrong.

// decorateCards attaches card_fields, origin and owner to a page of assets.
//
// Callers pass every asset they are about to return; withheld placeholders are
// skipped by id, because a placeholder's whole contract is that the only keys
// present are `id`, `restricted` and `owner_display_name` (#899). Adding a
// field strip to one would widen that allow-list through the back door.
func (h *Handler) decorateCards(ctx context.Context, out []openapi.Asset) error {
	ids := make([]pgtype.UUID, 0, len(out))
	index := make(map[uuid.UUID]int, len(out))
	for i := range out {
		if out[i].Restricted {
			continue
		}
		id := uuid.UUID(out[i].Id)
		if id == uuid.Nil {
			continue
		}
		ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
		index[id] = i
	}
	if len(ids) == 0 {
		return nil
	}
	q := New(h.Pool)

	// The card-field projection itself lives in `metadata` since #1133 —
	// the collection member grid renders the same tiles from the same
	// flag and could not reach it here (assets → posts → collections is
	// an import cycle), so it had never rendered one. See
	// metadata.CardFieldsForAssets; this is now the assets-side splice.
	fields, err := metadata.CardFieldsForAssets(ctx, h.Pool, ids)
	if err != nil {
		return err
	}
	for id, vals := range fields {
		i, ok := index[id]
		if !ok {
			continue
		}
		v := vals
		out[i].CardFields = &v
	}

	origins, err := q.ListAssetOrigins(ctx, ids)
	if err != nil {
		return err
	}
	for _, o := range origins {
		i, ok := index[uuid.UUID(o.AssetID.Bytes)]
		if !ok {
			continue
		}
		out[i].Origin = &openapi.ContentOrigin{
			PeerId:      o.PeerID.Bytes,
			DisplayName: o.DisplayName,
			InstanceUrl: &o.InstanceUrl,
		}
	}

	return h.decorateOwners(ctx, out, index)
}

// decorateOwners attaches `owner` — the renderable identity behind
// `owner_user_ref` — to a page of assets (#1047).
//
// # Why the card needs it at all
//
// The owner's density table gives the thumbnail view an artist block:
// avatar, name, clickable through to the profile. Until now the asset
// payload carried only `owner_user_ref`, and the schema note beside it
// said the client "can resolve the profile itself". On a browse page that
// is 50 round trips, and on any page it is the display-name ladder
// re-derived in a browser — rung 2 (`fullname`) is authenticated-only, so
// a client-side COALESCE is precisely how the anonymous rung gets skipped
// (#1023). The identity therefore rides the payload, resolved once.
//
// # One rule, obtained rather than restated
//
// [users.LookupAuthors] is the whole implementation: the same function
// the post feed's author header uses, so an asset card and a post card
// answer "who made this" identically, including the ADR 0024 opt-out. A
// ref whose owner hid from anonymous readers is simply MISSING from the
// map for an anonymous caller — an omission, never a redacted entry — so
// the failure mode of any bug here is a card with no artist block, never
// one with an artist who opted out.
//
// # Batched, and per-caller
//
// One query for the DISTINCT owner refs on the page, which is the same
// contract the two passes above hold to: adding assets to a page must not
// add queries. It runs here, on the way out, rather than in a cached
// projection, for the reason posts/author_enrich.go spells out at length
// — whether the identity appears AT ALL depends on the caller, so a
// cross-caller cache entry would hand the first authenticated reader's
// answer to the next anonymous one.
//
// Restricted placeholders never reach this: `index` was built by skipping
// them, and their owner name comes from [users.PlaceholderOwnerName] on
// the read path instead, under the narrower allow-list #899 defined.
func (h *Handler) decorateOwners(ctx context.Context, out []openapi.Asset, index map[uuid.UUID]int) error {
	anonymous := auth.IdentityFromContext(ctx) == nil

	refSet := make(map[int64]struct{}, len(index))
	for _, i := range index {
		out[i].Owner = nil
		if out[i].OwnerUserRef != nil {
			refSet[*out[i].OwnerUserRef] = struct{}{}
		}
	}
	if len(refSet) == 0 {
		return nil
	}
	refs := make([]int64, 0, len(refSet))
	for ref := range refSet {
		refs = append(refs, ref)
	}

	owners, err := users.LookupAuthors(ctx, h.Pool, refs, anonymous)
	if err != nil {
		return err
	}
	for _, i := range index {
		if out[i].OwnerUserRef == nil {
			continue
		}
		if a, ok := owners[*out[i].OwnerUserRef]; ok {
			owner := a
			out[i].Owner = &owner
		}
	}
	return nil
}
