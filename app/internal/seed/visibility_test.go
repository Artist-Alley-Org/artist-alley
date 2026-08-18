// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Pure (no-DB) unit tests for the seeded post visibility rule (#1176).
//
// Every seeded post used to be written 'org-only', so an instance with
// public mode ON answered an anonymous GET /posts with 200 and zero
// items. These tests pin the rule that ends that — and the coverage
// requirement that stops a shrunk CI catalogue from quietly going back
// to an all-org-only fixture.

package seed

import "testing"

func TestPostVisibility_PublicNeedsBothTheDeclarationAndAReadableCover(t *testing.T) {
	cases := []struct {
		name      string
		postTier  string
		coverTier string
		want      string
	}{
		{"declared public with a public cover", "public", "public", "public"},
		// The cover is the card's image. A member the viewer may not
		// read is redacted (#883), so a team-tier cover would put a
		// blank placeholder tile on the anonymous wall.
		{"declared public, team cover", "public", "team", "org-only"},
		{"declared public, restricted cover", "public", "restricted", "org-only"},
		// No resolvable member at all — applyPosts skips these, but the
		// rule must not treat an absent tier as permission.
		{"declared public, no cover", "public", "", "org-only"},
		{"team content stays inside the org", "team", "public", "org-only"},
		{"restricted content stays inside the org", "restricted", "public", "org-only"},
		// An absent post tier is NOT the same as a declared public one:
		// sensitivity() defaults an asset to public, and inheriting that
		// default here would publish the entire corpus.
		{"undeclared post tier", "", "public", "org-only"},
	}
	for _, c := range cases {
		got := postVisibility(manifestPost{SensitivityTier: c.postTier}, c.coverTier)
		if got != c.want {
			t.Errorf("%s: postVisibility(tier=%q, cover=%q) = %q, want %q",
				c.name, c.postTier, c.coverTier, got, c.want)
		}
	}
}

func TestCoverTier_ReadsTheFirstSurvivingMember(t *testing.T) {
	tiers := map[string]string{"b": "public", "c": "team"}
	// "a" is a member the manifest lost — applyPosts skips it when
	// building members, so "b" becomes the cover and its tier is the one
	// that decides.
	if got := coverTier(manifestPost{AssetIDs: []string{"a", "b", "c"}}, tiers); got != "public" {
		t.Errorf("coverTier = %q, want the first surviving member's tier %q", got, "public")
	}
	if got := coverTier(manifestPost{AssetIDs: []string{"a"}}, tiers); got != "" {
		t.Errorf("coverTier with no surviving member = %q, want empty", got)
	}
}

func TestAssetTierIndex_NormalisesLikeApplyAssets(t *testing.T) {
	tiers := assetTierIndex([]manifestAsset{
		{ID: "a", SensitivityTier: "team"},
		{ID: "b", SensitivityTier: ""},         // sensitivity() defaults to public
		{ID: "c", SensitivityTier: "nonsense"}, // ditto
	})
	for id, want := range map[string]string{"a": "team", "b": "public", "c": "public"} {
		if tiers[id] != want {
			t.Errorf("tier[%s] = %q, want %q", id, tiers[id], want)
		}
	}
}

// The gap the post.vis dimension exists to close: a catalogue can be
// full of posts DECLARING sensitivity_tier 'public' and still seed zero
// publicly visible rows, because each one's cover asset is team-tier. A
// required set that only asked for post.sens=public would be satisfied
// by that catalogue, and every anonymous spec fed by it would assert
// against an empty feed and pass on nothing.
func TestCoverageProfile_ErrorsWhenNoPostWouldBeSeededPublic(t *testing.T) {
	c := covFixture(t)
	for i := range c.Assets {
		c.Assets[i].SensitivityTier = "team"
	}
	for i := range c.Posts {
		c.Posts[i].SensitivityTier = "public"
	}

	rep, err := covRun(t, c, 4)
	if err == nil {
		t.Fatal("expected an error: no post in this catalogue can be seeded public")
	}
	if rep == nil {
		t.Fatal("report should come back even on the error path")
	}
	if !containsDim(rep.MissingReq, dim{dimPostVis, "public"}) {
		t.Errorf("MissingReq = %v, want it to name %s", rep.MissingReq, dim{dimPostVis, "public"})
	}
	// The declaration alone is everywhere in this catalogue — proof the
	// two dimensions are not interchangeable.
	if containsDim(rep.MissingReq, dim{dimPostSens, "public"}) {
		t.Errorf("MissingReq unexpectedly names %s", dim{dimPostSens, "public"})
	}
}

func TestCoverageProfile_KeepsAPubliclyVisiblePost(t *testing.T) {
	rep, err := covRun(t, covFixture(t), 4)
	if err != nil {
		t.Fatalf("coverage profile: %v", err)
	}
	if containsDim(rep.MissingReq, dim{dimPostVis, "public"}) {
		t.Errorf("MissingReq = %v, want a publicly visible post to survive selection", rep.MissingReq)
	}
	if containsDim(rep.Uncovered, dim{dimPostVis, "public"}) {
		t.Errorf("Uncovered = %v, want a publicly visible post to survive selection", rep.Uncovered)
	}
}
