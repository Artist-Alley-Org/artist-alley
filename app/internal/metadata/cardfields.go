// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// The card-field projection (#552, one home since #1133)
// ---------------------------------------------------------------------------
//
// # What this is
//
// An operator marks a field `show_on_card`; every surface that renders
// an asset TILE shows that field's value on it. This resolves the flag
// for a page of asset ids, in one query, into the exact array the
// `card_fields` key on the wire carries.
//
// # Why it is in `metadata` and not in `assets`
//
// It was in `assets`, as the first half of that package's
// `decorateCards`, and for a year `show_on_card` therefore worked on
// exactly the surfaces `assets` serves. `GET /collections/{id}/resources`
// builds its rows in `collections`, which cannot import `assets` —
// assets → posts → collections is an import cycle — so the collection
// member grid could not have called it even if anyone had noticed. It
// silently rendered no card fields at all from the day #552 shipped
// (#1133).
//
// The two ways out were "restate the projection in collections" and
// "move it somewhere both can reach". The first is the #665 defect
// shape: two projections of one display rule, drifting the first time a
// field type is added, with each one's comment asserting the other's
// behaviour. So it moved here, beside [DisplayValue] — the rule it
// feeds, and the reason a value becomes a string at all (ADR 0012).
// `metadata` imports neither container package and is imported by both.
//
// # What the caller still owns
//
// Visibility. This runs AFTER the caller's own gated page query has
// chosen its rows, and it is handed only ids the caller may already
// read; ADR 0012 puts `show_on_card` in `display_order`'s class, so
// nothing may gate access, filtering or correctness on it and nothing
// here does. It never adds an id, removes one, or reorders a page.
//
// A withheld placeholder must be dropped by the caller BEFORE the call
// (both callers do, by id): a placeholder's whole contract is that the
// only keys present are the container row's own plus `restricted` and
// `owner_display_name` (#883/#899), and a field strip attached to one
// would widen that allow-list through the back door.
//
// Gated fields are unreachable from here by construction rather than by
// a filter someone has to remember: migration 00045's CHECK constraint
// refuses `show_on_card` on a field carrying a `read_capability`, so the
// query has no capability argument to get wrong.

// CardFieldsForAssets resolves the `show_on_card` values for a page of
// assets, keyed by asset id.
//
// The slice for each id is in the order the field definitions declare
// (display_group, display_order, code) — the query's ORDER BY, preserved
// by appending in row order. An id with nothing to show is ABSENT from
// the map rather than present-and-empty, which is the same "no value, no
// row" contract the field read path honours: the card then falls back to
// its own default instead of rendering an empty strip.
//
// An empty input is answered without a round trip.
func CardFieldsForAssets(
	ctx context.Context,
	db DBTX,
	ids []pgtype.UUID,
) (map[uuid.UUID][]openapi.CardField, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := New(db).ListCardFieldValues(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]openapi.CardField, len(ids))
	for _, r := range rows {
		// DisplayValue, not a local formatter: resolving a stored slug to
		// its label is ADR 0012's rule and it has one home. An empty
		// answer means this asset carries nothing for the field, and the
		// entry is dropped rather than rendered blank.
		text := r.ValueText
		var textPtr *string
		if text != "" {
			textPtr = &text
		}
		value := DisplayValue(r.Type, r.Options, textPtr, r.ValueNum, r.ValueDate, r.ValueOptions)
		if value == "" {
			continue
		}
		id := uuid.UUID(r.AssetID.Bytes)
		out[id] = append(out[id], openapi.CardField{
			Code:  r.Code,
			Label: r.Label,
			Value: value,
		})
	}
	return out, nil
}
