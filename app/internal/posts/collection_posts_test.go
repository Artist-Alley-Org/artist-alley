// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #882 — collecting someone ELSE's post.
//
// The owner's sentence was: "allow users to add other users' posts or
// single assets to their own collections. The owner can still delete it
// from everywhere." Four separable claims live in that sentence and
// each one has a test here:
//
//  1. You can add ANOTHER USER's post to YOUR collection —
//     TestAddCollectionPost_MemberGate's "org-only post by a stranger"
//     case. Without it every other case in this file passes on a
//     handler that refuses everything.
//  2. You can only add what you can READ —
//     TestAddCollectionPost_MemberGate's private / followers /
//     soft-deleted cases, plus the byte-for-byte comparison against a
//     nonexistent UUID that keeps the refusal from being an
//     enumeration oracle.
//  3. The AUTHOR can still delete it from everywhere —
//     TestCollectionPost_OriginDeleteRemovesTheReference, which drives
//     BOTH deletes the product has: the soft delete the API performs,
//     and the hard delete the FK cascade answers.
//  4. Removing is YOURS alone —
//     TestRemoveCollectionPost_TouchesNothingElse.
//
// Every refusal asserts the PERSISTED `collection_posts` rows, not the
// response body. A 404 whose write went through anyway is not a
// refusal, and a body assertion cannot tell the difference.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	cpCurator int64 = 8820001 // owns the collection, does the adding
	cpAuthor  int64 = 8820002 // writes the posts; never calls anything
	cpThird   int64 = 8820003 // owns a SECOND collection over the same post
)

// cpSeedPost writes one post by cpAuthor at a given tier. Direct SQL
// rather than CreatePost so the matrix is built from the COLUMN's
// catalogue rather than from whatever the write gate admits today —
// the two disagreed for most of this file's life (`public` was
// unwritable through the API until #1176) and the gate under test here
// is the READ rule, which has to answer for every value the column can
// hold however it got there.
func cpSeedPost(t *testing.T, pool *pgxpool.Pool, visibility, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,$3,$4)`,
		id, cpAuthor, title, visibility); err != nil {
		t.Fatalf("seed post (%s): %v", visibility, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// cpSeedCollection creates a collection through the REAL create handler.
//
// Not a raw INSERT: the collection is the container half of the gate
// under test, and a fixture built by hand is exactly the shape that
// passes a test while the product path writes something different
// (a missing `membership`, a NULL owner). The handler is cheap to call
// and it is what users hit.
func cpSeedCollection(t *testing.T, pool *pgxpool.Pool, owner int64, name string) uuid.UUID {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := collections.NewHandler(pool, logger, nil)
	ch.SetActivitiesWriter(activities.NewWriter(pool, logger, nil),
		func(ctx context.Context) string { return "https://test.example" })

	vis := openapi.CollectionCreateVisibilityPrivate
	resp, err := ch.CreateCollection(ctxAs(owner), openapi.CreateCollectionRequestObject{
		Body: &openapi.CollectionCreate{Name: name, Visibility: &vis},
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	created, ok := resp.(openapi.CreateCollection201JSONResponse)
	if !ok {
		t.Fatalf("CreateCollection returned %T, want 201", resp)
	}
	id := uuid.UUID(created.Id)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE collection_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id=$1`, id)
	})
	return id
}

