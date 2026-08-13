// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #902 — THE ATTACK, written as the attack.
//
// #899 removed a restricted asset's title from its search-result
// payload. It could not remove the title from the asset's `search_text`
// document, because the document is what the OWNER finds their own work
// with — so the words were still there, and `@@` still answered
// questions about them. Search a phrase that occurs only in a restricted
// asset's withheld title and the total moves 0→1: that is one bit of the
// title, and the title is recoverable a token at a time by repeating it.
//
// So the assertions here are NOT "the response is redacted". They are
// COMPARATIVE and per-token:
//
//   - a caller who may not read the fields gets 0 hits and total 0, for
//     EVERY token of the withheld title in turn (TokenWalk);
//   - the owner, a content.read.all holder, a system.admin and a
//     team-scoped assets.admin holder get the row for the same tokens
//     (Entitled). Opposite verdicts, same row, same query — a test where
//     both callers get the same answer proves nothing, and a change that
//     hid restricted assets from everyone would satisfy the first half
//     alone.
//
// The counterweight matters as much as the leak: this product is a DAM,
// and an artist who cannot find their own restricted work by typing its
// name has no product.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	stoOwner    int64 = 9021101
	stoStranger int64 = 9021102
	stoAdmin    int64 = 9021103
)

// stoTitle is the withheld title under attack. Every token is nonsense,
// so a hit is attributable to this fixture and to nothing else in any
// developer's database — and so the token walk cannot be satisfied by an
// unrelated row happening to match one of the words.
const stoTitle = "zarquil vothrenn maplebisk quennow"

// stoDescription and stoFieldValue are the OTHER two ingredients
// rebuild_asset_search_text folds into the document (weights B and D).
// #902 is not a title bug — it is a document bug, and a fix that gated
// only the title would leave the description and the searchable field
// values recoverable by exactly the same walk.
const (
	stoDescription = "brindlewax phossiter"
	stoFieldValue  = "gorbulent"
)

func stoTokens() []string {
	return append(strings.Fields(stoTitle), strings.Fields(stoDescription)...)
}

// stoSeed plants one restricted asset owned by stoOwner, on a team, with
// a searchable field value, and returns its id + the team.
func stoSeed(t *testing.T, pool *pgxpool.Pool, sensitivity string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, ref := range []int64{stoOwner, stoStranger, stoAdmin} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO "user" (ref, username) VALUES ($1, $2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			ref, "sto-user-"+uuid.NewString()[:8]); err != nil {
			t.Fatalf("seed user %d: %v", ref, err)
		}
	}
	team := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug) VALUES ($1, $2, $3)`,
		team, "sto-team-"+team.String()[:8], "sto-"+team.String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, team_id, file_extension)
		VALUES ($1,$2,$3,$4,(SELECT MIN(ref) FROM asset_types),'active',$5,'ready',$6,'png')`,
		id, stoTitle, stoDescription, stoOwner, sensitivity, team); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	// A searchable, active field value — weight D of the document, and
	// the ingredient the rebuild function filters on
	// `f.searchable = TRUE AND f.status = 'active'`.
	var fieldID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM field_definition
		  WHERE searchable = TRUE AND status = 'active' AND type = 'text'
		    AND mirrors_column IS NULL
		  ORDER BY id LIMIT 1`).Scan(&fieldID); err != nil {
		t.Fatalf("no searchable text field definition to attach a value to: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_field_value (asset_id, field_id, value_text)
		 VALUES ($1, $2, $3)`, id, fieldID, stoFieldValue); err != nil {
		t.Fatalf("seed field value: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id = $1`, team)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, team)
	})
	return id, team
}

// stoProbe is one attack query: run the engine for `token` and report
// whether the target row came back, and what total_count said.
func stoProbe(
	t *testing.T,
	pool *pgxpool.Pool,
	ref *int64,
	caps visibility.ContentCaps,
	mut visibility.AssetMutationCaps,
	token string,
	target uuid.UUID,
) (found bool, total int) {
	t.Helper()
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          token,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		CallerUserRef: ref,
		Caps:          caps,
		MutationCaps:  mut,
	})
	if err != nil {
		t.Fatalf("search %q: %v", token, err)
	}
	for _, h := range res.Hits {
		if h.ID == target {
			found = true
		}
	}
	return found, res.TotalCount
}

