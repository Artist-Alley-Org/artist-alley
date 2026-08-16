// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #902 on BROWSE — the surface a search-only fix misses.
//
// `/assets?q=` runs its own `search_text @@ plainto_tsquery(...)`
// against the same documents /search does, so gating the search package
// alone would have left the identical word-by-word recovery one endpoint
// over: type a phrase from a restricted asset's withheld title into
// browse and watch the page go from empty to one placeholder.
//
// This suite is deliberately SEPARATE from the search one and drives
// ListAssetsPageGated directly, because "the fix is in a shared helper"
// is a claim about the code and this is the evidence for it at the other
// call site.
//
// The pairing that makes it a real test:
//
//   - the same token returns the row for the OWNER and not for a
//     stranger — opposite verdicts, same row, same query;
//   - the row is STILL LISTED with no `?q=` at all, which is what ADR
//     0064's placeholder means and what a "just drop restricted rows
//     from browse" implementation would break.
//
// Skips without AA_DB_PASSWORD.

package assets

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	btoOwner    int64 = 9022001
	btoStranger int64 = 9022002
)

// btoTitle + btoDescription are the withheld document. Nonsense tokens
// so a hit is attributable to this fixture alone.
const (
	btoTitle       = "vandriel quorbeck thistlewane"
	btoDescription = "murrelbind"
)

func btoTokens() []string {
	return append(strings.Fields(btoTitle), strings.Fields(btoDescription)...)
}

func btoSeed(t *testing.T, pool *pgxpool.Pool, sensitivity string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	team := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, slug) VALUES ($1,$2,$3)`,
		team, "bto-"+team.String()[:8], "bto-"+team.String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, team_id)
		VALUES ($1,$2,$3,$4,(SELECT MIN(ref) FROM asset_types),'active',$5,'ready',$6)`,
		id, btoTitle, btoDescription, btoOwner, sensitivity, team); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, team)
	})
	return id, team
}

// btoBrowse runs one browse page and reports whether the target row came
// back. `q` empty means no text filter at all.
func btoBrowse(
	t *testing.T,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	caps visibility.CapabilityChecker,
	mut visibility.AssetMutationCaps,
	q string,
	target uuid.UUID,
) bool {
	t.Helper()
	p := ListAssetsPageGatedParams{RowLimit: 200, MutationCaps: mut}
	if q != "" {
		p.Q = &q
	}
	rows, err := ListAssetsPageGated(context.Background(), pool, caller, caps, p)
	if err != nil {
		t.Fatalf("browse %q: %v", q, err)
	}
	for _, r := range rows {
		if r.ID.Valid && r.ID.Bytes == target {
			return true
		}
	}
	return false
}

// TestBrowseTextOracle_TokenWalk is the exploit against `/assets?q=`.
func TestBrowseTextOracle_TokenWalk(t *testing.T) {
	pool := listPagePool(t)
	target, _ := btoSeed(t, pool, "restricted")

	stranger := btoStranger
	strangerCaller := visibility.NewCaller(&stranger)
	anon := visibility.NewCaller(nil)

	for _, tok := range btoTokens() {
		if btoBrowse(t, pool, strangerCaller, nil, visibility.AssetMutationCaps{}, tok, target) {
			t.Errorf("token %q: browse returned the restricted row to a caller who may not "+
				"read its fields — the withheld word is confirmed, and the rest of the "+
				"title follows one query at a time", tok)
		}
		if btoBrowse(t, pool, anon, nil, visibility.AssetMutationCaps{}, tok, target) {
			t.Errorf("token %q: browse returned the restricted row to an ANONYMOUS caller", tok)
		}
	}
	// The whole phrase too — plainto_tsquery ANDs its terms, so this is
	// the strongest single probe.
	if btoBrowse(t, pool, strangerCaller, nil, visibility.AssetMutationCaps{}, btoTitle, target) {
		t.Errorf("browse matched the full withheld title for a caller who may not read it")
	}
}

