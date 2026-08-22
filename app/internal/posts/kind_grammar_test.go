// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 — THE FEED'S `kind` FILTER, COMPOSED THROUGH THE SHARED GRAMMAR.
//
// kind_filter_test.go is the acceptance this sprint INHERITED rather
// than invented: badge agreement, the no-probe property, and the
// three-conjunct composition. It is unchanged, and it still passes.
// What is here is what the convergence adds, and each one exists because
// a specific wrong implementation passes everything else.
//
//  1. [TestKindSQLMatchesForAsset] — the oracle the old arms could not
//     have. The predicate is now viewkind.KindSQL, the SQL twin of the
//     Go resolver the card's badge comes from, so the whole vocabulary
//     can be driven through Postgres and diffed against Go. "The filter
//     agrees with the badge" stops being a property a fixture samples
//     and becomes one a test exhausts.
//
//  2. [TestKindFilter_RequestedAndAbsentAreOpposite] — the two readings
//     of an empty selection, which are one line apart in
//     ListPostsPageParams and differ by the whole feed. Losing the
//     distinction turns `?kind=nonsense` from an empty page into every
//     page, which is a filter WIDENING.
//
//  3. [TestKindFilter_MultiSelectIsTheUnion] and
//     [TestKindFilter_IntersectsWithTag] — the N≥2 arithmetic, written
//     out, because ADR 0093's amendment records that a count assertion
//     which passes on a union passes on the bug. Sprint 6 shipped
//     `907 + 596 → 1191` — the exact union — where the answer was 312,
//     and every single-filter test passed either way.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

// ---------------------------------------------------------------------------
// The oracle: SQL agrees with Go, for the whole vocabulary
// ---------------------------------------------------------------------------

