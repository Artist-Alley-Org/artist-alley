// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #902 — FieldsReadableSQL must agree with FieldsReadable, always.
//
// The SQL twin exists because a full-text MATCH decides in SQL whether
// a row is returned at all, so there is no per-row Go step downstream of
// it to withhold in. That is the exception ContentReadableSQL's doc
// carves out, and this test is its price — the exhaustive twin of
// TestContentReadableSQL_MatchesGo.
//
// It drives MORE dimensions than that test does, and the extra ones are
// deliberate. FieldsReadable is PreviewReadable OR CallerMayMutate, so
// it depends on caller↔row RELATIONSHIPS — ownership, team membership,
// and a team-scoped `assets.admin` grant — which is exactly why the
// rejected "one document per sensitivity tier" design could not have
// expressed it, and therefore exactly where a wrong implementation
// hides. So the matrix includes:
//
//   - OWNERSHIP: an owned row, a stranger's row, an OWNERLESS row (the
//     NULL-owner guard) and the anonymous sentinel (ref 0, which must
//     never match an owner_user_ref of 0);
//   - MUTATION SCOPE: no scope, a GLOBAL assets.admin, a scope on the
//     row's own team, a scope on some other team, and a scope
//     containing only uuid.Nil (which MayMutate refuses rather than
//     treating as "no scope required");
//   - the ANONYMOUS-only status conjuncts, which need draft / archived
//     / still-processing rows to fire at all.
//
// If you edit FieldsReadable or PreviewReadable and this goes red, edit
// FieldsReadableSQL — that is what the test is for.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	frOwner    int64 = 9021001
	frStranger int64 = 9021002
	frMember   int64 = 9021003
)