// TestSearchTextOracle_TokenWalk is the exploit. Walk the withheld
// document token by token as a caller who cannot read the fields, and
// require that NOTHING moves: not the hit array, not total_count.
//
// A single-token test can pass by luck — one stopword, one stemmer
// collision — which is why every token is probed and every failure names
// the token that leaked.
func TestSearchTextOracle_TokenWalk(t *testing.T) {
	pool := coPool(t)
	target, _ := stoSeed(t, pool, "restricted")
	stranger := stoStranger

	for _, tok := range append(stoTokens(), stoFieldValue) {
		found, total := stoProbe(t, pool, &stranger, visibility.ContentCaps{},
			visibility.AssetMutationCaps{}, tok, target)
		if found {
			t.Errorf("token %q: the restricted row came back to a caller who may not read "+
				"its fields — that IS the withheld word, confirmed", tok)
		}
		if total != 0 {
			t.Errorf("token %q: total_count = %d, want 0 — the COUNT is the oracle even when "+
				"the array is redacted, because the caller only needs the number to move", tok, total)
		}
	}

	// The phrase, not just the tokens: plainto_tsquery ANDs its terms,
	// so a caller who guesses two words at once is a stronger probe than
	// one who guesses one.
	if found, total := stoProbe(t, pool, &stranger, visibility.ContentCaps{},
		visibility.AssetMutationCaps{}, stoTitle, target); found || total != 0 {
		t.Errorf("the full withheld title matched for a caller who may not read it "+
			"(found=%v total=%d)", found, total)
	}

	// And the anonymous caller, who is a different branch of the rule
	// (the status/processing conjuncts wrap the whole plane).
	for _, tok := range stoTokens() {
		if found, total := stoProbe(t, pool, nil, visibility.ContentCaps{},
			visibility.AssetMutationCaps{}, tok, target); found || total != 0 {
			t.Errorf("anonymous, token %q: found=%v total=%d, want absent/0", tok, found, total)
		}
	}
}

// TestSearchTextOracle_EntitledCallersStillFind is the other verdict on
// the SAME row and the SAME queries. Without it, "hide restricted assets
// from everybody" passes the walk above and ships a DAM whose artists
// cannot find their own work.
func TestSearchTextOracle_EntitledCallersStillFind(t *testing.T) {
	pool := coPool(t)
	target, team := stoSeed(t, pool, "restricted")

	owner, stranger, admin := stoOwner, stoStranger, stoAdmin
	cases := []struct {
		name string
		ref  *int64
		caps visibility.ContentCaps
		mut  visibility.AssetMutationCaps
	}{
		{"the owner", &owner, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
		{"content.read.all", &stranger, visibility.ContentCaps{ContentReadAll: true}, visibility.AssetMutationCaps{}},
		{"system.admin", &stranger, visibility.ContentCaps{SystemAdmin: true}, visibility.AssetMutationCaps{}},
		{"global assets.admin", &admin, visibility.ContentCaps{}, visibility.AssetMutationCaps{Global: true}},
		// The disjunct a per-tier document could never have expressed:
		// readability that depends on the caller's SCOPE over this row's
		// team.
		{"assets.admin scoped to the asset's team", &admin, visibility.ContentCaps{},
			visibility.AssetMutationCaps{Teams: []uuid.UUID{team}}},
	}
	for _, c := range cases {
		for _, tok := range append(stoTokens(), stoFieldValue) {
			found, total := stoProbe(t, pool, c.ref, c.caps, c.mut, tok, target)
			if !found {
				t.Errorf("%s: token %q did not find the asset they are entitled to read — "+
					"the gate is too wide and the product is broken", c.name, tok)
			}
			if total < 1 {
				t.Errorf("%s: token %q gave total_count %d, want at least 1", c.name, tok, total)
			}
		}
	}

	// A team MEMBER of a team-tier asset is the fifth entitled caller,
	// and it needs its own fixture because the tier is part of the rule.
	teamAsset, teamID := stoSeed(t, pool, "team")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, teamID, stoStranger); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	for _, tok := range stoTokens() {
		if found, _ := stoProbe(t, pool, &stranger, visibility.ContentCaps{},
			visibility.AssetMutationCaps{}, tok, teamAsset); !found {
			t.Errorf("a member of the asset's team could not find it by token %q", tok)
		}
	}
	// ...and a NON-member of that team still cannot, on the same row.
	nonMember := stoAdmin
	for _, tok := range stoTokens() {
		if found, _ := stoProbe(t, pool, &nonMember, visibility.ContentCaps{},
			visibility.AssetMutationCaps{}, tok, teamAsset); found {
			t.Errorf("a NON-member matched the team-tier asset by token %q", tok)
		}
	}
}

// TestSearchTextOracle_PublicRowsAreUnaffected is the regression fence
// around the whole change: the gate must narrow nothing for a row every
// caller may read. If this goes red, /search stopped working.
func TestSearchTextOracle_PublicRowsAreUnaffected(t *testing.T) {
	pool := coPool(t)
	target, _ := stoSeed(t, pool, "public")

	stranger := stoStranger
	for _, c := range []struct {
		name string
		ref  *int64
	}{{"anonymous", nil}, {"a stranger", &stranger}} {
		for _, tok := range append(stoTokens(), stoFieldValue) {
			found, total := stoProbe(t, pool, c.ref, visibility.ContentCaps{},
				visibility.AssetMutationCaps{}, tok, target)
			if !found || total < 1 {
				t.Errorf("%s: token %q lost a PUBLIC asset (found=%v total=%d) — "+
					"the readability gate is filtering rows it must not touch",
					c.name, tok, found, total)
			}
		}
	}
}
