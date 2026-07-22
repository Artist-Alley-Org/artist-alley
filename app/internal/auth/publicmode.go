// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import "strings"

// Public mode (#445) — the install-wide switch that decides whether an
// anonymous caller may reach the public read surface that #415 / #437 /
// #438 opened.
//
// ---------------------------------------------------------------------
// WHY THIS IS A DENY-LIST AND NOT AN ALLOWLIST
// ---------------------------------------------------------------------
//
// The obvious shape is an allowlist: reject every anonymous request
// except a named set, so a route added later is gated by default. That
// shape was specified, and it is the right instinct for a security
// gate. It is not implementable here, and the reason is worth writing
// down because it will be proposed again.
//
// An allowlist is only as safe as it is COMPLETE, and completeness
// cannot be established for this router:
//
//   - The route inventory it was derived from enumerates the routes
//     registered by hand in server.go. That is 22 routes. The generated
//     mux (openapi.HandlerFromMux) registers 263 more, and they are
//     invisible to that derivation.
//   - Among those 263 are POST /auth/login, GET /setup/status, POST
//     /setup/complete, GET /auth/providers, POST /auth/register, POST
//     /auth/verify-email and GET /appearance — every one of which must
//     serve anonymous callers or the install cannot be logged into,
//     completed, or rendered.
//   - There is no mechanical way to derive the set. openapi.yaml
//     declares no global `security:` default, and `security: []` is
//     present on only 9 operations. The spec is silent about the
//     anonymous-reachability of the other ~254, so the allowlist would
//     have to be hand-reasoned — the exact method that produced a route
//     table that was wrong in four places on its first pass.
//
// The failure modes are not symmetric. An allowlist that misses
// /auth/login locks every operator out of their own install with no
// recovery path short of a database shell. A deny-list that misses a
// public-surface route leaves that route reachable by anonymous callers
// — who still face the visibility predicate (ADR 0063) and the content
// checker (ADR 0064), and so can only ever see public-tier rows and
// public-tier bytes. One is unrecoverable; the other is the behaviour
// that already shipped.
//
// So: this list names the surface the toggle GOVERNS. Everything not
// named here is untouched by public mode and keeps whatever
// authentication its own handler already enforces — which for the
// ~254 unclassified operations is a 401, today, unchanged.
//
// The property the allowlist would have bought — "a new public route is
// gated by default" — is recovered at BUILD time instead of request
// time, by TestPublicSurfaceCoversAnonymousOperations: any operation
// that gains `security: []` in openapi.yaml without being named here
// fails the test. That is a stronger guarantee than the runtime one,
// because it fails in CI in front of the author rather than silently in
// production.
//
// ---------------------------------------------------------------------
// OVER-INCLUSION IS SAFE HERE; UNDER-INCLUSION IS THE RISK
// ---------------------------------------------------------------------
//
// Being named here means: rejected when the toggle is OFF, passed
// through to the handler when it is ON. "Passed through" is exactly
// what happens today with no toggle at all, so naming a route that did
// not need naming costs nothing and grants nothing — the handler's own
// authentication still runs either way. A prefix that sweeps in a write
// endpoint (POST /assets, POST /search/save-as-collection) therefore
// does NOT put that write in anonymous reach; it puts it back where it
// already is, behind its own 401.
//
// That is the inverse of the allowlist orientation, where a prefix
// sweeping in a write WOULD publish it. The concern was correct for
// that shape and does not carry over to this one.
type publicRoute struct {
	// path is matched against the request path with any "/api/v1"
	// prefix removed.
	path string
	// prefix matches path and everything beneath it. Used where the
	// route carries a path parameter ("/assets/{id}/file") and an
	// exact string cannot express it.
	prefix bool
	why    string
}

