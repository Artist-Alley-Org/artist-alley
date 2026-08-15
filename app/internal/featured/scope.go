// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package featured

import "github.com/mscrnt/artist-alley/app/internal/visibility"

// Scope values for featured_items.scope, mirroring
// featured_items_scope_check (migration 00010). Named here so the
// readers, the writer and the tests all spell them the same way.
const (
	// ScopePublic is the audience that includes anonymous visitors.
	ScopePublic = "public"
	// ScopeOrg is the internal signed-in audience and the default for
	// a curation write (ADR 0065).
	ScopeOrg = "org"
	// ScopeTeam is modelled by the table and is NOT writable through
	// the API. See AddInput.Scope for why.
	ScopeTeam = "team"
)

// ScopeVisibleSQL renders the featured_items AUDIENCE predicate for one
// caller (#1104).
//
// # What scope means, and what it does not
//
// `scope` models WHO MAY SEE a placement. It does NOT select WHICH
// SURFACE renders one. Before #1104 every reader had picked a scope by
// hand and the two choices disagreed: the browse rail read `public`
// only, the collections hub's Featured tab read `org` only, and the
// admin write path produced `org` only — so no row could ever satisfy
// both surfaces and the hub tab was permanently empty on an install
// whose placements came from the seed (all `public`). That is the whole
// bug; the fix is that a reader shows every audience its VIEWER
// qualifies for, from this one expression.
//
//   - anonymous → `public` only. Byte-identical to what the rail asked
//     for before #1104, deliberately: the rail is (or will be) served
//     signed-out, and widening the anonymous arm would put internal
//     placements in front of logged-out readers. That is a visibility
//     decision, and #1104 does not make it.
//   - signed in → `org` + `public`. `public` is a SUPERSET audience,
//     not a different surface, so a signed-in reader qualifies for it
//     too. This is the arm that was missing.
//
// `team` is never returned by this predicate. A team placement's
// audience is one team, which needs a membership test rather than a
// scope test; the signed-in teams rail resolves those through the teams
// package (teams/featured.go), and nothing else reads them. Adding
// `team` here would hand every signed-in caller every team's placements.
//
// # Why literals rather than placeholders
//
// The house discipline (ADR 0063) is that caller data binds as
// arguments. Nothing caller-derived appears in this fragment: the
// branch is taken in Go on [visibility.Caller.IsAnonymous], and both
// arms are closed sets of constants drawn from the CHECK constraint. A
// bound $n would add splice bookkeeping to three call sites to
// parameterise a value no caller can influence. The predicate stays a
// string, and the sqlc readers that cannot splice Go at all (the
// collections parity oracle, the teams rail) are pinned to it by
// TestScopeVisibleSQL_PinnedInStaticQueries rather than trusted to
// match.
//
// The returned fragment is a bare boolean expression with no leading
// AND and no surrounding parentheses beyond its own — callers place it
// in a WHERE or an EXISTS as they see fit.
func ScopeVisibleSQL(alias string, caller visibility.Caller) string {
	if caller.IsAnonymous {
		return alias + ".scope = '" + ScopePublic + "'"
	}
	return alias + ".scope IN ('" + ScopeOrg + "', '" + ScopePublic + "')"
}
