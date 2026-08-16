// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// The stored-preference fields on CurrentUser — language, theme,
// default_views — must be present on EVERY response that carries a
// CurrentUser, not only on /auth/me.
//
// # Why this file exists
//
// #706 shipped the browse-view seeding and #677 the theme adoption,
// both reading their values off the session the client already holds.
// Both worked on a full page load and neither worked immediately after
// signing in, because /auth/login returns the same `CurrentUser`
// schema from a different code path — one that built the struct from
// the Identity alone and never joined the profile or preferences rows.
// The client's stores then saw "this account has no preference",
// correctly did nothing, and the user got built-in defaults until they
// happened to reload.
//
// Signing in is precisely the moment a cross-device preference has to
// prove itself: it is the first thing a user does on a second machine.
// So it gets its own tests, and they assert the SIGN-IN response
// rather than a session that is already established.
//
// The frontend store tests are deliberately not the guard here. They
// passed throughout — they hand the store a session object that
// already carries the values, which is the state this bug prevented
// from ever existing. A test that constructs the input the bug
// destroys cannot see the bug. The observable had to move to the
// response.

// clearAccountPrefs removes the fixture user's profile + preferences
// rows.
//
// Called at the START of every test here rather than registered as
// cleanup at the end, and that is not a style preference. withFixture
// `defer pool.Close()`s around its call to the test body, so the pool
// is already closed by the time any t.Cleanup registered inside the
// body runs — a cleanup DELETE there executes against a closed pool,
// fails, and gets swallowed by the `_, _ =` convention these tests use
// for best-effort teardown. It looks like isolation and provides none.
//
// Found the honest way: the "no stored preferences" test below failed
// on the previous test's leftover row. Pre-cleaning makes each test
// deterministic no matter what ran before it, including a previous
// run that was killed mid-way.
func clearAccountPrefs(t *testing.T, ctx context.Context, fx *fixture) {
	t.Helper()
	for _, sql := range []string{
		`DELETE FROM user_preferences WHERE user_ref = $1`,
		`DELETE FROM user_profiles    WHERE user_ref = $1`,
	} {
		if _, err := fx.pool.Exec(ctx, sql, fx.userRef); err != nil {
			t.Fatalf("pre-clean %s: %v", sql, err)
		}
	}
}

// seedAccountPrefs writes a profile + preferences row for the fixture
// user. Written through fx.pool rather than a transaction because the
// handler reads on its own connection.
func seedAccountPrefs(t *testing.T, ctx context.Context, fx *fixture, language, theme, viewsJSON string) {
	t.Helper()
	clearAccountPrefs(t, ctx, fx)
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO user_profiles (user_ref, language, theme)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_ref) DO UPDATE
		SET language = EXCLUDED.language, theme = EXCLUDED.theme`,
		fx.userRef, language, theme,
	); err != nil {
		t.Fatalf("seed user_profiles: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO user_preferences (user_ref, default_views)
		VALUES ($1, $2::jsonb)
		ON CONFLICT (user_ref) DO UPDATE
		SET default_views = EXCLUDED.default_views`,
		fx.userRef, viewsJSON,
	); err != nil {
		t.Fatalf("seed user_preferences: %v", err)
	}
}