// PublicSurfaceRoutes is the governed set. Keep the `why` populated —
// this list is the reviewable record of what "public mode" means, and
// an unexplained entry is one nobody can audit.
var PublicSurfaceRoutes = []publicRoute{
	// The four read operations opened by #437 / #438. Prefixed rather
	// than exact because the item routes carry {id}, and because the
	// binary paths beneath them (/file, /variants/*, /archive/*) are
	// part of the same surface and must move together with it. A
	// public mode that lists assets but refuses their thumbnails is a
	// broken page, not a safer one.
	{path: "/assets", prefix: true, why: "asset list, item, bytes, variants and archive entries"},
	{path: "/collections", prefix: true, why: "collection list, item and contents"},

	// Public user-profile pages (#478 slice-1, ADR 0070). Only the by-*
	// read paths are opened — deliberately NOT the /users/{ref} prefix,
	// which would drag in the follow/followers/relationship/block
	// sub-routes (#462, out of scope). A profile is a display header plus
	// an owner-scoped browse of the (already-public) /assets + /collections
	// above; posts stay members-only, so an anonymous profile shows none.
	{path: "/users/by-username", prefix: true, why: "public profile page by username"},
	{path: "/users/by-ref", prefix: true, why: "public profile page by stable ref"},

	// Post-by-asset lookup (#478 slice-2, ADR 0070). Anonymous sees the
	// public posts featuring an asset; the handler filters to visibility
	// 'public' for anonymous callers. Scoped to /posts/by-asset only — the
	// rest of /posts stays members-only (not a public surface).
	{path: "/posts/by-asset", prefix: true, why: "public post-by-asset lookup"},

	// The featured rail (#417). This is the landing page for a public
	// install — with posts members-only, it is the only content an
	// anonymous visitor sees at `/`. Its own query composes the
	// visibility predicate, so gating here decides audience, not
	// access.
	{path: "/featured", prefix: true, why: "public featured rail"},

	// Search. Anonymous search is what makes a public install
	// browsable rather than merely linkable. The results are subject
	// to the same visibility predicate as everything else.
	{path: "/search", prefix: true, why: "search, facets, suggest, by-image"},

	// IIIF. Mounted twice — under /api/v1 by the group that carries
	// this middleware, and at the ROOT by the dual-mount block in
	// server.go, which mounts ResolveIdentity again for exactly that
	// purpose. Both mounts reach this gate, and the root mount is why
	// these entries are matched without the /api/v1 prefix as well.
	//
	// Defence in depth rather than a live hole: verified against a
	// running server, the IIIF handlers return their own 401 to
	// anonymous callers even with the toggle ON, so they are not
	// anonymous-capable today. They are listed anyway because the
	// moment somebody opens them — which is the natural next step
	// after opening the asset reads — the gate must already cover
	// them. An image API that returns pixels is exactly the surface
	// you do not want to discover was ungated after the fact.
	{path: "/iiif/2", prefix: true, why: "IIIF Image API 2.x redirect surface"},
	{path: "/iiif/3", prefix: true, why: "IIIF Image API 3 + Presentation manifests + content search"},
}

// NOT governed by this toggle, recorded so that "absent" and
// "deliberately absent" are distinguishable:
//
//   - /auth/*, /setup/*, /appearance, /build-info — must serve
//     anonymous callers in BOTH states or the install cannot be set up,
//     rendered, or logged into. This is the constraint that ranks above
//     the feature.
//   - /unsubscribe — token-authenticated and anonymous by design.
//     Gating it would 401 every unsubscribe link in every email already
//     sent, which is a compliance problem, not a bug report.
//   - /auth/saml/login, /acs, /metadata — SSO. /metadata is fetched by
//     the identity provider, which has no session to present.
//   - /openapi.json — publishes the API reference; carries no data.
//   - /federation/inbox, /inbox/batch — authenticated by HTTP signature
//     on the request itself, so a peer resolves as anonymous. These are
//     mounted at the ROOT router and deliberately outside the /api/v1
//     group's middleware (see the comment at their registration), so
//     they never reach this gate at all. Structurally safe, not
//     safe-by-omission.
//   - /healthz, /readyz — root router, outside /api/v1, never reach
//     this middleware.
//   - Every /admin/* route, and the ~254 other generated operations —
//     already 401 for anonymous callers via their own handler gate.
//     Naming them here would change nothing.

// IsPublicSurface reports whether a request path is governed by the
// public-mode toggle.
//
// The "/api/v1" prefix is stripped first so the same table serves both
// mounts of this middleware: the /api/v1 group and the root-level IIIF
// group. Matching on the request path rather than chi's route pattern
// is deliberate — inside a sub-router's middleware chain the pattern
// for the eventual endpoint has not been resolved yet, so
// RoutePattern() is empty here and would silently match nothing.
func IsPublicSurface(path string) bool {
	path = strings.TrimPrefix(path, "/api/v1")
	if path == "" {
		path = "/"
	}
	for _, r := range PublicSurfaceRoutes {
		if r.prefix {
			// Guard the segment boundary: "/assets" must match
			// "/assets" and "/assets/x" but not "/assetsomething".
			if path == r.path || strings.HasPrefix(path, r.path+"/") {
				return true
			}
			continue
		}
		if path == r.path {
			return true
		}
	}
	return false
}
