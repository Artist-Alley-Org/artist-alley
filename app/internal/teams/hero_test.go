// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #982 — the team hero picture, driven through the real handlers.
//
// # What makes this file worth reading
//
// The write-side refusals are the easy half. Any implementation that
// checks `sensitivity = 'public' AND team_id = $team` before it writes
// passes TestTeamHero_NonPublicAssetIsRefused and
// TestTeamHero_AnotherTeamsAssetIsRefused.
//
// The half that would silently not work is
// TestTeamHero_FlipToRestrictedDropsOut and its cached twin
// TestTeamHero_FlipToRestrictedDropsOutThroughTheCache. A selection-time
// check is a note about the past: it says the asset qualified on the day
// an admin picked it. Nothing about editing an asset's sensitivity
// touches the teams row, and nothing invalidates the team cache, so an
// implementation that trusts the stored pointer goes on painting a
// no-longer-public picture into a navigation strip that anonymous
// readers can see — and passes every other test here.
//
// The cached variant is not a duplicate. fetchTeam reads through an LRU;
// an implementation that re-checks on the cache MISS branch only passes
// the uncached test and fails in production the moment a team is read
// twice.
//
// TestTeamHero_MembershipIsNotEnough is the negative control for the
// gate itself, and it asserts REACHABILITY before it asserts refusal:
// the same user, the same team, the same asset, refused as a mere member
// and accepted once granted team-scoped `teams.admin`. Without the
// accepted leg, a handler that refused everybody would pass.
//
// Skips without AA_DB_PASSWORD.

package teams_test

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/teams"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type thFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    *teams.Handler
	res  *auth.Resolver
	ctx  context.Context
}

// newHeroFixture builds the handler WITHOUT a cache registry; the cached
// path gets its own fixture below so the two are never confused.
func newHeroFixture(t *testing.T) *thFixture {
	t.Helper()
	return newHeroFixtureWith(t, false)
}

func newHeroFixtureWith(t *testing.T, withCache bool) *thFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	var registry *cache.Registry
	if withCache {
		registry = cache.NewRegistry(pool, logger)
	}
	return &thFixture{
		t:    t,
		pool: pool,
		h:    teams.NewHandler(pool, logger, registry),
		res:  &auth.Resolver{Pool: pool, Logger: logger},
		ctx:  context.Background(),
	}
}

// team seeds a live team and cleans it up.
func (f *thFixture) team(label string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	slug := "thw_" + id.String()[:8] + "_" + label
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		f.t.Fatalf("seed team %s: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

// user seeds an ordinary signed-in user holding the Base role, which is
// where `teams.read` lives. Without it every read below would 403 and
// this file would be testing the read gate instead of the hero.
func (f *thFixture) user(label string) int64 {
	f.t.Helper()
	name := "thw-" + label + "-" + uuid.NewString()[:8]
	var ref int64
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		name).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %s: %v", label, err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`,
		ref, baseRoleID); err != nil {
		f.t.Fatalf("assign Base role to %s: %v", label, err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, ref)
		_, _ = f.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// asset plants an asset that CAN paint a hero: a real storage object, a
// `col` variant, active + ready, carrying `team` and `sensitivity`.
//
// team and sensitivity are the only knobs, so a test that flips
// sensitivity from "public" to "restricted" changes exactly one thing
// about the row — which is what makes the flip test's conclusion sound.
func (f *thFixture) asset(team uuid.UUID, sensitivity string) uuid.UUID {
	f.t.Helper()
	// storage_objects.hash is CHECKed against ^[0-9a-f]{64}$; a UUID's
	// 32 hex digits doubled is exactly that and is unique per call.
	raw := uuid.New()
	hash := hex.EncodeToString(raw[:]) + hex.EncodeToString(raw[:])
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')`,
		hash); err != nil {
		f.t.Fatalf("seed storage object: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)`,
		hash); err != nil {
		f.t.Fatalf("seed col variant: %v", err)
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO assets (title, asset_type, file_hash, team_id,
		                     sensitivity, status, processing_status)
		 VALUES ('th_hero', 1, $1, $2, $3, 'active', 'ready') RETURNING id`,
		hash, team, sensitivity).Scan(&id); err != nil {
		f.t.Fatalf("seed asset: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return id
}

// admin returns a context whose identity holds `teams.admin` SCOPED to
// the given team — the caller this endpoint is for. Scoped rather than
// global on purpose: a global grant would pass any gate, including one
// that ignored the team argument entirely.
func (f *thFixture) admin(ref int64, team uuid.UUID) context.Context {
	f.t.Helper()
	id := f.res.LoadIdentity(f.ctx, ref)
	auth.SetIdentityScopedCapForTest(id, "teams.admin", team)
	return auth.WithIdentity(f.ctx, id)
}

// plain returns a context for a caller with no team-management rights.
func (f *thFixture) plain(ref int64) context.Context {
	f.t.Helper()
	return auth.WithIdentity(f.ctx, f.res.LoadIdentity(f.ctx, ref))
}

func (f *thFixture) member(team uuid.UUID, ref int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, team, ref); err != nil {
		f.t.Fatalf("seed membership: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM team_memberships WHERE team_id = $1 AND user_ref = $2`, team, ref)
	})
}

