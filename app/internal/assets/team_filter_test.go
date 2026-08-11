// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #684 — `GET /assets?team_id=` is the assets tab on a team page.
//
// # The property under test: THE FILTER NEVER WIDENS
//
// A team page is the surface most likely to be built as an access
// grant by accident, because the sentence that describes it — "show
// me this studio's work" — sounds like an entitlement. It is not one.
// The parameter is a plain conjunct ANDed beside the visibility
// predicate, so it can only ever REMOVE rows from the page the caller
// would have been served anyway.
//
// The failure this file exists to catch is the one-line version of
// that mistake: reading the filter as "the team's space" and ORing it
// into the read rule, or adding a `team_id = $n` disjunct to the field
// plane. Either would hand a non-member the titles, descriptions, tags
// and thumbhashes of a studio's restricted work, and both would pass a
// test that only asserted "the team page returns the team's assets".
//
// So the assertions are two-sided, per the discipline in
// field_plane_test.go. Every check that the caller SEES the team's
// public work is paired with a check that its restricted work is still
// a placeholder, and the pairing is what makes the test able to fail: a
// build that hid everything would pass a leak-only test, and a build
// that showed everything would pass a visibility-only test.
//
// # Why this builds its own hierarchy
//
// It reuses maFixture from mutation_authz_test.go for the reason
// field_plane_test.go gives: that fixture inserts real `team_parents`
// edges and lets the closure trigger materialise `team_closure`. The
// seeded database's teams are flat — every closure row is
// self-referential — so a test leaning on the seed would prove nothing
// about a hierarchy, and the parent/child case below is precisely where
// a closure-expanded grant could leak across the filter.
package assets_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// tfPage runs GET /assets through the handler for one caller, with an
// optional team filter, and returns the page keyed by asset id so
// assertions never depend on ordering.
//
// limit is deliberately the max: these fixtures live in a database that
// also holds the seed, and an unfiltered control page has to be able to
// contain the fixture rows for the comparison below to mean anything.
func tfPage(f *maFixture, ctx context.Context, team *uuid.UUID) map[uuid.UUID]openapi.Asset {
	f.t.Helper()
	limit := 200
	params := openapi.ListAssetsParams{Limit: &limit}
	if team != nil {
		u := openapi_types.UUID(*team)
		params.TeamId = &u
	}
	resp, err := f.h.ListAssets(ctx, openapi.ListAssetsRequestObject{Params: params})
	if err != nil {
		f.t.Fatalf("ListAssets: %v", err)
	}
	ok, isOK := resp.(openapi.ListAssets200JSONResponse)
	if !isOK {
		f.t.Fatalf("ListAssets: want 200, got %T", resp)
	}
	out := make(map[uuid.UUID]openapi.Asset, len(ok.Items))
	for _, a := range ok.Items {
		out[uuid.UUID(a.Id)] = a
	}
	return out
}

// tfMember joins a user to a team. Direct membership only — the
// content plane's `team` tier reads team_memberships, not the closure,
// so joining the child does NOT make you a member of the parent.
func tfMember(f *maFixture, team uuid.UUID, userRef int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, team, userRef); err != nil {
		f.t.Fatalf("seed membership: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM team_memberships WHERE team_id = $1 AND user_ref = $2`, team, userRef)
	})
}

