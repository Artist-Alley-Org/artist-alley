// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SystemAdmin is the wildcard capability that short-circuits every
// check in this package.
const SystemAdmin = "system.admin"

// ContentReadAll grants read access to asset BYTES at every sensitivity
// tier, and nothing else (#474). It is narrower than SystemAdmin by
// design: SystemAdmin is a wildcard that also unlocks every admin
// surface (system config, SMTP, federation keys, user PII), whereas
// ContentReadAll unlocks only the binary plane here in CanReadContent —
// no admin surfaces, no writes. It exists so a role like the public
// demo's demo-viewer can render every asset's derivatives across a
// mostly-restricted catalogue without being handed the wildcard. It is
// honoured ONLY at this content gate; holding it confers no other
// capability, because capability checks are independent — a caller holds
// exactly the codes granted to it.
const ContentReadAll = "content.read.all"

// AssetsAdmin lets a holder MUTATE assets that are not theirs —
// metadata edit, soft-delete, restore. Seeded by migration 00037 and
// enforced by assets.canMutateAsset; ADR 0010 Layer 5 makes it
// TEAM-SCOPED, so a grant on team X covers X and every descendant.
//
// It lives here, not in the assets package, because [FieldsReadable]
// consults it and `assets` imports this package — the dependency only
// runs one way. `assets` references THIS constant; there is deliberately
// no second declaration of the string.
//
// Per ADR 0064 (#939) holding it confers the FIELD plane and NOTHING
// else: see [FieldsReadable] for the disjunct and [PreviewReadable] for
// the plane it is deliberately absent from.
const AssetsAdmin = "assets.admin"

// CapabilityChecker answers "does this caller hold capability X".
// Declared as a func rather than an interface because auth.Identity.Can
// takes variadic options, so it does not satisfy a plain Can(string)
// interface — callers adapt it with a one-line closure at the call
// site, which also keeps this package free of a dependency on auth.
type CapabilityChecker func(code string) bool

// ContentPool is the subset of pgxpool.Pool CanReadContent uses.
type ContentPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ContentCaps is a CapabilityChecker resolved down to the only two
// questions the content plane ever asks it. It exists because a
// CapabilityChecker is a closure, and a closure cannot cross the two
// boundaries #899 needed it to cross: the search engine's per-caller
// CACHE KEY (cache.go) and any value that gets compared or logged.
//
// Resolve once at the HTTP edge, carry the struct, hand out a checker
// again with Checker(). Two booleans instead of an open-ended capability
// set is not a shortcut — [ContentReadable] consults exactly these two
// codes and nothing else, so a third code appearing here would have to
// be a deliberate edit in both places.
type ContentCaps struct {
	SystemAdmin    bool
	ContentReadAll bool
}

// ResolveContentCaps evaluates a CapabilityChecker down to the struct.
// A nil checker (anonymous) resolves to the zero value, which admits
// nothing.
func ResolveContentCaps(caps CapabilityChecker) ContentCaps {
	if caps == nil {
		return ContentCaps{}
	}
	return ContentCaps{
		SystemAdmin:    caps(SystemAdmin),
		ContentReadAll: caps(ContentReadAll),
	}
}

// Checker rebuilds a CapabilityChecker over the resolved answers. It
// reports false for every other code — this type is the content plane's
// view of a caller, never a general capability oracle, and using it as
// one would silently deny.
func (c ContentCaps) Checker() CapabilityChecker {
	return func(code string) bool {
		switch code {
		case SystemAdmin:
			return c.SystemAdmin
		case ContentReadAll:
			return c.ContentReadAll
		}
		return false
	}
}

// CacheKey is the stable string a per-caller cache must fold in
// alongside the user ref. Without it, a capability GRANT keeps serving
// the stale redacted result and — the direction that matters — a
// capability REVOKE keeps serving the stale UNREDACTED one for the rest
// of the TTL.
func (c ContentCaps) CacheKey() string {
	k := [2]byte{'0', '0'}
	if c.SystemAdmin {
		k[0] = '1'
	}
	if c.ContentReadAll {
		k[1] = '1'
	}
	return string(k[:])
}

