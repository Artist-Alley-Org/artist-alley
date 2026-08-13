// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1023 — OwnerDisplayNameSQL must agree with users.PlaceholderOwnerName,
// always.
//
// The display-name ladder has been transcribed twice before. #557 built
// users.ResolveDisplayName because the first copy dropped the
// authenticated-only `fullname` rung and nearly put real names on public
// cards; #1023 is the second, where THREE placeholder queries resolved
// the name in hand-written SQL that never consulted `hide_from_anonymous`
// at all, so an owner who took ADR 0024's opt-out had their username
// rendered to an anonymous caller.
//
// One expression of the rule lives in Go (users.PlaceholderOwnerName) and
// one in SQL, for the reason OwnerDisplayNameSQL's doc gives — the name
// rides the query that is already reading the asset row, and pulling it
// into Go would cost a round trip per page on exactly the pages that
// carry placeholders. This test is the price of that exception, and it is
// the same price TestContentReadableSQL_MatchesGo and
// TestFieldsReadableSQL_MatchesGo pay: drive every rung through BOTH
// forms against a real Postgres and fail on the first disagreement.
//
// It imports `users` in the TEST binary only. The production `visibility`
// package deliberately does not depend on it — `users` reaches auth,
// federation and openapi, and this package is the low-level gate those
// sit above.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/users"
)

// odnBase is the ref block this file owns. Distinct from ctOwner's so a
// parallel content test cannot collide with it.
const odnBase int64 = 4340100

// odnUser is one seeded (user, profile) pair — the four columns the
// ladder reads, in the two representations they can arrive in (a NULL
// column and an empty-string column must both fall through).
type odnUser struct {
	name        string
	username    *string
	fullname    *string
	displayName string
	// hasProfile false means NO user_profiles row at all: the LEFT JOIN
	// yields NULLs, which is a different path through the SQL than a
	// profile row with empty values, and the Go form must agree on both.
	hasProfile bool
	hidden     bool
}

func sp(s string) *string { return &s }

