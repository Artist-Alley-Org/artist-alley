// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119: `GET /posts/{id}/collections`, the read the post editor's
// membership section is built on.
//
// # ⛔ THE ASSERTION THIS FILE EXISTS FOR
//
// The editor draws a Remove button from `can_remove`, so `can_remove` is
// an AUTHORIZATION answer wearing a boolean. The thing that must not
// ship is the easy version of it ("the caller wrote the post, so let
// them tidy their own shelves"), because membership is COLLECTION-owned
// (#882): removal is the curator's act and authorship confers nothing.
//
// So the central fixture is MIXED AUTHORITY. One post, by one author, on
// two shelves: one the author owns and one a stranger owns. A single
// permissive fixture would pass whether the flag asked about the
// collection or about the post, which is exactly how an authorization
// widening reaches production looking tested.
//
// # And the cardinalities, because a list is where they hide
//
// N=0 (no shelf), N=1 (one shelf) and N>=2 (two shelves with DIFFERENT
// answers) are separate tests. A flag read off the first row, or a loop
// that resolved authority once and reused it, passes N=0 and N=1 and
// fails only the mixed pair.
//
// Skips without AA_DB_PASSWORD, same convention as the other integration
// suites in this package.

package collections_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	puAuthor    int64 = 11190001 // writes the post; owns shelf A
	puCurator   int64 = 11190002 // owns shelf B; never touches the post
	puStranger  int64 = 11190003 // signed in, no relationship whatsoever
	puCollAdmin int64 = 11190004 // collections.admin, owns nothing
)

// puSeedPost inserts a post directly. Direct SQL rather than CreatePost
// keeps the fixture independent of the activities writer and the member
// hydration, neither of which this read touches.
func puSeedPost(t *testing.T, pool *pgxpool.Pool, author int64, visibility string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility)
		 VALUES ($1, $2, 'pu_1119 post', '', $3)`,
		id, author, visibility); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE post_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

func puSeedCollection(t *testing.T, pool *pgxpool.Pool, owner int64, name, visibility string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collections (id, name, owner_user_ref, visibility)
		 VALUES ($1, $2, $3, $4)`,
		id, name, owner, visibility); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE collection_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

