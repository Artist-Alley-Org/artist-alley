// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1176 — a post created with visibility 'public' is stored public and
// is readable by an anonymous visitor.
//
// # What was wrong
//
// The compose form has always offered a "Public" option
// (PostComposeForm.svelte). Selecting it POSTed `visibility: "public"`,
// and the server's write gate — posts.validVisibility — refused the
// value outright with "'public' reserved for future public-fediverse
// phase". So the control was dead: it produced a 400, never a post.
//
// Everything else in the stack had already moved on. The column's CHECK
// admits the tier (migration 00008, #414), the read rule's anonymous
// branch IS `visibility = 'public'` (acl_read_test.go), ADR 0010 grants
// the Anonymous role `posts.read.public`, and public mode (#709)
// shipped the switch that lets anonymous callers in at all. Only the
// write gate still said no — which is why an instance with public mode
// ON answered an anonymous GET /posts with 200 and zero items. There
// was nothing in that tier for anyone to see, and no way to put
// anything there.
//
// # What these tests pin, and why they are shaped this way
//
// The house rule is never to trust the echo (#946): a handler that
// returns its own input proves nothing about what landed in the table.
// So every assertion here reads the PERSISTED value —
// `SELECT visibility FROM posts` — and then reads the post back through
// the ANONYMOUS list path, which is the caller the whole feature is
// for.
//
// The negative controls carry the weight. "Anonymous sees the public
// post" passes just as happily on a rule that hands anonymous callers
// everything, so the same author's org-only post is created in the same
// test and must NOT come back. And because the owner's ruling is that
// public is an OPTION and not the new default, the omitted-field case
// is asserted to still persist 'org-only'.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	ptAuthor int64 = 11760001 // creates the posts
)

// ptCreate drives the REAL CreatePost with exactly the body the compose
// form sends: a title, one member, and the visibility the <select> was
// left on. Nothing else — the point is that the form's payload works,
// not that some hand-tuned variant of it does.
func ptCreate(t *testing.T, h *Handler, member uuid.UUID, visibility *string) openapi.CreatePostResponseObject {
	t.Helper()
	title := "pt post"
	body := &openapi.PostCreate{
		Title:   &title,
		Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(member)}},
	}
	if visibility != nil {
		v := openapi.PostCreateVisibility(*visibility)
		body.Visibility = &v
	}
	resp, err := h.CreatePost(ctxAs(ptAuthor), openapi.CreatePostRequestObject{Body: body})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	return resp
}

// ptCreated unwraps a 201 and registers cleanup, failing with the
// refusal body when the create did not succeed — on the pre-fix gate
// that refusal is exactly what this test needs to print.
func ptCreated(t *testing.T, pool *pgxpool.Pool, resp openapi.CreatePostResponseObject) uuid.UUID {
	t.Helper()
	created, ok := resp.(openapi.CreatePost201JSONResponse)
	if !ok {
		if bad, isBad := resp.(openapi.CreatePost400JSONResponse); isBad {
			t.Fatalf("CreatePost refused the compose form's payload: %s", bad.Error)
		}
		t.Fatalf("CreatePost returned %T, want 201", resp)
	}
	id := uuid.UUID(created.Id)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

// ptStoredVisibility reads the column. This is the assertion that
// matters: the 201 body echoes whatever the handler decided, so only
// the table can say what was actually written.
func ptStoredVisibility(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT visibility FROM posts WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read visibility: %v", err)
	}
	return got
}

// ptAnonSees reports whether the anonymous branch of the read rule
// hands this author's post out. ListPostsPageGated rather than the HTTP
// handler for the same reason acl_read_test.go uses it: ListPosts wants
// a session, the rule underneath does not, and the anonymous branch is
// the one under test.
func ptAnonSees(t *testing.T, h *Handler, id uuid.UUID) bool {
	t.Helper()
	author := ptAuthor
	rows, err := h.ListPostsPageGated(t.Context(), nil, ListPostsPageParams{
		AuthorUserRef: &author,
		RowLimit:      200,
	})
	if err != nil {
		t.Fatalf("ListPostsPageGated(anonymous): %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID.Bytes) == id {
			return true
		}
	}
	return false
}