func odnSeed(t *testing.T, pool *pgxpool.Pool) []struct {
	odnUser
	ref int64
} {
	t.Helper()
	ctx := context.Background()

	fixtures := []odnUser{
		{name: "display+full+user, visible", username: sp("kb"), fullname: sp("Kenneth B"), displayName: "Blossom", hasProfile: true},
		{name: "display+full+user, OPTED OUT", username: sp("kb2"), fullname: sp("Kenneth B"), displayName: "Blossom", hasProfile: true, hidden: true},
		{name: "no display, full+user, visible", username: sp("kb3"), fullname: sp("Kenneth B"), displayName: "", hasProfile: true},
		{name: "no display, full+user, OPTED OUT", username: sp("kb4"), fullname: sp("Kenneth B"), displayName: "", hasProfile: true, hidden: true},
		{name: "username only, no profile row", username: sp("kb5"), fullname: nil, displayName: "", hasProfile: false},
		{name: "username only, OPTED OUT", username: sp("kb6"), fullname: nil, displayName: "", hasProfile: true, hidden: true},
		{name: "fullname only (no username)", username: nil, fullname: sp("Kenneth B"), displayName: "", hasProfile: true},
		{name: "fullname only, OPTED OUT", username: nil, fullname: sp("Kenneth B"), displayName: "", hasProfile: true, hidden: true},
		{name: "empty-string username + fullname", username: sp(""), fullname: sp("Kenneth B"), displayName: "", hasProfile: true},
		{name: "empty-string fullname, username set", username: sp("kb7"), fullname: sp(""), displayName: "", hasProfile: true},
		// Rung 4 territory: nothing to render at all. ResolveDisplayName
		// would answer "user {ref}"; the placeholder rule answers "".
		{name: "nothing resolvable", username: nil, fullname: nil, displayName: "", hasProfile: true},
		{name: "nothing resolvable, no profile row", username: nil, fullname: nil, displayName: "", hasProfile: false},
		{name: "display only, no username", username: nil, fullname: nil, displayName: "Blossom", hasProfile: true},
	}

	out := make([]struct {
		odnUser
		ref int64
	}, 0, len(fixtures))
	refs := make([]int64, 0, len(fixtures))
	for i, u := range fixtures {
		ref := odnBase + int64(i)
		refs = append(refs, ref)
		if _, err := pool.Exec(ctx,
			`INSERT INTO "user" (ref, username, fullname) VALUES ($1, $2, $3)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username, fullname = EXCLUDED.fullname`,
			ref, u.username, u.fullname); err != nil {
			t.Fatalf("seed user %q: %v", u.name, err)
		}
		if u.hasProfile {
			if _, err := pool.Exec(ctx,
				`INSERT INTO user_profiles (user_ref, display_name, hide_from_anonymous)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (user_ref) DO UPDATE
				    SET display_name = EXCLUDED.display_name,
				        hide_from_anonymous = EXCLUDED.hide_from_anonymous`,
				ref, u.displayName, u.hidden); err != nil {
				t.Fatalf("seed profile %q: %v", u.name, err)
			}
		}
		out = append(out, struct {
			odnUser
			ref int64
		}{u, ref})
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM user_profiles WHERE user_ref = ANY($1::BIGINT[])`, refs)
		_, _ = pool.Exec(bg, `DELETE FROM "user" WHERE ref = ANY($1::BIGINT[])`, refs)
	})
	return out
}

// odnAsk evaluates the SQL form for one owner ref and one caller kind.
// The ref is rendered as a LITERAL rather than bound, because the
// fragment under test takes an EXPRESSION naming the owning ref — that
// is how the splice sites use it (`assets.owner_user_ref`) — and a
// bound int is not an expression the surrounding query owns.
func odnAsk(t *testing.T, pool *pgxpool.Pool, ref int64, anonymous bool) string {
	t.Helper()
	sql := `SELECT ` + OwnerDisplayNameSQL(strconv.FormatInt(ref, 10)+`::BIGINT`, anonymous)
	var got string
	if err := pool.QueryRow(context.Background(), sql).Scan(&got); err != nil {
		t.Fatalf("OwnerDisplayNameSQL: %v\nSQL: %s", err, sql)
	}
	return got
}

func TestOwnerDisplayNameSQL_MatchesGo(t *testing.T) {
	pool := contentPool(t)
	seeded := odnSeed(t, pool)

	for _, s := range seeded {
		for _, anonymous := range []bool{false, true} {
			// The Go form reads what a profile row that is ABSENT
			// resolves to through the LEFT JOIN: no display name and no
			// opt-out.
			displayName, hidden := s.displayName, s.hidden
			if !s.hasProfile {
				displayName, hidden = "", false
			}
			want := users.PlaceholderOwnerName(displayName, s.fullname, s.username, hidden, anonymous)
			got := odnAsk(t, pool, s.ref, anonymous)
			if got != want {
				t.Errorf("%s / anonymous=%v: SQL says %q, Go says %q — the two expressions of the "+
					"placeholder's owner-name rule have drifted (display=%q fullname=%v username=%v hidden=%v)",
					s.name, anonymous, got, want, s.displayName, s.fullname, s.username, s.hidden)
			}
		}
	}
}

// TestOwnerDisplayNameSQL_OptOutIsAbsence is the positive control the
// agreement test above cannot be: it pins the two verdicts that matter,
// on ONE row, and states them in the language of the defect rather than
// in the language of the ladder.
//
// An agreement test only proves the two forms say the same thing. If
// both had omitted `hide_from_anonymous` it would still pass — which is
// exactly the state #1023 was filed against, since the Go form was never
// consulted at all.
func TestOwnerDisplayNameSQL_OptOutIsAbsence(t *testing.T) {
	pool := contentPool(t)
	seeded := odnSeed(t, pool)

	var optedOut, visible int64
	for _, s := range seeded {
		if s.name == "username only, OPTED OUT" {
			optedOut = s.ref
		}
		if s.name == "username only, no profile row" {
			visible = s.ref
		}
	}
	if optedOut == 0 || visible == 0 {
		t.Fatal("fixture lookup failed — the seeds this test names were renamed")
	}

	if got := odnAsk(t, pool, optedOut, true); got != "" {
		t.Errorf("anonymous caller, owner opted out of anonymous exposure: got %q, want \"\" — "+
			"ADR 0024's opt-out must not be defeated by a placeholder's JOIN (#1023)", got)
	}
	// The SAME row, the other caller. Without this the test above passes
	// on a fragment that returns "" for everyone.
	if got := odnAsk(t, pool, optedOut, false); got != "kb6" {
		t.Errorf("authenticated caller, owner opted out: got %q, want %q — the opt-out is "+
			"anonymous-only (ADR 0070 §3) and must not withhold from a signed-in caller", got, "kb6")
	}
	// And an owner who did NOT opt out is still named to an anonymous
	// caller, or the fix is just "withhold everything".
	if got := odnAsk(t, pool, visible, true); got != "kb5" {
		t.Errorf("anonymous caller, owner did NOT opt out: got %q, want %q — the placeholder still "+
			"names its owner, which is what #881's request-access attaches to", got, "kb5")
	}
}

// TestOwnerDisplayNameSQL_NeverEmitsTheRef pins the ONE place this rule
// deliberately diverges from users.ResolveDisplayName: rung 4.
//
// ResolveDisplayName's last resort is "user {ref}", and it exists so a
// user row is never rendered blank. A placeholder is a different
// contract: assets.withheldAsset omits `owner_user_ref` on purpose,
// calling the ref "a second way to ask", so a fallback that spelled the
// ref into the name column would hand back what the payload withholds.
func TestOwnerDisplayNameSQL_NeverEmitsTheRef(t *testing.T) {
	pool := contentPool(t)
	seeded := odnSeed(t, pool)

	for _, s := range seeded {
		if s.name != "nothing resolvable" && s.name != "nothing resolvable, no profile row" {
			continue
		}
		for _, anonymous := range []bool{false, true} {
			if got := odnAsk(t, pool, s.ref, anonymous); got != "" {
				t.Errorf("%s / anonymous=%v: got %q, want \"\" — a placeholder must never carry "+
					"the owner's ref, and ResolveDisplayName's rung 4 would", s.name, anonymous, got)
			}
		}
	}

	// An asset with NO owner at all resolves through the same expression
	// and must also be "", not NULL — every caller scans this column into
	// a plain string.
	if got := odnAsk(t, pool, -1, true); got != "" {
		t.Errorf("unowned asset: got %q, want \"\"", got)
	}
}
