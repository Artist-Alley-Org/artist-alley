// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1161 / ADR 0091 decision 7 — a post has a real draft state.
//
// # What was wrong
//
// The `post` workflow domain has held two states since the 00001
// baseline, `wip` and `published`, and NOTHING READ EITHER. `state_id`
// appeared in the create / get / update / list column lists and in no
// predicate anywhere in the product, so the draft state was not merely
// unreachable — the reading half did not exist. A post was born
// published, and ADR 0091's decision 6 ("unpublishing returns a post to
// its author, intact") could not be implemented because there was
// nowhere to return to.
//
// # What these tests pin, and why they are shaped this way
//
// The dangerous half of making drafts real is the READING half: one
// listing that forgets the rule is a disclosure, and there are eight of
// them. So the load-bearing test here is not "a draft is hidden" — it
// is PER SURFACE, one assertion each, over every listing a post can
// reach: browse, the author's own filtered feed, the tag feed, the
// by-asset lookup, a collection's contents, the profile post count, the
// federation activity ledger, and the drafts listing itself.
//
// The author's own feed is in that list deliberately. A rule written as
// "hide drafts unless they are mine" passes every other case here and
// fails that one, and it is the shape somebody reaches for first.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

// Synthetic refs, in this file's own range so no other suite collides.
const (
	dvAuthor    int64 = 6610001
	dvStranger  int64 = 6610002
	dvModerator int64 = 6610003
)

