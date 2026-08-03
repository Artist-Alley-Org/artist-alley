// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// #871 — the caller's capabilities must be on the session response
// itself, on EVERY producer of CurrentUser.
//
// # Why this file exists
//
// The SPA's auth store publishes `ready` the instant /auth/me (cold
// load) or /auth/login (sign-in) resolves, and the /admin shell decides
// what to render the moment `ready` flips: the page, or a red "you
// don't have permission" panel. Capabilities used to arrive on a
// SECOND request (GET /auth/me/capabilities), which by construction
// lands after that decision — so a real administrator got the refusal
// panel, and it silently corrected itself a round-trip later.
//
// That is not a client-ordering bug and there is no client-side fix
// for it: `ready` cannot wait on a response the store has not been
// told to want, and making it wait would just move the flash into a
// longer spinner. The fix is that the answer is IN the response the
// decision is made from, which makes it a server-side invariant, which
// is what these tests pin.
//
// These are deliberately not frontend tests. A store test hands the
// store a session object and asserts it adopts the capabilities on it
// — but "a session object that carries capabilities" is precisely the
// state the bug prevented from existing, so such a test passes on the
// broken build. Same lesson as account_prefs_session_test.go: the
// observable has to be the response.

// seedSessionCapRole gives the fixture user a global role carrying
// `caps`, and returns the role id. Capability rows are created first
// because role_capabilities FKs them.
func seedSessionCapRole(t *testing.T, ctx context.Context, fx *fixture, roleName string, caps ...string) [16]byte {
	t.Helper()
	for _, c := range caps {
		seedCap(t, ctx, fx.pool, c)
	}
	roleID := seedRole(t, ctx, fx.pool, roleName, nil, caps...)
	if err := New(fx.pool).SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
		UserRef: fx.userRef,
		RoleID:  pgtype.UUID{Bytes: roleID, Valid: true},
	}); err != nil {
		t.Fatalf("SetUserGlobalRole: %v", err)
	}
	return roleID
}

// capsOf derefs the wire field, distinguishing "absent" from "empty" —
// the distinction matters here, because the client treats both as
// "holds nothing" and we want the tests to say which one shipped.
func capsOf(t *testing.T, cu openapi.CurrentUser) []string {
	t.Helper()
	if cu.Capabilities == nil {
		t.Fatal("session response omitted `capabilities` entirely — " +
			"the client has nothing to gate on until a second round-trip lands, " +
			"which is the #871 defect")
	}
	return *cu.Capabilities
}

// The sign-in path. /auth/login mints the session the SPA then boots
// on, so its response is the first and sometimes only chance to
// deliver the capability set before an admin surface renders.
func TestLogin_ResponseCarriesCapabilities(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_871_LoginCaps",
			"test.871.read", "test.871.write")

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		assertHasAll(t, capsOf(t, cu), "test.871.read", "test.871.write")
	})
}

// The cold-load path, and the one the /admin gate actually reads on a
// hard navigate to /admin/*.
func TestMe_ResponseCarriesCapabilities(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_871_MeCaps", "test.871.me")

		cookie := fx.loginAndGetCookie(t)
		resp := fx.call(t, http.MethodGet, "/auth/me", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		assertHasAll(t, capsOf(t, cu), "test.871.me")
	})
}

// One schema, several producers — the same invariant
// account_prefs_session_test.go pins for the preference fields, for
// the same reason: the defect class is not "endpoint X forgot a
// field", it is "two endpoints return CurrentUser and only one of them
// fills it in". Comparing producers is what makes a third one that
// forgets fail here rather than in a browser three sprints later.
func TestLoginAndMe_AgreeOnCapabilities(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_871_Agree", "test.871.agree")

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		loginResp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if loginResp.StatusCode != http.StatusOK {
			t.Fatalf("login status=%d body=%s", loginResp.StatusCode, readBody(loginResp))
		}
		var cookie *http.Cookie
		for _, c := range loginResp.Cookies() {
			if c.Name == SessionCookieName {
				cookie = c
				break
			}
		}
		if cookie == nil {
			t.Fatal("no rs_session cookie set")
		}
		var fromLogin openapi.CurrentUser
		mustDecode(t, loginResp, &fromLogin)

		meResp := fx.call(t, http.MethodGet, "/auth/me", nil, cookie)
		if meResp.StatusCode != http.StatusOK {
			t.Fatalf("me status=%d body=%s", meResp.StatusCode, readBody(meResp))
		}
		var fromMe openapi.CurrentUser
		mustDecode(t, meResp, &fromMe)

		if !sameCapSet(capsOf(t, fromLogin), capsOf(t, fromMe)) {
			t.Errorf("login and /auth/me disagree: login=%v me=%v",
				*fromLogin.Capabilities, *fromMe.Capabilities)
		}

		// …and both must agree with the dedicated endpoint, which is
		// still published API surface. A session field that drifts from
		// /auth/me/capabilities would be worse than no session field:
		// two answers to one question, with the UI believing whichever
		// it happened to read.
		capsResp := fx.call(t, http.MethodGet, "/auth/me/capabilities", nil, cookie)
		if capsResp.StatusCode != http.StatusOK {
			t.Fatalf("capabilities status=%d body=%s", capsResp.StatusCode, readBody(capsResp))
		}
		var eff openapi.EffectiveCapabilities
		mustDecode(t, capsResp, &eff)
		if !sameCapSet(capsOf(t, fromMe), eff.Capabilities) {
			t.Errorf("/auth/me and /auth/me/capabilities disagree: me=%v dedicated=%v",
				*fromMe.Capabilities, eff.Capabilities)
		}
	})
}

