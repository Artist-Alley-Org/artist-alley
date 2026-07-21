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
	if caps != nil && caps(SystemAdmin) {
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

	// Owner always reaches their own bytes, at any tier.
	//
	// Two guards, both load-bearing:
	//   - owner_user_ref is nullable; a NULL owner must never match.
	//   - the caller must not be anonymous. AnonymousCaller is the
	//     sentinel int64(0), so an anonymous caller carries UserRef 0.
	//     Without this check, an asset with owner_user_ref = 0 would
	//     match an anonymous caller AS ITS OWNER — at every tier,
	//     including embargo. No user has ref 0 on any install today,
	//     but that is data, not a structural guarantee, and #415 will
	//     relax the anonymous short-circuit above so that the public
	//     tier can serve bytes. When that happens this comparison
	//     becomes reachable by anonymous callers, and this guard is
	//     what keeps the sentinel from being an ownership claim.
	if !caller.IsAnonymous && owner != nil && *owner == caller.UserRef {
		return true, nil
	}

	switch sensitivity {
	case "public":
		return true, nil

	case "team":
		// Anonymous callers hold no team membership by definition, and
		// we must not run a membership lookup for the sentinel ref.
		if caller.IsAnonymous {
			return false, nil
		}
		// team_id is nullable. A team-tier asset with no team has no
		// members, so there is nobody to admit.
		if !teamID.Valid {
			return false, nil
		}
		var member bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM team_memberships
			      WHERE team_id = $1 AND user_ref = $2
			 )`,
			teamID, caller.UserRef,
		).Scan(&member); err != nil {
			return false, fmt.Errorf("visibility.CanReadContent: team membership: %w", err)
		}
		return member, nil

	case "restricted":
		// Owner and system.admin already returned above; nobody else.
		//
		// Access grants are NOT honoured here, deliberately (#434).
		// resource_request.requested_capability is free text with no
		// enum, no pattern and no validation, and it is chosen by the
		// REQUESTER — so honouring a granted value would turn an
		// attacker-supplied string into a privilege token. Do not
		// "complete" this branch by inventing a capability vocabulary;
		// #434 has to constrain the column first.
		return false, nil

	case "embargo":
		// Same as restricted for now. ADR 0020's embargo machinery
		// (release dates, per-asset allowlists) is Phase 1.28 and does
		// not exist, so there is no condition under which a non-owner
		// is admitted — deny rather than approximate it.
		return false, nil

	default:
		// Unrecognised tier: deny. A new sensitivity value must be an
		// explicit decision here, not something that silently inherits
		// public behaviour.
		return false, nil
	}
}