// tfTeamTierAsset seeds an asset at the `team` sensitivity tier — the
// ONE tier team membership actually unlocks.
//
// This is not the same thing as fpAsset's `restricted`, and the
// difference is the whole reason both appear below.
// visibility.ContentReadable admits `team` to members and denies
// `restricted` to everyone except the owner and the wildcards. So:
//
//   - `restricted` proves the filter does not leak, but a member sees
//     a placeholder too, which means it cannot tell "correctly gated"
//     apart from "gates everything and is therefore useless";
//   - `team` supplies the missing half — one asset, two callers,
//     opposite verdicts, membership the only difference between them.
//
// A test carrying only the first would pass on a build that returned
// placeholders for every row on every team page.
func tfTeamTierAsset(f *maFixture, owner *int64, team uuid.UUID, title string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	hb := make([]byte, 16)
	_, _ = rand.Read(hb)
	hashHex := hex.EncodeToString(sha256.New().Sum(hb))[:64]
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`,
		hashHex); err != nil {
		f.t.Fatalf("seed storage_object: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO assets (id, title, description, asset_type, owner_user_ref, team_id,
		                    status, processing_status, file_hash, file_extension,
		                    file_size_bytes, sensitivity)
		VALUES ($1, $2, '', 1, $3, $4, 'active', 'ready', $5, 'png', 1024, 'team')`,
		id, title, owner, team, hashHex); err != nil {
		f.t.Fatalf("seed team-tier asset: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id
}

// ---------------------------------------------------------------------------
// ⭐ The never-widens proof
// ---------------------------------------------------------------------------

// TestTeamFilter_NeverWidens is the whole of the #684 read contract in
// one test, and it is deliberately not split — the halves only mean
// something together.
//
// Cast:
//   - parent / child: a real two-node hierarchy (team_parents edge,
//     closure materialised by the trigger)
//   - owner: a member of neither, owns the fixture assets
//   - stranger: a member of neither, holds no grant — the "someone
//     browsing a studio they have nothing to do with" case
//   - member: a member of `child` ONLY
//
// Content in `child`: one public asset and one restricted asset.
func TestTeamFilter_NeverWidens(t *testing.T) {
	f := newMAFixture(t)

	parent := f.team("tf-parent", nil)
	child := f.team("tf-child", &parent)

	owner := f.user("tf-owner")
	stranger := f.user("tf-stranger")
	member := f.user("tf-member")
	tfMember(f, child, member)

	const teamTierTitle = "tf-team-tier-work"
	publicAsset := f.asset(&owner, &child, "active") // sensitivity 'public'
	restricted := fpAsset(f, &owner, &child)         // sensitivity 'restricted'
	teamTier := tfTeamTierAsset(f, &owner, child, teamTierTitle)

	strangerCtx := f.identity(stranger)
	memberCtx := f.identity(member)

	// ── The stranger's TEAM PAGE ─────────────────────────────────────
	//
	// Both rows are on the page — restricted content stays listed as a
	// placeholder rather than disappearing, per ADR 0064, which is what
	// makes "request access" mean anything. What must NOT happen is the
	// placeholder filling in.
	page := tfPage(f, strangerCtx, &child)

	pub, ok := page[publicAsset]
	if !ok {
		t.Fatalf("the team's PUBLIC asset is missing from its own team page — "+
			"the filter is narrowing something it should not (team %s)", child)
	}
	if pub.Restricted {
		t.Errorf("the team's public asset came back as a placeholder on the team page")
	}

	res, ok := page[restricted]
	if !ok {
		t.Fatalf("the team's restricted asset vanished from the team page — " +
			"ADR 0064 keeps it listed as a placeholder; dropping the row is a " +
			"different bug from leaking it, and just as wrong")
	}
	assertFieldsWithheld(t, res, "restricted team asset on its own team page, non-member")
	assertPictureWithheld(t, res, "restricted team asset on its own team page, non-member")

	// The `team`-tier asset is ALSO a placeholder for the stranger. This
	// is the row the member unlocks further down, so pinning it here is
	// what makes that unlock attributable to membership.
	tier, ok := page[teamTier]
	if !ok {
		t.Fatalf("the team-tier asset is missing from the team page entirely")
	}
	if !tier.Restricted {
		t.Errorf("a `team`-tier asset was served in full to a NON-MEMBER on the team " +
			"page — asking for a team's page is not the same as being in it")
	}
	if tier.Title != nil && *tier.Title == teamTierTitle {
		t.Errorf("the team-tier asset LEAKED its title to a non-member")
	}

	// And the bytes are still refused. A field-plane leak and a binary
	// leak are separate failures; #939's whole point is that a test
	// asserting one passes on a build that shipped the other.
	assertBytesRefused(t, f, strangerCtx, restricted,
		"restricted team asset, non-member on the team page")

	// ── The filter NARROWS relative to unfiltered browse ─────────────
	//
	// This is the load-bearing comparison, and it is stated as a subset
	// relation rather than as two independent expectations. For every
	// fixture asset: presence on the team page implies presence on
	// unfiltered browse, and the `restricted` marker must be IDENTICAL
	// on both. A build where the team page reveals more than browse
	// fails here even if every assertion above somehow passed.
	browse := tfPage(f, strangerCtx, nil)
	for _, id := range []uuid.UUID{publicAsset, restricted, teamTier} {
		onTeam, inTeamPage := page[id]
		onBrowse, inBrowse := browse[id]
		if inTeamPage && !inBrowse {
			t.Errorf("asset %s appears on the team page but NOT in unfiltered browse — "+
				"the team filter widened the result set", id)
			continue
		}
		if !inTeamPage || !inBrowse {
			continue
		}
		if onTeam.Restricted != onBrowse.Restricted {
			t.Errorf("asset %s: restricted=%v on the team page but %v in browse — "+
				"the team filter changed the visibility verdict",
				id, onTeam.Restricted, onBrowse.Restricted)
		}
		if onTeam.Title != nil && onBrowse.Title == nil {
			t.Errorf("asset %s: the team page served a title that browse withholds", id)
		}
	}

	// ── MEMBERSHIP is what grants access, not the filter ─────────────
	//
	// The same `team`-tier asset, the same team page, one thing changed:
	// this caller is a member. It comes back in full.
	//
	// Without this the whole test could be satisfied by a build that
	// withheld every row from every caller — perfectly "secure" and
	// completely broken. And because the ONLY difference between the two
	// callers is the team_memberships row, a pass here attributes the
	// unlock to membership rather than to the filter.
	memberPage := tfPage(f, memberCtx, &child)
	memberTier, ok := memberPage[teamTier]
	if !ok {
		t.Fatalf("the team-tier asset is missing from a MEMBER's team page")
	}
	if memberTier.Restricted {
		t.Errorf("a member of the owning team sees a placeholder for their own team's " +
			"`team`-tier asset — the content plane's team tier is not being reached")
	}
	if memberTier.Title == nil || *memberTier.Title != teamTierTitle {
		t.Errorf("member's team page: title = %v, want %q", memberTier.Title, teamTierTitle)
	}

	// The `restricted` tier stays shut even for a member. Membership is
	// not a master key: ContentReadable admits `team` to members and
	// denies `restricted` to everyone but the owner and the wildcards.
	// Asserting it here stops "members see their team's work" from
	// quietly becoming "members see everything with their team's id on
	// it".
	memberRes, ok := memberPage[restricted]
	if !ok {
		t.Fatalf("the restricted asset is missing from a member's team page")
	}
	if !memberRes.Restricted {
		t.Errorf("team membership unlocked a `restricted` asset — that tier is owner-only")
	}

	// ── The PARENT's page does not inherit the child's content ───────
	//
	// team_id is an exact column match, not a closure walk. An asset in
	// `child` is not on `parent`'s page. This is worth pinning because
	// the closure IS consulted elsewhere in this codebase (scoped
	// capability grants expand through it), and "the filter should
	// probably expand too" is a plausible-sounding change that would
	// silently move other studios' work onto a parent's page.
	parentPage := tfPage(f, memberCtx, &parent)
	if _, leaked := parentPage[restricted]; leaked {
		t.Errorf("the CHILD team's restricted asset appears on the PARENT team's page — "+
			"team_id is an exact match, not a closure expansion (parent %s, child %s)",
			parent, child)
	}
	if _, leaked := parentPage[publicAsset]; leaked {
		t.Errorf("the child team's public asset appears on the parent team's page")
	}

	// ── Unknown and soft-deleted teams are one answer: empty ─────────
	//
	// No 404, no error, no distinction between them. Anything else
	// makes this endpoint a team-existence oracle over every studio on
	// the instance — the discipline visibility.CanAssignToTeam carries
	// on the write side.
	unknown := uuid.New()
	if got := tfPage(f, strangerCtx, &unknown); len(got) != 0 {
		t.Errorf("filtering by a nonexistent team returned %d assets, want 0", len(got))
	}

	tombstoned := f.team("tf-tombstoned", nil)
	tombAsset := f.asset(&owner, &tombstoned, "active")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, tombstoned); err != nil {
		t.Fatalf("soft-delete team: %v", err)
	}
	// The asset outlives its team's tombstone — a soft delete cascades
	// nothing — and the filter is an exact column match that does not
	// join `teams`, so it still finds it. That is the current contract
	// and it is asserted rather than assumed.
	//
	// What matters for the oracle argument is the SHAPE of the answer,
	// not its contents: both this and the unknown-team call above
	// returned a 200 page. Neither raised a 404, so neither tells a
	// caller whether the UUID they guessed names a real studio.
	tombPage := tfPage(f, strangerCtx, &tombstoned)
	if _, present := tombPage[tombAsset]; !present {
		t.Errorf("an asset belonging to a soft-deleted team vanished from its filtered " +
			"page — a team tombstone cascades nothing, so the row is still live and " +
			"still reachable through unfiltered browse; dropping it here would make " +
			"the team filter narrower than the predicate")
	}
}
