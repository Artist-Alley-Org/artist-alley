// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import "context"

// The request-scoped mature viewer (#1116, ADR 0090).
//
// # Why this is on the CONTEXT and not a parameter
//
// [MatureViewer]'s own doc says the three inputs are "resolved once at
// the HTTP edge and carried". This file is the carrying.
//
// The alternative — threading a MatureViewer through every signature
// between the edge and a predicate — was tried first and rejected on
// what it did to the CONTENT plane specifically. `CanReadContent` is
// called from four byte handlers, an IIIF tile server, the similar-assets
// endpoint and two asset endpoints, and `CanSeeAssetContent` from three
// more packages. Each would have needed its own resolver field, its own
// setter, its own nil check and its own wiring line — ten copies of
// "what happens when this is missing", which is ten chances for one of
// them to answer "widen".
//
// Here there is exactly one answer, and it is [MatureFromContext]'s:
// ABSENT MEANS DISQUALIFIED. A handler that was never wired, a test that
// builds a bare context, a code path that predates this arc — all of
// them get the zero value, and the zero value of MatureViewer is the
// viewer who qualifies for nothing.
//
// # What this does NOT do
//
// It does not make the value ambient policy. Call sites still take the
// viewer as an explicit argument to the predicate; the context is how it
// reaches them, not where they read it from mid-rule. Nothing in this
// package consults the context inside a predicate — [MatureItemVisible]
// and [MatureFilterSQL] remain pure functions of their arguments, which
// is what lets the table test drive them exhaustively.
type matureCtxKey struct{}

// WithMatureViewer carries a resolved viewer on the request context.
// Called ONCE, by the HTTP middleware, after identity resolution.
func WithMatureViewer(ctx context.Context, v MatureViewer) context.Context {
	return context.WithValue(ctx, matureCtxKey{}, v)
}

// MatureFromContext reads the request's resolved viewer.
//
// ⚠️ AN ABSENT VALUE IS THE DISQUALIFIED VIEWER, not an error and not a
// permissive default. This is the single place that decision is made for
// the content plane, and it is the same direction
// [ResolveMatureOr] takes for the row plane: a gate that has lost its
// inputs must refuse rather than widen.
//
// The consequence worth stating plainly, because it is the one that will
// confuse someone: a handler reached through a route that skips the
// middleware serves NO mature content to anybody except owners and
// admins. That is a visible bug — a reader reports "I opted in and still
// cannot see it" — and it is the failure mode chosen over the invisible
// one, where an un-wired route quietly serves everything to everyone.
func MatureFromContext(ctx context.Context) MatureViewer {
	if v, ok := ctx.Value(matureCtxKey{}).(MatureViewer); ok {
		return v
	}
	return AnonymousMatureViewer
}
