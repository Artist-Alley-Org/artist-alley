// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1084 — the featured-team slot in the teams rail.
//
// # The write is driven through the REAL curation endpoint
//
// These tests do not INSERT into featured_items. They call
// featured.HTTPHandler.AddFeaturedItem as a system.admin, because the
// point of this change is that four separately-maintained statements of
// the admissible subject list agree — the CHECK constraint, the Go
// validation, its error string and the OpenAPI enum. A test that seeded
// the row by hand would pass with the migration applied and the handler
// still refusing `team`, which is one of the exact half-done states
// #1084 exists to close.
//
// # What would silently not work
//
// TestFeaturedTeams_SoftDeletedTeamDoesNotAppear is the negative control
// for placement-vs-grant, and it is the one an implementation is most
// likely to get wrong, because getting it wrong looks like nothing: the
// placement resolves, the tile renders, and only a reader who should
// never have seen that studio notices.
//
// Worth being precise about what "cannot see" means for a team here,
// because it is narrower than for an asset or a collection: teams have
// NO per-viewer visibility predicate in this codebase — there is no
// visibility.EntityTeam and no private-team column — so a team is
// readable by every teams.read holder, and the only state that hides one
// from everybody is the tombstone. That, plus the capability gate
// (TestFeaturedTeams_RequiresTeamsRead), is the whole readability rule a
// team has today, and both are asserted. When per-team visibility
// arrives, this file is where its featured-path control belongs.
//
// TestFeaturedTeams_RestrictedHeroFallsBackToInitials is the second one:
// it proves the featured path goes THROUGH the TeamHeroes render-time
// re-check rather than reading teams.hero_asset_id, which is the
// difference between a picture that drops out when it stops being public
// and one that lingers in a navigation strip.
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
	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/teams"
)

// adminRoleIDT is the seeded Admin role, which carries system.admin —
// the capability featuring already requires. #1084 adds none.
const adminRoleIDT = "aa6b632d-5bef-4924-93d4-aba070dfe503"

type ftFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    *teams.Handler
	feat *featured.HTTPHandler
	res  *auth.Resolver
	ctx  context.Context
}

func newFeaturedTeamFixture(t *testing.T) *ftFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	f := &ftFixture{
		t:    t,
		pool: pool,
		h:    teams.NewHandler(pool, logger, nil),
		feat: featured.NewHTTPHandler(featured.NewHandler(pool, logger), logger),
		res:  &auth.Resolver{Pool: pool, Logger: logger},
		ctx:  context.Background(),
	}
	// Placements are global rows with no owner, so a leftover from a
	// failed run would join this run's rail. Cleared per team below
	// rather than wholesale — DELETE FROM featured_items would destroy
	// the dev install's curation when the suite is pointed anywhere but
	// the disposable test database.
	return f
}

func (f *ftFixture) team(label string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	slug := "ftt_" + id.String()[:8] + "_" + label
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		f.t.Fatalf("seed team %s: %v", label, err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM featured_items WHERE subject_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

func (f *ftFixture) user(label, roleID string) int64 {
	f.t.Helper()
	name := "ftt-" + label + "-" + uuid.NewString()[:8]
	var ref int64
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		name).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %s: %v", label, err)
	}
	if roleID != "" {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, ref, roleID); err != nil {
			f.t.Fatalf("assign role to %s: %v", label, err)
		}
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, ref)
		_, _ = f.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func (f *ftFixture) identity(ref int64) context.Context {
	f.t.Helper()
	return auth.WithIdentity(f.ctx, f.res.LoadIdentity(f.ctx, ref))
}