// The regression test for the review finding: sign in, and the
// response must already carry what the browse store and the theme
// store need. No second round-trip, because a second round-trip lands
// after the page has painted.
func TestLogin_ResponseCarriesAccountPreferences(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedAccountPrefs(t, ctx, fx, "es", "system",
			`{"browse_layout":"list","home_tab":"following","browse_sort":"oldest"}`)

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)

		if cu.Theme == nil {
			t.Fatal("login response omitted theme — the theme cannot follow the user to this device")
		}
		if *cu.Theme != openapi.CurrentUserTheme("system") {
			t.Errorf("theme=%q want system", *cu.Theme)
		}
		// `system` specifically, not just any non-nil: it is the value
		// that used to be indistinguishable from "never chose", so it
		// is the one most likely to be flattened by a later change.

		if cu.DefaultViews == nil {
			t.Fatal("login response omitted default_views — browse opens on the built-in defaults")
		}
		if cu.DefaultViews.BrowseLayout == nil || *cu.DefaultViews.BrowseLayout != "list" {
			t.Errorf("browse_layout=%v want list", cu.DefaultViews.BrowseLayout)
		}
		if cu.DefaultViews.HomeTab == nil || *cu.DefaultViews.HomeTab != "following" {
			t.Errorf("home_tab=%v want following", cu.DefaultViews.HomeTab)
		}
		if cu.DefaultViews.BrowseSort == nil || *cu.DefaultViews.BrowseSort != "oldest" {
			t.Errorf("browse_sort=%v want oldest", cu.DefaultViews.BrowseSort)
		}

		// `language` had the identical hole and nobody had noticed,
		// because a locale that only applies on the second page load
		// reads as a slow render rather than a bug. Pinned so the next
		// person to touch this path cannot reintroduce it either.
		if cu.Language == nil || *cu.Language != "es" {
			t.Errorf("language=%v want es", cu.Language)
		}
	})
}

// One schema, two producers. The defect was not "login is missing a
// field" so much as "two endpoints return CurrentUser and only one of
// them fills it in", and that is the shape that will recur the next
// time an endpoint starts returning a session.
//
// Comparing the two responses states the invariant directly, so a
// third producer that forgets the join fails here rather than in a
// browser three sprints later.
func TestLoginAndMe_AgreeOnAccountPreferences(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedAccountPrefs(t, ctx, fx, "fr", "light", `{"browse_layout":"masonry"}`)

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

		deref := func(s *string) string {
			if s == nil {
				return "<nil>"
			}
			return *s
		}
		if deref(fromLogin.Language) != deref(fromMe.Language) {
			t.Errorf("language: login=%s me=%s", deref(fromLogin.Language), deref(fromMe.Language))
		}
		themeOf := func(cu openapi.CurrentUser) string {
			if cu.Theme == nil {
				return "<nil>"
			}
			return string(*cu.Theme)
		}
		if themeOf(fromLogin) != themeOf(fromMe) {
			t.Errorf("theme: login=%s me=%s", themeOf(fromLogin), themeOf(fromMe))
		}
		layoutOf := func(cu openapi.CurrentUser) string {
			if cu.DefaultViews == nil || cu.DefaultViews.BrowseLayout == nil {
				return "<nil>"
			}
			return string(*cu.DefaultViews.BrowseLayout)
		}
		if layoutOf(fromLogin) != layoutOf(fromMe) {
			t.Errorf("browse_layout: login=%s me=%s", layoutOf(fromLogin), layoutOf(fromMe))
		}
		if layoutOf(fromLogin) != "masonry" {
			t.Errorf("browse_layout=%s want masonry — both agreed, on the wrong value", layoutOf(fromLogin))
		}
	})
}

// The read-side sanitizing has to apply on the sign-in path too.
// Sharing one helper is what guarantees that, and this pins it: a
// second copy of the join that forgot to sanitize would hand the
// browse store a mode it cannot render, leaving the view switcher with
// no active segment.
func TestLogin_DropsStaleViewSelections(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedAccountPrefs(t, ctx, fx, "", "",
			`{"home_tab":"trending","browse_layout":"masonry","browse_sort":"popular"}`)

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a stale stored value must not fail the login: status=%d body=%s",
				resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.DefaultViews == nil {
			t.Fatal("the still-valid browse_layout should have survived")
		}
		if cu.DefaultViews.HomeTab != nil {
			t.Errorf("stale home_tab must be dropped, got %q", *cu.DefaultViews.HomeTab)
		}
		if cu.DefaultViews.BrowseSort != nil {
			t.Errorf("stale browse_sort must be dropped, got %q", *cu.DefaultViews.BrowseSort)
		}
		if cu.DefaultViews.BrowseLayout == nil || *cu.DefaultViews.BrowseLayout != "masonry" {
			t.Errorf("valid browse_layout must survive, got %v", cu.DefaultViews.BrowseLayout)
		}
	})
}