// TestCreatePost_PublicTierPersistsAndReachesAnonymous is #1176's
// acceptance criterion. It FAILS on the pre-fix gate at ptCreated, with
// the 400 body naming the reserved tier.
func TestCreatePost_PublicTierPersistsAndReachesAnonymous(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", true, ptAuthor)

	public := "public"
	pubID := ptCreated(t, h.Pool, ptCreate(t, h, asset, &public))

	if got := ptStoredVisibility(t, h.Pool, pubID); got != "public" {
		t.Fatalf("posts.visibility = %q, want %q — the form's choice did not survive the write", got, "public")
	}
	if !ptAnonSees(t, h, pubID) {
		t.Error("anonymous caller cannot see the public post; the tier is writable but unreadable")
	}

	// Negative control. Without it, "anonymous sees the public post"
	// would pass on a rule that returns everything to everyone.
	orgOnly := "org-only"
	orgID := ptCreated(t, h.Pool, ptCreate(t, h, asset, &orgOnly))
	if got := ptStoredVisibility(t, h.Pool, orgID); got != "org-only" {
		t.Fatalf("posts.visibility = %q, want %q", got, "org-only")
	}
	if ptAnonSees(t, h, orgID) {
		t.Error("anonymous caller can see an ORG-ONLY post — the read rule is not narrowing at all")
	}

	// The signed-in author still sees both, so admitting the tier did
	// not narrow anything for the people who could already read them.
	author := ptAuthor
	rows, err := h.ListPostsPageGated(t.Context(), lvIdentity(ptAuthor), ListPostsPageParams{
		AuthorUserRef: &author,
		RowLimit:      200,
	})
	if err != nil {
		t.Fatalf("ListPostsPageGated(author): %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		seen[uuid.UUID(r.ID.Bytes)] = true
	}
	if !seen[pubID] || !seen[orgID] {
		t.Errorf("author sees public=%v org-only=%v, want both", seen[pubID], seen[orgID])
	}
}

// The owner's ruling for #1176: uploaders get the OPTION of anonymous
// viewing, public does NOT become the default. The OpenAPI schema
// claimed `default: public` on PostCreate.visibility — a value its own
// enum did not even list — so this pins the behaviour the handler has
// always had against the contract that used to contradict it.
func TestCreatePost_OmittedVisibilityStaysOrgOnly(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", true, ptAuthor)

	id := ptCreated(t, h.Pool, ptCreate(t, h, asset, nil))
	if got := ptStoredVisibility(t, h.Pool, id); got != "org-only" {
		t.Fatalf("posts.visibility = %q with the field omitted, want %q", got, "org-only")
	}
	if ptAnonSees(t, h, id) {
		t.Error("a post created with no visibility field is readable anonymously")
	}
}

// PATCH reaches the same tier, and admitting the value does not move
// the disclosure boundary: canWidenPostAccess still decides WHO may
// change a post's reach, and the persisted value is what gets checked.
func TestUpdatePost_PublicTierPersists(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", true, ptAuthor)
	id := ptCreated(t, h.Pool, ptCreate(t, h, asset, nil))

	v := openapi.PostUpdateVisibility("public")
	resp, err := h.UpdatePost(auth.WithIdentity(t.Context(), lvIdentity(ptAuthor)),
		openapi.UpdatePostRequestObject{
			Id:   openapi_types.UUID(id),
			Body: &openapi.PostUpdate{Visibility: &v},
		})
	if err != nil {
		t.Fatalf("UpdatePost: %v", err)
	}
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		if bad, isBad := resp.(openapi.UpdatePost400JSONResponse); isBad {
			t.Fatalf("UpdatePost refused 'public': %s", bad.Error)
		}
		t.Fatalf("UpdatePost returned %T, want 200", resp)
	}
	if got := ptStoredVisibility(t, h.Pool, id); got != "public" {
		t.Fatalf("posts.visibility = %q after PATCH, want %q", got, "public")
	}
	if !ptAnonSees(t, h, id) {
		t.Error("anonymous caller cannot see the post PATCHed to public")
	}
}