// feature drives the REAL curation endpoint as a system.admin and
// returns the placement id, so a test can remove it the same way an
// operator would.
func (f *ftFixture) feature(team uuid.UUID) uuid.UUID {
	f.t.Helper()
	admin := f.identity(f.user("curator", adminRoleIDT))
	resp, err := f.feat.AddFeaturedItem(admin, openapi.AddFeaturedItemRequestObject{
		Body: &openapi.FeaturedItemInput{
			SubjectKind: "team",
			SubjectId:   openapi_types.UUID(team),
		},
	})
	if err != nil {
		f.t.Fatalf("AddFeaturedItem(team): %v", err)
	}
	created, ok := resp.(openapi.AddFeaturedItem201JSONResponse)
	if !ok {
		f.t.Fatalf("AddFeaturedItem(team): want 201, got %T (%+v)", resp, resp)
	}
	return uuid.UUID(created.Id)
}

func (f *ftFixture) unfeature(placement uuid.UUID) {
	f.t.Helper()
	admin := f.identity(f.user("uncurator", adminRoleIDT))
	resp, err := f.feat.RemoveFeaturedItem(admin, openapi.RemoveFeaturedItemRequestObject{
		Id: openapi_types.UUID(placement),
	})
	if err != nil {
		f.t.Fatalf("RemoveFeaturedItem: %v", err)
	}
	if _, ok := resp.(openapi.RemoveFeaturedItem204Response); !ok {
		f.t.Fatalf("RemoveFeaturedItem: want 204, got %T (%+v)", resp, resp)
	}
}

// railFeatured reads the endpoint the rail calls and returns the teams
// in the order it would draw them.
func (f *ftFixture) railFeatured(ctx context.Context) []openapi.Team {
	f.t.Helper()
	resp, err := f.h.ListFeaturedTeams(ctx, openapi.ListFeaturedTeamsRequestObject{})
	if err != nil {
		f.t.Fatalf("ListFeaturedTeams: %v", err)
	}
	ok, isOK := resp.(openapi.ListFeaturedTeams200JSONResponse)
	if !isOK {
		f.t.Fatalf("ListFeaturedTeams: want 200, got %T (%+v)", resp, resp)
	}
	return ok
}

func (f *ftFixture) railHas(ctx context.Context, team uuid.UUID) (openapi.Team, bool) {
	f.t.Helper()
	for _, it := range f.railFeatured(ctx) {
		if uuid.UUID(it.Id) == team {
			return it, true
		}
	}
	return openapi.Team{}, false
}

