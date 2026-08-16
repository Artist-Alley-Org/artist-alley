// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	nethttp "net/http"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// matureViewerMiddleware resolves the mature-content axis ONCE per
// request and carries it on the context (#1116, ADR 0090).
//
// # It must run AFTER ResolveIdentity
//
// The first of the three conjuncts is "is this caller signed in", and
// that answer does not exist until the identity resolver has run. Mounted
// before it, this would resolve every request as anonymous and every
// reader would be disqualified — which fails safe, but silently, and
// "nobody can see mature content and no error is logged" is a bug that
// takes a long time to find. It is mounted immediately after, in the same
// groups, so the ordering is visible at the mount site rather than
// asserted here.
//
// # Anonymous requests cost nothing
//
// The adapter short-circuits before either lookup for a caller with no
// identity, which is the majority of traffic on a public install. A
// signed-in request pays one userprefs read — through the same LRU
// ChannelsFor and ShowRestrictedFeedMembers already use, so normally a
// cache hit — and one uncached `system_config` read, which
// sysconfig.GetMatureContent documents as deliberately uncached because a
// stale TRUE keeps serving mature content on an install whose operator
// has just switched it off.
//
// # A failed resolve disqualifies, and does not fail the request
//
// visibility.ResolveMatureOr converts an error into the disqualified
// viewer. The request proceeds and the reader sees the library an
// opted-out reader sees. The alternative — 500 the request — would turn a
// preferences-table blip into an outage of the whole browse surface, for
// a gate whose entire job is to show LESS.
// # The resolver arrives as a GETTER, not a value
//
// chi requires `Use` before any route is mounted, and the apiServer that
// owns the resolver is constructed INSIDE the route callback — so at
// mount time there is nothing to pass. A getter closes over the pointer
// and dereferences per request, which is also what makes the nil case
// real rather than theoretical: requests served before construction
// completes get the disqualified viewer, not a panic.
func matureViewerMiddleware(get func() visibility.MatureResolver) func(nethttp.Handler) nethttp.Handler {
	return func(next nethttp.Handler) nethttp.Handler {
		return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, req *nethttp.Request) {
			ctx := req.Context()
			var r visibility.MatureResolver
			if get != nil {
				r = get()
			}
			caller := visibility.NewCaller(nil)
			if id := auth.IdentityFromContext(ctx); id != nil && id.UserRef != 0 {
				ref := id.UserRef
				caller = visibility.NewCaller(&ref)
			}
			ctx = visibility.WithMatureViewer(ctx, visibility.ResolveMatureOr(ctx, r, caller))
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}
