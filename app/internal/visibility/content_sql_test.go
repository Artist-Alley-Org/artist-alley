// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #899 — ContentReadableSQL must agree with ContentReadable, always.
//
// Two expressions of one authorization rule is the defect ADR 0063
// exists to prevent, and the SQL form exists only because the AGGREGATE
// surfaces (search facets, suggest completions) reduce many rows to one
// number or one string and have no per-row Go step to decide in. This
// test is the price of that exception: it drives every combination of
// (tier × owner × caller × caps × team membership) through BOTH forms
// against a real Postgres and fails on the first disagreement.
//
// If you edit ContentReadable and this goes red, edit ContentReadableSQL
// — that is what the test is for.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// crsAsk runs the SQL form for one asset and one caller.
func crsAsk(t *testing.T, pool *pgxpool.Pool, caller Caller, caps ContentCaps, id uuid.UUID) bool {
	t.Helper()
	sql := `SELECT EXISTS (SELECT 1 FROM assets a WHERE a.id = $1` +
		ContentReadableSQL("a", strconv.FormatInt(caller.UserRef, 10), caps) + `)`
	var ok bool
	if err := pool.QueryRow(context.Background(), sql, id).Scan(&ok); err != nil {
		t.Fatalf("ContentReadableSQL: %v\nSQL: %s", err, sql)
	}
	return ok
}

// crsRow reads back the columns the Go form needs, so both forms are
// answering about the SAME row rather than about the fixture's
// intentions.
func crsRow(t *testing.T, pool *pgxpool.Pool, caller Caller, id uuid.UUID) (string, *int64, bool) {
	t.Helper()
	var (
		sens   string
		owner  *int64
		member bool
	)
	err := pool.QueryRow(context.Background(), `
		SELECT sensitivity, owner_user_ref,
		       (team_id IS NOT NULL AND EXISTS (
		            SELECT 1 FROM team_memberships tm
		             WHERE tm.team_id = assets.team_id AND tm.user_ref = $2::BIGINT))
		  FROM assets WHERE id = $1`, id, caller.UserRef).Scan(&sens, &owner, &member)
	if err != nil {
		t.Fatalf("crsRow: %v", err)
	}
	return sens, owner, member
}

func TestContentReadableSQL_MatchesGo(t *testing.T) {
	pool := contentPool(t)

	team := seedTeamWithMember(t, pool, ctMember)
	otherTeam := seedTeamWithMember(t, pool, ctStranger)

	type fixture struct {
		name string
		id   uuid.UUID
	}
	fixtures := []fixture{
		{"public", seedContentAsset(t, pool, "public", nil, false)},
		{"restricted", seedContentAsset(t, pool, "restricted", nil, false)},
		{"embargo", seedContentAsset(t, pool, "embargo", nil, false)},
		{"team (caller's team)", seedContentAsset(t, pool, "team", &team, false)},
		{"team (someone else's team)", seedContentAsset(t, pool, "team", &otherTeam, false)},
		// A team-tier asset with NO team is the fail-closed case the Go
		// form spells out; the SQL form must not admit it either.
		{"team (no team_id)", seedContentAsset(t, pool, "team", nil, false)},
		// An ownerless row must never match — including for the
		// anonymous sentinel, whose UserRef is 0.
		{"public ownerless", seedContentAsset(t, pool, "public", nil, true)},
		{"restricted ownerless", seedContentAsset(t, pool, "restricted", nil, true)},
	}

	owner, stranger, member := ctOwner, ctStranger, ctMember
	callers := []struct {
		name   string
		caller Caller
	}{
		{"anonymous", NewCaller(nil)},
		{"owner", NewCaller(&owner)},
		{"stranger", NewCaller(&stranger)},
		{"team member", NewCaller(&member)},
	}
	capsCases := []struct {
		name string
		caps ContentCaps
	}{
		{"no caps", ContentCaps{}},
		{"content.read.all", ContentCaps{ContentReadAll: true}},
		{"system.admin", ContentCaps{SystemAdmin: true}},
	}

	for _, f := range fixtures {
		for _, c := range callers {
			for _, cc := range capsCases {
				sens, rowOwner, isMember := crsRow(t, pool, c.caller, f.id)
				want := ContentReadable(sens, rowOwner, c.caller, cc.caps.Checker(), isMember)
				got := crsAsk(t, pool, c.caller, cc.caps, f.id)
				if got != want {
					t.Errorf("%s / %s / %s: SQL says %v, Go says %v — the two expressions of "+
						"the content plane have drifted (sensitivity=%q owner=%v member=%v)",
						f.name, c.name, cc.name, got, want, sens, rowOwner, isMember)
				}
			}
		}
	}
}

// TestContentCaps_RoundTrip pins the two properties the search cache
// depends on: Checker() answers only the two content-plane codes, and
// CacheKey() distinguishes every combination. A CacheKey that collided
// would serve one caller's redaction to another.
func TestContentCaps_RoundTrip(t *testing.T) {
	all := []ContentCaps{
		{},
		{ContentReadAll: true},
		{SystemAdmin: true},
		{SystemAdmin: true, ContentReadAll: true},
	}
	seen := map[string]bool{}
	for _, c := range all {
		k := c.CacheKey()
		if seen[k] {
			t.Errorf("CacheKey collision at %+v (key %q) — two capability states sharing a "+
				"cache key is one caller reading another's redaction", c, k)
		}
		seen[k] = true
		if c.Checker()(SystemAdmin) != c.SystemAdmin {
			t.Errorf("%+v: Checker disagrees on system.admin", c)
		}
		if c.Checker()(ContentReadAll) != c.ContentReadAll {
			t.Errorf("%+v: Checker disagrees on content.read.all", c)
		}
		if c.Checker()("assets.publish") {
			t.Errorf("%+v: Checker answered true for an unrelated capability — this type is the "+
				"content plane's view of a caller, never a general capability oracle", c)
		}
	}

	// ResolveContentCaps(nil) is the anonymous path and must admit
	// nothing.
	if z := ResolveContentCaps(nil); z.SystemAdmin || z.ContentReadAll {
		t.Errorf("ResolveContentCaps(nil) = %+v, want the zero value", z)
	}
}
