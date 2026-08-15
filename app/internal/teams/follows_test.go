// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #577 — the follow endpoints, driven through the real handlers.
//
// The positive case is one line and barely worth a test. Everything
// interesting here is a NEGATIVE, and every negative is asserted to be
// REACHABLE before it is asserted to be refused — a test that refuses
// something the handler could never have been asked in the first place
// proves nothing. So the soft-deleted-team case follows the same team
// through a successful follow, an unfollow, a tombstone, and only then
// a refusal: the refusal is demonstrably about the tombstone and not
// about the fixture being wrong.
//
// The properties:
//
//   - follow is idempotent (double-follow → 204, one row)
//   - unfollow is idempotent (unfollow of a non-follow → 204)
//   - a nonexistent team is refused
//   - a SOFT-DELETED team is refused, which the FK cannot do
//   - ⭐ those two refusals are INDISTINGUISHABLE — same status, same
//     body — or the endpoint enumerates every studio on the instance
//   - a follow confers NOTHING: no membership row, and no capability
//     the follower did not already hold
package teams_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/teams"
)

type tfFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    *teams.Handler
	res  *auth.Resolver
	ctx  context.Context
}

func newFollowFixture(t *testing.T) *tfFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &tfFixture{
		t:    t,
		pool: pool,
		h:    teams.NewHandler(pool, logger, nil),
		res:  &auth.Resolver{Pool: pool, Logger: logger},
		ctx:  context.Background(),
	}
}

// baseRoleID is the seeded Base role — the one an ordinary signed-up
// user holds, and the one that carries `teams.read`. Pinned as a literal
// the way the baseline pins it.
const baseRoleID = "80ec6003-7fd5-4dac-9415-d26d39169d42"