// cpMembership asks the DATABASE how many membership rows exist for the
// pair. "The write did not happen" must not be answered by the same
// code path that might have let it happen.
func cpMembership(t *testing.T, pool *pgxpool.Pool, colID, postID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collection_posts WHERE collection_id=$1 AND post_id=$2`,
		colID, postID).Scan(&n); err != nil {
		t.Fatalf("count collection_posts: %v", err)
	}
	return n
}

// cpAdd drives the real handler.
func cpAdd(t *testing.T, h *Handler, caller int64, colID, postID uuid.UUID) openapi.AddCollectionPostResponseObject {
	t.Helper()
	resp, err := h.AddCollectionPost(ctxAs(caller), openapi.AddCollectionPostRequestObject{
		Id:   openapi_types.UUID(colID),
		Body: &openapi.AddCollectionPostJSONRequestBody{PostId: openapi_types.UUID(postID)},
	})
	if err != nil {
		t.Fatalf("AddCollectionPost: %v", err)
	}
	return resp
}

// cpRefusalBody returns the refusal's exact error string, or fails if
// the response was not a 404. The post UUID is never echoed into it, so
// two refusals naming different posts are directly comparable.
func cpRefusalBody(t *testing.T, resp openapi.AddCollectionPostResponseObject) string {
	t.Helper()
	nf, is := resp.(openapi.AddCollectionPost404JSONResponse)
	if !is {
		t.Fatalf("response is %T, want AddCollectionPost404JSONResponse", resp)
	}
	return nf.Error
}

// TestAddCollectionPost_MemberGate is #882's post half.
//
// The DISCRIMINATING case is "org-only post by a stranger" paired with
// "private post by a stranger". A handler with no member gate at all
// passes the first and fails the second; a handler that refuses
// everything passes the second and fails the first. Only a gate that
// runs the real post read rule passes both, and the remaining cases
// close the cheap ways of faking it.
func TestAddCollectionPost_MemberGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	col := cpSeedCollection(t, pool, cpCurator, "cp_gate")

	orgOnly := cpSeedPost(t, pool, "org-only", "cp org-only")
	private := cpSeedPost(t, pool, "private", "cp private")
	followers := cpSeedPost(t, pool, "followers", "cp followers")
	deleted := cpSeedPost(t, pool, "org-only", "cp deleted")
	if _, err := pool.Exec(context.Background(),
		`UPDATE posts SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatalf("soft-delete post: %v", err)
	}
	missing := uuid.New()

	cases := []struct {
		name    string
		post    uuid.UUID
		wantRow int
		why     string
	}{
		{
			name: "org-only post by a stranger", post: orgOnly, wantRow: 1,
			why: "THE FEATURE. Collecting someone else's work is the whole of #882 — " +
				"a deny-everything gate passes every other case here and fails only this one",
		},
		{
			name: "private post by a stranger", post: private, wantRow: 0,
			why: "THE DISCRIMINATING REFUSAL. A handler with no member gate — which is " +
				"what shipped for collection_resources before #898 and what this endpoint " +
				"would have been without one — pins it happily",
		},
		{
			name: "followers-tier post, caller does not follow", post: followers, wantRow: 0,
			why: "the relationship tiers resolve through user_follows in the rule's own " +
				"statement; a non-follower is refused. A gate that hardcoded " +
				"{public, org-only} as 'the readable tiers' passes this by accident and " +
				"would refuse a FOLLOWED author's post too",
		},
		{
			name: "soft-deleted post by a stranger", post: deleted, wantRow: 0,
			why: "the soft-delete conjunct on its own account. It is not part of the read " +
				"rule expression — Predicate.ToSQL adds it, and IncludeSoftDeleted can " +
				"waive it — so a gate that passed IncludeSoftDeleted would pin a deleted " +
				"post and create a membership the listing then drops",
		},
		{
			name: "nonexistent uuid", post: missing, wantRow: 0,
			why: "unchanged behaviour; its BODY is compared against the private case below",
		},
	}

	bodies := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := cpAdd(t, h, cpCurator, col, tc.post)
			if tc.wantRow == 1 {
				if _, ok := resp.(openapi.AddCollectionPost204Response); !ok {
					t.Fatalf("response is %T, want 204\nwhy this case exists: %s", resp, tc.why)
				}
			} else {
				bodies[tc.name] = cpRefusalBody(t, resp)
			}
			// The assertion that matters: what PERSISTED. A refusal that
			// still wrote the membership is not a refusal, and the
			// response object cannot tell you which happened.
			if got := cpMembership(t, pool, col, tc.post); got != tc.wantRow {
				t.Errorf("collection_posts rows = %d, want %d\nwhy this case exists: %s",
					got, tc.wantRow, tc.why)
			}
		})
	}

	// The enumeration assertion. An unreadable post and a nonexistent
	// one must be indistinguishable: same status (asserted above by both
	// having gone through cpRefusalBody) AND the same bytes. If the
	// private case ever answers 403, or "forbidden", or anything the
	// missing case does not, POST becomes a UUID-existence probe — the
	// caller learns that a UUID they were never shown names a real post.
	if bodies["private post by a stranger"] != bodies["nonexistent uuid"] {
		t.Errorf("an unreadable post and a nonexistent one must be BYTE-IDENTICAL:\n"+
			"  unreadable:  %q\n  nonexistent: %q",
			bodies["private post by a stranger"], bodies["nonexistent uuid"])
	}
}