// A user with no role at all must come back with an EMPTY list, not an
// absent one. Both read as "holds nothing" at the client, so this is
// not about the gate's answer — it is about the response saying which
// it means. Absent is reserved for "the lookup failed", and collapsing
// the two would make a broken capability query indistinguishable from
// a correctly powerless account in any log or bug report.
func TestSession_CapabilitiesEmptyForRolelessUser(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// withFixture already pre-cleans user_roles, grants and
		// revokes, so the fixture user starts with nothing.
		cookie := fx.loginAndGetCookie(t)
		resp := fx.call(t, http.MethodGet, "/auth/me", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if got := capsOf(t, cu); len(got) != 0 {
			t.Errorf("roleless user got caps %v, want none", got)
		}
	})
}

// Team-scoped capabilities must NOT leak into the session field.
//
// The wire field is a flat list of codes with nowhere to say "…but
// only inside team X", so a scoped code flattened into it reads as
// global — and the UI would then offer a control that 403s on click,
// which is the exact failure the admin gate exists to prevent. The
// dedicated endpoint has always been global-only for this reason; the
// session field inherits the rule.
func TestSession_CapabilitiesOmitTeamScoped(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedCap(t, ctx, fx.pool, "test.871.global")
		seedCap(t, ctx, fx.pool, "test.871.scoped")
		globalRole := seedRole(t, ctx, fx.pool, "test_871_ScopeGlobal", nil, "test.871.global")
		scopedRole := seedRole(t, ctx, fx.pool, "test_871_ScopeTeam", nil, "test.871.scoped")
		team := seedTeam(t, ctx, fx.pool, "871scope")

		q := New(fx.pool)
		if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
			UserRef: fx.userRef,
			RoleID:  pgtype.UUID{Bytes: globalRole, Valid: true},
		}); err != nil {
			t.Fatalf("SetUserGlobalRole: %v", err)
		}
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO user_roles (user_ref, role_id, team_id) VALUES ($1, $2, $3)`,
			fx.userRef, pgtype.UUID{Bytes: scopedRole, Valid: true}, team,
		); err != nil {
			t.Fatalf("assign team-scoped role: %v", err)
		}

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		loginResp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if loginResp.StatusCode != http.StatusOK {
			t.Fatalf("login status=%d body=%s", loginResp.StatusCode, readBody(loginResp))
		}
		var cookie *http.Cookie
		for _, c := range loginResp.Cookies() {
			if c.Name == SessionCookieName {
				cookie = c
				break
			}
		}
		if cookie == nil {
			t.Fatal("no rs_session cookie set")
		}
		var fromLogin openapi.CurrentUser
		mustDecode(t, loginResp, &fromLogin)

		meResp := fx.call(t, http.MethodGet, "/auth/me", nil, cookie)
		if meResp.StatusCode != http.StatusOK {
			t.Fatalf("me status=%d body=%s", meResp.StatusCode, readBody(meResp))
		}
		var fromMe openapi.CurrentUser
		mustDecode(t, meResp, &fromMe)

		// Both producers, because they resolve the set by different
		// routes — login queries the DB, /auth/me reuses what the
		// resolver middleware already put on the Identity — and a rule
		// that only one of them follows is not a rule.
		for name, cu := range map[string]openapi.CurrentUser{"login": fromLogin, "me": fromMe} {
			got := capsOf(t, cu)
			assertHasAll(t, got, "test.871.global")
			if hasCap(got, "test.871.scoped") {
				t.Errorf("%s leaked a team-scoped capability into the session field: %v", name, got)
			}
		}
	})
}

// Anonymous callers must not come away holding anything. /auth/me
// refuses them outright, so the guarantee is that there is no
// CurrentUser to carry capabilities at all — checked here rather than
// assumed, because the whole change is "the session response now
// hands out capabilities" and the one caller who must never receive
// any is the one with no session.
func TestAnonymous_GetsNoSessionCapabilities(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_871_AnonGuard", "test.871.anon")

		resp := fx.call(t, http.MethodGet, "/auth/me", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous /auth/me status=%d want 401, body=%s",
				resp.StatusCode, readBody(resp))
		}
		if body := readBody(resp); strings.Contains(body, "test.871.anon") {
			t.Errorf("anonymous 401 body leaked a capability: %s", body)
		}
	})
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func sameCapSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, v := range a {
		set[v]++
	}
	for _, v := range b {
		set[v]--
		if set[v] < 0 {
			return false
		}
	}
	return true
}
