// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
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
// absent one, and must be reported `resolved` — because the lookup
// genuinely succeeded and the honest answer genuinely is "nothing".
//
// This is the case #956's degraded state must never be confused with,
// in EITHER direction. The client renders "you don't have permission"
// here and must go on doing so; if this account started rendering "we
// could not determine your rights" the fix would have made the product
// worse, by turning a true statement into a retry loop that never
// resolves.
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
		if cu.CapabilitiesStatus != openapi.Resolved {
			t.Errorf("roleless user got capabilities_status=%q, want %q — "+
				"a powerless account is a RESOLVED answer, not a failed lookup (#956)",
				cu.CapabilitiesStatus, openapi.Resolved)
		}
	})
}

// #956 — a failed capability lookup must be distinguishable on the wire
// from an account that holds nothing.
//
// # Why this is a direct call rather than an HTTP round-trip
//
// Every other test in this file drives the real router, and for good
// reason (account_prefs_session_test.go's lesson: the observable has to
// be the response). This one cannot. The defect is what the handler
// does when the capability QUERY fails while the session itself
// succeeds, and both run on the same pool — there is no way to break
// one from the outside without breaking the other, so an HTTP-level
// version of this test would be testing a 500, which is a different
// bug. The seam is the function.
//
// What makes that acceptable is that the two halves of the contract are
// pinned in different places and meet in the middle: this asserts a
// failing resolver produces `unavailable` with NO capability list, and
// scripts/dogfood/ui/tests/standalone/admin-caps-871.spec.ts asserts
// that a session carrying exactly that shape renders the degraded panel
// and no admin controls. Neither half proves the behaviour alone.
func TestHydrateCapabilities_FailedLookupReportsUnavailable(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// A closed pool fails every query deterministically, with no
		// network and no timing. That is the whole stimulus: any
		// resolver error at all, not one specific pg error code.
		broken := openPool(t, os.Getenv("AA_DB_PASSWORD"))
		broken.Close()

		h := &Handler{Pool: broken}
		cu := openapi.CurrentUser{Ref: fx.userRef, Username: fx.username, AuthMethod: "session"}
		h.hydrateCapabilities(ctx, fx.userRef, &cu)

		if cu.CapabilitiesStatus != openapi.Unavailable {
			t.Errorf("failed lookup reported capabilities_status=%q, want %q — "+
				"this is the #956 defect: the client cannot tell a broken resolver "+
				"from a powerless account, so it tells an administrator they have "+
				"no permission", cu.CapabilitiesStatus, openapi.Unavailable)
		}
		// The list must be ABSENT, not empty. An empty array alongside
		// `unavailable` would be a claim about the account that this
		// response is in no position to make.
		if cu.Capabilities != nil {
			t.Errorf("failed lookup still published a capability list (%v); "+
				"it must omit the key entirely", *cu.Capabilities)
		}
	})
}

// The success path of the same function, so the test above cannot pass
// by the status being hardwired to `unavailable`. Two assertions that
// can only both hold if the branch is real.
func TestHydrateCapabilities_SuccessfulLookupReportsResolved(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_956_Resolved", "test.956.ok")

		h := &Handler{Pool: fx.pool}
		cu := openapi.CurrentUser{Ref: fx.userRef, Username: fx.username, AuthMethod: "session"}
		h.hydrateCapabilities(ctx, fx.userRef, &cu)

		if cu.CapabilitiesStatus != openapi.Resolved {
			t.Fatalf("working lookup reported capabilities_status=%q, want %q",
				cu.CapabilitiesStatus, openapi.Resolved)
		}
		assertHasAll(t, capsOf(t, cu), "test.956.ok")
	})
}

// `capabilities_status` is a REQUIRED wire field whose Go zero value
// ("") is not a member of its enum, so a producer that forgets to set
// it ships a response no client can read — and the client's fail-closed
// mapping (mapCapsStatus) would read it as `unavailable`, i.e. a
// healthy admin session rendering the degraded panel forever.
//
// Same "one schema, several producers" reasoning as
// TestLoginAndMe_AgreeOnCapabilities above, and the same reason it is
// worth its own test: the defect class is not "endpoint X forgot a
// field", it is "N endpoints return CurrentUser and only some of them
// fill it in".
func TestSession_CapabilitiesStatusOnEveryProducer(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedSessionCapRole(t, ctx, fx, "test_956_Producers", "test.956.producers")

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

		for name, cu := range map[string]openapi.CurrentUser{"login": fromLogin, "me": fromMe} {
			if !cu.CapabilitiesStatus.Valid() {
				t.Errorf("%s returned capabilities_status=%q, which is not a member of the enum "+
					"— the client reads anything it does not recognise as `unavailable`, so this "+
					"ships a permanent degraded panel to a healthy session (#956)",
					name, cu.CapabilitiesStatus)
				continue
			}
			if cu.CapabilitiesStatus != openapi.Resolved {
				t.Errorf("%s returned capabilities_status=%q on a healthy lookup, want %q",
					name, cu.CapabilitiesStatus, openapi.Resolved)
			}
		}
	})
}

// The raw JSON, not the decoded struct. `capabilities_status` is
// required, so it must be a key on the wire — and a decode into a Go
// struct with a non-pointer field cannot tell an absent key from an
// empty one, which is exactly the distinction the client's fail-closed
// mapping turns on.
func TestSession_CapabilitiesStatusIsOnTheWire(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		cookie := fx.loginAndGetCookie(t)
		resp := fx.call(t, http.MethodGet, "/auth/me", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatalf("decode raw: %v", err)
		}
		got, ok := raw["capabilities_status"]
		if !ok {
			t.Fatal("/auth/me omitted `capabilities_status` from the response body — " +
				"the client reads an absent status as `unavailable` and renders the " +
				"degraded panel to every session (#956)")
		}
		if string(got) != `"resolved"` {
			t.Errorf("capabilities_status on the wire = %s, want \"resolved\"", got)
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