// TestAddCollectionPost_ContainerGate is the other half of the pair:
// the post is perfectly readable and the COLLECTION is not the
// caller's.
//
// It also pins the response SHAPE. The asset route answers 403 "not the
// owner of this collection" here, which tells an outsider that the
// collection exists; this route answers 404, the same as an absent one.
// If someone later "aligns" the two by copying the asset route's 403
// onto this one, this fails.
func TestAddCollectionPost_ContainerGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	theirs := cpSeedCollection(t, pool, cpThird, "cp_not_mine")
	post := cpSeedPost(t, pool, "org-only", "cp container")
	missingCol := uuid.New()

	resp := cpAdd(t, h, cpCurator, theirs, post)
	if _, is := resp.(openapi.AddCollectionPost404JSONResponse); !is {
		t.Fatalf("adding to someone else's collection returned %T, want a 404 — "+
			"a 403 here confirms the collection exists to a caller who holds nothing on it", resp)
	}
	if n := cpMembership(t, pool, theirs, post); n != 0 {
		t.Errorf("collection_posts rows = %d, want 0 — the refusal wrote the row anyway", n)
	}

	// Absent collection: same response, so the two are not separable.
	absent := cpAdd(t, h, cpCurator, missingCol, post)
	if cpRefusalBody(t, absent) != cpRefusalBody(t, resp) {
		t.Errorf("a collection that is not yours and one that does not exist must be "+
			"BYTE-IDENTICAL:\n  not yours: %q\n  absent:    %q",
			cpRefusalBody(t, resp), cpRefusalBody(t, absent))
	}
}

