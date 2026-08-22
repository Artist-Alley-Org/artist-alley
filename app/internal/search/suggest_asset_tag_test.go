// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1077 — THE TAG CORPUS HAS TWO HALVES AND THEY HAVE DIFFERENT GATES.
//
// facet.tagAgg has counted `asset_tag` beside `post_tags` since #907;
// suggest completed `post_tags` alone. So the rail could offer `sculpt
// 12` and typing `scul` returned nothing — the mismatch #1077 names.
//
// The issue's own warning is what this file is mostly about:
//
//	"the two sources have DIFFERENT visibility rules, so a naive UNION
//	 gated by only one of them re-opens #1075 on the other side.
//	 Whichever ships, the acceptance is the same shape: a tag reachable
//	 ONLY through an unreadable asset must not be completed, with a
//	 positive control for a readable one."
//
// suggest_tag_leak_test.go is that acceptance on the POST half. This is
// the same acceptance on the ASSET half, and it is deliberately the same
// SHAPE rather than a fresh idea: comparative, both directions, and with
// the same-string flip that proves the gate keys on the ROW's
// readability rather than on anything about the word.
//
// The gate here is the FIELD plane, not the bytes plane. A tag is one of
// the fields visibility.FieldsReadable withholds, which is the clause
// facet.buildAssetVisibilityAppendedSQL composes for the COUNTING half —
// and it has to be the same one, or the completion and the rail would
// describe different corpora.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs. assets.owner_user_ref carries no FK to the user table.
const (
	satOwner    int64 = 10771101
	satStranger int64 = 10771102
)

// satPrefix is the typed prefix. Nonsense on purpose so every completion
// in this file is attributable to this fixture and to nothing else in any
// developer's database.
const satPrefix = "zarnpholet"

const (
	// satHiddenTag lives ONLY on a `restricted` asset. It is the value
	// #1077's hazard is about: reachable through a row this caller may
	// not open, and therefore not completable by them.
	satHiddenTag = satPrefix + "-hidden"
	// satOpenTag lives on a `public/active/ready` asset. The positive
	// control, in the same call, for the same caller.
	satOpenTag = satPrefix + "-open"
)

