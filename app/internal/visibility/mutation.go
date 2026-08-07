// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// AssetMutationCaps is the caller's resolved answer to "may I mutate an
// asset owned by team T" — the third capability the FIELD plane
// consults, added by #939 / ADR 0064.
//
// # Why a resolved value and not a CapabilityChecker
//
// Same reason [ContentCaps] exists: this answer has to cross two
// boundaries a closure cannot. It reaches the search engine's per-caller
// CACHE KEY (search/cache.go), and it is carried on the search Query
// value rather than re-derived per row.
//
// # Why it is NOT folded into ContentCaps
//
// [ContentCaps] is two booleans and its CacheKey is two bytes, because
// the two codes it answers for are GLOBAL. `assets.admin` is
// team-scoped, so the honest resolved form is a SET of team IDs, and
// widening ContentCaps to carry one would change the meaning of a type
// whose whole contract is "the content plane's view of a caller".
// A separate type keeps ContentCaps at two booleans and keeps the
// mutation right visibly on its own plane.
//
// # Why the team set is resolved in Go and not recomputed in SQL
//
// Deciding this with an EXISTS against `user_capability_grants` in the
// SELECT list would be a SECOND expression of the capability resolver —
// the defect ADR 0063 exists to prevent — and it would be a WRONG one.
// auth.EffectiveScopedCapabilitiesForUser resolves a scoped capability
// from FOUR inputs, not one:
//
//   - `user_capability_grants` scoped by team_id;
//   - `role_capabilities` reached through a RECURSIVE walk of
//     `roles.parent_id`, carrying the `user_roles.team_id` of the
//     assignment that seeded the walk — this is the ordinary way an
//     operator confers a capability, and a grants-only EXISTS misses it
//     entirely and silently;
//   - `user_capability_revokes`, subtracted at the exact
//     (code, team_id) pair with NULLs-not-distinct, BEFORE expansion;
//   - `team_closure`, which fans the survivors out to descendant teams.
//
// The resolver already did all four, at request time, into
// Identity.scopedCaps. Reading its answer is one map iteration;
// re-deriving it is a security rule with two expressions and no
// TestXxxSQL_MatchesGo twin to hold them together.
type AssetMutationCaps struct {
	// Global is a GLOBAL `assets.admin` (or the `system.admin`
	// wildcard) — a right over every asset regardless of team.
	Global bool

	// Teams is the closure-expanded set of teams the caller holds a
	// SCOPED `assets.admin` over, sorted so [AssetMutationCaps.CacheKey]
	// is stable. Never contains the nil UUID.
	Teams []uuid.UUID
}

// ResolveAssetMutationCaps evaluates a caller down to the struct.
// `caps` answers the two global codes; `scopedTeams` is the caller's
// closure-expanded team set for [AssetsAdmin], which only the auth
// package can produce (Identity.ScopedTeams).
//
// A nil checker with no teams (anonymous) resolves to the zero value,
// which permits nothing — the zero value fails CLOSED, which is what
// makes it safe for a surface that has not been wired yet to simply not
// set it.
func ResolveAssetMutationCaps(caps CapabilityChecker, scopedTeams []uuid.UUID) AssetMutationCaps {
	m := AssetMutationCaps{}
	if caps != nil {
		m.Global = caps(SystemAdmin) || caps(AssetsAdmin)
	}
	if len(scopedTeams) > 0 {
		m.Teams = make([]uuid.UUID, 0, len(scopedTeams))
		for _, t := range scopedTeams {
			if t == uuid.Nil {
				continue
			}
			m.Teams = append(m.Teams, t)
		}
		sort.Slice(m.Teams, func(i, j int) bool {
			return bytes.Compare(m.Teams[i][:], m.Teams[j][:]) < 0
		})
	}
	return m
}

// MayMutate reports whether the caller may mutate an asset owned by
// this team. `teamID` is the asset's `team_id`, nil when the asset has
// none.
//
// A TEAM-LESS asset has no scope for a scoped grant to match, so a
// scoped holder gets nothing from it — it must never fall back to "no
// scope required, therefore anyone passes". This mirrors
// assets.canMutateAsset's handling of the same trap exactly; the two
// are the same rule seen from the read side and the write side.
//
// Ownership is deliberately NOT considered here. The owner already
// reaches their own asset through [FieldsReadable]'s own owner branch,
// and folding it in twice would put the anonymous-sentinel trap
// (UserRef 0) in a second place.
func (m AssetMutationCaps) MayMutate(teamID *uuid.UUID) bool {
	if m.Global {
		return true
	}
	if teamID == nil || *teamID == uuid.Nil {
		return false
	}
	for _, t := range m.Teams {
		if t == *teamID {
			return true
		}
	}
	return false
}

// CacheKey is the stable string a per-caller cache must fold in
// alongside the user ref and [ContentCaps.CacheKey].
//
// # Why this is folded in when is_team_member is not
//
// The obvious precedent — `is_team_member`, computed per query in SQL —
// is NOT in any cache key, so leaving a team does not evict a cached
// result today. Matching that precedent here was the cheaper option and
// it was rejected, because the cost of closing it is nil:
//
// keyForQuery ALREADY includes the caller's user_ref, so every caller
// has its own cache entries regardless. Adding per-caller capability
// state therefore causes ZERO extra fragmentation between callers — it
// only invalidates that one caller's entries, and only when that
// caller's own grants change, which is rare. There is no hit-rate
// argument for leaving it open.
//
// The direction that matters is REVOKE. Without this, a holder stripped
// of `assets.admin` would keep being served the cached FIELDS of a
// restricted asset — titles, descriptions, tags — for the rest of the
// TTL. That is precisely the disclosure the read rule refuses, so it is
// closed at the key rather than documented as acceptable.
//
// (The `is_team_member` instance is real and remains open; it predates
// this and closing it needs the caller's TEAM set at the HTTP edge,
// which is a different change. Recorded here so the next sweep finds it
// already known rather than re-deriving it.)
func (m AssetMutationCaps) CacheKey() string {
	if !m.Global && len(m.Teams) == 0 {
		return "0"
	}
	var sb strings.Builder
	if m.Global {
		sb.WriteString("1")
	} else {
		sb.WriteString("0")
	}
	sb.WriteByte(':')
	sb.WriteString(strconv.Itoa(len(m.Teams)))
	for _, t := range m.Teams {
		sb.WriteByte(':')
		sb.WriteString(t.String())
	}
	return sb.String()
}