// TestKindSQLMatchesForAsset drives every extension the resolver knows,
// under every overriding asset_type ref and under none, through
// [viewkind.KindSQL] in Postgres and compares each answer to
// [viewkind.ForAsset] in Go.
//
// This is the exhaustive twin the codebase already keeps for its
// security fragments (visibility.TestFieldsReadableSQL_MatchesGo,
// TestMatureFilterSQL_MatchesGo) applied to a derivation rather than a
// gate — and it is the assertion that makes badge agreement structural.
// The badge is drawn from ForAsset; the filter selects on KindSQL; if
// they ever disagree about one extension, this goes red naming that
// extension, rather than a card somewhere quietly failing to appear
// under its own label.
//
// It needs no fixture rows. The expression reads two columns and nothing
// else, so a VALUES list is a complete input for it — which also means
// the test costs one query and leaves nothing behind.
func TestKindSQLMatchesForAsset(t *testing.T) {
	pool := previewPool(t)

	// The refs that OVERRIDE the extension, plus two that do not and the
	// absent case. Hardcoded rather than derived from the override map,
	// so that a change to that map has to be noticed here too — the
	// parity test holds the map itself to the frontend.
	refs := []*int64{nil, kgRef(1), kgRef(2), kgRef(6), kgRef(11), kgRef(13)}

	exts := make([]*string, 0, len(viewkind.KnownExtensions())+8)
	for _, e := range viewkind.KnownExtensions() {
		exts = append(exts, kgExt(e))
	}
	// The edges the vocabulary does not contain, each of which the Go
	// resolver has a definite answer for.
	for _, e := range []string{"", "   ", "PNG", ".png", " .PnG ", "nosuchext", "tar.gz"} {
		exts = append(exts, kgExt(e))
	}
	exts = append(exts, nil)

	var (
		inRefs []*int64
		inExts []*string
		want   []string
	)
	for _, r := range refs {
		for _, e := range exts {
			inRefs = append(inRefs, r)
			inExts = append(inExts, e)
			want = append(want, string(viewkind.ForAsset(r, e)))
		}
	}

	rows, err := pool.Query(t.Context(), `
		SELECT `+viewkind.KindSQL("t")+`
		  FROM unnest($1::BIGINT[], $2::TEXT[])
		    WITH ORDINALITY AS t(asset_type, file_extension, n)
		 ORDER BY t.n`, inRefs, inExts)
	if err != nil {
		t.Fatalf("KindSQL: %v", err)
	}
	defer rows.Close()

	var i int
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if i >= len(want) {
			t.Fatalf("Postgres returned more rows than were sent")
		}
		if got != want[i] {
			describe := func(p *string) string {
				if p == nil {
					return "<NULL>"
				}
				return *p
			}
			ref := "<NULL>"
			if inRefs[i] != nil {
				ref = strconv.FormatInt(*inRefs[i], 10)
			}
			t.Errorf("asset_type=%s extension=%q: SQL says %q, ForAsset says %q — "+
				"the filter and the badge disagree about this row",
				ref, describe(inExts[i]), got, want[i])
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if i != len(want) {
		t.Fatalf("Postgres answered for %d of %d inputs", i, len(want))
	}
	// Cheap guard against a silently empty drive: if the input ever
	// collapsed to nothing, every assertion above would vacuously pass.
	if i < 500 {
		t.Errorf("only %d (asset_type, extension) pairs were checked; the vocabulary "+
			"is larger than that and this test has stopped exhausting it", i)
	}
}

func kgRef(v int64) *int64   { return &v }
func kgExt(v string) *string { return &v }

// ---------------------------------------------------------------------------
// The two meanings of an empty selection
// ---------------------------------------------------------------------------

// TestKindFilter_RequestedAndAbsentAreOpposite asserts the pair that
// ListPostsPageParams.KindsRequested exists for, on the same fixture in
// the same test, because they are one line apart in the source and
// differ by the whole feed.
//
// ABSENT (`?kind=` not sent, or sent blank) → no conjunct → the post is
// on the page. REQUESTED but naming nothing renderable (`?kind=nonsense`,
// `?kind=sequence`) → a filter that selects nothing → the page is empty.
//
// Collapsing them is the one direction a narrowing filter may never
// move: a typo in the query string would WIDEN the result and serve the
// whole feed under a label promising one kind.
//
// ⚠️ It asserts the WHOLE PAGE is empty, not merely that this fixture's
// post is absent. "Absent" is satisfied by a filter that returned the
// entire seeded corpus minus one row; "empty" is not.
func TestKindFilter_RequestedAndAbsentAreOpposite(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	post := kfPost(t, pool, kfAuthor, "public", png, png)

	// Absent — every meaning of "the caller did not ask". Note that a
	// bare "," is NOT one of them: viewkind.ParseList only reports an
	// absent filter for an empty-or-whitespace value, so "," is a
	// PRESENT filter naming no kind and belongs in the loop below. That
	// is the pre-existing reading and it is the fail-closed one.
	for _, absent := range []string{"", "   "} {
		got := kfFeed(t, h, kfAuthor, absent)
		kfAssertPresent(t, "kind="+absent+" (absent parameter → no conjunct)", got, post)
	}

	// Requested, and nothing renderable was named.
	for _, empty := range []string{",", "nonsense", "sequence", "nonsense,alsojunk", "sequence,nonsense"} {
		got := kfFeed(t, h, kfAuthor, empty)
		if len(got) != 0 {
			t.Errorf("kind=%s returned %d posts; a filter naming nothing renderable "+
				"must select NOTHING, not the whole feed", empty, len(got))
		}
	}

	// And the junk-beside-real case stays a NARROWING, which is why the
	// two wire vocabularies differ: `?kind=` is a comma list where there
	// is still something to narrow to, `filter=kind:` is one value where
	// there is not.
	narrowed := kfFeed(t, h, kfAuthor, "image,nonsense")
	kfAssertPresent(t, "kind=image,nonsense", narrowed, post)
}

// ---------------------------------------------------------------------------
// N ≥ 2, with the arithmetic written out
// ---------------------------------------------------------------------------

// kfCountOf reports how many of `ids` are on the page — the fixture's
// own contribution, so a seeded corpus around it cannot move the
// numbers.
func kfCountOf(got map[uuid.UUID]bool, ids ...uuid.UUID) int {
	n := 0
	for _, id := range ids {
		if got[id] {
			n++
		}
	}
	return n
}

// kfFeedTagged is kfFeed with the rail's tag chip beside the footer's
// type filter — the two-dimension page the browse surface actually
// renders. Empty strings mean "that control is not set", which is how
// the frontend spells it.
//
// `tags` is VARIADIC since #1251 slice 2 made `?tag=` repeatable, and
// blanks are dropped so the existing single-tag call sites — which spell
// "not set" as `""` — keep meaning exactly what they meant.
func kfFeedTagged(t *testing.T, h *Handler, callerRef int64, kind string, tags ...string) map[uuid.UUID]bool {
	t.Helper()
	ctx := context.Background()
	if callerRef != kfNoOneAt {
		ctx = auth.WithIdentity(ctx, &auth.Identity{UserRef: callerRef, AuthMethod: "session"})
	}
	limit := 200
	vis := openapi.ListPostsParamsVisibility("public")
	params := openapi.ListPostsParams{Limit: &limit, Visibility: &vis}
	if kind != "" {
		params.Kind = &kind
	}
	set := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag != "" {
			set = append(set, tag)
		}
	}
	if len(set) > 0 {
		params.Tag = &set
	}
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts(kind=%q tag=%v): %v", kind, tags, err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// TestKindFilter_MultiSelectIsTheUnion pins how two values of THIS
// dimension combine, with the arithmetic stated rather than sampled.
//
// The rule is OR — `?kind=image,video` is "show me images and videos",
// and the browse footer's boxes are a multi-select. So the count of the
// pair is the INCLUSION-EXCLUSION sum, and the fixture is built so that
// sum is not reachable by any other rule:
//
//	|image ∪ video| = |image| + |video| − |image ∩ video|
//	              4 =    2    +    3    −        1
//
// AND would give 1, "the first term wins" would give 2 or 3, and a
// dropped conjunct would give 5 (the whole fixture). No two of those are
// the same number.
func TestKindFilter_MultiSelectIsTheUnion(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	mp4 := kfAsset(t, pool, "mp4", 3, "public", kfAuthor)
	glb := kfAsset(t, pool, "glb", 5, "public", kfAuthor)

	imageOnly := kfPost(t, pool, kfAuthor, "public", png, png)
	videoOnly := kfPost(t, pool, kfAuthor, "public", mp4, mp4)
	bundle := kfPost(t, pool, kfAuthor, "public", uuid.Nil, glb, mp4)
	// ⭐ The overlap: one post that is BOTH, so the union is strictly
	// less than the sum and a test that ignored the overlap would be
	// asserting the wrong number.
	both := kfPost(t, pool, kfAuthor, "public", png, png, mp4)
	// Neither, so "the filter ran at all" is separable from "the filter
	// ran correctly".
	neither := kfPost(t, pool, kfAuthor, "public", glb, glb)

	all := []uuid.UUID{imageOnly, videoOnly, bundle, both, neither}

	images := kfFeed(t, h, kfAuthor, "image")
	videos := kfFeed(t, h, kfAuthor, "video")
	union := kfFeed(t, h, kfAuthor, "image,video")

	a := kfCountOf(images, all...)
	b := kfCountOf(videos, all...)
	overlap := kfCountOf(images, both) // the one post in both arms
	u := kfCountOf(union, all...)

	if a != 2 || b != 3 || overlap != 1 {
		t.Fatalf("the fixture is not what this test assumes: |image|=%d (want 2) "+
			"|video|=%d (want 3) |overlap|=%d (want 1)", a, b, overlap)
	}
	if u != a+b-overlap {
		t.Errorf("|image ∪ video| = %d, want %d = %d + %d − %d. AND would give %d, "+
			"a dropped conjunct would give %d",
			u, a+b-overlap, a, b, overlap, overlap, len(all))
	}
	// The union is strictly bigger than either arm, which is the half a
	// "first term wins" bug fails.
	if u <= a || u <= b {
		t.Errorf("|image ∪ video| = %d is not strictly larger than both arms "+
			"(%d, %d) — one of the terms is being discarded", u, a, b)
	}
	kfAssertAbsent(t, "kind=image,video (a post that is neither)", union, neither)
}

// TestKindFilter_IntersectsWithTag is the cross-dimension arithmetic:
// `kind` beside `tag` must INTERSECT.
//
// TestKindFilter_ComposesWithTagAndTeam already asserts the membership
// version of this and is the regression test this sprint inherits. This
// one asserts the NUMBER, because ADR 0093's amendment records the exact
// failure a membership assertion survives: two dimensions that ORed
// returned `907 + 596 = 1191` where the answer was 312, and every
// single-filter check passed either way. `both > 0` passes on the union;
// `both < min(a, b)` does not.
func TestKindFilter_IntersectsWithTag(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	mp4 := kfAsset(t, pool, "mp4", 3, "public", kfAuthor)

	imageTagged := kfPost(t, pool, kfAuthor, "public", png, png)
	imageUntagged := kfPost(t, pool, kfAuthor, "public", png, png)
	videoTagged := kfPost(t, pool, kfAuthor, "public", mp4, mp4)

	const tag = "kg-intersect"
	for _, id := range []uuid.UUID{imageTagged, videoTagged} {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO post_tags (post_id, tag) VALUES ($1,$2)`, id, tag); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM post_tags WHERE post_id = ANY($1)`,
			[]uuid.UUID{imageTagged, videoTagged})
	})

	all := []uuid.UUID{imageTagged, imageUntagged, videoTagged}

	byKind := kfCountOf(kfFeedTagged(t, h, kfAuthor, "image", ""), all...)
	byTag := kfCountOf(kfFeedTagged(t, h, kfAuthor, "", tag), all...)
	byBoth := kfCountOf(kfFeedTagged(t, h, kfAuthor, "image", tag), all...)

	if byKind != 2 || byTag != 2 {
		t.Fatalf("the fixture is not what this test assumes: |kind=image|=%d (want 2) "+
			"|tag|=%d (want 2)", byKind, byTag)
	}
	min := byKind
	if byTag < min {
		min = byTag
	}
	if byBoth >= min {
		t.Errorf("kind+tag returned %d, which is not strictly fewer than min(%d, %d). "+
			"The union would be %d and it is the number a `both > 0` assertion "+
			"cannot tell apart from the intersection",
			byBoth, byKind, byTag, byKind+byTag-1)
	}
	if byBoth != 1 {
		t.Errorf("kind+tag returned %d, want exactly 1 (the image post that carries "+
			"the tag)", byBoth)
	}
}
