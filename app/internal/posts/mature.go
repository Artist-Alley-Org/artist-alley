// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// matureOwnerArg is the value bound to MatureFilterSQL's owner
// placeholder: the caller's user_ref, or 0 for anonymous.
//
// ⚠️ ZERO IS THE ANONYMOUS SENTINEL AND IT IS LOAD-BEARING, not a
// fallback. MatureFilterSQL wraps this argument in `NULLIF(…, 0)`
// precisely so an anonymous caller's ref cannot match a row whose owner
// column happens to hold 0 — the same guard MatureItemVisible spells as
// `isOwner && v.SignedIn` on the Go side, and the disagreement
// TestMatureFilterSQL_MatchesGo was written to catch.
//
// Returning a plain int64 rather than a *int64 keeps that contract
// unavoidable: a nil pointer would arrive as SQL NULL, `NULLIF(NULL,0)`
// is NULL, and `owner = NULL` is NULL — which happens to be falsy here
// and would therefore be correct BY ACCIDENT, through three-valued
// logic nobody reading the call site would check.
func matureOwnerArg(id *auth.Identity) int64 {
	if id == nil {
		return 0
	}
	return id.UserRef
}

// resolveMature answers the mature axis for this request.
//
// One lookup per request, at the top of a handler, carried down into the
// query params — never consulted per row. See visibility.MatureResolver
// for why the interface lives there and the implementation at the edge.
//
// A nil resolver, or a failed lookup, yields the DISQUALIFIED viewer.
// That is visibility.ResolveMatureOr's decision and its doc carries the
// argument; the seam is repeated in no handler.
func (h *Handler) resolveMature(ctx context.Context, id *auth.Identity) visibility.MatureViewer {
	caller := visibility.NewCaller(nil)
	if id != nil && id.UserRef != 0 {
		ref := id.UserRef
		caller = visibility.NewCaller(&ref)
	}
	return visibility.ResolveMatureOr(ctx, h.matureResolver, caller)
}

// SetMatureResolver wires the mature-content lookup (#1116).
//
// Injected rather than constructed here for the reason SetFeedFilters
// is: this package must not learn what `system_config` or
// `user_preferences` are to answer a question about a post.
func (h *Handler) SetMatureResolver(r visibility.MatureResolver) { h.matureResolver = r }