// setHero drives the real endpoint and returns the raw response so a
// caller can assert on its concrete type.
func (f *thFixture) setHero(ctx context.Context, team uuid.UUID, body openapi.TeamHeroUpdate) openapi.SetTeamHeroResponseObject {
	f.t.Helper()
	resp, err := f.h.SetTeamHero(ctx, openapi.SetTeamHeroRequestObject{
		Id:   openapi_types.UUID(team),
		Body: &body,
	})
	if err != nil {
		f.t.Fatalf("SetTeamHero: %v", err)
	}
	return resp
}

func (f *thFixture) mustSetHero(ctx context.Context, team, asset uuid.UUID) {
	f.t.Helper()
	a := openapi_types.UUID(asset)
	resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{AssetId: &a})
	if _, ok := resp.(openapi.SetTeamHero200JSONResponse); !ok {
		f.t.Fatalf("SetTeamHero: want 200, got %T (%+v)", resp, resp)
	}
}

// heroOnGet reads the hero the way the team page does.
func (f *thFixture) heroOnGet(ctx context.Context, team uuid.UUID) *uuid.UUID {
	f.t.Helper()
	resp, err := f.h.GetTeam(ctx, openapi.GetTeamRequestObject{Id: openapi_types.UUID(team)})
	if err != nil {
		f.t.Fatalf("GetTeam: %v", err)
	}
	ok, isOK := resp.(openapi.GetTeam200JSONResponse)
	if !isOK {
		f.t.Fatalf("GetTeam: want 200, got %T", resp)
	}
	return heroOf(openapi.Team(ok))
}

// heroOnList reads the hero the way the /teams directory does.
func (f *thFixture) heroOnList(ctx context.Context, team uuid.UUID) *uuid.UUID {
	f.t.Helper()
	resp, err := f.h.ListTeams(ctx, openapi.ListTeamsRequestObject{})
	if err != nil {
		f.t.Fatalf("ListTeams: %v", err)
	}
	ok, isOK := resp.(openapi.ListTeams200JSONResponse)
	if !isOK {
		f.t.Fatalf("ListTeams: want 200, got %T", resp)
	}
	for _, it := range ok.Items {
		if uuid.UUID(it.Id) == team {
			return heroOf(it)
		}
	}
	f.t.Fatalf("team %s absent from ListTeams", team)
	return nil
}

// heroOnRail reads the hero the way the followed-teams rail does — the
// surface #982 exists for.
func (f *thFixture) heroOnRail(ctx context.Context, team uuid.UUID) *uuid.UUID {
	f.t.Helper()
	resp, err := f.h.GetMyFollowedTeams(ctx, openapi.GetMyFollowedTeamsRequestObject{})
	if err != nil {
		f.t.Fatalf("GetMyFollowedTeams: %v", err)
	}
	ok, isOK := resp.(openapi.GetMyFollowedTeams200JSONResponse)
	if !isOK {
		f.t.Fatalf("GetMyFollowedTeams: want 200, got %T", resp)
	}
	for _, it := range ok {
		if uuid.UUID(it.Id) == team {
			return heroOf(it)
		}
	}
	f.t.Fatalf("team %s absent from the followed-teams rail", team)
	return nil
}

