// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package teams

// Team follows — the teams-rail bookmark model (#577).
//
// # A follow is a bookmark
//
// Nothing in this file participates in authorization. Following a
// studio does not make the caller a member of it, does not widen a
// single row of what they can see of its work, and is never consulted
// by the visibility planes. What it does is put the team in the
// caller's teams rail — and, since #1048, select on the browse
// feed's "Following" filter, which is the same bookmark read as a
// display preference: a NARROWING conjunct ANDed beside the read rule
// (posts/list_page.go), never a disjunct with it, so it can only
// remove rows from a page the caller could already see.
//
// That is worth stating in code because the table sits one join away
// from team_memberships, which IS an authorization table, and the two
// look alike. They are not alike. If a future change wants to read
// team_follows in a read rule, that is a product decision about
// granting access by subscription, and it needs its own argument — it
// must not arrive as a convenient join.
//
// # No counts, no unread, no notifications
//
// Deliberately absent, per the sprint's decisions: no denormalised
// follower count, no last-read watermark, no notification fanout.
// A follow is a bookmark, not a subscription; #520's rules arc owns
// notifications.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// FollowTeam bookmarks a team into the caller's teams rail.
//
// # Liveness is probed explicitly, and it has to be
//
// The team_id FK references teams(id), and team deletion in this API
// is SOFT — the row stays, `deleted_at` is set. So the FK is satisfied
// by a tombstoned team and would happily accept the insert. The probe
// below is the only thing standing between "this studio was deleted"
// and a permanent entry in somebody's sidebar. This is the same trap
// visibility.CanAssignToTeam documents at length (#955); the fix is the
// same, and it is a probe rather than a constraint because a
// constraint cannot see deleted_at.
//
// # Nonexistent and soft-deleted are ONE answer
//
// Both fail the probe and both return the same 404 with the same body.
// They must stay indistinguishable. Any difference — a different
// status, a different message, even a different latency profile from
// an extra query — turns this endpoint into an oracle that enumerates
// every studio on the instance, one UUID guess at a time. Note that
// there is deliberately no separate "does this team exist" query for
// the two cases to disagree about: one EXISTS, one boolean, one
// refusal.
//
// # Idempotent
//
// A team already followed is a 204, not a 409. The insert is ON
// CONFLICT DO NOTHING, so the second press of a double-tapped button
// and a retried request produce the outcome the caller asked for.
func (h *Handler) FollowTeam(
	ctx context.Context,
	req openapi.FollowTeamRequestObject,
) (openapi.FollowTeamResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.FollowTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.FollowTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)

	live, err := q.IsTeamLive(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("teams: follow liveness probe: %w", err)
	}
	if !live {
		return openapi.FollowTeam404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
		}, nil
	}

	if err := q.FollowTeam(ctx, FollowTeamParams{
		UserRef: caller.UserRef,
		TeamID:  pgID,
	}); err != nil {
		return nil, fmt.Errorf("teams: follow: %w", err)
	}
	return openapi.FollowTeam204Response{}, nil
}

// UnfollowTeam drops the caller's bookmark.
//
// No liveness probe and no 404 branch, unlike FollowTeam. The
// asymmetry is intentional: FOLLOW writes a row that must point at a
// live team, so it has to check; UNFOLLOW only ever removes the
// caller's own row, and the row most in need of removing is precisely
// the one pointing at a team that has since been deleted. Probing here
// would strand that row in the user's rail forever.
//
// Idempotent for the same reason follow is: unfollowing something you
// do not follow has already achieved what you asked for.
func (h *Handler) UnfollowTeam(
	ctx context.Context,
	req openapi.UnfollowTeamRequestObject,
) (openapi.UnfollowTeamResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UnfollowTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.UnfollowTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}

	if _, err := New(h.Pool).UnfollowTeam(ctx, UnfollowTeamParams{
		UserRef: caller.UserRef,
		TeamID:  pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("teams: unfollow: %w", err)
	}
	return openapi.UnfollowTeam204Response{}, nil
}

// GetMyFollowedTeams returns the caller's teams rail.
//
// Distinct from GetMyTeams, which returns MEMBERSHIPS. The two are
// different questions with different answers and they stay separate
// endpoints so a bookmark is never mistaken for an authorization fact
// by whatever reads them.
func (h *Handler) GetMyFollowedTeams(
	ctx context.Context,
	req openapi.GetMyFollowedTeamsRequestObject,
) (openapi.GetMyFollowedTeamsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetMyFollowedTeams401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.GetMyFollowedTeams403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}

	rows, err := New(h.Pool).ListFollowedTeams(ctx, caller.UserRef)
	if err != nil {
		return nil, fmt.Errorf("teams: list followed teams: %w", err)
	}
	items, err := h.teamsToAPI(ctx, rows)
	if err != nil {
		return nil, err
	}
	return openapi.GetMyFollowedTeams200JSONResponse(items), nil
}