// ContentReadableSQL is the SQL transcription of [ContentReadable], as
// a WHERE-clause conjunct (it starts with " AND "). `alias` is the
// assets table alias ("" for none) and `callerArg` is the placeholder
// holding the caller's user_ref.
//
// It exists for the AGGREGATE surfaces — the search facets and the
// suggest completions — which reduce many rows to one number or one
// string and so have no per-row Go step to decide readability in. Every
// other surface calls [ContentReadable] or [FieldsReadable] in Go, and
// should keep doing so; this is not the preferred form.
//
// Two expressions of one rule is exactly the defect ADR 0063 exists to
// prevent, so this one is held to the rule by
// TestContentReadableSQL_MatchesGo, which drives every
// (tier × owner × caller × caps × membership) combination through both
// and fails on the first disagreement. If you edit ContentReadable,
// that test tells you to edit this.
//
// The capability short-circuit folds to a constant TRUE rather than a
// bound parameter: the caller already resolved it, and a constant lets
// Postgres drop the rest of the disjunction.
func ContentReadableSQL(alias, callerArg string, caps ContentCaps) string {
	if caps.SystemAdmin || caps.ContentReadAll {
		return ""
	}
	return ` AND (` + contentReadableCoreSQL(columnPrefix(alias), callerArg) + `)`
}

// columnPrefix turns a table alias into the column-reference prefix the
// SQL builders in this package emit ("" for no alias, "a." for "a").
// One helper rather than the same three lines in each builder.
func columnPrefix(alias string) string {
	if alias == "" {
		return ""
	}
	return alias + "."
}

// contentReadableCoreSQL is the BODY of [ContentReadableSQL] — the bare
// disjunction, with no leading " AND " and no outer parentheses, so it
// can be composed into a wider expression.
//
// It is factored out because [FieldsReadableSQL] is [ContentReadableSQL]
// plus two more disjuncts, and transcribing the tier rule a second time
// inside it would put THREE expressions of the content plane in one
// package — the defect ADR 0063 exists to prevent, made worse by the
// fact that only one of the three has a TestXxxSQL_MatchesGo twin.
//
// Mirrors ContentReadable clause for clause:
//
//	owner match (never for the anonymous sentinel — an anonymous caller
//	carries ref 0, and the NULLIF makes ref 0 match nothing); public
//	admits everyone; team admits this asset's team; every other tier,
//	including unrecognised ones, denies.
func contentReadableCoreSQL(p, callerArg string) string {
	return p + `owner_user_ref = NULLIF(` + callerArg + `::BIGINT, 0)
	       OR ` + p + `sensitivity = 'public'
	       OR (` + p + `sensitivity = 'team' AND ` + p + `team_id IS NOT NULL AND EXISTS (
	            SELECT 1 FROM team_memberships tm
	             WHERE tm.team_id = ` + p + `team_id AND tm.user_ref = NULLIF(` + callerArg + `::BIGINT, 0)))`
}