func heroOf(t openapi.Team) *uuid.UUID {
	if t.HeroAssetId == nil {
		return nil
	}
	v := uuid.UUID(*t.HeroAssetId)
	return &v
}

// storedHero reads the COLUMN, not the response. A body assertion passes
// on a handler that echoes its own input without the write landing.
func (f *thFixture) storedHero(team uuid.UUID) *string {
	f.t.Helper()
	var v *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT hero_asset_id::TEXT FROM teams WHERE id = $1`, team).Scan(&v); err != nil {
		f.t.Fatalf("read back hero_asset_id: %v", err)
	}
	return v
}

func (f *thFixture) setSensitivity(asset uuid.UUID, s string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET sensitivity = $2 WHERE id = $1`, asset, s); err != nil {
		f.t.Fatalf("set sensitivity: %v", err)
	}
}

func (f *thFixture) follow(ref int64, team uuid.UUID) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_follows (user_ref, team_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, ref, team); err != nil {
		f.t.Fatalf("seed follow: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM team_follows WHERE user_ref = $1 AND team_id = $2`, ref, team)
	})
}

// ---------------------------------------------------------------------------
// The happy path, on all three surfaces
// ---------------------------------------------------------------------------

// TestTeamHero_ValidSelectionRendersOnEverySurface pins that a public,
// team-owned asset is accepted and then appears on the three surfaces
// that paint a team: the team page, the directory and the rail.
//
// All three are asserted rather than one, because the hero is stamped by
// an enrichment pass and "the helper exists" is not evidence that a
// given surface calls it. That is exactly how #1026's cover reached
// production working on the detail path and blank on the list path.
func TestTeamHero_ValidSelectionRendersOnEverySurface(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_ok")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")
	f.follow(ref, team)

	f.mustSetHero(ctx, team, asset)

	if got := f.storedHero(team); got == nil || *got != asset.String() {
		t.Fatalf("stored hero_asset_id = %v, want %s", got, asset)
	}
	for name, got := range map[string]*uuid.UUID{
		"getTeam":            f.heroOnGet(ctx, team),
		"listTeams":          f.heroOnList(ctx, team),
		"getMyFollowedTeams": f.heroOnRail(ctx, team),
	} {
		if got == nil || *got != asset {
			t.Errorf("%s: hero_asset_id = %v, want %s", name, got, asset)
		}
	}
}

// ---------------------------------------------------------------------------
// ⭐ The re-check — the half that would silently not work
// ---------------------------------------------------------------------------

// TestTeamHero_FlipToRestrictedDropsOut is the reason the read path
// re-derives instead of trusting the column.
//
// The asset qualified when it was chosen. Setting it to `restricted`
// touches only the assets row — the teams row still points at it and has
// no idea anything changed. An implementation whose read path ships the
// stored pointer keeps painting a non-public picture into a strip
// anonymous readers see, and passes every write-side test in this file.
//
// The pointer is asserted to SURVIVE, too: dropping out of the payload
// is a render decision, not a destructive one, so restoring the asset's
// sensitivity must bring the picture back without an admin re-picking
// it. That last leg also proves the drop-out was caused by the flip
// rather than by the fixture having been broken all along.
func TestTeamHero_FlipToRestrictedDropsOut(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_flip")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")
	f.follow(ref, team)

	f.mustSetHero(ctx, team, asset)
	if got := f.heroOnGet(ctx, team); got == nil || *got != asset {
		t.Fatalf("precondition: hero = %v, want %s — the flip below would prove nothing", got, asset)
	}

	f.setSensitivity(asset, "restricted")

	for name, got := range map[string]*uuid.UUID{
		"getTeam":            f.heroOnGet(ctx, team),
		"listTeams":          f.heroOnList(ctx, team),
		"getMyFollowedTeams": f.heroOnRail(ctx, team),
	} {
		if got != nil {
			t.Errorf("%s: hero_asset_id = %s after the asset became restricted, "+
				"want absent so the client falls back to initials", name, *got)
		}
	}

	// The POINTER stays put — this is a render fallback, not a delete.
	if got := f.storedHero(team); got == nil || *got != asset.String() {
		t.Errorf("stored hero_asset_id = %v after the flip, want %s to survive so "+
			"restoring the asset restores the picture", got, asset)
	}

	// And restoring the asset restores the picture, with no second write.
	f.setSensitivity(asset, "public")
	if got := f.heroOnGet(ctx, team); got == nil || *got != asset {
		t.Errorf("hero = %v after restoring the asset to public, want %s", got, asset)
	}
}