func puPin(t *testing.T, pool *pgxpool.Pool, colID, postID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
		 VALUES ($1, $2, 0, TRUE)`, colID, postID); err != nil {
		t.Fatalf("pin post: %v", err)
	}
}

func puIdentity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

func puList(
	t *testing.T,
	h *collections.Handler,
	id *auth.Identity,
	postID uuid.UUID,
) openapi.ListPostCollectionsResponseObject {
	t.Helper()
	resp, err := h.ListPostCollections(
		auth.WithIdentity(context.Background(), id),
		openapi.ListPostCollectionsRequestObject{Id: openapi_types.UUID(postID)},
	)
	if err != nil {
		t.Fatalf("ListPostCollections: %v", err)
	}
	return resp
}

func puOK(t *testing.T, resp openapi.ListPostCollectionsResponseObject) openapi.PostCollectionUsage {
	t.Helper()
	ok, is := resp.(openapi.ListPostCollections200JSONResponse)
	if !is {
		t.Fatalf("want 200, got %T", resp)
	}
	return openapi.PostCollectionUsage(ok)
}

// puFlags reduces a listing to collection id -> can_remove, so an
// assertion names the shelf it is about rather than an array index.
func puFlags(u openapi.PostCollectionUsage) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, m := range u.Items {
		out[uuid.UUID(m.Collection.Id)] = m.CanRemove
	}
	return out
}

// nil registry: this read never touches the by-id cache, and the other
// collections tests wire it the same way.
func puNewHandler(t *testing.T, pool *pgxpool.Pool) *collections.Handler {
	t.Helper()
	return collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// ⛔ THE TEST THIS FILE IS FOR. One post, two shelves, two answers.
//
// A build that granted removal from post authorship passes every other
// test in this file and fails this one on shelf B. A build that granted
// it from the collection's own owner check passes both.
func TestListPostCollections_MixedAuthorityIsPerCollection(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	mine := puSeedCollection(t, pool, puAuthor, "pu_1119 mine", "public")
	theirs := puSeedCollection(t, pool, puCurator, "pu_1119 theirs", "public")
	puPin(t, pool, mine, postID)
	puPin(t, pool, theirs, postID)

	got := puFlags(puOK(t, puList(t, h, puIdentity(puAuthor), postID)))
	if len(got) != 2 {
		t.Fatalf("the author should see BOTH public shelves; got %d items", len(got))
	}
	if !got[mine] {
		t.Errorf("can_remove on the author's OWN collection = false, want true: " +
			"the curator of that shelf is the author")
	}
	if got[theirs] {
		t.Errorf("can_remove on ANOTHER USER's collection = true, want false. " +
			"⛔ authoring the post must not confer removal authority over " +
			"somebody else's shelf (#882). This is an authorization widening, " +
			"not a display bug: the editor draws a Remove button from this flag.")
	}
}

// The same fixture from the OTHER side: the curator's own answer is the
// mirror image. Asserted because "false for everybody but the admin"
// would also pass the test above.
func TestListPostCollections_TheCuratorCanRemoveFromTheirOwnShelf(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	mine := puSeedCollection(t, pool, puAuthor, "pu_1119 author shelf", "public")
	theirs := puSeedCollection(t, pool, puCurator, "pu_1119 curator shelf", "public")
	puPin(t, pool, mine, postID)
	puPin(t, pool, theirs, postID)

	// The curator is not the author, so they may not ASK, which is the
	// gate's own assertion below. `collections.admin` is the principal
	// who can both ask and answer here, and the flag has to follow the
	// COLLECTION for them too: an admin may mutate any shelf.
	got := puFlags(puOK(t, puList(t, h, puIdentity(puCollAdmin, "posts.admin", "collections.admin"), postID)))
	if !got[mine] || !got[theirs] {
		t.Errorf("collections.admin: can_remove = %v/%v, want true/true: "+
			"canMutateCollection admits the capability holder on every shelf",
			got[mine], got[theirs])
	}
}

// N=0. The zero state is a real answer, not an error and not a nil
// slice: the editor renders "not in any collection yet" from it.
func TestListPostCollections_NoMembershipsIsAnEmptyList(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	u := puOK(t, puList(t, h, puIdentity(puAuthor), postID))
	if len(u.Items) != 0 {
		t.Errorf("N=0: got %d items, want 0", len(u.Items))
	}
	if u.WithheldCount != 0 {
		t.Errorf("N=0: withheld_count = %d, want 0", u.WithheldCount)
	}
	if u.Items == nil {
		t.Errorf("items must be an empty array, not null. A client that " +
			"renders `items.length` on null shows a broken section rather " +
			"than the zero state")
	}
}

// N=1.
func TestListPostCollections_OneMembership(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	mine := puSeedCollection(t, pool, puAuthor, "pu_1119 only shelf", "public")
	puPin(t, pool, mine, postID)

	u := puOK(t, puList(t, h, puIdentity(puAuthor), postID))
	if len(u.Items) != 1 {
		t.Fatalf("N=1: got %d items, want 1", len(u.Items))
	}
	if uuid.UUID(u.Items[0].Collection.Id) != mine {
		t.Errorf("N=1: listed the wrong collection")
	}
	if !u.Items[0].CanRemove {
		t.Errorf("N=1: can_remove = false on the author's own shelf, want true")
	}
	if u.WithheldCount != 0 {
		t.Errorf("N=1: withheld_count = %d, want 0: the one shelf is readable", u.WithheldCount)
	}
}

// A shelf the author may not READ is a COUNT and nothing else. The whole
// point of the field, and the assertion that keeps it a count: the
// private collection's id must not come back at all.
func TestListPostCollections_UnreadableShelfIsCountedNotNamed(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	visible := puSeedCollection(t, pool, puAuthor, "pu_1119 visible", "public")
	hidden := puSeedCollection(t, pool, puCurator, "pu_1119 hidden", "private")
	puPin(t, pool, visible, postID)
	puPin(t, pool, hidden, postID)

	u := puOK(t, puList(t, h, puIdentity(puAuthor), postID))
	got := puFlags(u)
	if len(got) != 1 || !got[visible] {
		t.Fatalf("want exactly the one readable shelf; got %v", got)
	}
	if _, named := got[hidden]; named {
		t.Errorf("a collection the author may not read was NAMED in items. " +
			"the count is the whole disclosure")
	}
	if u.WithheldCount != 1 {
		t.Errorf("withheld_count = %d, want 1: a stranger's private shelf holds "+
			"this post and the author is entitled to know how many do",
			u.WithheldCount)
	}
}

// The tombstone rule, which the read rule's fragment does NOT state on
// the system.admin arm, so this is the caller stating it, and the test
// that it did.
func TestListPostCollections_SoftDeletedShelfIsNeitherListedNorCounted(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	gone := puSeedCollection(t, pool, puAuthor, "pu_1119 tombstoned", "public")
	puPin(t, pool, gone, postID)
	if _, err := pool.Exec(context.Background(),
		`UPDATE collections SET deleted_at = NOW() WHERE id = $1`, gone); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	for _, who := range []*auth.Identity{
		puIdentity(puAuthor),
		// The admin arm is the one with no predicate to rely on.
		puIdentity(puCollAdmin, "system.admin"),
	} {
		u := puOK(t, puList(t, h, who, postID))
		if len(u.Items) != 0 || u.WithheldCount != 0 {
			t.Errorf("ref %d: a soft-deleted collection came back (%d items, %d withheld). "+
				"a tombstoned shelf is not somewhere the post appears, and a Remove "+
				"button on one is a control over a row already in the trash",
				who.UserRef, len(u.Items), u.WithheldCount)
		}
	}
}

// An un-pinned membership row is not a membership, matching what
// ListCollectionPostsGated already excludes on the other side.
func TestListPostCollections_UnpinnedAndExpiredRowsAreNotMemberships(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	unpinned := puSeedCollection(t, pool, puAuthor, "pu_1119 unpinned", "public")
	expired := puSeedCollection(t, pool, puAuthor, "pu_1119 expired", "public")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
		 VALUES ($1, $2, 0, FALSE)`, unpinned, postID); err != nil {
		t.Fatalf("seed unpinned: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned, expires_at)
		 VALUES ($1, $2, 0, TRUE, NOW() - INTERVAL '1 hour')`, expired, postID); err != nil {
		t.Fatalf("seed expired: %v", err)
	}

	u := puOK(t, puList(t, h, puIdentity(puAuthor), postID))
	if len(u.Items) != 0 || u.WithheldCount != 0 {
		t.Errorf("got %d items / %d withheld, want 0/0. `pinned = FALSE` and a "+
			"passed `expires_at` are both 'not a member' on the collection side, "+
			"so they have to be here too",
			len(u.Items), u.WithheldCount)
	}
}

// ── The gate: who may ask ─────────────────────────────────────────────

// A signed-in stranger gets the SAME 404 a nonexistent post gets. Not a
// 403: "this post sits on 3 shelves you cannot see" is the author's
// information about their own work, and answering it for anybody would
// make this a shelving oracle over every collection on the instance.
func TestListPostCollections_StrangerGetsTheAbsentPostAnswer(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	mine := puSeedCollection(t, pool, puAuthor, "pu_1119 gated", "public")
	puPin(t, pool, mine, postID)

	resp := puList(t, h, puIdentity(puStranger), postID)
	got, is := resp.(openapi.ListPostCollections404JSONResponse)
	if !is {
		t.Fatalf("a stranger must get 404, got %T; a 403 confirms the post exists", resp)
	}

	// BYTE-IDENTICAL to the nonexistent case, or the pair of answers is
	// itself the oracle.
	absent := puList(t, h, puIdentity(puStranger), uuid.New())
	gone, is := absent.(openapi.ListPostCollections404JSONResponse)
	if !is {
		t.Fatalf("a nonexistent post must get 404, got %T", absent)
	}
	if got.Error != gone.Error {
		t.Errorf("refusal bodies differ: %q for a real post, %q for an absent one. "+
			"the two must be indistinguishable", got.Error, gone.Error)
	}
}

// The moderator arm, which the stranger test would otherwise make look
// like "only the author, ever".
func TestListPostCollections_PostsAdminMayAsk(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	mine := puSeedCollection(t, pool, puAuthor, "pu_1119 moderated", "public")
	puPin(t, pool, mine, postID)

	if _, is := puList(t, h, puIdentity(puStranger, "posts.admin"), postID).(openapi.ListPostCollections200JSONResponse); !is {
		t.Errorf("a posts.admin holder must be able to ask")
	}
}

func TestListPostCollections_AnonymousIsRefused(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)
	postID := puSeedPost(t, pool, puAuthor, "public")

	anon := &auth.Identity{UserRef: 0, AuthMethod: "anonymous"}
	if _, is := puList(t, h, anon, postID).(openapi.ListPostCollections401JSONResponse); !is {
		t.Errorf("an anonymous caller must get 401: this endpoint is somebody's " +
			"answer about their own work and has no anonymous reading")
	}
}

// REF 0 IS A SENTINEL, NOT A USER, and it has to fail to match on BOTH
// sides of the ownership comparison. `IsAnonymous()` is false for a
// malformed `session` identity carrying ref 0, so that caller reaches the
// authorship gate, where a post whose `author_user_ref` is also 0 would
// match it on `author == caller.UserRef` alone.
//
// The same trap acl_listing_test pins for collections, one table over.
// The refusal is the absent-post answer rather than a 401 because the
// gate, not the sign-in check, is what turns it away.
func TestListPostCollections_RefZeroSessionDoesNotMatchARefZeroPost(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	zeroOwned := puSeedPost(t, pool, 0, "public")
	shelf := puSeedCollection(t, pool, 0, "pu_1119 ref-zero shelf", "public")
	puPin(t, pool, shelf, zeroOwned)

	zeroSession := &auth.Identity{UserRef: 0, AuthMethod: "session"}
	resp := puList(t, h, zeroSession, zeroOwned)
	if _, is := resp.(openapi.ListPostCollections404JSONResponse); !is {
		t.Errorf("a ref-0 session was admitted to a ref-0-authored post (%T). "+
			"ref 0 must not match on either side of the ownership comparison", resp)
	}

	// And the post's shelving is still reachable by an admin, so the
	// refusal above is not "this row is unaskable by anyone".
	if _, is := puList(t, h, puIdentity(puCollAdmin, "system.admin"), zeroOwned).(openapi.ListPostCollections200JSONResponse); !is {
		t.Errorf("system.admin cannot ask about a ref-0-authored post, want 200")
	}
}

// A SOFT-DELETED post is not somebody's post any more, so the gate does
// not resolve an author for it and the answer is the absent one.
func TestListPostCollections_DeletedPostIsAbsent(t *testing.T) {
	pool := puPool(t)
	h := puNewHandler(t, pool)

	postID := puSeedPost(t, pool, puAuthor, "public")
	if _, err := pool.Exec(context.Background(),
		`UPDATE posts SET deleted_at = NOW() WHERE id = $1`, postID); err != nil {
		t.Fatalf("tombstone post: %v", err)
	}
	if _, is := puList(t, h, puIdentity(puAuthor), postID).(openapi.ListPostCollections404JSONResponse); !is {
		t.Errorf("a soft-deleted post must answer 404")
	}
}

func puPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	return pool
}