// frSeedAsset plants one asset with every column FieldsReadable
// consults set explicitly — no reliance on table defaults, because a
// default that changes would silently stop exercising an arm.
func frSeedAsset(
	t *testing.T,
	pool *pgxpool.Pool,
	sensitivity, status, processing string,
	teamID *uuid.UUID,
	ownerless bool,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var owner any = frOwner
	if ownerless {
		owner = nil
	}
	var team any
	if teamID != nil {
		team = *teamID
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type,
		                    sensitivity, status, processing_status, team_id)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6,$7)`,
		id, "fr-"+sensitivity+"-"+status, owner, sensitivity, status, processing, team)
	if err != nil {
		t.Fatalf("seed %s/%s/%s: %v", sensitivity, status, processing, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// frRow reads back the columns the Go form needs THROUGH
// FieldsColumnsSQL, so both forms answer about the same row, and so the
// test also pins that the fragment the production surfaces select is the
// one FieldsRow scans.
func frRow(t *testing.T, pool *pgxpool.Pool, caller Caller, id uuid.UUID) FieldsRow {
	t.Helper()
	var (
		row       FieldsRow
		ownerName string
	)
	err := pool.QueryRow(context.Background(),
		`SELECT `+FieldsColumnsSQL("assets", "$2", caller)+` FROM assets WHERE id = $1`,
		id, caller.UserRef,
	).Scan(&row.Sensitivity, &row.Status, &row.ProcessingStatus,
		&row.OwnerUserRef, &row.TeamID, &row.IsTeamMember, &ownerName)
	if err != nil {
		t.Fatalf("frRow: %v", err)
	}
	return row
}

// frAsk runs the SQL form for one asset, one caller and one capability
// state.
func frAsk(
	t *testing.T,
	pool *pgxpool.Pool,
	caller Caller,
	caps ContentCaps,
	mut AssetMutationCaps,
	id uuid.UUID,
) bool {
	t.Helper()
	sql := `SELECT EXISTS (SELECT 1 FROM assets a WHERE a.id = $1` +
		FieldsReadableSQL("a", strconv.FormatInt(caller.UserRef, 10), caller, caps, mut) + `)`
	var ok bool
	if err := pool.QueryRow(context.Background(), sql, id).Scan(&ok); err != nil {
		t.Fatalf("FieldsReadableSQL: %v\nSQL: %s", err, sql)
	}
	return ok
}

// frFixture is one seeded asset plus the name the failure message uses.
type frFixture struct {
	name string
	id   uuid.UUID
}

// frFixtures plants the asset matrix both SQL-twin tests drive. Shared
// rather than written out twice: the picture plane is the field plane
// minus one disjunct, so a row that distinguishes the two forms of ONE
// of them is exactly the row the other needs, and a fixture added to a
// private copy would only ever protect half the surface.
func frFixtures(t *testing.T, pool *pgxpool.Pool, callerTeam, otherTeam uuid.UUID) []frFixture {
	t.Helper()
	var fixtures []frFixture
	add := func(name, sens, status, proc string, team *uuid.UUID, ownerless bool) {
		fixtures = append(fixtures, frFixture{
			name: name,
			id:   frSeedAsset(t, pool, sens, status, proc, team, ownerless),
		})
	}
	// The tier spread, all active + ready, mirroring the content twin.
	add("public", "public", "active", "ready", nil, false)
	add("restricted", "restricted", "active", "ready", nil, false)
	add("embargo", "embargo", "active", "ready", nil, false)
	add("team (caller's team)", "team", "active", "ready", &callerTeam, false)
	add("team (someone else's team)", "team", "active", "ready", &otherTeam, false)
	// A team-tier asset with NO team is the fail-closed case the Go form
	// spells out; the SQL form must not admit it either.
	add("team (no team_id)", "team", "active", "ready", nil, false)
	// Ownerless rows must never match on the owner branch — including
	// for the anonymous sentinel, whose UserRef is 0.
	add("public ownerless", "public", "active", "ready", nil, true)
	add("restricted ownerless", "restricted", "active", "ready", nil, true)
	// The ANONYMOUS-only status conjuncts. Public tier throughout, so
	// the content plane admits and ONLY the status/processing guard can
	// deny — an implementation that dropped it passes every arm above.
	add("public draft", "public", "draft", "ready", nil, false)
	add("public archived", "public", "archived", "ready", nil, false)
	add("public still processing", "public", "active", "processing", nil, false)
	add("public failed processing", "public", "active", "failed", nil, false)
	// The same guard on a row the mutation disjunct could rescue: a
	// non-public tier, on a team, in a state anonymous callers are
	// refused. This is where a wrong precedence between the guard and
	// the mutation branch shows up.
	add("restricted draft on caller's team", "restricted", "draft", "ready", &callerTeam, false)
	add("restricted ready on caller's team", "restricted", "active", "ready", &callerTeam, false)
	add("restricted on another team", "restricted", "active", "ready", &otherTeam, false)
	return fixtures
}

// frCallers is the caller spread both twin tests drive.
func frCallers() []struct {
	name   string
	caller Caller
} {
	owner, stranger, member := frOwner, frStranger, frMember
	return []struct {
		name   string
		caller Caller
	}{
		{"anonymous", NewCaller(nil)},
		{"owner", NewCaller(&owner)},
		{"stranger", NewCaller(&stranger)},
		{"team member", NewCaller(&member)},
	}
}

func TestFieldsReadableSQL_MatchesGo(t *testing.T) {
	pool := contentPool(t)

	callerTeam := seedTeamWithMember(t, pool, frMember)
	otherTeam := seedTeamWithMember(t, pool, frStranger)

	fixtures := frFixtures(t, pool, callerTeam, otherTeam)
	callers := frCallers()
	capsCases := []struct {
		name string
		caps ContentCaps
	}{
		{"no caps", ContentCaps{}},
		{"content.read.all", ContentCaps{ContentReadAll: true}},
		{"system.admin", ContentCaps{SystemAdmin: true}},
	}
	mutCases := []struct {
		name string
		mut  AssetMutationCaps
	}{
		{"no mutation scope", AssetMutationCaps{}},
		{"global assets.admin", AssetMutationCaps{Global: true}},
		{"assets.admin on caller's team", AssetMutationCaps{Teams: []uuid.UUID{callerTeam}}},
		{"assets.admin on another team", AssetMutationCaps{Teams: []uuid.UUID{otherTeam}}},
		{"assets.admin on both teams", AssetMutationCaps{Teams: []uuid.UUID{callerTeam, otherTeam}}},
		// uuid.Nil in the scope set must match NOTHING — not the
		// team-less rows, which is the trap MayMutate spells out.
		{"assets.admin scoped to the nil team", AssetMutationCaps{Teams: []uuid.UUID{uuid.Nil}}},
	}

	for _, f := range fixtures {
		for _, c := range callers {
			for _, cc := range capsCases {
				for _, mc := range mutCases {
					row := frRow(t, pool, c.caller, f.id)
					row.ApplyMutationCaps(mc.mut)
					want := FieldsReadable(row, c.caller, cc.caps.Checker())
					got := frAsk(t, pool, c.caller, cc.caps, mc.mut, f.id)
					if got != want {
						t.Errorf("%s / %s / %s / %s: SQL says %v, Go says %v — the two "+
							"expressions of the FIELD plane have drifted "+
							"(sensitivity=%q status=%q processing=%q owner=%v team=%v "+
							"member=%v mayMutate=%v)",
							f.name, c.name, cc.name, mc.name, got, want,
							row.Sensitivity, row.Status, row.ProcessingStatus,
							row.OwnerUserRef, row.TeamID, row.IsTeamMember, row.CallerMayMutate)
					}
				}
			}
		}
	}
}

// prAsk runs PreviewReadableSQL for one asset, one caller and one
// capability state.
func prAsk(t *testing.T, pool *pgxpool.Pool, caller Caller, caps ContentCaps, id uuid.UUID) bool {
	t.Helper()
	sql := `SELECT EXISTS (SELECT 1 FROM assets a WHERE a.id = $1` +
		PreviewReadableSQL("a", strconv.FormatInt(caller.UserRef, 10), caller, caps) + `)`
	var ok bool
	if err := pool.QueryRow(context.Background(), sql, id).Scan(&ok); err != nil {
		t.Fatalf("PreviewReadableSQL: %v\nSQL: %s", err, sql)
	}
	return ok
}

// TestPreviewReadableSQL_MatchesGo — #1026. The collection cover mosaic
// decides "can this member render" in SQL, so PreviewReadableSQL must
// agree with PreviewReadable on every row, exactly as its FIELD-plane
// sibling above does.
//
// The mutation axis is deliberately absent from the matrix rather than
// pinned to the zero value: PreviewReadableSQL takes no
// AssetMutationCaps, because ADR 0064 gives a mutation holder the fields
// and not the picture. The rows that WOULD be rescued by a mutation
// scope are still in the fixture set (they are the "on caller's team"
// arms), and here they must come back DENIED for a non-member — which is
// the assertion that would fail if someone re-expressed this fragment
// via FieldsReadableSQL.
func TestPreviewReadableSQL_MatchesGo(t *testing.T) {
	pool := contentPool(t)

	callerTeam := seedTeamWithMember(t, pool, frMember)
	otherTeam := seedTeamWithMember(t, pool, frStranger)

	fixtures := frFixtures(t, pool, callerTeam, otherTeam)
	capsCases := []struct {
		name string
		caps ContentCaps
	}{
		{"no caps", ContentCaps{}},
		{"content.read.all", ContentCaps{ContentReadAll: true}},
		{"system.admin", ContentCaps{SystemAdmin: true}},
	}

	for _, f := range fixtures {
		for _, c := range frCallers() {
			for _, cc := range capsCases {
				row := frRow(t, pool, c.caller, f.id)
				// NOT ApplyMutationCaps: the picture plane never
				// consults CallerMayMutate, and leaving it zero here is
				// what makes the Go side answer the same question the
				// SQL side can even be asked.
				want := PreviewReadable(row, c.caller, cc.caps.Checker())
				got := prAsk(t, pool, c.caller, cc.caps, f.id)
				if got != want {
					t.Errorf("%s / %s / %s: SQL says %v, Go says %v — the two "+
						"expressions of the PICTURE plane have drifted "+
						"(sensitivity=%q status=%q processing=%q owner=%v team=%v member=%v)",
						f.name, c.name, cc.name, got, want,
						row.Sensitivity, row.Status, row.ProcessingStatus,
						row.OwnerUserRef, row.TeamID, row.IsTeamMember)
				}
			}
		}
	}
}

// TestPreviewReadableSQL_IgnoresMutationScope pins the ADR 0064 boundary
// structurally: a GLOBAL assets.admin empties FieldsReadableSQL, and must
// NOT empty this one. If a later edit routes the picture plane through
// the field fragment, this is the test that catches it before the cover
// mosaic starts painting restricted assets.
func TestPreviewReadableSQL_IgnoresMutationScope(t *testing.T) {
	anon := NewCaller(nil)
	if got := FieldsReadableSQL("a", "$2", anon, ContentCaps{}, AssetMutationCaps{Global: true}); got != "" {
		t.Fatalf("premise changed: a global assets.admin no longer empties the FIELD fragment (%q)", got)
	}
	if got := PreviewReadableSQL("a", "$2", anon, ContentCaps{}); got == "" {
		t.Error("a global assets.admin emptied the PICTURE fragment — ADR 0064 says the " +
			"mutation plane never confers the binary plane")
	}
	// The two agree exactly when there is no mutation scope to differ
	// over — the property that makes previewReadableExpr shared rather
	// than copied.
	field := FieldsReadableSQL("a", "$2", anon, ContentCaps{}, AssetMutationCaps{})
	if pic := PreviewReadableSQL("a", "$2", anon, ContentCaps{}); field != pic {
		t.Errorf("with no mutation scope the two fragments should be identical:\nfield: %s\npic:   %s", field, pic)
	}
}

// TestFieldsReadableSQL_CapsShortCircuit pins the empty-fragment
// contract the search COUNT's tautologies depend on: for a caller whose
// capabilities admit everything, the fragment is EMPTY and therefore
// never names `callerArg`. query.go's COUNT binds $3 anyway, as a
// tautology, precisely because of this — deleting either half breaks the
// other (ADR 0063 placeholder discipline).
func TestFieldsReadableSQL_CapsShortCircuit(t *testing.T) {
	anon := NewCaller(nil)
	for _, c := range []struct {
		name string
		caps ContentCaps
		mut  AssetMutationCaps
	}{
		{"system.admin", ContentCaps{SystemAdmin: true}, AssetMutationCaps{}},
		{"content.read.all", ContentCaps{ContentReadAll: true}, AssetMutationCaps{}},
		{"global assets.admin", ContentCaps{}, AssetMutationCaps{Global: true}},
	} {
		if got := FieldsReadableSQL("a", "$3", anon, c.caps, c.mut); got != "" {
			t.Errorf("%s: fragment = %q, want empty — a resolved short-circuit must let "+
				"Postgres plan as though the gate were not there", c.name, got)
		}
	}
	// And the converse: an unprivileged caller MUST get a fragment, or
	// every splice site silently reverts to the #902 leak.
	if got := FieldsReadableSQL("a", "$3", anon, ContentCaps{}, AssetMutationCaps{}); got == "" {
		t.Fatal("an unprivileged caller got an EMPTY readability fragment — that is #902 restored")
	}
}

// TestAssetSearchMatchSQL_Composition pins the shape every full-text
// splice site depends on: the readability conjunct is ANDed INSIDE the
// parentheses that wrap the `@@`, so a site can drop the whole
// expression behind `$4 IS NULL OR …` (browse does exactly that) without
// the gate escaping its branch.
func TestAssetSearchMatchSQL_Composition(t *testing.T) {
	anon := NewCaller(nil)
	frag := AssetSearchMatchSQL("assets", `plainto_tsquery('english', $1)`, "$3",
		anon, ContentCaps{}, AssetMutationCaps{})
	if !strings.HasPrefix(frag, "(assets.search_text @@ plainto_tsquery('english', $1)") {
		t.Errorf("fragment does not open with the parenthesised match: %s", frag)
	}
	if !strings.HasSuffix(frag, ")") {
		t.Errorf("fragment is not closed: %s", frag)
	}
	if !strings.Contains(frag, " AND (") {
		t.Errorf("fragment carries no readability conjunct at all — that is #902 restored: %s", frag)
	}
	// A caps-holding caller gets the bare match, still parenthesised.
	full := AssetSearchMatchSQL("assets", `plainto_tsquery('english', $1)`, "$3",
		anon, ContentCaps{SystemAdmin: true}, AssetMutationCaps{})
	if full != "(assets.search_text @@ plainto_tsquery('english', $1))" {
		t.Errorf("system.admin fragment = %q, want the bare parenthesised match", full)
	}
	// No alias renders bare column references, for the un-aliased
	// `FROM assets` statements.
	bare := AssetSearchMatchSQL("", `plainto_tsquery('english', $1)`, "$3",
		anon, ContentCaps{SystemAdmin: true}, AssetMutationCaps{})
	if bare != "(search_text @@ plainto_tsquery('english', $1))" {
		t.Errorf("un-aliased fragment = %q", bare)
	}
}
