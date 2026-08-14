// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package teams

// The operator-curated featured-team slot in the teams rail (#1084).
//
// # Why this lives in the teams package and not in `featured`
//
// It reads featured_items, but what it RETURNS is a team, and a team is
// only correct once its hero has been re-derived. That re-derivation —
// attachHeroes / the TeamHeroes query (#982, migration 00047) — is a
// teams concern with a teams-package cache interaction, and exporting it
// so another package could remember to call it is precisely the shape of
// mistake it exists to prevent. So the placement table is read from here
// and the answer goes out through teamsToAPI like every other list of
// teams in the product.
//
// The featured package keeps ownership of curation WRITES; nothing about
// featuring is duplicated here. Adding a team to the list is
// POST /admin/featured with subject_kind=team, gated on system.admin by
// featured/http.go. No new capability was invented for this, and none
// should be: this is placement, and system.admin already owns placement.
//
// # A placement is not a grant
//
// Featuring a team must not reveal a team, or a team's picture, to
// anyone who could not already see it. Three things hold that line, and
// none of them is a restatement of a rule that lives elsewhere:
//
//	the capability gate — teams.read, the same one GetMyFollowedTeams
//	and the /teams directory hold. A caller who may not read teams gets
//	403 here too, featured or not.
//
//	the JOIN — ListFeaturedTeams inner-joins live teams, so a
//	soft-deleted team drops out of the curation list on its own. Worth
//	being explicit about what "cannot see" means for a team today:
//	teams have NO per-viewer visibility predicate in this codebase.
//	There is no visibility.EntityTeam and no private-team column, so a
//	team is readable by every teams.read holder and hidden from
//	everyone only by being tombstoned. That is the negative control this
//	endpoint can actually have, and when per-team visibility arrives the
//	JOIN is where it lands.
//
//	the hero re-check — the picture comes from TeamHeroes, which
//	demands the asset still be public AND still belong to that team.
//	A featured team whose hero has since been set to `restricted` falls
//	back to its initials tile here, exactly as it does in the follows
//	rail, because it is the same code path rather than a copy of it.

import (
	"context"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ListFeaturedTeams serves GET /featured/teams — the curated slot that
// renders first in the teams rail.
//
// Returns an empty array, not 404, when nothing is curated: "no team is
// featured" is the normal state of a default install, and the rail draws
// its followed teams as usual.
func (h *Handler) ListFeaturedTeams(
	ctx context.Context,
	_ openapi.ListFeaturedTeamsRequestObject,
) (openapi.ListFeaturedTeamsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListFeaturedTeams401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.ListFeaturedTeams403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}

	rows, err := New(h.Pool).ListFeaturedTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("teams: list featured teams: %w", err)
	}
	// teamsToAPI, not a local mapper: it is what runs attachHeroes, and
	// a hand-rolled conversion here would ship teams.hero_asset_id — the
	// STORED pointer — which is only as true as the moment it was
	// written.
	items, err := h.teamsToAPI(ctx, rows)
	if err != nil {
		return nil, err
	}
	return openapi.ListFeaturedTeams200JSONResponse(items), nil
}
