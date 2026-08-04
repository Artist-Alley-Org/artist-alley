// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// withheldAsset is the #899 placeholder: the payload for an asset whose
// COLUMNS the caller may not receive.
//
// Written as a COMPLETE LITERAL, the same discipline posts.PostMember
// and collections.CollectionResource already use, so that a field added
// to openapi.Asset later is absent by construction rather than by
// remembering to clear it. Do not turn this into "copy the record and
// blank some fields" — that is a deny-list, and a deny-list fails open
// on the next field someone adds. It is how `config` leaked SSO secrets
// in v0.8.0 and how the free-form `metadata` blob leaked EXIF `Artist`
// and `GPSLatitude` in the #892 baseline.
//
// The permitted key set is exactly `id`, `restricted`,
// `owner_display_name` — identical to the key set PostMember's
// placeholder carries, because this surface must never be wider than
// the same asset reached through a container.
//
// `owner_display_name` is ABSENT rather than empty when unresolvable:
// a client that could tell "withheld" from "genuinely empty" could
// infer from the difference.
//
// Deliberately NOT carried: `owner_user_ref`. The owner's rule permits
// the owner's NAME on a placeholder, and a ref is a second way to ask.
func withheldAsset(id openapi_types.UUID, ownerDisplayName string) openapi.Asset {
	out := openapi.Asset{
		Id:         id,
		Restricted: true,
	}
	if ownerDisplayName != "" {
		v := ownerDisplayName
		out.OwnerDisplayName = &v
	}
	return out
}

// withholdSingleAsset applies the #899 rule to a single-asset payload,
// loading the row's readability inputs for this caller.
//
// The single-asset paths (GetAsset, UpdateAsset, the create/dedup
// splice) reach one asset at a time and have no readability decision in
// hand, so this one does its own round trip — the same trade
// enrichAssetDerived already makes for pixel dimensions and the variant
// flags, and detail is not a hot loop. The browse list does NOT call
// this: it already resolves the same columns and the same decision in
// its own single pass (list_page.go), and a second lookup per row would
// be an N+1 on the hot path.
//
// Fails CLOSED: a lookup error withholds. An asset whose sensitivity we
// could not read is an asset whose columns we do not hand out.
func (h *Handler) withholdSingleAsset(ctx context.Context, a openapi.Asset) (openapi.Asset, error) {
	assetID := uuid.UUID(a.Id)
	if assetID == uuid.Nil {
		return a, nil
	}
	caller, caps := contentCaller(ctx)
	row, ownerName, err := visibility.LoadFieldsRow(ctx, h.Pool, caller, assetID)
	if err != nil {
		return withheldAsset(a.Id, ""), err
	}
	if visibility.FieldsReadable(row, caller, caps) {
		return a, nil
	}
	return withheldAsset(a.Id, ownerName), nil
}
