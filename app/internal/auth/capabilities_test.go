package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// TestCanMethod_Basics exercises the in-memory permission check.
// No DB needed.
func TestCanMethod_Basics(t *testing.T) {
	cases := []struct {
		name string
		id   *Identity
		cap  string
		want bool
	}{
		{"nil identity is never authorised", nil, "anything", false},
		{"empty cap never matches", &Identity{Capabilities: []string{"users.read"}}, "", false},
		{"direct match", &Identity{Capabilities: []string{"users.read"}}, "users.read", true},
		{"no match", &Identity{Capabilities: []string{"users.read"}}, "users.write", false},
		{"system.admin wildcards everything", &Identity{Capabilities: []string{"system.admin"}}, "anything-at-all", true},
		{"empty cap set", &Identity{Capabilities: []string{}}, "users.read", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Can(tc.cap); got != tc.want {
				t.Errorf("Can(%q)=%v want %v", tc.cap, got, tc.want)
			}
		})
	}
}

// TestEffectiveCapabilities_ResolvesRoleChain creates a small role
// hierarchy (Base ← Artist ← Director) and verifies the recursive CTE
// returns the union of every level.
func TestEffectiveCapabilities_ResolvesRoleChain(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// Add three new capabilities + three roles in a chain.
		q := New(fx.pool)
		seedCap(t, ctx, fx.pool, "test.base")
		seedCap(t, ctx, fx.pool, "test.artist")
		seedCap(t, ctx, fx.pool, "test.director")
		baseID := seedRole(t, ctx, fx.pool, "test_Base", nil, "test.base")
		artistID := seedRole(t, ctx, fx.pool, "test_Artist", &baseID, "test.artist")
		dirID := seedRole(t, ctx, fx.pool, "test_Director", &artistID, "test.director")

		// Put the test user into the Director role.
		if err := q.SetUserRole(ctx, SetUserRoleParams{
			RsUserID: fx.userRef,
			RoleID:   pgtype.UUID{Bytes: dirID, Valid: true},
		}); err != nil {
			t.Fatalf("SetUserRole: %v", err)
		}

		caps, err := q.EffectiveCapabilitiesForUser(ctx, fx.userRef)
		if err != nil {
			t.Fatalf("EffectiveCapabilitiesForUser: %v", err)
		}
		assertHasAll(t, caps, "test.base", "test.artist", "test.director")
	})
}

// TestEffectiveCapabilities_GrantsAndRevokes verifies that per-user
// grants are unioned in and per-user revokes are removed, even from
// the role chain.
func TestEffectiveCapabilities_GrantsAndRevokes(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		q := New(fx.pool)
		seedCap(t, ctx, fx.pool, "test.granted")
		seedCap(t, ctx, fx.pool, "test.role_cap")
		seedCap(t, ctx, fx.pool, "test.to_revoke")
		roleID := seedRole(t, ctx, fx.pool, "test_GrantsRevokes", nil, "test.role_cap", "test.to_revoke")

		if err := q.SetUserRole(ctx, SetUserRoleParams{
			RsUserID: fx.userRef,
			RoleID:   pgtype.UUID{Bytes: roleID, Valid: true},
		}); err != nil {
			t.Fatalf("SetUserRole: %v", err)
		}

		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO user_capability_grants (rs_user_id, capability_code) VALUES ($1, $2)`,
			fx.userRef, "test.granted"); err != nil {
			t.Fatalf("grant insert: %v", err)
		}
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO user_capability_revokes (rs_user_id, capability_code) VALUES ($1, $2)`,
			fx.userRef, "test.to_revoke"); err != nil {
			t.Fatalf("revoke insert: %v", err)
		}

		caps, err := q.EffectiveCapabilitiesForUser(ctx, fx.userRef)
		if err != nil {
			t.Fatalf("EffectiveCapabilitiesForUser: %v", err)
		}
		assertHasAll(t, caps, "test.role_cap", "test.granted")
		assertHasNone(t, caps, "test.to_revoke")
	})
}