// storedAsset plants an asset that CAN paint a hero: a real object, a
// `col` variant, active + ready. sensitivity is the only knob, so the
// flip test changes exactly one thing.
func (f *ftFixture) storedAsset(team uuid.UUID, sensitivity string) uuid.UUID {
	f.t.Helper()
	raw := uuid.New()
	hash := hex.EncodeToString(raw[:]) + hex.EncodeToString(raw[:])
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')`, hash); err != nil {
		f.t.Fatalf("seed storage object: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)`,
		hash); err != nil {
		f.t.Fatalf("seed col variant: %v", err)
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO assets (title, asset_type, file_hash, team_id, sensitivity, status, processing_status)
		 VALUES ('ftt_hero', 1, $1, $2, $3, 'active', 'ready') RETURNING id`,
		hash, team, sensitivity).Scan(&id); err != nil {
		f.t.Fatalf("seed asset: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `UPDATE teams SET hero_asset_id = NULL WHERE hero_asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return id
}

// ---------------------------------------------------------------------------
// The slot itself
// ---------------------------------------------------------------------------

// TestFeaturedTeams_PlacementAppearsInCurationOrder — the positive case,
// end to end through the real write endpoint, plus the ORDER, which is
// the whole product requirement ("the featured team runs first").
//
// Two teams so the order assertion has something to be wrong about: a
// single-element list is in order under every implementation, including
// one that sorts by name.
func TestFeaturedTeams_PlacementAppearsInCurationOrder(t *testing.T) {
	f := newFeaturedTeamFixture(t)
	reader := f.identity(f.user("reader", baseRoleID))

	// Named so alphabetical order is the REVERSE of curation order. An
	// implementation that fell back to ORDER BY name — the ordering
	// every other teams list uses — fails here rather than passing by
	// coincidence.
	zulu := f.team("zulu")
	alpha := f.team("alpha")

	if _, ok := f.railHas(reader, zulu); ok {
		t.Fatal("team is in the featured rail before anything featured it")
	}

	f.feature(zulu)
	f.feature(alpha)

	got := f.railFeatured(reader)
	var order []uuid.UUID
	for _, it := range got {
		if id := uuid.UUID(it.Id); id == zulu || id == alpha {
			order = append(order, id)
		}
	}
	if len(order) != 2 {
		t.Fatalf("featured rail returned %d of the 2 placed teams", len(order))
	}
	if order[0] != zulu || order[1] != alpha {
		t.Errorf("rail order = %v; want curation order [%s %s]", order, zulu, alpha)
	}
}

// TestFeaturedTeams_RemovalRestoresPlainOrdering — un-featuring is the
// operator's undo, and it has to actually undo. Asserted through the
// real DELETE endpoint rather than a DELETE statement, for the same
// reason the add is.
func TestFeaturedTeams_RemovalRestoresPlainOrdering(t *testing.T) {
	f := newFeaturedTeamFixture(t)
	reader := f.identity(f.user("reader", baseRoleID))

	team := f.team("temporary")
	placement := f.feature(team)
	if _, ok := f.railHas(reader, team); !ok {
		t.Fatal("featured team absent from the rail; the rest of this test would prove nothing")
	}

	f.unfeature(placement)

	if _, ok := f.railHas(reader, team); ok {
		t.Error("team still in the featured rail after its placement was removed")
	}
	// The team itself must survive: removing a PLACEMENT deletes a
	// placement, not a studio.
	var alive bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT deleted_at IS NULL FROM teams WHERE id = $1`, team).Scan(&alive); err != nil {
		t.Fatalf("re-read team: %v", err)
	}
	if !alive {
		t.Error("removing a placement deleted the team")
	}
}

// ---------------------------------------------------------------------------
// Placement is not a grant
// ---------------------------------------------------------------------------

// TestFeaturedTeams_SoftDeletedTeamDoesNotAppear — ⭐ the negative
// control. A tombstoned team is the one state a team can be in that
// hides it from every reader, and a placement must not resurrect it.
//
// REACHABILITY IS ASSERTED FIRST: the same team, the same placement,
// present in the rail while live and gone once tombstoned. Without that
// leg, an implementation that returned nothing at all would pass.
func TestFeaturedTeams_SoftDeletedTeamDoesNotAppear(t *testing.T) {
	f := newFeaturedTeamFixture(t)
	reader := f.identity(f.user("reader", baseRoleID))

	team := f.team("doomed")
	f.feature(team)
	if _, ok := f.railHas(reader, team); !ok {
		t.Fatal("featured live team absent from the rail; the refusal below would prove nothing")
	}

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, team); err != nil {
		t.Fatalf("tombstone team: %v", err)
	}

	if _, ok := f.railHas(reader, team); ok {
		t.Error("a featured SOFT-DELETED team surfaced in the rail: " +
			"featuring widened what the caller can see")
	}
	// And the placement row is still there — the read dropped it, rather
	// than the write having been cleaned up by something else. Otherwise
	// this test could pass for a reason it is not testing.
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM featured_items WHERE subject_id = $1 AND subject_kind = 'team'`,
		team).Scan(&n); err != nil {
		t.Fatalf("count placements: %v", err)
	}
	if n != 1 {
		t.Fatalf("placement rows = %d, want 1: the read path is not what dropped the team", n)
	}
}