// dvStateID resolves one of the post domain's states.
func dvStateID(t *testing.T, pool *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT id FROM workflow_states WHERE domain = $1 AND code = $2`,
		visibility.PostWorkflowDomain, code).Scan(&id); err != nil {
		t.Fatalf("resolve %q state: %v", code, err)
	}
	return id
}

// dvSeedPost plants one post. `draft` chooses its state EXPLICITLY
// rather than leaning on the column default, because the point of the
// fixture is the difference between the two states.
func dvSeedPost(t *testing.T, pool *pgxpool.Pool, author int64, vis, title string, draft bool) uuid.UUID {
	t.Helper()
	code := visibility.PostPublishedStateCode
	if draft {
		code = visibility.PostDraftStateCode
	}
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, state_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, author, title, "dv body", vis, dvStateID(t, pool, code)); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

func dvID(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

// dvListIDs runs one feed request and returns the ids it handed out.
func dvListIDs(t *testing.T, h *Handler, id *auth.Identity, p openapi.ListPostsParams) map[uuid.UUID]bool {
	t.Helper()
	limit := maxListLimit
	p.Limit = &limit
	ctx := t.Context()
	if id != nil {
		ctx = auth.WithIdentity(ctx, id)
	}
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: p})
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	out := map[uuid.UUID]bool{}
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// TestDraft_AbsentFromEverySharedSurface is the sprint's centre of
// gravity: ONE assertion per listing, named individually so a failure
// says which surface leaked rather than "a draft was visible".
func TestDraft_AbsentFromEverySharedSurface(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := t.Context()

	// PUBLIC on purpose. `visibility` answers who may read the post once
	// it is published; a rule that leaned on the tier would pass with a
	// `private` draft and prove nothing.
	draft := dvSeedPost(t, pool, dvAuthor, "public", "dv draft probe", true)
	live := dvSeedPost(t, pool, dvAuthor, "public", "dv published probe", false)

	// A tag on both, so the tag feed is exercised with something to find.
	for _, id := range []uuid.UUID{draft, live} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_tags (post_id, tag) VALUES ($1, 'dvprobe')`, id); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}

	author := dvID(dvAuthor)
	stranger := dvID(dvStranger)
	moderator := dvID(dvModerator, CapPostsAdmin)

	authorRef := dvAuthor
	tag := []string{"dvprobe"}

	surfaces := []struct {
		name   string
		caller *auth.Identity
		params openapi.ListPostsParams
	}{
		{"browse, author", author, openapi.ListPostsParams{}},
		{"browse, stranger", stranger, openapi.ListPostsParams{}},
		{"browse, posts.admin", moderator, openapi.ListPostsParams{}},
		{"the author's OWN filtered feed", author, openapi.ListPostsParams{AuthorRef: &authorRef}},
		{"tag feed", author, openapi.ListPostsParams{Tag: &tag}},
	}
	for _, s := range surfaces {
		got := dvListIDs(t, h, s.caller, s.params)
		if got[draft] {
			t.Errorf("%s: the DRAFT was listed", s.name)
		}
		if !got[live] {
			t.Errorf("%s: the PUBLISHED post was missing — the rule is too wide, "+
				"so this surface's draft result proves nothing", s.name)
		}
	}

	t.Run("by-asset", func(t *testing.T) {
		// A member asset shared by both posts: the lookup must return the
		// published one and not the draft.
		asset := dvSeedAsset(t, pool, dvAuthor)
		for _, id := range []uuid.UUID{draft, live} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`,
				id, asset); err != nil {
				t.Fatalf("seed member: %v", err)
			}
		}
		resp, err := h.GetPostsByAsset(auth.WithIdentity(ctx, author),
			openapi.GetPostsByAssetRequestObject{Id: openapi_types.UUID(asset)})
		if err != nil {
			t.Fatalf("GetPostsByAsset: %v", err)
		}
		ok, is := resp.(openapi.GetPostsByAsset200JSONResponse)
		if !is {
			t.Fatalf("GetPostsByAsset returned %T, want 200", resp)
		}
		var sawDraft, sawLive bool
		for _, p := range ok.Items {
			sawDraft = sawDraft || uuid.UUID(p.Id) == draft
			sawLive = sawLive || uuid.UUID(p.Id) == live
		}
		if sawDraft {
			t.Error("by-asset: the DRAFT was listed")
		}
		if !sawLive {
			t.Error("by-asset: the PUBLISHED post was missing")
		}
	})

	t.Run("a collection's contents", func(t *testing.T) {
		col := dvSeedCollection(t, pool, dvAuthor)
		for _, id := range []uuid.UUID{draft, live} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
				 VALUES ($1,$2,0,TRUE)`, col, id); err != nil {
				t.Fatalf("pin post: %v", err)
			}
		}
		ids, err := h.ListCollectionPostsGated(ctx, author, col, 100,
			visibility.MatureViewer{}, true)
		if err != nil {
			t.Fatalf("collection contents: %v", err)
		}
		var sawDraft, sawLive bool
		for _, pid := range ids {
			sawDraft = sawDraft || uuid.UUID(pid.Bytes) == draft
			sawLive = sawLive || uuid.UUID(pid.Bytes) == live
		}
		if sawDraft {
			t.Error("collection: the DRAFT was listed")
		}
		if !sawLive {
			t.Error("collection: the PUBLISHED post was missing")
		}
	})

	t.Run("the profile post count", func(t *testing.T) {
		// The count is a shared surface too: a number that moved when
		// somebody STARTED a draft would announce that they had.
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*)::BIGINT FROM posts
			  WHERE author_user_ref = $1 AND deleted_at IS NULL
			    AND state_id = (SELECT id FROM workflow_states
			                     WHERE domain = 'post' AND code = 'published')`,
			dvAuthor).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("profile post_count = %d, want 1 (the published post only)", n)
		}
	})

	t.Run("the drafts listing is the ONE that returns it", func(t *testing.T) {
		want := true
		got := dvListIDs(t, h, author, openapi.ListPostsParams{Draft: &want})
		if !got[draft] {
			t.Error("?draft=true did not return the author's own draft — " +
				"a draft nobody can list is a write-only hole, not a draft")
		}
		if got[live] {
			t.Error("?draft=true returned a PUBLISHED post; it must return drafts only")
		}
		// And it is not a way round the read rule.
		if dvListIDs(t, h, stranger, openapi.ListPostsParams{Draft: &want})[draft] {
			t.Error("?draft=true handed a stranger somebody else's draft")
		}
	})
}

// TestDraft_SingleItemReadIsAuthorAndModeratorOnly pins the OTHER half:
// the listing rule is waivable, the authorization rule is not.
//
// A `public` draft read by a signed-in stranger is the case that
// matters. The tier says "everyone may read this", and it means
// "everyone may read this once it is published" — reading the tier as
// consent to publish would make the compose form's visibility control
// double as a publish button.
func TestDraft_SingleItemReadIsAuthorAndModeratorOnly(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	draft := dvSeedPost(t, pool, dvAuthor, "public", "dv single-item draft", true)

	for _, tc := range []struct {
		name   string
		caller *auth.Identity
		want   bool
	}{
		{"its author", dvID(dvAuthor), true},
		{"posts.admin", dvID(dvModerator, CapPostsAdmin), true},
		{"a signed-in stranger", dvID(dvStranger), false},
		{"anonymous", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var caller *auth.Identity
			if tc.caller != nil {
				caller = tc.caller
			} else {
				caller = &auth.Identity{UserRef: 0, Username: "anonymous", AuthMethod: "anonymous"}
			}
			got, err := h.postReadable(t.Context(), caller, draft)
			if err != nil {
				t.Fatalf("postReadable: %v", err)
			}
			if got != tc.want {
				t.Errorf("postReadable(%s, public DRAFT) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestCreatePost_CannotChooseItsOwnState is finding 2's regression test.
//
// `PostCreate` used to carry `state_id`, and the handler wrote it into
// the row with a comment saying the domain was not validated — so the
// request body could name ANY state row on the instance. Harmless while
// nothing read state; a way to publish by UUID the moment state decides
// visibility.
//
// The field is gone from the schema, so the assertion is on the
// PERSISTED value rather than on a status: send the published state's
// UUID alongside `draft: true` in a body the server will parse, and the
// post must still be a draft.
func TestCreatePost_CannotChooseItsOwnState(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	asset := dvSeedAsset(t, pool, dvAuthor)

	yes := true
	resp, err := h.CreatePost(ctxAs(dvAuthor), openapi.CreatePostRequestObject{
		Body: &openapi.PostCreate{
			Title:   ptr("dv state choice probe"),
			Draft:   &yes,
			Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(asset)}},
		},
	})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	created, is := resp.(openapi.CreatePost201JSONResponse)
	if !is {
		t.Fatalf("CreatePost returned %T, want 201", resp)
	}
	postID := uuid.UUID(created.Id)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, postID)
	})

	// The PERSISTED state, read back from the row — not the handler's
	// echo of its own write.
	var code string
	if err := pool.QueryRow(t.Context(),
		`SELECT ws.code FROM posts p JOIN workflow_states ws ON ws.id = p.state_id
		  WHERE p.id = $1`, postID).Scan(&code); err != nil {
		t.Fatalf("read back state: %v", err)
	}
	if code != visibility.PostDraftStateCode {
		t.Errorf("persisted state = %q, want %q", code, visibility.PostDraftStateCode)
	}
	if !created.Draft {
		t.Error("the response says the post is published; the row says it is a draft")
	}

	// And the schema itself no longer offers the lever. This is the
	// assertion that keeps someone from re-adding the field with a
	// validator instead of leaving it out: PostCreate must have no
	// state field at all.
	if hasStateIDField(openapi.PostCreate{}) {
		t.Error("PostCreate has a state field again — publication is not the caller's to name")
	}
	if hasStateIDField(openapi.PostUpdate{}) {
		t.Error("PostUpdate has a state field again — PATCH must not be a publish path")
	}
}

// TestPublishUnpublish_RoundTrip is ADR 0091 decisions 6 and 7: a post
// leaves the shared surfaces without being deleted, keeps everything,
// and comes back.
func TestPublishUnpublish_RoundTrip(t *testing.T) {
	// wireWriteHandler, not peHandler: publishing records a federation
	// activity in the same transaction as the state move, so a handler
	// without the activities writer is not the handler this endpoint
	// runs as.
	h := wireWriteHandler(t)
	pool := h.Pool
	h.SetWorkflow(dvWorkflow(pool))
	ctx := t.Context()

	asset := dvSeedAsset(t, pool, dvAuthor)
	draft := dvSeedPost(t, pool, dvAuthor, "public", "dv round trip", true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`,
		draft, asset); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_tags (post_id, tag) VALUES ($1,'dvround')`, draft); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	author := dvID(dvAuthor, CapPostsPublish)

	// Unpublishing something already unpublished is refused, not
	// silently accepted: the state machine has no such edge, and a 200
	// would tell the caller a move happened.
	res, err := h.movePublication(auth.WithIdentity(ctx, author), draft, false)
	if err != nil {
		t.Fatalf("unpublish a draft: %v", err)
	}
	if res.status != 409 {
		t.Errorf("unpublish a draft: status %d, want 409", res.status)
	}

	// Publish.
	res, err = h.movePublication(auth.WithIdentity(ctx, author), draft, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.status != 200 {
		t.Fatalf("publish: status %d (%s), want 200", res.status, res.message)
	}
	if dvDraftFlag(t, h, author, draft) {
		t.Error("after publish the post still reports draft=true")
	}
	if !dvListIDs(t, h, author, openapi.ListPostsParams{})[draft] {
		t.Error("after publish the post is still missing from browse")
	}

	// Unpublish, then READ IT BACK from the API rather than trusting the
	// handler's own echo.
	res, err = h.movePublication(auth.WithIdentity(ctx, author), draft, false)
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if res.status != 200 {
		t.Fatalf("unpublish: status %d (%s), want 200", res.status, res.message)
	}
	back := dvGet(t, h, author, draft)
	switch {
	case !back.Draft:
		t.Error("after unpublish the post is not a draft")
	case back.Title != "dv round trip":
		t.Errorf("title lost: %q", back.Title)
	case back.Description != "dv body":
		t.Errorf("description lost: %q", back.Description)
	case len(back.Members) != 1 || uuid.UUID(back.Members[0].AssetId) != asset:
		t.Errorf("members lost: %+v", back.Members)
	case len(back.Tags) != 1 || back.Tags[0] != "dvround":
		t.Errorf("tags lost: %+v", back.Tags)
	case string(back.Visibility) != "public":
		t.Errorf("visibility tier changed to %q", back.Visibility)
	}
	if dvListIDs(t, h, author, openapi.ListPostsParams{})[draft] {
		t.Error("after unpublish the post is still on browse")
	}

	// The member asset is untouched either way — storage and
	// publication are separate lifecycles.
	var alive bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at IS NULL FROM assets WHERE id=$1`, asset).Scan(&alive); err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if !alive {
		t.Error("unpublishing touched the member asset")
	}
}