// TestCollectionPost_OriginDeleteRemovesTheReference is the owner's
// second clause: "the owner can still delete it from everywhere."
//
// TWO deletes, because the product has two and only one of them is the
// FK cascade the schema advertises:
//
//   - The SOFT delete, which is what DELETE /posts/{id} performs and
//     therefore what actually happens when an author deletes their
//     work. It leaves the `collection_posts` row in place — the FK
//     cascade cannot fire, nothing was removed from `posts` — so the
//     reference stops appearing only because the LISTING excludes
//     `deleted_at IS NOT NULL`. That conjunct is the whole mechanism on
//     the product path, and if it is ever dropped the row is still
//     sitting there to be rendered.
//   - The HARD delete, which is the purge / admin path, where
//     `collection_posts_post_id_fkey … ON DELETE CASCADE` removes the
//     row itself.
//
// Both are asserted in SQL rather than through the API, because "it
// disappeared from the collection" is a claim about rows.
func TestCollectionPost_OriginDeleteRemovesTheReference(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	ctx := context.Background()

	col := cpSeedCollection(t, pool, cpCurator, "cp_cascade")
	post := cpSeedPost(t, pool, "org-only", "cp cascade")

	if _, ok := cpAdd(t, h, cpCurator, col, post).(openapi.AddCollectionPost204Response); !ok {
		t.Fatal("setup: the curator could not pin the author's org-only post")
	}
	if n := cpMembership(t, pool, col, post); n != 1 {
		t.Fatalf("setup: collection_posts rows = %d, want 1", n)
	}

	// --- the SOFT delete, through the production handler, as the author.
	resp, err := h.DeletePost(ctxAs(cpAuthor), openapi.DeletePostRequestObject{
		Id: openapi_types.UUID(post),
	})
	if err != nil {
		t.Fatalf("DeletePost: %v", err)
	}
	if _, ok := resp.(openapi.DeletePost204Response); !ok {
		t.Fatalf("DeletePost returned %T, want 204", resp)
	}

	var softDeleted bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at IS NOT NULL FROM posts WHERE id=$1`, post).Scan(&softDeleted); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if !softDeleted {
		t.Fatal("the API's delete is not a soft delete any more — this test's premise moved")
	}
	// The membership row SURVIVES a soft delete. Asserted rather than
	// assumed: it is why the listing's conjunct is load-bearing.
	if n := cpMembership(t, pool, col, post); n != 1 {
		t.Errorf("collection_posts rows after SOFT delete = %d, want 1 — if the row is "+
			"gone the mechanism changed and the listing's deleted_at conjunct is now "+
			"belt-and-braces rather than the thing doing the work", n)
	}
	// …and the reference is nonetheless gone from the curator's view.
	// The DISQUALIFIED viewer + no admin waiver (#1147). Every fixture in
	// this file is non-mature, and MatureItemVisible's first branch says a
	// non-mature item is visible on this axis to everybody — so the axis
	// is inert here and these assertions are about the read rule alone.
	ids, err := h.ListCollectionPostsGated(ctx,
		&auth.Identity{UserRef: cpCurator, AuthMethod: "session"}, col, 50,
		visibility.MatureViewer{}, false)
	if err != nil {
		t.Fatalf("ListCollectionPostsGated: %v", err)
	}
	for _, got := range ids {
		if uuid.UUID(got.Bytes) == post {
			t.Error("a soft-deleted post is still listed in the collection that " +
				"referenced it — the author deleted it and it did not go away")
		}
	}

	// --- the HARD delete: the FK cascade the schema promises.
	if _, err := pool.Exec(ctx, `DELETE FROM posts WHERE id=$1`, post); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if n := cpMembership(t, pool, col, post); n != 0 {
		t.Errorf("collection_posts rows after HARD delete = %d, want 0 — "+
			"collection_posts_post_id_fkey is supposed to be ON DELETE CASCADE", n)
	}
}

// TestRemoveCollectionPost_TouchesNothingElse is the third clause:
// removing someone else's post from YOUR collection is yours alone.
//
// Two collections owned by two different people reference the same
// post. One removes it. The assertions are all in SQL: the post row
// still exists, and the OTHER collection still references it.
func TestRemoveCollectionPost_TouchesNothingElse(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	ctx := context.Background()

	mine := cpSeedCollection(t, pool, cpCurator, "cp_mine")
	theirs := cpSeedCollection(t, pool, cpThird, "cp_theirs")
	post := cpSeedPost(t, pool, "org-only", "cp shared")

	for _, s := range []struct {
		caller int64
		col    uuid.UUID
	}{{cpCurator, mine}, {cpThird, theirs}} {
		if _, ok := cpAdd(t, h, s.caller, s.col, post).(openapi.AddCollectionPost204Response); !ok {
			t.Fatalf("setup: %d could not pin the post", s.caller)
		}
	}

	resp, err := h.RemoveCollectionPost(ctxAs(cpCurator), openapi.RemoveCollectionPostRequestObject{
		Id:     openapi_types.UUID(mine),
		PostId: openapi_types.UUID(post),
	})
	if err != nil {
		t.Fatalf("RemoveCollectionPost: %v", err)
	}
	if _, ok := resp.(openapi.RemoveCollectionPost204Response); !ok {
		t.Fatalf("RemoveCollectionPost returned %T, want 204", resp)
	}

	if n := cpMembership(t, pool, mine, post); n != 0 {
		t.Errorf("my collection still references the post after I removed it (rows=%d)", n)
	}
	if n := cpMembership(t, pool, theirs, post); n != 1 {
		t.Errorf("removing from MY collection changed SOMEONE ELSE's (rows=%d, want 1) — "+
			"the delete is missing its collection_id predicate", n)
	}
	var alive bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM posts WHERE id=$1 AND deleted_at IS NULL)`, post).Scan(&alive); err != nil {
		t.Fatalf("probe post: %v", err)
	}
	if !alive {
		t.Error("un-pinning a post from a collection deleted the POST — a reference is " +
			"not ownership, and removing one must never reach the referent")
	}

	// Removal of a membership that is not there is a no-op, not an
	// error and not an existence probe.
	again, err := h.RemoveCollectionPost(ctxAs(cpCurator), openapi.RemoveCollectionPostRequestObject{
		Id:     openapi_types.UUID(mine),
		PostId: openapi_types.UUID(post),
	})
	if err != nil {
		t.Fatalf("RemoveCollectionPost (repeat): %v", err)
	}
	if _, ok := again.(openapi.RemoveCollectionPost204Response); !ok {
		t.Errorf("removing an absent membership returned %T, want 204", again)
	}
}

