// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// withheldHit is the #899 placeholder for a search hit whose asset
// columns the caller may not receive.
//
// Written as a COMPLETE LITERAL, the same discipline as
// assets.withheldAsset and the PostMember placeholder: a field added to
// Hit later is absent by construction rather than by remembering to
// clear it. Do not rewrite this as "copy h and blank some fields" —
// that is a deny-list, and a deny-list fails open on the next field.
//
// The scores survive because the caller's page ordering is computed
// from them AFTER projection (normalisation, the hybrid merge, the
// cursor). Dropping them here would not withhold anything — the
// ordering is visible in the array — it would just corrupt paging.
func withheldHit(h Hit, ownerDisplayName string) Hit {
	return Hit{
		Type:             h.Type,
		ID:               h.ID,
		Restricted:       true,
		OwnerDisplayName: ownerDisplayName,
		RawScore:         h.RawScore,
		NormalisedScore:  h.NormalisedScore,
		VectorScore:      h.VectorScore,
		HybridScore:      h.HybridScore,
	}
}

// callerOf builds the (caller, capability checker) pair every asset
// projection in this package decides readability with. One place, so a
// new projection cannot accidentally pass a nil checker and quietly
// deny a demo-viewer their catalogue.
func callerOf(q Query) (visibility.Caller, visibility.CapabilityChecker) {
	return visibility.NewCaller(q.CallerUserRef), q.Caps.Checker()
}

// mutCapsOf is the caller's resolved asset-mutation capabilities (#939),
// carried on the Query rather than read from the request context.
//
// That is not incidental. The engine runs behind a result CACHE whose
// key is built from the Query value alone (cache.go), and on a hit no
// per-row readability code executes at all. A capability read from
// anywhere but the Query would therefore be absent from the key, which
// is exactly the stale-unredacted-cache defect #899 closed. Anything
// that widens a projection must travel by a route the key can see.
func mutCapsOf(q Query) visibility.AssetMutationCaps {
	return q.MutationCaps
}

// renderContextOf is the caller half of [facet.Selection.SQL] (#1251) —
// the inputs a dimension needs when its predicate has to decide, in SQL,
// whether this caller may see the value it selects on.
//
// `callerArg` is the placeholder the CALLING STATEMENT has already bound
// with the caller's ref — $3 in runAssets and runPosts, $2 in
// enrichAssetHits — so there is one such value per statement rather than
// one per filter term. Passing "" is how a statement says it has no such
// placeholder (runCollections does not bind one and needs none), and the
// dimensions that require it then render nothing — see
// facet.RenderContext for why that is the fail-closed direction.
//
// Same routing argument as [mutCapsOf] above: every component here is
// already on the Query, so every component is already in the result
// cache key.
func renderContextOf(q Query, callerArg string) facet.RenderContext {
	return facet.RenderContext{
		Caller:       visibility.NewCaller(q.CallerUserRef),
		Caps:         q.Caps,
		MutationCaps: q.MutationCaps,
		CallerArg:    callerArg,
	}
}

// callerRefOf is the bare user_ref the FieldsColumnsSQL fragment binds
// for its team-membership EXISTS. Anonymous carries
// visibility.AnonymousCaller (0), which matches no membership row.
func callerRefOf(q Query) int64 {
	return visibility.NewCaller(q.CallerUserRef).UserRef
}
