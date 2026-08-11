// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// CanAssignToTeam answers "may this caller put a row they are creating
// into team T" — the one rule behind `PostCreate.team_id` (#954) and
// `AssetCreate.team_id` (#953).
//
// # Why it lives here, and why there is only one of it
//
// `team_id` looks like a label and behaves like a grant. It is an
// AUTHORIZATION INPUT on both tables it appears on:
//
//   - it decides who may MUTATE the row — posts.canMutatePost and
//     assets.canMutateAsset each consult a team-scoped admin grant, and
//     only when the row carries a team;
//   - on assets it also decides who may READ it at the `team`
//     sensitivity tier — [ContentReadable] is `case "team": return
//     isTeamMember`.
//
// Two call sites need the identical question, so it gets exactly one
// home (epic #665; #892 and #904 each spent a sprint deleting a second
// copy of a security rule). It sits beside [CanAttachAsset], the other
// write-side gate in this package, for the same reason that one does.
//
// # The rule
//
// A caller may assign to a team when ANY of:
//
//  1. they are a DIRECT MEMBER of it (`team_memberships`);
//  2. they hold a SCOPED admin grant over it — `scopedTeams` is the
//     caller's closure-expanded team set for the relevant code, which
//     only the auth package can produce (Identity.ScopedTeams);
//  3. they hold `system.admin`.
//
// # Membership is DIRECT; grants CLOSE over the hierarchy
//
// The asymmetry is deliberate and is the reason this function takes two
// separate inputs rather than one merged set.
//
// A scoped grant closes over `team_closure` because that is what a
// DELEGATED ADMINISTRATIVE right means under ADR 0010 Layer 5: an admin
// of a parent team administers its descendants. The resolver has
// already done that expansion, so `scopedTeams` arrives flat.
//
// Membership does not, because membership is not administration — it is
// "I am one of these people", and `team_memberships` records exactly
// that, with no closure anywhere. Closing it would also make assignment
// INCOHERENT with the read side it feeds: `is_team_member` is computed
// as a plain EXISTS against `team_memberships` in [ContentReadable],
// [ContentReadableSQL] and [FieldsColumnsSQL], with no closure walk. A
// member of a PARENT team allowed to assign into a DESCENDANT would be
// handing an audience they are not themselves in — putting their own
// asset somewhere they cannot read it at `sensitivity='team'`. Direct
// membership is also the narrower answer, so it fails closed, and
// widening it later is additive.
//
// # A soft-deleted team is not assignable, by anyone
//
// The FK does not look at `teams.deleted_at`, so without this the
// column would happily point at a deleted team. The liveness probe runs
// BEFORE the authorization disjunction — including before the
// `system.admin` short-circuit — so a deleted team is refused
// uniformly rather than being reachable by whoever happens to hold the
// wildcard.
//
// # A GLOBAL admin grant is deliberately NOT enough
//
// [Identity.ScopedTeams] excludes global holdings and the `system.admin`
// wildcard by design, and that exclusion is load-bearing here: a global
// `posts.admin` is the instance-moderator role, and #954 is precisely
// about a post appearing in a studio's space with nothing to do with
// it. Moderating everyone's posts is not a claim on any team's
// identity. `system.admin` remains the one escape hatch, checked
// explicitly.
//
// # Indistinguishability
//
// Returns (false, nil) for an unauthorised team, a nonexistent team, a
// soft-deleted team and the nil UUID alike — the four collapse into one
// boolean so the caller can answer them all with the SAME response.
// Callers MUST do so. Any difference turns the endpoint into a
// team-existence probe across every studio on the instance, the same
// discipline [CanAttachAsset] documents for assets.
func CanAssignToTeam(
	ctx context.Context,
	pool Pool,
	caller Caller,
	caps CapabilityChecker,
	scopedTeams []uuid.UUID,
	teamID uuid.UUID,
) (bool, error) {
	if teamID == uuid.Nil {
		return false, nil
	}
	// Anonymous is refused before the query rather than after: it can
	// satisfy none of the three disjuncts (the sentinel ref 0 matches no
	// membership row, and an anonymous identity holds no capability), so
	// the round trip would only ever confirm a foregone answer.
	if caller.IsAnonymous {
		return false, nil
	}

	// One round trip for both facts. NULLIF(...,0) keeps the anonymous
	// sentinel from matching a membership row for user 0 — the same
	// guard [ContentReadableSQL] carries, for the same reason.
	var live, member bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM teams t
		                WHERE t.id = $1 AND t.deleted_at IS NULL),
		       EXISTS (SELECT 1 FROM team_memberships tm
		                WHERE tm.team_id = $1
		                  AND tm.user_ref = NULLIF($2::BIGINT, 0))`,
		teamID, caller.UserRef,
	).Scan(&live, &member); err != nil {
		return false, fmt.Errorf("visibility.CanAssignToTeam: probe team: %w", err)
	}
	if !live {
		return false, nil
	}
	if member {
		return true, nil
	}
	if caps != nil && caps(SystemAdmin) {
		return true, nil
	}
	for _, t := range scopedTeams {
		if t == teamID {
			return true, nil
		}
	}
	return false, nil
}
