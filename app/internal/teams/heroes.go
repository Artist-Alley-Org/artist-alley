// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package teams

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The team hero picture — selection gate + render-time re-check (#982,
// #1147, migration 00047, ADR 0088/0090)
// ---------------------------------------------------------------------------
//
// # Why these two queries left sqlc
//
// Both were plain sqlc queries until #1147. A team's hero is a DERIVED
// PICTURE — the team row holds a pointer and the client paints that
// asset's `col` rendition — so it has to compose the mature axis, and
// the axis is a per-viewer SQL fragment that folds to nothing for a
// qualified reader. sqlc has no way to express a conjunct that is
// sometimes absent, and the alternatives are worse than moving the SQL
// here: a constant TRUE would make Postgres reason about a filter on the
// common path, and a second Go pass over the rows would be a second
// expression of a rule ADR 0063 says must have one.
//
// The generated `TeamHeroes` / `TeamHeroCandidate` were DELETED rather
// than left beside these, because an ungated query with the obvious name
// sitting next to the gated one is the trap that produced #1147 in the
// first place — the leak was never a wrong rule, it was a caller
// reaching a construction that had never heard of the rule.
//
// # The two are deliberately NOT the same query
//
// The selection gate does not require a stored object or a `col`
// rendition and the render-time re-check does. Renditions are produced
// asynchronously, so refusing a just-uploaded asset would hand the admin
// an error they cannot act on, while the read path falls back to the
// initials tile until the rendition lands. That asymmetry predates this
// file; see collections.CallerMayPictureAsset vs ComposeCovers, which
// makes the identical trade for the identical reason.

// heroMatureFrag is the mature conjunct both queries splice, over the
// `a` alias.
//
// The owner ref is rendered as a LITERAL rather than bound as `$n`, the
// same choice collections.ComposeCovers makes and for the sharper of its
// two reasons: [visibility.MatureFilterSQL] returns the EMPTY STRING for
// a qualified viewer and for a system admin, so a placeholder naming the
// ref would be bound and never referenced — and Postgres answers that
// with 42P18 ("could not determine data type of parameter"), an outright
// failure on every request by exactly the readers who qualify. A literal
// has no such arm. The value is an int64 the auth resolver produced
// inside this process, not caller text.
//
// ⚠️ ZERO IS THE ANONYMOUS SENTINEL, not a fallback: MatureFilterSQL
// wraps the argument in `NULLIF(…, 0)` so an anonymous reader cannot
// match an asset whose owner column holds 0 AS ITS OWNER.
func heroMatureFrag(caller visibility.Caller, mature visibility.MatureViewer, isAdmin bool) string {
	return visibility.MatureFilterSQL("a", visibility.MatureOwnerColAsset,
		strconv.FormatInt(caller.UserRef, 10), mature, isAdmin)
}

// heroViewerFromContext resolves the three caller-side inputs both hero
// queries take, in ONE place so the selection gate and the render-time
// re-check cannot pick up different answers about the same person.
//
// ⚠️ visibility.MatureFromContext returns the DISQUALIFIED viewer when
// the middleware never ran — see its doc for why the visible failure
// ("I opted in and the hero still doesn't show") was chosen over the
// invisible one.
//
// The admin waiver is `system.admin` and NOT `teams.admin`: ADR 0090's
// exemption exists so a moderator can see what the instance switch hid,
// and CapTeamsAdmin is an administrative scope over a team's membership
// and settings, not a clearance for content ratings. Widening it here
// would be #881's lesson — scoping a gate to its principal's standing
// rather than to its payload.
func heroViewerFromContext(ctx context.Context) (visibility.Caller, visibility.MatureViewer, bool) {
	mature := visibility.MatureFromContext(ctx)
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.UserRef == 0 {
		return visibility.NewCaller(nil), mature, false
	}
	ref := id.UserRef
	return visibility.NewCaller(&ref), mature, id.Can(CapSystemAdmin)
}

// TeamHeroesRow is one team's re-derived hero pointer.
type TeamHeroesRow struct {
	TeamID      pgtype.UUID
	HeroAssetID pgtype.UUID
}

// TeamHeroesGated is the RENDER-time re-check, batched over one page of
// teams, for one viewer.
//
// THIS IS THE HALF THAT GETS FORGOTTEN, so it is worth being blunt about
// why it exists. SetTeamHero validated the asset when it was chosen. That
// says nothing about now: an asset that is public today can be set to
// 'restricted' tomorrow, or moved to another team, or soft-deleted, and
// none of those touch the teams row. Re-deriving the answer on every read
// is what makes the hero DROP OUT and fall back to the initials tile
// instead of lingering in a strip that anonymous readers can see.
//
// A team whose hero no longer qualifies simply returns no row, which the
// caller reads as "no hero" — the same outcome as never having set one.
// The pointer itself is left alone, so restoring the asset's sensitivity
// brings the picture back without the admin re-picking it. #1147 makes
// that property carry the mature axis too, and it is the reason a mature
// hero costs nobody a broken tile: withheld and never-set are one answer
// here, so a disqualified reader gets the initials tile that every
// heroless team already gets.
//
// The two extra conditions the write side does not impose — a stored
// object and a `col` rendition — are renderability rather than
// permission: without them the client would paint a broken image.
//
// # It is now PER-VIEWER, and that is safe where it is called
//
// Before #1147 this answered the same for everybody, which is why
// `attachHeroes` could get away with anything. It cannot now — but the
// enrichment already ran strictly AFTER the by-id cache, for the
// staleness reason attachHeroes documents, and ADR 0013's amendment
// wants a per-viewer value computed exactly there. The two requirements
// happen to want the same placement; nothing moved.
func TeamHeroesGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	teamIDs []pgtype.UUID,
	caller visibility.Caller,
	mature visibility.MatureViewer,
	isAdmin bool,
) ([]TeamHeroesRow, error) {
	sql := `SELECT t.id AS team_id, a.id AS hero_asset_id
  FROM teams t
  JOIN assets a ON a.id = t.hero_asset_id
 WHERE t.id = ANY($1::UUID[])
   AND a.team_id = t.id
   AND a.sensitivity = 'public'
   AND a.deleted_at IS NULL
   AND a.file_hash IS NOT NULL
   AND EXISTS (SELECT 1 FROM storage_variants sv
                WHERE sv.object_hash = a.file_hash
                  AND sv.variant_key = 'col')` + heroMatureFrag(caller, mature, isAdmin)

	rows, err := pool.Query(ctx, sql, teamIDs)
	if err != nil {
		return nil, fmt.Errorf("teams: hero re-check: %w", err)
	}
	defer rows.Close()
	var items []TeamHeroesRow
	for rows.Next() {
		var i TeamHeroesRow
		if err := rows.Scan(&i.TeamID, &i.HeroAssetID); err != nil {
			return nil, fmt.Errorf("teams: hero re-check scan: %w", err)
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: hero re-check rows: %w", err)
	}
	return items, nil
}

// TeamHeroCandidateGated is the SELECTION-time admissibility gate: may
// this asset be this team's hero?
//
// The rule narrowed out of ADR 0088 in migration 00047, stated once: the
// asset is PUBLIC and it BELONGS TO THIS TEAM. Both halves are load
// bearing — see the migration for why either alone is wrong.
//
// One boolean for "no such asset" and "not admissible" together: telling
// them apart would make the endpoint an existence oracle for asset ids.
//
// # Why the mature axis is on the WRITE side too (#1147)
//
// Not because picking a mature hero would leak anything — the re-check
// above is what decides, and it now withholds per viewer. It is the
// oracle sentence one paragraph up, which the axis would otherwise
// undo: a disqualified admin cannot see their own team's mature assets
// in any listing (assets.ListAssetsPageGated drops the rows), so a gate
// that answered "admissible" for one would let them confirm by id
// exactly the fact every listing withholds from them.
//
// It costs an artist nothing: the owner exemption lives inside
// MatureFilterSQL, so an admin may always point at their OWN asset.
func TeamHeroCandidateGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	assetID uuid.UUID,
	teamID pgtype.UUID,
	caller visibility.Caller,
	mature visibility.MatureViewer,
	isAdmin bool,
) (bool, error) {
	sql := `SELECT EXISTS (
    SELECT 1 FROM assets a
     WHERE a.id = $1
       AND a.team_id = $2
       AND a.sensitivity = 'public'
       AND a.deleted_at IS NULL` + heroMatureFrag(caller, mature, isAdmin) + `)`
	var admissible bool
	if err := pool.QueryRow(ctx, sql, assetID, teamID).Scan(&admissible); err != nil {
		return false, fmt.Errorf("teams: hero candidate: %w", err)
	}
	return admissible, nil
}