// TestTeamHero_FlipToRestrictedDropsOutThroughTheCache is the same
// property against a handler that HAS its byID LRU.
//
// Not a duplicate of the test above. fetchTeam serves the team page
// through that cache, nothing about an asset's sensitivity invalidates
// it, and nothing sensibly could — the asset does not know which teams
// point at it. An implementation that re-checks only on the cache-miss
// branch, or that bakes the hero into the cached value, passes the
// uncached test and then serves a withdrawn picture for as long as the
// entry lives.
//
// The first read is deliberately performed BEFORE the flip: it is what
// populates the entry, and without it the second read would be a miss
// and the cache would never be exercised.
func TestTeamHero_FlipToRestrictedDropsOutThroughTheCache(t *testing.T) {
	f := newHeroFixtureWith(t, true)
	team := f.team("hero_cached")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")

	f.mustSetHero(ctx, team, asset)
	if got := f.heroOnGet(ctx, team); got == nil || *got != asset {
		t.Fatalf("precondition: hero = %v, want %s", got, asset)
	}
	// Second read, guaranteed to be a cache HIT: still the picture.
	if got := f.heroOnGet(ctx, team); got == nil || *got != asset {
		t.Fatalf("cache hit: hero = %v, want %s", got, asset)
	}

	f.setSensitivity(asset, "restricted")

	if got := f.heroOnGet(ctx, team); got != nil {
		t.Errorf("hero = %s served from a warm cache after the asset became "+
			"restricted, want absent — the re-check must run on the cache HIT "+
			"branch, not only on the miss", *got)
	}
}

// ---------------------------------------------------------------------------
// Selection-time refusals
// ---------------------------------------------------------------------------

// TestTeamHero_NonPublicAssetIsRefused covers both non-public values the
// install actually uses, and pins that a refusal writes nothing.
//
// The refusal is also compared against the one a NONEXISTENT id gets:
// they must be indistinguishable, or an admin can enumerate asset ids
// and read the difference as "this id exists and is hidden from me".
func TestTeamHero_NonPublicAssetIsRefused(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_nonpublic")
	ref := f.user("admin")
	ctx := f.admin(ref, team)

	for _, sensitivity := range []string{"restricted", "team"} {
		asset := f.asset(team, sensitivity)
		a := openapi_types.UUID(asset)
		resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{AssetId: &a})
		if _, bad := resp.(openapi.SetTeamHero400JSONResponse); !bad {
			t.Errorf("sensitivity=%q: got %T, want 400 — a team hero renders to "+
				"anonymous readers, so anything but 'public' is a leak", sensitivity, resp)
		}
		if got := f.storedHero(team); got != nil {
			t.Errorf("sensitivity=%q: hero_asset_id persisted as %s after a refused "+
				"write, want NULL", sensitivity, *got)
		}
	}

	missing := openapi_types.UUID(uuid.New())
	respMissing := f.setHero(ctx, team, openapi.TeamHeroUpdate{AssetId: &missing})
	if _, bad := respMissing.(openapi.SetTeamHero400JSONResponse); !bad {
		t.Errorf("nonexistent asset: got %T, want the same 400 a withheld one gets — "+
			"a different answer makes this an existence oracle", respMissing)
	}
}