// user seeds an ORDINARY signed-in user: a "user" row plus the Base
// role.
//
// The role assignment is not boilerplate. These endpoints gate on
// `teams.read`, Base is what holds it, and a bare "user" row resolves to
// zero capabilities — so a fixture without the role would 403 on every
// call and the whole file would be testing the capability gate over and
// over instead of the follow semantics. Assigning Base also keeps the
// fixture honest about WHO these endpoints are for: the floor of the
// permission system, not an admin.
func (f *tfFixture) user(label string) int64 {
	f.t.Helper()
	name := "tfw-" + label + "-" + uuid.NewString()[:8]
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

func (f *tfFixture) team(label string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	slug := "tfw_" + id.String()[:8] + "_" + label
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		f.t.Fatalf("seed team %s: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

// identity loads the Identity the middleware would produce, so the
// caller's capabilities come from the database rather than from a
// literal this test wrote. Base holds teams.read, so a plain seeded
// user is exactly the caller these endpoints expect.
func (f *tfFixture) identity(ref int64) context.Context {
	f.t.Helper()
	return auth.WithIdentity(f.ctx, f.res.LoadIdentity(f.ctx, ref))
}

func (f *tfFixture) follow(ctx context.Context, team uuid.UUID) openapi.FollowTeamResponseObject {
	f.t.Helper()
	resp, err := f.h.FollowTeam(ctx, openapi.FollowTeamRequestObject{Id: openapi_types.UUID(team)})
	if err != nil {
		f.t.Fatalf("FollowTeam: %v", err)
	}
	return resp
}

func (f *tfFixture) unfollow(ctx context.Context, team uuid.UUID) openapi.UnfollowTeamResponseObject {
	f.t.Helper()
	resp, err := f.h.UnfollowTeam(ctx, openapi.UnfollowTeamRequestObject{Id: openapi_types.UUID(team)})
	if err != nil {
		f.t.Fatalf("UnfollowTeam: %v", err)
	}
	return resp
}

func (f *tfFixture) followedIDs(ctx context.Context) []uuid.UUID {
	f.t.Helper()
	resp, err := f.h.GetMyFollowedTeams(ctx, openapi.GetMyFollowedTeamsRequestObject{})
	if err != nil {
		f.t.Fatalf("GetMyFollowedTeams: %v", err)
	}
	ok, isOK := resp.(openapi.GetMyFollowedTeams200JSONResponse)
	if !isOK {
		f.t.Fatalf("GetMyFollowedTeams: want 200, got %T", resp)
	}
	out := make([]uuid.UUID, 0, len(ok))
	for _, t := range ok {
		out = append(out, uuid.UUID(t.Id))
	}
	return out
}

func (f *tfFixture) followRows(ref int64, team uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM team_follows WHERE user_ref = $1 AND team_id = $2`,
		ref, team).Scan(&n); err != nil {
		f.t.Fatalf("count follows: %v", err)
	}
	return n
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Idempotence
// ---------------------------------------------------------------------------

// TestFollowTeam_Idempotent pins that a double-tapped button and a
// retried request are one request. The row count is asserted alongside
// the status: a handler that returned 204 while writing a second row
// would pass a status-only check and then break the PK on the next
// schema change.
func TestFollowTeam_Idempotent(t *testing.T) {
	f := newFollowFixture(t)
	user := f.user("idem")
	team := f.team("idem")
	ctx := f.identity(user)

	if resp := f.follow(ctx, team); !is204Follow(resp) {
		t.Fatalf("first follow: want 204, got %T", resp)
	}
	if n := f.followRows(user, team); n != 1 {
		t.Fatalf("after one follow: %d rows, want 1", n)
	}

	if resp := f.follow(ctx, team); !is204Follow(resp) {
		t.Errorf("SECOND follow: want 204 (idempotent), got %T — a re-follow must not "+
			"be an error the client has to know how to swallow", resp)
	}
	if n := f.followRows(user, team); n != 1 {
		t.Errorf("after a double follow: %d rows, want 1", n)
	}
	if ids := f.followedIDs(ctx); !contains(ids, team) {
		t.Errorf("the followed team is missing from the rail")
	}
}

// TestUnfollowTeam_Idempotent — unfollowing something you do not follow
// has already achieved what you asked for.
func TestUnfollowTeam_Idempotent(t *testing.T) {
	f := newFollowFixture(t)
	user := f.user("unidem")
	team := f.team("unidem")
	ctx := f.identity(user)

	// REACHABLE FIRST: the endpoint works at all on this fixture, so
	// the no-op assertion below is about the no-op and not about a
	// broken setup.
	if resp := f.follow(ctx, team); !is204Follow(resp) {
		t.Fatalf("setup follow: want 204, got %T", resp)
	}
	if resp := f.unfollow(ctx, team); !is204Unfollow(resp) {
		t.Fatalf("first unfollow: want 204, got %T", resp)
	}
	if n := f.followRows(user, team); n != 0 {
		t.Fatalf("after unfollow: %d rows, want 0", n)
	}

	if resp := f.unfollow(ctx, team); !is204Unfollow(resp) {
		t.Errorf("SECOND unfollow: want 204 (idempotent), got %T", resp)
	}
	if ids := f.followedIDs(ctx); contains(ids, team) {
		t.Errorf("the unfollowed team is still in the rail")
	}
}

// ---------------------------------------------------------------------------
// ⭐ The indistinguishability pair
// ---------------------------------------------------------------------------

// TestFollowTeam_NonexistentAndSoftDeletedAreIndistinguishable is the
// security-shaped test in this file.
//
// A soft-deleted team must be refused — the FK cannot refuse it,
// because `teams.deleted_at` is invisible to a foreign key, so a
// tombstoned team satisfies the constraint perfectly. Only the
// handler's explicit liveness probe stands in the way (the #955
// discipline).
//
// And the refusal must be BYTE-IDENTICAL to the one a nonexistent team
// gets. If the two ever differ — different status, different message,
// even different wording — POST /teams/{uuid}/follow becomes an oracle
// that enumerates every studio on the instance, deleted ones included,
// one guess at a time. That is why they are asserted against each
// other rather than each against a literal.
//
// The soft-deleted arm proves REACHABILITY first: the same team is
// followed successfully, unfollowed, and only then tombstoned. So the
// final refusal is demonstrably caused by the tombstone.
func TestFollowTeam_NonexistentAndSoftDeletedAreIndistinguishable(t *testing.T) {
	f := newFollowFixture(t)
	user := f.user("probe")
	ctx := f.identity(user)

	// ── Arm 1: a team that never existed ─────────────────────────────
	ghost := uuid.New()
	ghostResp := f.follow(ctx, ghost)
	ghost404, ok := ghostResp.(openapi.FollowTeam404JSONResponse)
	if !ok {
		t.Fatalf("following a nonexistent team: want 404, got %T", ghostResp)
	}

	// ── Arm 2: a team that existed, was followable, then was deleted ──
	tomb := f.team("tomb")
	if resp := f.follow(ctx, tomb); !is204Follow(resp) {
		t.Fatalf("REACHABILITY: the fixture team was not followable while live "+
			"(got %T) — the refusal below would prove nothing", resp)
	}
	if resp := f.unfollow(ctx, tomb); !is204Unfollow(resp) {
		t.Fatalf("setup unfollow: want 204, got %T", resp)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, tomb); err != nil {
		t.Fatalf("soft-delete team: %v", err)
	}

	tombResp := f.follow(ctx, tomb)
	tomb404, ok := tombResp.(openapi.FollowTeam404JSONResponse)
	if !ok {
		t.Fatalf("following a SOFT-DELETED team: want 404, got %T — the FK cannot "+
			"catch this (deleted_at is invisible to it), so an explicit liveness "+
			"probe is the only thing that can", tombResp)
	}
	if n := f.followRows(user, tomb); n != 0 {
		t.Errorf("a follow row was written for a soft-deleted team (%d rows)", n)
	}

	// ── The pair must be identical ───────────────────────────────────
	if ghost404.Error != tomb404.Error {
		t.Errorf("the two refusals differ: nonexistent says %q, soft-deleted says %q — "+
			"any difference makes this endpoint a team-existence oracle",
			ghost404.Error, tomb404.Error)
	}
}

// ---------------------------------------------------------------------------
// A follow confers nothing
// ---------------------------------------------------------------------------

// TestFollowTeam_ConfersNothing is the decision "a follow is a
// bookmark, not a relationship" written as a test.
//
// It checks the two things a future refactor could plausibly break:
// that following does not write a membership row, and that the
// follower's resolved capability set is unchanged. The second is the
// one worth having — a membership row would be obvious in review, but
// a capability arriving through some scoped-grant path keyed on
// team_follows would not be.
func TestFollowTeam_ConfersNothing(t *testing.T) {
	f := newFollowFixture(t)
	user := f.user("bookmark")
	team := f.team("bookmark")
	ctx := f.identity(user)

	before := f.res.LoadIdentity(f.ctx, user)
	beforeCaps := len(before.Capabilities)

	if resp := f.follow(ctx, team); !is204Follow(resp) {
		t.Fatalf("follow: want 204, got %T", resp)
	}

	var memberships int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM team_memberships WHERE team_id = $1 AND user_ref = $2`,
		team, user).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 0 {
		t.Errorf("following wrote %d team_memberships row(s) — a follow is a bookmark "+
			"and must stay a universe away from the authorization tables", memberships)
	}

	after := f.res.LoadIdentity(f.ctx, user)
	if len(after.Capabilities) != beforeCaps {
		t.Errorf("the follower's capability count changed from %d to %d — "+
			"following a team must confer nothing", beforeCaps, len(after.Capabilities))
	}
	if got := after.ScopedTeams("assets.admin"); len(got) != 0 {
		t.Errorf("following produced scoped team grants: %v", got)
	}
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

// TestFollowedTeams_ExcludesSoftDeleted — a studio that is deleted
// leaves the rail, without the follow row having to go anywhere. The
// row stays (nothing cascades on a tombstone) and the read filters it,
// so a restored team comes back.
func TestFollowedTeams_ExcludesSoftDeleted(t *testing.T) {
	f := newFollowFixture(t)
	user := f.user("rail")
	team := f.team("rail")
	ctx := f.identity(user)

	if resp := f.follow(ctx, team); !is204Follow(resp) {
		t.Fatalf("follow: want 204, got %T", resp)
	}
	if ids := f.followedIDs(ctx); !contains(ids, team) {
		t.Fatalf("REACHABILITY: the team is not in the rail while live")
	}

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, team); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if ids := f.followedIDs(ctx); contains(ids, team) {
		t.Errorf("a soft-deleted team is still in the teams rail")
	}
	if n := f.followRows(user, team); n != 1 {
		t.Errorf("the follow row was destroyed by a tombstone (%d rows, want 1) — "+
			"the read filters, it does not delete, so a restore brings the team back", n)
	}

	// Restore, and it returns — which is what makes "filter, don't
	// delete" the right call rather than merely the lazy one.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET deleted_at = NULL WHERE id = $1`, team); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if ids := f.followedIDs(ctx); !contains(ids, team) {
		t.Errorf("a restored team did not come back to the rail")
	}
}

// TestFollowEndpoints_RequireAuth — anonymous gets 401 on all three,
// never a 404 or a silent success.
func TestFollowEndpoints_RequireAuth(t *testing.T) {
	f := newFollowFixture(t)
	team := f.team("anon")
	anon := context.Background() // no identity in context at all

	if resp, err := f.h.FollowTeam(anon,
		openapi.FollowTeamRequestObject{Id: openapi_types.UUID(team)}); err != nil {
		t.Fatalf("FollowTeam: %v", err)
	} else if _, ok := resp.(openapi.FollowTeam401JSONResponse); !ok {
		t.Errorf("anonymous follow: want 401, got %T", resp)
	}
	if resp, err := f.h.UnfollowTeam(anon,
		openapi.UnfollowTeamRequestObject{Id: openapi_types.UUID(team)}); err != nil {
		t.Fatalf("UnfollowTeam: %v", err)
	} else if _, ok := resp.(openapi.UnfollowTeam401JSONResponse); !ok {
		t.Errorf("anonymous unfollow: want 401, got %T", resp)
	}
	if resp, err := f.h.GetMyFollowedTeams(anon,
		openapi.GetMyFollowedTeamsRequestObject{}); err != nil {
		t.Fatalf("GetMyFollowedTeams: %v", err)
	} else if _, ok := resp.(openapi.GetMyFollowedTeams401JSONResponse); !ok {
		t.Errorf("anonymous rail: want 401, got %T", resp)
	}
}

// TestFollowEndpoints_RequireTeamsRead — the capability gate is real,
// and it is asserted against a user who differs from the working
// fixture in EXACTLY one way: no Base role, therefore no `teams.read`.
//
// Without this the 204s elsewhere in the file would be consistent with
// a handler that never checked a capability at all.
func TestFollowEndpoints_RequireTeamsRead(t *testing.T) {
	f := newFollowFixture(t)
	team := f.team("nocap")

	// A roleless user — same shape as f.user, minus the Base assignment.
	var ref int64
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"tfw-nocap-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatalf("seed roleless user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	ctx := f.identity(ref)

	if resp := f.follow(ctx, team); !is403Follow(resp) {
		t.Errorf("follow without teams.read: want 403, got %T", resp)
	}
	if n := f.followRows(ref, team); n != 0 {
		t.Errorf("a refused follow still wrote %d row(s)", n)
	}

	// A user WITH the role can follow the same team — so the refusal
	// above is about the capability and not about the fixture.
	if resp := f.follow(f.identity(f.user("hascap")), team); !is204Follow(resp) {
		t.Errorf("REACHABILITY: a Base user could not follow the same team (got %T)", resp)
	}
}

func is403Follow(r openapi.FollowTeamResponseObject) bool {
	_, ok := r.(openapi.FollowTeam403JSONResponse)
	return ok
}

func is204Follow(r openapi.FollowTeamResponseObject) bool {
	_, ok := r.(openapi.FollowTeam204Response)
	return ok
}

func is204Unfollow(r openapi.UnfollowTeamResponseObject) bool {
	_, ok := r.(openapi.UnfollowTeam204Response)
	return ok
}