// satAsset plants one asset at a sensitivity carrying `tag`.
func satAsset(t *testing.T, pool *pgxpool.Pool, owner int64, sensitivity, tag string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_extension)
		VALUES ($1,$2,$3,1,'active',$4,'ready','png')`,
		id, "sat fixture "+sensitivity, owner, sensitivity); err != nil {
		t.Fatalf("seed %s asset: %v", sensitivity, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_tag (asset_id, tag) VALUES ($1,$2)`, id, tag); err != nil {
		t.Fatalf("seed asset tag %q: %v", tag, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_tag WHERE asset_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// satTags runs the endpoint's service under a scope and returns the TAG
// completions as a set.
func satTags(t *testing.T, pool *pgxpool.Pool, ref *int64, scope suggest.Scope) map[string]bool {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix: satPrefix,
		Caller: visibility.NewCaller(ref),
		Scope:  scope,
		Limit:  suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	out := map[string]bool{}
	for _, s := range resp.Suggestions {
		if s.Kind == suggest.KindTag {
			out[s.Value] = true
		}
	}
	return out
}

// TestSuggestAssetTags_RestrictedAssetTagNeverCompletes is #1077's
// acceptance, verbatim: the negative and the positive control, for the
// same caller in the same call.
//
// ⚠️ The OWNER row is what makes this a gate test rather than a removal
// test. "Stop completing asset tags" satisfies every negative assertion
// here and closes nothing — it is #1077's option 2, which that issue
// argues against because the rail describes real results.
func TestSuggestAssetTags_RestrictedAssetTagNeverCompletes(t *testing.T) {
	pool := coPool(t)
	satAsset(t, pool, satOwner, "restricted", satHiddenTag)
	satAsset(t, pool, satOwner, "public", satOpenTag)

	stranger, owner := satStranger, satOwner
	for _, c := range []struct {
		name       string
		ref        *int64
		wantHidden bool
	}{
		{"anonymous", nil, false},
		{"a signed-in stranger", &stranger, false},
		// The owner may open their own restricted asset, so its tag is
		// theirs to complete. This is the direction a blanket removal
		// fails.
		{"the owner", &owner, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := satTags(t, pool, c.ref, suggest.ScopeSearch)

			if got[satHiddenTag] != c.wantHidden {
				if c.wantHidden {
					t.Errorf("the RESTRICTED asset's tag %q did not complete for its "+
						"own owner. #1077 option 2 (drop the corpus) passes every "+
						"other assertion in this file and closes nothing",
						satHiddenTag)
				} else {
					t.Errorf("the RESTRICTED asset's tag %q completed for %s. "+
						"/search/suggest takes a PREFIX, so this walks the alphabet "+
						"and reads back the tag vocabulary of work they were refused "+
						"— #1075 re-opened on the asset side, which is the hazard "+
						"#1077 names by name", satHiddenTag, c.name)
				}
			}

			// The positive control, in the same call: widening the corpus
			// actually widened it.
			if !got[satOpenTag] {
				t.Errorf("the PUBLIC asset's tag %q did not complete for %s — the "+
					"corpus is gated shut rather than gated, and #1077 is still open",
					satOpenTag, c.name)
			}
		})
	}
}

// TestSuggestAssetTags_SameTagStringOppositeVerdicts proves the gate
// keys on the ROW rather than on the word.
//
// One tag string, two assets, two sensitivities. A stranger must be
// completed it — because a readable asset carries it — and the reason
// must be that asset rather than the restricted one. So the restricted
// asset is REMOVED and the completion has to disappear with it: if it
// survives, the gate was never consulted and the earlier test passed for
// the wrong reason.
func TestSuggestAssetTags_SameTagStringOppositeVerdicts(t *testing.T) {
	pool := coPool(t)
	const shared = satPrefix + "-shared"

	satAsset(t, pool, satOwner, "restricted", shared)
	stranger := satStranger

	if satTags(t, pool, &stranger, suggest.ScopeSearch)[shared] {
		t.Fatalf("%q completed for a stranger while it existed only on a RESTRICTED "+
			"asset", shared)
	}

	open := satAsset(t, pool, satOwner, "public", shared)
	if !satTags(t, pool, &stranger, suggest.ScopeSearch)[shared] {
		t.Errorf("%q still did not complete after a READABLE asset acquired it — the "+
			"gate is keyed on the string rather than on the row", shared)
	}

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM asset_tag WHERE asset_id=$1`, open); err != nil {
		t.Fatalf("unseed: %v", err)
	}
	if satTags(t, pool, &stranger, suggest.ScopeSearch)[shared] {
		t.Errorf("%q completed again once only the RESTRICTED asset carried it — the "+
			"earlier PASS came from somewhere other than the gate", shared)
	}
}

// TestSuggestAssetTags_BrowseScopeOffersOnlyWhatBrowseCanExecute is the
// #1155 half of the same widening.
//
// Browse is `GET /posts`, whose `?tag=` reads `post_tags` — the shared
// grammar's tag dimension on the POST entity. An asset-only tag executed
// there returns an empty feed, so offering it under ScopeBrowse would
// reintroduce #1155's defect through the fix for its sibling: a
// completion the executing surface cannot answer.
//
// The assertion is comparative, like every other one here. The SAME tag
// is present under ScopeSearch — whose Engine matches `asset_tag`
// through the `tag:` dimension on EntityAsset — which is the proof this
// is a corpus gate and not a removal.
func TestSuggestAssetTags_BrowseScopeOffersOnlyWhatBrowseCanExecute(t *testing.T) {
	pool := coPool(t)
	const assetOnly = satPrefix + "-assetonly"
	satAsset(t, pool, satOwner, "public", assetOnly)

	stranger := satStranger
	if !satTags(t, pool, &stranger, suggest.ScopeSearch)[assetOnly] {
		t.Fatalf("%q did not complete on the SEARCH scope; the rest of this test "+
			"would be vacuous", assetOnly)
	}
	if satTags(t, pool, &stranger, suggest.ScopeBrowse)[assetOnly] {
		t.Errorf("%q completed on the BROWSE scope. Browse matches `post_tags`, no "+
			"post carries this tag, so picking it lands on an empty feed — #1155's "+
			"defect arriving through #1077's fix", assetOnly)
	}

	// The positive control for browse: a tag on a readable POST still
	// completes there, so the branch above removed exactly what browse
	// cannot answer and nothing else.
	postTag := satPrefix + "-postside"
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1,$2,$3,'fixture body','public')`,
		id, satOwner, satPrefix+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_tags (post_id, tag) VALUES ($1,$2)`, id, postTag); err != nil {
		t.Fatalf("seed post tag: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})

	if !satTags(t, pool, &stranger, suggest.ScopeBrowse)[postTag] {
		t.Errorf("%q did not complete on the BROWSE scope even though a readable "+
			"POST carries it — the scope branch removed more than the asset half",
			postTag)
	}
}

// TestSuggestAssetTags_ExecuteToAResult is the property #1155 states and
// #1077's fix has to keep: every tag this endpoint completes must return
// at least one row when EXECUTED — under the execution model a picked
// tag now has.
//
// ⭐ THAT MODEL CHANGED, AND THAT IS THE POINT OF #1077's SECOND HALF. A
// picked tag used to be committed as free text, and a tag-only word is
// in NO search document: `fantasy`, `kit` and `lowpoly` are each FALSE
// against their own asset's `assets.search_text` on the dev seed. So the
// executability question for a tag is no longer "does the text match"
// but "does the structured `tag:` filter return a row", which is what
// SearchBar's pick handler now sends (see web/src/lib/search/
// commitTarget.ts).
//
// This runs that filter — the real facet.Selection, through the real
// Engine — for each completion, rather than transcribing the predicate.
func TestSuggestAssetTags_ExecuteToAResult(t *testing.T) {
	pool := coPool(t)
	satAsset(t, pool, satOwner, "restricted", satHiddenTag)
	satAsset(t, pool, satOwner, "public", satOpenTag)

	stranger, owner := satStranger, satOwner
	for _, c := range []struct {
		name string
		ref  *int64
	}{
		{"anonymous", nil},
		{"a signed-in stranger", &stranger},
		{"the owner", &owner},
	} {
		t.Run(c.name, func(t *testing.T) {
			tags := satTags(t, pool, c.ref, suggest.ScopeSearch)
			if len(tags) == 0 {
				t.Fatalf("the fixture produced NO tag completions for %q — the "+
					"property would pass vacuously", satPrefix)
			}
			for tag := range tags {
				var n int
				pred, err := visibility.Filter(t.Context(), visibility.EntityAsset,
					visibility.NewCaller(c.ref))
				if err != nil {
					t.Fatalf("asset predicate: %v", err)
				}
				frag, args := pred.ToSQL("a", 1)
				qArgs := append([]any{tag}, args...)
				if err := pool.QueryRow(t.Context(), `
					SELECT COUNT(*) FROM assets a
					 WHERE EXISTS (SELECT 1 FROM asset_tag at
					                WHERE at.asset_id = a.id AND at.tag = $1)`+frag,
					qArgs...).Scan(&n); err != nil {
					t.Fatalf("execute tag:%s: %v", tag, err)
				}
				if n == 0 {
					t.Errorf("the completion %q returns ZERO assets for this viewer "+
						"under `filter=tag:%s`. A suggestion is a promise; this is "+
						"#1155's defect on the corpus #1077 added", tag, tag)
				}
			}
		})
	}
}