// TestPublish_IsNotOpenToEveryMutator pins the asymmetry: publishing
// WIDENS reach and takes the narrow gate; unpublishing narrows it and
// takes the ordinary one.
func TestPublish_IsNotOpenToEveryMutator(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool
	h.SetWorkflow(dvWorkflow(pool))

	draft := dvSeedPost(t, pool, dvAuthor, "public", "dv gate probe", true)

	// A signed-in stranger gets the SAME answer an absent post gets, so
	// the endpoint is not a post-existence probe.
	res, err := h.movePublication(
		auth.WithIdentity(t.Context(), dvID(dvStranger, CapPostsPublish)), draft, true)
	if err != nil {
		t.Fatalf("stranger publish: %v", err)
	}
	if res.status != 404 {
		t.Errorf("stranger publishing someone else's draft: status %d, want 404", res.status)
	}
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// dvSeedAsset plants a real, readable asset row so the member joins have
// something to find (see seedAsset for why a synthetic uuid would make
// the test pass on a bug).
func dvSeedAsset(t *testing.T, pool *pgxpool.Pool, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, owner_user_ref, title, asset_type, status, processing_status, sensitivity)
		 VALUES ($1, $2, 'dv member', (SELECT MIN(ref) FROM asset_types), 'active', 'ready', 'public')`,
		id, owner); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

func dvSeedCollection(t *testing.T, pool *pgxpool.Pool, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO collections (id, owner_user_ref, name, visibility)
		 VALUES ($1, $2, 'dv collection', 'public')`, id, owner); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id=$1`, id)
	})
	return id
}

// dvWorkflow builds the state machine the publish handlers drive. Real
// rather than a fake: the whole point of routing publication through it
// is the edge list and the audit row, and a fake would assert neither.
func dvWorkflow(pool *pgxpool.Pool) *workflow.Service {
	return workflow.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func dvGet(t *testing.T, h *Handler, id *auth.Identity, postID uuid.UUID) openapi.Post {
	t.Helper()
	resp, err := h.GetPost(auth.WithIdentity(t.Context(), id),
		openapi.GetPostRequestObject{Id: openapi_types.UUID(postID)})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	ok, is := resp.(openapi.GetPost200JSONResponse)
	if !is {
		t.Fatalf("GetPost returned %T, want 200", resp)
	}
	return openapi.Post(ok)
}

func dvDraftFlag(t *testing.T, h *Handler, id *auth.Identity, postID uuid.UUID) bool {
	t.Helper()
	return dvGet(t, h, id, postID).Draft
}

// hasStateIDField reports whether a generated request schema still
// carries a workflow-state field. Reflection rather than a compile-time
// reference, because the point is to fail when somebody ADDS one back —
// a compile error would only catch removal.
func hasStateIDField(v any) bool {
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		if strings.EqualFold(rt.Field(i).Name, "StateId") {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