// TestListCollectionPosts_MembershipNeverWidens is #883's rule applied
// to the surface #882 creates. Being IN a collection is not a grant.
//
// The fixture is the state #882 makes ordinary: a collection holding a
// post the CURATOR may read and one they may not. The second is pinned
// directly, which is the state a moderator's add, a tier change after
// pinning, or a restore reaches — the gate on ADD stops it being
// created by the curator, and the LISTING is what stops it being
// rendered once it exists by some other route. Two gates; this one is
// the second.
func TestListCollectionPosts_MembershipNeverWidens(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	ctx := context.Background()

	col := cpSeedCollection(t, pool, cpCurator, "cp_listing")
	readable := cpSeedPost(t, pool, "org-only", "cp listed")
	hidden := cpSeedPost(t, pool, "private", "cp hidden")

	for i, p := range []uuid.UUID{readable, hidden} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
			 VALUES ($1,$2,$3,TRUE)`, col, p, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}

	got := func(caller int64) map[uuid.UUID]bool {
		t.Helper()
		ids, err := h.ListCollectionPostsGated(ctx,
			&auth.Identity{UserRef: caller, AuthMethod: "session"}, col, 50,
			visibility.MatureViewer{}, false)
		if err != nil {
			t.Fatalf("ListCollectionPostsGated(%d): %v", caller, err)
		}
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[uuid.UUID(id.Bytes)] = true
		}
		return out
	}

	curatorSees := got(cpCurator)
	if !curatorSees[readable] {
		t.Error("the curator cannot see the org-only post they pinned — the read rule is " +
			"refusing the tier every authenticated caller is supposed to have")
	}
	if curatorSees[hidden] {
		t.Error("the curator can see the AUTHOR'S PRIVATE post because it sits in their " +
			"own collection — membership widened the post, which is exactly what #883 " +
			"closed on the asset side")
	}

	authorSees := got(cpAuthor)
	if !authorSees[hidden] || !authorSees[readable] {
		t.Errorf("the author cannot see their own posts in someone else's collection "+
			"(readable=%v hidden=%v) — the rule lost the author disjunct",
			authorSees[readable], authorSees[hidden])
	}

	// Anonymous — the nil-identity path, which is how the listing is
	// reached with public mode on. Neither tier is public, so nothing.
	//
	// Spelled as nil rather than as an Identity carrying ref 0: an
	// Identity is anonymous when its AUTH METHOD says so
	// (auth.Identity.IsAnonymous), not when its ref happens to be zero,
	// so a hand-built {UserRef: 0, AuthMethod: "session"} takes the
	// AUTHENTICATED branch and sees the org-only post. Asserting against
	// that would have been asserting against a fixture nobody can
	// construct at runtime.
	anon, err := h.ListCollectionPostsGated(ctx, nil, col, 50, visibility.MatureViewer{}, false)
	if err != nil {
		t.Fatalf("ListCollectionPostsGated(anon): %v", err)
	}
	if len(anon) != 0 {
		t.Errorf("an anonymous caller sees %d posts in a collection of org-only + "+
			"private work", len(anon))
	}

	// The anonymous branch is not simply refusing everything: a public
	// post in the same collection IS listed to it. Without this, the
	// assertion above passes just as happily on a rule that returns
	// nothing at all to anyone.
	//
	// `public` is seeded directly for the same reason cpSeedPost exists:
	// the read rule has to answer for the value whatever wrote it.
	pub := cpSeedPost(t, pool, "public", "cp public")
	if _, err := pool.Exec(ctx,
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
		 VALUES ($1,$2,2,TRUE)`, col, pub); err != nil {
		t.Fatalf("seed public membership: %v", err)
	}
	anon, err = h.ListCollectionPostsGated(ctx, nil, col, 50, visibility.MatureViewer{}, false)
	if err != nil {
		t.Fatalf("ListCollectionPostsGated(anon, public): %v", err)
	}
	if len(anon) != 1 || uuid.UUID(anon[0].Bytes) != pub {
		t.Errorf("an anonymous caller sees %d posts, want exactly the public one — "+
			"the empty result above was a rule that refuses everyone, not one that "+
			"refuses the right people", len(anon))
	}
}