// A user with no profile and no preferences row must still log in.
// The join is a render hint on the call that gates the whole app, so
// "nothing stored" is a normal state, not an error.
func TestLogin_NoStoredPreferencesStillSucceeds(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		clearAccountPrefs(t, ctx, fx)

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.DefaultViews != nil {
			t.Errorf("default_views should be omitted entirely, got %+v", *cu.DefaultViews)
		}
		if cu.Theme != nil {
			t.Errorf("theme should be omitted entirely, got %q", *cu.Theme)
		}
	})
}

// seedTeamRail writes only the browse_rail blob, leaving every other
// preference at its default. Separate from seedAccountPrefs because the
// interesting case for #1113 is an account that has curated its RAIL
// and nothing else — which is also the account whose /auth/me used to
// carry no preferences object at all.
func seedTeamRail(t *testing.T, ctx context.Context, fx *fixture, railJSON string) {
	t.Helper()
	clearAccountPrefs(t, ctx, fx)
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO user_preferences (user_ref, browse_rail)
		VALUES ($1, $2::jsonb)
		ON CONFLICT (user_ref) DO UPDATE
		SET browse_rail = EXCLUDED.browse_rail`,
		fx.userRef, railJSON,
	); err != nil {
		t.Fatalf("seed browse_rail: %v", err)
	}
}

// The rail's curation has to arrive on the SESSION, before first paint.
// It decides what the rail draws, so learning it from a later fetch
// means painting the uncurated rail and rearranging it in front of the
// reader (#1113) — the same first-paint race #706 closed for
// default_views, which is why this asserts the same two producers.
func TestSession_CarriesTeamRailCuration(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		const hidden = "3b6770c6-b35a-90d1-88c7-e35d00136825"
		const first = "988ed4d0-4b3e-66a9-5606-c056c32bee04"
		const second = "c0e2652d-65b1-cb0f-8c25-3260a5b8834d"
		seedTeamRail(t, ctx, fx, `{"hidden_team_ids":["`+hidden+`"],`+
			`"team_order":["`+first+`","`+second+`"]}`)

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.BrowseRail == nil {
			t.Fatal("login response omitted browse_rail — the rail paints uncurated and then jumps")
		}
		if cu.BrowseRail.HiddenTeamIds == nil || len(*cu.BrowseRail.HiddenTeamIds) != 1 ||
			(*cu.BrowseRail.HiddenTeamIds)[0].String() != hidden {
			t.Errorf("hidden_team_ids=%v want [%s]", cu.BrowseRail.HiddenTeamIds, hidden)
		}
		// Sequence, not membership: team_order's entire content IS the
		// order, so a set assertion would pass on a handler that sorted.
		if cu.BrowseRail.TeamOrder == nil || len(*cu.BrowseRail.TeamOrder) != 2 ||
			(*cu.BrowseRail.TeamOrder)[0].String() != first ||
			(*cu.BrowseRail.TeamOrder)[1].String() != second {
			t.Errorf("team_order=%v want [%s %s]", cu.BrowseRail.TeamOrder, first, second)
		}
	})
}

// Omitted for every account that has not curated its rail — which is
// every account today. Same "absent means the build's default" contract
// feed_filters carries, and the reason this change alters no existing
// session response.
func TestSession_OmitsTeamRailWhenUncurated(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		seedTeamRail(t, ctx, fx, `{}`)

		resp := fx.call(t, http.MethodPost, "/auth/login",
			openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.BrowseRail != nil {
			t.Errorf("browse_rail should be omitted entirely, got %+v", *cu.BrowseRail)
		}
	})
}