// TestTeamHero_AnotherTeamsAssetIsRefused pins the ownership half of the
// rule. The asset is PUBLIC, so an implementation checking only
// sensitivity accepts it and lets any team pin any public asset in the
// install onto itself.
func TestTeamHero_AnotherTeamsAssetIsRefused(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_mine")
	other := f.team("hero_theirs")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(other, "public")

	a := openapi_types.UUID(asset)
	resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{AssetId: &a})
	if _, bad := resp.(openapi.SetTeamHero400JSONResponse); !bad {
		t.Errorf("another team's public asset: got %T, want 400", resp)
	}
	if got := f.storedHero(team); got != nil {
		t.Errorf("hero_asset_id persisted as %s after a refused write, want NULL", *got)
	}

	// Reachability: the SAME team, the same admin, an asset that differs
	// only in team_id, is accepted. Without this leg a handler that
	// refused everything would pass.
	mine := f.asset(team, "public")
	f.mustSetHero(ctx, team, mine)
}

// ---------------------------------------------------------------------------
// The gate: management rights, not membership
// ---------------------------------------------------------------------------

// TestTeamHero_MembershipIsNotEnough is the negative control for the
// capability gate.
//
// Being in a team says you are one of its people; it does not say you
// speak for it, and a team's picture is the most public thing about it.
// The same user, team and asset are driven twice — refused as a mere
// member, accepted once granted team-scoped `teams.admin` — so the
// refusal is demonstrably about the capability and not about the fixture
// being unreachable.
func TestTeamHero_MembershipIsNotEnough(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_gate")
	ref := f.user("member")
	f.member(team, ref)
	asset := f.asset(team, "public")
	a := openapi_types.UUID(asset)

	resp := f.setHero(f.plain(ref), team, openapi.TeamHeroUpdate{AssetId: &a})
	if _, forbidden := resp.(openapi.SetTeamHero403JSONResponse); !forbidden {
		t.Errorf("a member without teams.admin: got %T, want 403 — membership is "+
			"not authority to speak for the team", resp)
	}
	if got := f.storedHero(team); got != nil {
		t.Errorf("hero_asset_id persisted as %s for a caller who was refused", *got)
	}

	// Same everything, plus the capability: accepted.
	f.mustSetHero(f.admin(ref, team), team, asset)
}

// TestTeamHero_ScopedAdminCannotReachAnotherTeam pins that the gate reads
// the team ARGUMENT. A holder of team-scoped `teams.admin` for team A is
// an ordinary user everywhere else; a gate that checked the capability
// without its scope would hand them every team on the instance.
func TestTeamHero_ScopedAdminCannotReachAnotherTeam(t *testing.T) {
	f := newHeroFixture(t)
	mine := f.team("hero_scope_mine")
	theirs := f.team("hero_scope_theirs")
	ref := f.user("scoped")
	ctx := f.admin(ref, mine) // scoped to `mine` only
	asset := f.asset(theirs, "public")

	a := openapi_types.UUID(asset)
	resp := f.setHero(ctx, theirs, openapi.TeamHeroUpdate{AssetId: &a})
	if _, forbidden := resp.(openapi.SetTeamHero403JSONResponse); !forbidden {
		t.Errorf("admin of another team: got %T, want 403", resp)
	}
}

// ---------------------------------------------------------------------------
// Clearing, and the tri-state that makes it expressible
// ---------------------------------------------------------------------------

// TestTeamHero_ClearRevertsToInitials drives `clear_hero`.
//
// It asserts the PERSISTED COLUMN, not the response body. The handler
// re-reads the team and echoes it, so a body assertion would also pass on
// an implementation whose CASE never fired — which is precisely the bug
// #1073 shipped for three releases.
func TestTeamHero_ClearRevertsToInitials(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_clear")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")

	f.mustSetHero(ctx, team, asset)
	if got := f.storedHero(team); got == nil {
		t.Fatalf("precondition: nothing to clear")
	}

	yes := true
	resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{ClearHero: &yes})
	if _, ok := resp.(openapi.SetTeamHero200JSONResponse); !ok {
		t.Fatalf("clear_hero: got %T, want 200", resp)
	}
	if got := f.storedHero(team); got != nil {
		t.Errorf("hero_asset_id = %s after clear_hero, want NULL in the COLUMN", *got)
	}
	if got := f.heroOnGet(ctx, team); got != nil {
		t.Errorf("hero on the wire = %s after clear_hero, want absent", *got)
	}
}