// TestFeaturedTeams_RequiresTeamsRead — the capability half of the same
// rule. A caller with no roles holds no teams.read and is refused, so a
// placement cannot be a back door into the teams surface for someone the
// install does not let read teams at all.
func TestFeaturedTeams_RequiresTeamsRead(t *testing.T) {
	f := newFeaturedTeamFixture(t)

	team := f.team("gated")
	f.feature(team)

	// Reachable for a Base caller...
	if _, ok := f.railHas(f.identity(f.user("reader", baseRoleID)), team); !ok {
		t.Fatal("featured team absent for a teams.read caller; the refusal below would prove nothing")
	}

	// ...and refused for one holding nothing.
	resp, err := f.h.ListFeaturedTeams(
		f.identity(f.user("nobody", "")), openapi.ListFeaturedTeamsRequestObject{})
	if err != nil {
		t.Fatalf("ListFeaturedTeams: %v", err)
	}
	if _, ok := resp.(openapi.ListFeaturedTeams403JSONResponse); !ok {
		t.Fatalf("ListFeaturedTeams without teams.read: want 403, got %T", resp)
	}

	// Anonymous is 401, not an empty list: this rail is a signed-in
	// surface and a placement does not make it public.
	anonResp, err := f.h.ListFeaturedTeams(f.ctx, openapi.ListFeaturedTeamsRequestObject{})
	if err != nil {
		t.Fatalf("ListFeaturedTeams (anon): %v", err)
	}
	if _, ok := anonResp.(openapi.ListFeaturedTeams401JSONResponse); !ok {
		t.Fatalf("ListFeaturedTeams anonymously: want 401, got %T", anonResp)
	}
}

// ---------------------------------------------------------------------------
// The hero comes from the render-time re-check, not the stored pointer
// ---------------------------------------------------------------------------

// TestFeaturedTeams_RestrictedHeroFallsBackToInitials — the featured
// path must go through TeamHeroes.
//
// The picture is present while the asset is public and ABSENT once it is
// restricted, with nothing else changed and nothing invalidated in
// between. An implementation that read teams.hero_asset_id directly —
// the obvious way to write a bespoke query for this endpoint — passes
// the first half and fails the second, and in production goes on
// painting a no-longer-public image into a navigation strip.
//
// Absent, note, rather than broken: the client renders the initials tile
// when hero_asset_id is missing, so "no field" IS the fallback.
func TestFeaturedTeams_RestrictedHeroFallsBackToInitials(t *testing.T) {
	f := newFeaturedTeamFixture(t)
	reader := f.identity(f.user("reader", baseRoleID))

	team := f.team("pictured")
	hero := f.storedAsset(team, "public")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET hero_asset_id = $1 WHERE id = $2`, hero, team); err != nil {
		t.Fatalf("set hero: %v", err)
	}
	f.feature(team)

	got, ok := f.railHas(reader, team)
	if !ok {
		t.Fatal("featured team absent from the rail")
	}
	if got.HeroAssetId == nil || uuid.UUID(*got.HeroAssetId) != hero {
		t.Fatalf("hero on the featured slot = %v, want %s", got.HeroAssetId, hero)
	}

	// One knob. The pointer in teams.hero_asset_id is untouched.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, hero); err != nil {
		t.Fatalf("restrict hero asset: %v", err)
	}

	got, ok = f.railHas(reader, team)
	if !ok {
		t.Fatal("featured team vanished when its hero stopped qualifying; " +
			"the fallback is initials, not disappearance")
	}
	if got.HeroAssetId != nil {
		t.Errorf("hero still %s after the asset became restricted: the featured path "+
			"is reading teams.hero_asset_id instead of the TeamHeroes re-check",
			uuid.UUID(*got.HeroAssetId))
	}

	var stored *uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT hero_asset_id FROM teams WHERE id = $1`, team).Scan(&stored); err != nil {
		t.Fatalf("re-read stored pointer: %v", err)
	}
	if stored == nil || *stored != hero {
		t.Errorf("stored hero_asset_id = %v; the READ must drop the picture without "+
			"clearing the operator's choice", stored)
	}
}