// TestBrowseTextOracle_UnfilteredStillLists is the half of ADR 0064 the
// gate must NOT break, and the reason the conjunct is scoped to the
// `?q=` branch rather than added beside the predicate: with no query
// text the restricted row is still listed for the stranger, as the
// placeholder that makes "request access" (#881) mean anything.
func TestBrowseTextOracle_UnfilteredStillLists(t *testing.T) {
	pool := listPagePool(t)
	target, _ := btoSeed(t, pool, "restricted")

	stranger := btoStranger
	if !btoBrowse(t, pool, visibility.NewCaller(&stranger), nil,
		visibility.AssetMutationCaps{}, "", target) {
		t.Fatal("an UNFILTERED browse dropped the restricted row — #902 gated the `?q=` " +
			"match, not the row plane, and ADR 0064 keeps the placeholder listed")
	}
}

// TestBrowseTextOracle_EntitledCallersStillFind is the opposite verdict
// on the same row and the same tokens. An artist who cannot find their
// own restricted work by typing its name has no product.
func TestBrowseTextOracle_EntitledCallersStillFind(t *testing.T) {
	pool := listPagePool(t)
	target, team := btoSeed(t, pool, "restricted")

	owner, stranger := btoOwner, btoStranger
	admin := func(code string) bool {
		return code == visibility.SystemAdmin
	}
	readAll := func(code string) bool {
		return code == visibility.ContentReadAll
	}
	cases := []struct {
		name   string
		caller visibility.Caller
		caps   visibility.CapabilityChecker
		mut    visibility.AssetMutationCaps
	}{
		{"the owner", visibility.NewCaller(&owner), nil, visibility.AssetMutationCaps{}},
		{"system.admin", visibility.NewCaller(&stranger), admin, visibility.AssetMutationCaps{}},
		{"content.read.all", visibility.NewCaller(&stranger), readAll, visibility.AssetMutationCaps{}},
		{"global assets.admin", visibility.NewCaller(&stranger), nil,
			visibility.AssetMutationCaps{Global: true}},
		{"assets.admin scoped to the asset's team", visibility.NewCaller(&stranger), nil,
			visibility.AssetMutationCaps{Teams: []uuid.UUID{team}}},
	}
	for _, c := range cases {
		for _, tok := range btoTokens() {
			if !btoBrowse(t, pool, c.caller, c.caps, c.mut, tok, target) {
				t.Errorf("%s: browse `?q=%s` lost an asset they are entitled to read — "+
					"the gate is too wide", c.name, tok)
			}
		}
	}
	// The negative control on the mutation arm: a scope over SOME OTHER
	// team buys nothing. Without this, `Global: true` in disguise passes.
	otherTeam := uuid.New()
	for _, tok := range btoTokens() {
		if btoBrowse(t, pool, visibility.NewCaller(&stranger), nil,
			visibility.AssetMutationCaps{Teams: []uuid.UUID{otherTeam}}, tok, target) {
			t.Errorf("an assets.admin scope over an UNRELATED team matched the restricted "+
				"asset by token %q", tok)
		}
	}
}

// TestBrowseTextOracle_PublicRowsAreUnaffected is the regression fence:
// `?q=` must still work for rows every caller may read.
func TestBrowseTextOracle_PublicRowsAreUnaffected(t *testing.T) {
	pool := listPagePool(t)
	target, _ := btoSeed(t, pool, "public")

	stranger := btoStranger
	for _, c := range []struct {
		name   string
		caller visibility.Caller
	}{
		{"anonymous", visibility.NewCaller(nil)},
		{"a stranger", visibility.NewCaller(&stranger)},
	} {
		for _, tok := range btoTokens() {
			if !btoBrowse(t, pool, c.caller, nil, visibility.AssetMutationCaps{}, tok, target) {
				t.Errorf("%s: `?q=%s` lost a PUBLIC asset — the readability gate is "+
					"filtering rows it must not touch", c.name, tok)
			}
		}
	}
}