// TestTeamHero_ContradictoryAndEmptyBodiesAreRefused pins the two shapes
// the endpoint refuses to guess at: both fields (two intentions, no
// basis for preferring either) and neither (a single-valued endpoint told
// nothing is a client bug, not a no-op).
func TestTeamHero_ContradictoryAndEmptyBodiesAreRefused(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_body")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")
	f.mustSetHero(ctx, team, asset)

	a := openapi_types.UUID(asset)
	yes := true
	if resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{AssetId: &a, ClearHero: &yes}); func() bool {
		_, bad := resp.(openapi.SetTeamHero400JSONResponse)
		return !bad
	}() {
		t.Errorf("asset_id + clear_hero together: got %T, want 400", resp)
	}
	if resp := f.setHero(ctx, team, openapi.TeamHeroUpdate{}); func() bool {
		_, bad := resp.(openapi.SetTeamHero400JSONResponse)
		return !bad
	}() {
		t.Errorf("empty body: got %T, want 400", resp)
	}

	// Neither refusal disturbed the existing choice.
	if got := f.storedHero(team); got == nil || *got != asset.String() {
		t.Errorf("hero_asset_id = %v after two refused writes, want %s untouched", got, asset)
	}
}

// ---------------------------------------------------------------------------
// The FK
// ---------------------------------------------------------------------------

// TestTeamHero_DeletedAssetRevertsToInitials drives ON DELETE SET NULL.
//
// The DELETE itself is part of the assertion: under RESTRICT it would
// fail, which would mean one team's branding choice could block an
// unrelated asset's deletion.
func TestTeamHero_DeletedAssetRevertsToInitials(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_fk")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")

	f.mustSetHero(ctx, team, asset)

	if _, err := f.pool.Exec(f.ctx, `DELETE FROM assets WHERE id = $1`, asset); err != nil {
		t.Fatalf("hard-delete the hero asset: %v — a team's choice must not block "+
			"an asset deletion", err)
	}
	if got := f.storedHero(team); got != nil {
		t.Errorf("hero_asset_id = %s after the asset was deleted, want NULL via "+
			"ON DELETE SET NULL", *got)
	}
	if got := f.heroOnGet(ctx, team); got != nil {
		t.Errorf("hero on the wire = %s after the asset was deleted, want absent", *got)
	}
}

// TestTeamHero_AcceptedBeforeItsRenditionExists pins the deliberate
// asymmetry between the two checks: the write side does NOT require a
// `col` rendition, because renditions are produced asynchronously and
// refusing a just-uploaded asset would be an error the admin cannot act
// on. The read side does require it, so the team paints initials until
// the rendition lands rather than a broken image.
func TestTeamHero_AcceptedBeforeItsRenditionExists(t *testing.T) {
	f := newHeroFixture(t)
	team := f.team("hero_norendition")
	ref := f.user("admin")
	ctx := f.admin(ref, team)
	asset := f.asset(team, "public")

	// Drop the rendition, leaving an otherwise perfectly good asset.
	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM storage_variants sv USING assets a
		  WHERE a.id = $1 AND sv.object_hash = a.file_hash AND sv.variant_key = 'col'`,
		asset); err != nil {
		t.Fatalf("drop col variant: %v", err)
	}

	f.mustSetHero(ctx, team, asset) // accepted
	if got := f.storedHero(team); got == nil || *got != asset.String() {
		t.Errorf("stored hero = %v, want %s — the pointer is kept while the "+
			"rendition catches up", got, asset)
	}
	if got := f.heroOnGet(ctx, team); got != nil {
		t.Errorf("hero on the wire = %s with no col rendition, want absent so the "+
			"client paints initials instead of a broken image", *got)
	}
}