// TestHandlers_CapabilityEnforcement verifies that 401 is returned
// when anonymous and 403 when authenticated without the required
// capability. Then assigns the seeded Admin role and confirms 200.
func TestHandlers_CapabilityEnforcement(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// Anonymous: 401.
		resp := fx.call(t, http.MethodGet, "/auth/capabilities", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous status=%d want 401", resp.StatusCode)
		}

		// Authenticated but no role: 403.
		cookie := fx.loginAndGetCookie(t)
		resp = fx.call(t, http.MethodGet, "/auth/capabilities", nil, &cookie)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("no-role status=%d want 403 body=%s", resp.StatusCode, readBody(resp))
		}

		// Grant the test user the Admin role (seeded by migration 00002).
		var adminID pgtype.UUID
		if err := fx.pool.QueryRow(ctx, `SELECT id FROM roles WHERE name='Admin'`).Scan(&adminID); err != nil {
			t.Fatalf("lookup Admin role: %v", err)
		}
		q := New(fx.pool)
		if err := q.SetUserRole(ctx, SetUserRoleParams{
			RsUserID: fx.userRef,
			RoleID:   adminID,
		}); err != nil {
			t.Fatalf("assign Admin: %v", err)
		}

		// Same cookie, new capabilities loaded fresh per request.
		resp = fx.call(t, http.MethodGet, "/auth/capabilities", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("with-Admin status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var caps []openapi.Capability
		mustDecode(t, resp, &caps)
		if len(caps) < 3 {
			t.Errorf("expected at least 3 caps, got %d", len(caps))
		}
	})
}

// TestGetMyCapabilities verifies the full resolved-cap-set endpoint
// after the same setup.
func TestGetMyCapabilities_FullShape(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		cookie := fx.loginAndGetCookie(t)

		var adminID pgtype.UUID
		if err := fx.pool.QueryRow(ctx, `SELECT id FROM roles WHERE name='Admin'`).Scan(&adminID); err != nil {
			t.Fatalf("lookup Admin: %v", err)
		}
		q := New(fx.pool)
		if err := q.SetUserRole(ctx, SetUserRoleParams{RsUserID: fx.userRef, RoleID: adminID}); err != nil {
			t.Fatalf("assign Admin: %v", err)
		}

		resp := fx.call(t, http.MethodGet, "/auth/me/capabilities", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var eff openapi.EffectiveCapabilities
		mustDecode(t, resp, &eff)

		if eff.UserRef != fx.userRef {
			t.Errorf("user_ref=%d want %d", eff.UserRef, fx.userRef)
		}
		if eff.Role == nil || eff.Role.Name != "Admin" {
			gotName := "<nil>"
			if eff.Role != nil {
				gotName = eff.Role.Name
			}
			t.Errorf("role.name=%q want Admin", gotName)
		}
		// Admin inherits Base → effective set must include both layers.
		seen := map[string]bool{}
		for _, c := range eff.Capabilities {
			seen[c] = true
		}
		for _, want := range []string{"system.admin", "users.read", "users.write", "caps.read", "roles.read"} {
			if !seen[want] {
				t.Errorf("expected effective cap %q, set=%v", want, eff.Capabilities)
			}
		}
	})
}

// --- helpers ---------------------------------------------------------------

func seedCap(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO capabilities (code) VALUES ($1) ON CONFLICT (code) DO NOTHING`, code); err != nil {
		t.Fatalf("seed cap %q: %v", code, err)
	}
}

func seedRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, parent *[16]byte, caps ...string) [16]byte {
	t.Helper()
	var parentArg interface{}
	if parent != nil {
		parentArg = pgtype.UUID{Bytes: *parent, Valid: true}
	}
	pgRaw := pgtype.UUID{}
	if err := pool.QueryRow(ctx,
		`INSERT INTO roles (name, parent_id)
		 VALUES ($1, $2)
		 ON CONFLICT (name) DO UPDATE SET parent_id = EXCLUDED.parent_id
		 RETURNING id`,
		name, parentArg).Scan(&pgRaw); err != nil {
		t.Fatalf("seed role %q: %v", name, err)
	}
	for _, c := range caps {
		if _, err := pool.Exec(ctx,
			`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			pgRaw, c); err != nil {
			t.Fatalf("attach cap %q to role %q: %v", c, name, err)
		}
	}
	return pgRaw.Bytes
}

func assertHasAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing cap %q in %v", w, got)
		}
	}
}

func assertHasNone(t *testing.T, got []string, forbidden ...string) {
	t.Helper()
	for _, g := range got {
		for _, f := range forbidden {
			if g == f {
				t.Errorf("forbidden cap %q present in %v", f, got)
			}
		}
	}
}
