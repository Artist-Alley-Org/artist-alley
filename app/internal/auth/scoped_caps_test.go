// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the Phase 1.7.B-3 changes: the scoped capability
// resolver (EffectiveScopedCapabilitiesForUser) and the new Can(code,
// InTeam(...)) API. They go through the real Resolver.loadCapabilities
// path so what we're really testing is "what does the request handler
// see in id.Can() after the middleware runs".

// loadIdentity builds an Identity for the given user and populates its
// caps via the Resolver, exactly as the middleware does. Returns the
// Identity ready for Can() calls.
func loadIdentity(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userRef int64) *Identity {
	t.Helper()
	id := &Identity{UserRef: userRef}
	r := &Resolver{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.loadCapabilities(ctx, New(pool), id)
	return id
}

// seedTeam inserts a team and returns its UUID. Cleanup is via the
// caller's t.Cleanup (typically a CASCADE delete of the test user).
func seedTeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	slug := "test_team_" + id.String()[:8] + "_" + name
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, slug, name,
	); err != nil {
		t.Fatalf("seed team %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

// linkTeams adds a parent->child edge. Closure rows materialise via the
// 00001 trigger.
func linkTeams(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent, child uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`,
		parent, child,
	); err != nil {
		t.Fatalf("link teams: %v", err)
	}
}

// assignTeamRole adds a team-scoped role assignment for the user.
func assignTeamRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userRef int64, roleID [16]byte, team uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_roles (user_ref, role_id, team_id) VALUES ($1, $2, $3)`,
		userRef, pgtype.UUID{Bytes: roleID, Valid: true},
		pgtype.UUID{Bytes: team, Valid: true},
	); err != nil {
		t.Fatalf("assign team role: %v", err)
	}
}

// TestCan_GlobalRolePassesEverywhere: a globally-assigned role grants
// the cap unscoped and for any team scope.
func TestCan_GlobalRolePassesEverywhere(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedCap(t, ctx, fx.pool, "test.scoped.editor")
		roleID := seedRole(t, ctx, fx.pool, "test_GlobalEditor", nil, "test.scoped.editor")

		q := New(fx.pool)
		if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
			UserRef: fx.userRef,
			RoleID:  pgtype.UUID{Bytes: roleID, Valid: true},
		}); err != nil {
			t.Fatalf("assign global: %v", err)
		}

		teamA := seedTeam(t, ctx, fx.pool, "A")

		id := loadIdentity(t, ctx, fx.pool, fx.userRef)
		if !id.Can("test.scoped.editor") {
			t.Errorf("global cap missing on unscoped Can()")
		}
		if !id.Can("test.scoped.editor", InTeam(teamA)) {
			t.Errorf("global cap missing on scoped Can(InTeam=A)")
		}
		if !id.Can("test.scoped.editor", InTeam(uuid.New())) {
			t.Errorf("global cap missing for an unrelated random team")
		}
	})
}

// TestCan_TeamScopedRoleIsTeamOnly: a team-scoped role assignment grants
// the cap ONLY for that team's scope — never for unscoped Can() and
// never for unrelated teams.
func TestCan_TeamScopedRoleIsTeamOnly(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedCap(t, ctx, fx.pool, "test.scoped.reviewer")
		roleID := seedRole(t, ctx, fx.pool, "test_TeamReviewer", nil, "test.scoped.reviewer")

		teamA := seedTeam(t, ctx, fx.pool, "A")
		teamB := seedTeam(t, ctx, fx.pool, "B")

		assignTeamRole(t, ctx, fx.pool, fx.userRef, roleID, teamA)

		id := loadIdentity(t, ctx, fx.pool, fx.userRef)
		if id.Can("test.scoped.reviewer") {
			t.Errorf("team-scoped cap leaked into unscoped Can()")
		}
		if !id.Can("test.scoped.reviewer", InTeam(teamA)) {
			t.Errorf("team-scoped cap missing on Can(InTeam=A)")
		}
		if id.Can("test.scoped.reviewer", InTeam(teamB)) {
			t.Errorf("team-scoped cap leaked to unrelated team B")
		}
	})
}

// TestCan_DescendantsInheritScope: assigning a role scoped to a parent
// team also grants the cap to every descendant team via the closure
// expansion done at SQL time.
func TestCan_DescendantsInheritScope(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedCap(t, ctx, fx.pool, "test.scoped.director")
		roleID := seedRole(t, ctx, fx.pool, "test_Director", nil, "test.scoped.director")

		root := seedTeam(t, ctx, fx.pool, "root")
		mid := seedTeam(t, ctx, fx.pool, "mid")
		leaf := seedTeam(t, ctx, fx.pool, "leaf")
		linkTeams(t, ctx, fx.pool, root, mid)
		linkTeams(t, ctx, fx.pool, mid, leaf)

		assignTeamRole(t, ctx, fx.pool, fx.userRef, roleID, root)

		id := loadIdentity(t, ctx, fx.pool, fx.userRef)
		for label, team := range map[string]uuid.UUID{"root": root, "mid": mid, "leaf": leaf} {
			if !id.Can("test.scoped.director", InTeam(team)) {
				t.Errorf("director cap missing for descendant %s", label)
			}
		}
	})
}

// TestCan_SystemAdminBypassesScope: the global system.admin wildcard
// satisfies any scoped check, regardless of which team is requested.
func TestCan_SystemAdminBypassesScope(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		var adminID pgtype.UUID
		if err := fx.pool.QueryRow(ctx, `SELECT id FROM roles WHERE name='Admin'`).Scan(&adminID); err != nil {
			t.Fatalf("lookup Admin: %v", err)
		}
		q := New(fx.pool)
		if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
			UserRef: fx.userRef,
			RoleID:  adminID,
		}); err != nil {
			t.Fatalf("assign Admin: %v", err)
		}

		teamA := seedTeam(t, ctx, fx.pool, "A")
		id := loadIdentity(t, ctx, fx.pool, fx.userRef)
		if !id.Can("nonexistent.cap", InTeam(teamA)) {
			t.Errorf("system.admin should satisfy any scoped check")
		}
	})
}

// TestCan_PerUserScopedGrant: an explicit per-user grant with team
// scope feeds the same expansion path as a role-derived scoped cap.
func TestCan_PerUserScopedGrant(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedCap(t, ctx, fx.pool, "test.scoped.directgrant")
		teamA := seedTeam(t, ctx, fx.pool, "A")
		child := seedTeam(t, ctx, fx.pool, "child")
		linkTeams(t, ctx, fx.pool, teamA, child)

		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1, $2, $3)`,
			fx.userRef, "test.scoped.directgrant",
			pgtype.UUID{Bytes: teamA, Valid: true},
		); err != nil {
			t.Fatalf("grant scoped: %v", err)
		}

		id := loadIdentity(t, ctx, fx.pool, fx.userRef)
		if id.Can("test.scoped.directgrant") {
			t.Errorf("scoped direct grant leaked into unscoped Can()")
		}
		if !id.Can("test.scoped.directgrant", InTeam(teamA)) {
			t.Errorf("scoped direct grant missing for the scoping team")
		}
		if !id.Can("test.scoped.directgrant", InTeam(child)) {
			t.Errorf("scoped direct grant should also cover descendants")
		}
	})
}