// CanReadContent reports whether a caller may receive the BYTES of an
// asset (#433, ADR 0064).
//
// This is the enforcement point for the BINARY plane, and it is
// deliberately separate from the row predicate. Sensitivity gates
// CONTENT, not rows: ADR 0020 specifies that restricted and embargo
// assets stay listed — blurred, with a lock icon — so filtering them
// out of queries would contradict the accepted design and would move
// every predicate splice site. A caller can therefore legitimately see
// that an asset exists, and still be refused its bytes. That asymmetry
// is the design, not a bug.
//
// Every handler that streams asset bytes calls this, rather than each
// growing its own copy of the rule — six copies of a security check is
// six places for it to drift.
//
// Fails CLOSED on every uncertainty: unknown tier, lookup error,
// missing owner, or a team asset with no team.
//
// This does NOT re-check row visibility (soft-delete, publication).
// Callers reach it after their own identity guard, and row-level
// entitlement is the predicate's job.
func CanReadContent(
	ctx context.Context,
	pool ContentPool,
	caller Caller,
	caps CapabilityChecker,
	assetID uuid.UUID,
	mature MatureViewer,
) (bool, error) {
	// ⚠️ THE MATURE AXIS IS RESOLVED BELOW THE SHORT-CIRCUIT, NOT ABOVE
	// IT (#1116, ADR 0090 §1). `system.admin` is exempt and returns
	// early with the other admin arm; `content.read.all` is NOT, and
	// that separation is the whole reason this function grew a
	// parameter instead of an early return.
	//
	// `content.read.all` is a CLEARANCE capability — "admits the bytes
	// at EVERY tier", for the demo-viewer that has to render a
	// mostly-restricted catalogue. Mature is a RATING axis, and the two
	// are orthogonal by ADR 0090 §1. Letting a clearance capability
	// satisfy a rating conjunct scopes the gate to the caller's
	// STANDING rather than to the payload, which is #881's lesson, and
	// it would have had a live consequence rather than a theoretical
	// one: the public demo runs on exactly this capability, so every
	// mature asset on it would have streamed its bytes to anonymous
	// visitors while the feed correctly hid the rows.
	//
	// Anonymous callers are NOT rejected outright (#415): a public-tier
	// asset is readable by anyone, which is what public mode means.
	// They resolve against the sensitivity tier alone and never reach
	// the ownership comparison below — see the !IsAnonymous guard there,
	// which stops the AnonymousCaller sentinel (int64 0) from matching
	// an asset owned by ref 0.
	// SystemAdmin (wildcard) OR ContentReadAll (binary-plane-only, #474)
	// short-circuits the sensitivity resolution below. ContentReadAll
	// admits the bytes at EVERY tier — restricted, team, embargo — which
	// is the point: a demo-viewer must render a mostly-restricted
	// catalogue. It grants nothing beyond these bytes; see its doc.
	if caps != nil && caps(SystemAdmin) {
		return true, nil
	}

	var (
		sensitivity string
		owner       *int64
		teamID      pgtype.UUID
		isMature    bool
	)
	err := pool.QueryRow(ctx,
		`SELECT sensitivity, owner_user_ref, team_id, mature FROM assets WHERE id = $1`,
		assetID,
	).Scan(&sensitivity, &owner, &teamID, &isMature)
	if err != nil {
		// Includes pgx.ErrNoRows: an asset we cannot read is an asset
		// whose bytes we do not hand out.
		return false, fmt.Errorf("visibility.CanReadContent: load asset: %w", err)
	}

	// Team-tier assets need a membership lookup; every other tier is
	// decided from the row fields alone. Resolve membership here — the
	// one place holding the pool — then hand the boolean to
	// ContentReadable, the SINGLE expression of the rule. The browse
	// list-query path (#471) shares that same core, joining membership
	// into its query instead of a per-asset lookup.
	member := false
	if sensitivity == "team" && !caller.IsAnonymous && teamID.Valid {
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM team_memberships
			      WHERE team_id = $1 AND user_ref = $2
			 )`,
			teamID, caller.UserRef,
		).Scan(&member); err != nil {
			return false, fmt.Errorf("visibility.CanReadContent: team membership: %w", err)
		}
	}
	// Both axes, ANDed, never merged (ADR 0090 §1). The clearance
	// answer is unchanged — ContentReadable still owns it, including
	// the `content.read.all` arm that no longer short-circuits above.
	if !ContentReadable(sensitivity, owner, caller, caps, member) {
		return false, nil
	}
	isOwner := owner != nil && *owner == caller.UserRef
	return MatureItemVisible(mature, isMature, isOwner, caps != nil && caps(SystemAdmin)), nil
}

// ContentReadable is the query-free core of the binary-plane rule (ADR
// 0064): given a row's already-resolved sensitivity + owner + a
// pre-computed team-membership answer, decide whether the caller may
// receive the bytes. CanReadContent (which loads the row + membership
// itself) and the browse list query (which joins them) both call this,
// so the rule has exactly one home — the same argument as ADR 0063's
// predicate. isTeamMember MUST already fold in "the asset is team-tier
// AND the caller is a member of THIS asset's team"; it is consulted only
// for the team tier.
//
// Guards, both load-bearing:
//   - owner_user_ref is nullable; a NULL owner must never match.
//   - the caller must not be anonymous. AnonymousCaller is the sentinel
//     int64(0), so an anonymous caller carries UserRef 0. Without the
//     !IsAnonymous guard, an asset with owner_user_ref = 0 would match an
//     anonymous caller AS ITS OWNER at every tier. No user has ref 0
//     today, but that is data, not a structural guarantee.
//
// restricted / embargo / any unrecognised tier deny: a new sensitivity
// value must be an explicit decision here, never a silent inherit of
// public. Access grants are deliberately NOT honoured (#434): the
// requested_capability column is unvalidated requester-supplied text.
func ContentReadable(
	sensitivity string,
	owner *int64,
	caller Caller,
	caps CapabilityChecker,
	isTeamMember bool,
) bool {
	// SystemAdmin (wildcard) OR ContentReadAll (binary-plane-only, #474)
	// admits the bytes at every tier.
	if caps != nil && (caps(SystemAdmin) || caps(ContentReadAll)) {
		return true
	}
	// Owner always reaches their own bytes, at any tier.
	if !caller.IsAnonymous && owner != nil && *owner == caller.UserRef {
		return true
	}
	switch sensitivity {
	case "public":
		return true
	case "team":
		return isTeamMember
	default:
		return false
	}
}
