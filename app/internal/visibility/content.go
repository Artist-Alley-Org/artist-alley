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
) (bool, error) {
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
	if caps != nil && (caps(SystemAdmin) || caps(ContentReadAll)) {
		return true, nil
	}

	var (
		sensitivity string
		owner       *int64
		teamID      pgtype.UUID
	)
	err := pool.QueryRow(ctx,
		`SELECT sensitivity, owner_user_ref, team_id FROM assets WHERE id = $1`,
		assetID,
	).Scan(&sensitivity, &owner, &teamID)
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
	return ContentReadable(sensitivity, owner, caller, caps, member), nil
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