// TestListCollectionPosts_ParentGate covers the other half of the
// listing: the collection itself.
//
// Both gates are required and they answer different questions. This one
// is what stops a stranger enumerating a private collection's contents;
// the member gate above is what stops a legitimate reader of the
// collection reaching a post inside it that is not theirs to read.
func TestListCollectionPosts_ParentGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	col := cpSeedCollection(t, pool, cpCurator, "cp_parent")
	post := cpSeedPost(t, pool, "org-only", "cp parent")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
		 VALUES ($1,$2,0,TRUE)`, col, post); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	resp, err := h.ListCollectionPosts(ctxAs(cpThird), openapi.ListCollectionPostsRequestObject{
		Id: openapi_types.UUID(col),
	})
	if err != nil {
		t.Fatalf("ListCollectionPosts: %v", err)
	}
	if _, is := resp.(openapi.ListCollectionPosts404JSONResponse); !is {
		t.Fatalf("a stranger listing a PRIVATE collection's posts got %T, want 404 — "+
			"the post inside is org-only and therefore readable by them, so the ONLY "+
			"thing standing between them and the curator's private shelf is this gate", resp)
	}

	// The owner gets it, so the gate is not simply refusing everyone.
	ok, err := h.ListCollectionPosts(ctxAs(cpCurator), openapi.ListCollectionPostsRequestObject{
		Id: openapi_types.UUID(col),
	})
	if err != nil {
		t.Fatalf("ListCollectionPosts (owner): %v", err)
	}
	list, is := ok.(openapi.ListCollectionPosts200JSONResponse)
	if !is {
		t.Fatalf("the owner listing their own collection got %T, want 200", ok)
	}
	if len(list.Items) != 1 || uuid.UUID(list.Items[0].Id) != post {
		t.Errorf("owner sees %d items, want the 1 post they pinned", len(list.Items))
	}
}

// TestListCollectionPosts_MatureMemberIsAbsent — #1147's sweep find in
// this file.
//
// The two sibling listings, ListSharedWithMeGated and
// ListPostsByAssetGated, both took the mature conjunct in #1116. This
// one is the same rule reached through a collection and it was missed,
// which mattered more than either of theirs: the ids it returns go on to
// fetchFullPost + enrichForCaller, so a disqualified viewer got the
// mature post's cover ids AND its members' thumbhashes. A thumbhash is a
// blur — that is a picture, not a listing.
//
// The post is PUBLIC, so no read-rule conjunct is what hides it, and it
// is authored by somebody other than the caller so the owner exemption
// is not what shows it. The only difference between the two legs is the
// reader's opt-in.
func TestListCollectionPosts_MatureMemberIsAbsent(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	ctx := context.Background()

	col := cpSeedCollection(t, pool, cpCurator, "cp_mature_listing")
	plain := cpSeedPost(t, pool, "public", "cp plain")
	adult := cpSeedPost(t, pool, "public", "cp adult")
	// Set through the column the trigger maintains, so this asserts
	// against the value the product produces rather than a hand-written
	// one. (The trigger's own coverage lives with the migration.)
	if _, err := pool.Exec(ctx, `UPDATE posts SET mature = TRUE WHERE id = $1`, adult); err != nil {
		t.Fatalf("mark post mature: %v", err)
	}
	for i, p := range []uuid.UUID{plain, adult} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
			 VALUES ($1,$2,$3,TRUE)`, col, p, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}

	list := func(v visibility.MatureViewer) map[uuid.UUID]bool {
		t.Helper()
		ids, err := h.ListCollectionPostsGated(ctx,
			&auth.Identity{UserRef: cpCurator, AuthMethod: "session"}, col, 50, v, false)
		if err != nil {
			t.Fatalf("ListCollectionPostsGated: %v", err)
		}
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[uuid.UUID(id.Bytes)] = true
		}
		return out
	}

	out := list(visibility.MatureViewer{})
	if out[adult] {
		t.Errorf("the mature post is listed for a viewer who never opted in. It is "+
			"PUBLIC, so the read rule was never going to catch it — and the caller "+
			"goes on to enrich each id with its cover and its members' thumbhashes "+
			"(post=%s)", adult)
	}
	if !out[plain] {
		t.Error("the non-mature post vanished too — the conjunct is filtering the whole " +
			"listing rather than the mature rows")
	}

	in := list(visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true})
	if !in[adult] || !in[plain] {
		t.Errorf("qualified listing = %v, want both posts — the withholding above has to "+
			"be the mature axis and not an outage", in)
	}
}
