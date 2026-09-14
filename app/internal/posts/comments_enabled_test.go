// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21d: the comments-enabled setting as a POST field, on
// the create and update wires.
//
// Every assertion reads the column back out of Postgres. CreatePost and
// UpdatePost both RETURN the row they wrote, so a body assertion is
// derived from the same statement under test and would pass on a query
// that echoed the request and stored nothing. The body is checked too,
// but only after the column.
//
// The three create cases (omitted, explicit true, explicit false) are
// one test on purpose: what is under test is that the wire
// distinguishes them, and a suite that ran them separately could have
// each pass against a handler that stored a constant.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	ceAuthor    int64 = 21190001
	ceCommenter int64 = 21190002 // holds posts.comment and nothing else
)

func ceStored(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(),
		`SELECT comments_enabled FROM posts WHERE id = $1`, postID).Scan(&v); err != nil {
		t.Fatalf("read comments_enabled: %v", err)
	}
	return v
}

func ceCreate(t *testing.T, h *Handler, asset uuid.UUID, title string, enabled *bool) (uuid.UUID, openapi.Post) {
	t.Helper()
	resp, err := h.CreatePost(ctxAs(ceAuthor), openapi.CreatePostRequestObject{
		Body: &openapi.PostCreate{
			Title:           &title,
			Members:         []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(asset)}},
			CommentsEnabled: enabled,
		},
	})
	if err != nil {
		t.Fatalf("CreatePost(%s): %v", title, err)
	}
	created, ok := resp.(openapi.CreatePost201JSONResponse)
	if !ok {
		t.Fatalf("CreatePost(%s): response is %T, want 201", title, resp)
	}
	post, _ := bodyOf(t, created.VisitCreatePostResponse)
	id := uuid.UUID(post.Id)
	t.Cleanup(func() {
		_, _ = h.Pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	return id, post
}

func cePatch(t *testing.T, h *Handler, ctx context.Context, postID uuid.UUID, body *openapi.PostUpdate) openapi.UpdatePostResponseObject {
	t.Helper()
	resp, err := h.UpdatePost(ctx, openapi.UpdatePostRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: body,
	})
	if err != nil {
		t.Fatalf("UpdatePost: %v", err)
	}
	return resp
}

func ceGet(t *testing.T, h *Handler, postID uuid.UUID) openapi.Post {
	t.Helper()
	resp, err := h.GetPost(ctxAs(ceAuthor), openapi.GetPostRequestObject{Id: openapi_types.UUID(postID)})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	got, ok := resp.(openapi.GetPost200JSONResponse)
	if !ok {
		t.Fatalf("GetPost: response is %T, want 200", resp)
	}
	return openapi.Post(got)
}

func boolp(v bool) *bool { return &v }

// TestPostCommentsEnabled_ExistingRowDefaultsTrue is the migration's
// promise for every post that predates it: a row inserted without
// naming the column reads true. seedTierPost is exactly such an insert.
func TestPostCommentsEnabled_ExistingRowDefaultsTrue(t *testing.T) {
	h := wireWriteHandler(t)
	postID := seedTierPost(t, h.Pool, ceAuthor, "public")
	if !ceStored(t, h.Pool, postID) {
		t.Fatalf("a post row that never named comments_enabled must read true (migration 00070 default)")
	}
	if got := ceGet(t, h, postID); !got.CommentsEnabled {
		t.Fatalf("GET must report comments_enabled=true for a pre-existing post")
	}
}

// TestPostCommentsEnabled_CreateOmittedTrueFalse: the wire
// distinguishes absent from false.
func TestPostCommentsEnabled_CreateOmittedTrueFalse(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", false, ceAuthor)

	omittedID, omitted := ceCreate(t, h, asset, "ce omitted", nil)
	trueID, explicitTrue := ceCreate(t, h, asset, "ce true", boolp(true))
	falseID, explicitFalse := ceCreate(t, h, asset, "ce false", boolp(false))

	if !ceStored(t, h.Pool, omittedID) {
		t.Errorf("omitted: stored false, want true (omitted means enabled)")
	}
	if !ceStored(t, h.Pool, trueID) {
		t.Errorf("explicit true: stored false")
	}
	if ceStored(t, h.Pool, falseID) {
		t.Errorf("explicit false: stored true (false was lost as absent)")
	}
	// The three are independent rows: a constant would have made them
	// agree.
	if ceStored(t, h.Pool, omittedID) == ceStored(t, h.Pool, falseID) {
		t.Fatalf("omitted and explicit-false posts must differ; a handler storing a constant cannot pass this")
	}

	// The bodies agree with the columns, and so does a fresh read.
	for _, c := range []struct {
		name string
		id   uuid.UUID
		body openapi.Post
		want bool
	}{
		{"omitted", omittedID, omitted, true},
		{"true", trueID, explicitTrue, true},
		{"false", falseID, explicitFalse, false},
	} {
		if c.body.CommentsEnabled != c.want {
			t.Errorf("%s: 201 body comments_enabled=%v, want %v", c.name, c.body.CommentsEnabled, c.want)
		}
		if got := ceGet(t, h, c.id); got.CommentsEnabled != c.want {
			t.Errorf("%s: GET comments_enabled=%v, want %v", c.name, got.CommentsEnabled, c.want)
		}
	}
}

// TestPostCommentsEnabled_UpdateRoundTrip: enabled -> disabled ->
// enabled through PATCH, each persisted; a PATCH that says nothing
// about the setting leaves it where it was; and the setting is a
// separate write from publication.
func TestPostCommentsEnabled_UpdateRoundTrip(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", false, ceAuthor)
	postID, _ := ceCreate(t, h, asset, "ce round trip", nil)
	author := ctxAs(ceAuthor)

	// enabled -> disabled
	resp := cePatch(t, h, author, postID, &openapi.PostUpdate{CommentsEnabled: boolp(false)})
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		t.Fatalf("disable: response is %T, want 200", resp)
	}
	if ceStored(t, h.Pool, postID) {
		t.Fatalf("enabled -> disabled did not persist")
	}
	if got := ceGet(t, h, postID); got.CommentsEnabled {
		t.Fatalf("GET after disable still reports enabled")
	}

	// A metadata-only PATCH must not touch it (an absent field is not a
	// false).
	title := "renamed while disabled"
	cePatch(t, h, author, postID, &openapi.PostUpdate{Title: &title})
	if ceStored(t, h.Pool, postID) {
		t.Fatalf("a PATCH without comments_enabled re-enabled comments; absent was read as a write")
	}
	if got := ceGet(t, h, postID); got.Title != title {
		t.Fatalf("the metadata PATCH itself did not land: title=%q", got.Title)
	}

	// disabled -> enabled
	cePatch(t, h, author, postID, &openapi.PostUpdate{CommentsEnabled: boolp(true)})
	if !ceStored(t, h.Pool, postID) {
		t.Fatalf("disabled -> enabled did not persist")
	}
	if got := ceGet(t, h, postID); !got.CommentsEnabled {
		t.Fatalf("GET after re-enable still reports disabled")
	}

	// The setting is not a publication move: the post is still
	// published (created without draft) after every write above.
	if got := ceGet(t, h, postID); got.Draft {
		t.Fatalf("toggling comments must not change publication state")
	}
}

// TestPostCommentsEnabled_StaleWriteRefused: the setting rides the
// if_unchanged_since guard. A stale PATCH that carries it is refused
// as a whole and writes nothing.
func TestPostCommentsEnabled_StaleWriteRefused(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", false, ceAuthor)
	postID, _ := ceCreate(t, h, asset, "ce stale", nil)
	author := ctxAs(ceAuthor)

	// The baseline a client would have loaded.
	baseline := ceGet(t, h, postID).UpdatedAt

	// Somebody else moves the row.
	time.Sleep(2 * time.Millisecond) // updated_at is microsecond precision
	other := "moved by someone else"
	cePatch(t, h, author, postID, &openapi.PostUpdate{Title: &other})

	resp := cePatch(t, h, author, postID, &openapi.PostUpdate{
		CommentsEnabled:  boolp(false),
		IfUnchangedSince: &baseline,
	})
	if _, ok := resp.(openapi.UpdatePost409JSONResponse); !ok {
		t.Fatalf("a stale PATCH carrying comments_enabled must be refused with 409, got %T", resp)
	}
	if !ceStored(t, h.Pool, postID) {
		t.Fatalf("the refused stale PATCH still wrote comments_enabled=false")
	}

	// With the fresh baseline the same write lands, and only then.
	fresh := ceGet(t, h, postID).UpdatedAt
	resp = cePatch(t, h, author, postID, &openapi.PostUpdate{
		CommentsEnabled:  boolp(false),
		IfUnchangedSince: &fresh,
	})
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		t.Fatalf("a current PATCH must land, got %T", resp)
	}
	if ceStored(t, h.Pool, postID) {
		t.Fatalf("the guarded write did not persist")
	}
}

// TestPostCommentsEnabled_CommenterCannotChangeIt: holding
// posts.comment confers nothing on the setting. The refusal is the
// existing canMutatePost 403, so no mutation authority widened.
func TestPostCommentsEnabled_CommenterCannotChangeIt(t *testing.T) {
	h := wireWriteHandler(t)
	asset := seedPreviewAssetOwned(t, h.Pool, "public", false, ceAuthor)
	postID, _ := ceCreate(t, h, asset, "ce authority", nil)

	commenter := auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef:      ceCommenter,
		AuthMethod:   "session",
		Capabilities: []string{"posts.comment"},
	})
	resp := cePatch(t, h, commenter, postID, &openapi.PostUpdate{CommentsEnabled: boolp(false)})
	if _, ok := resp.(openapi.UpdatePost403JSONResponse); !ok {
		t.Fatalf("a non-author with posts.comment must be refused with 403, got %T", resp)
	}
	if !ceStored(t, h.Pool, postID) {
		t.Fatalf("the refused PATCH wrote comments_enabled=false")
	}

	// And the author, holding no comment capability at all, may.
	resp = cePatch(t, h, ctxAs(ceAuthor), postID, &openapi.PostUpdate{CommentsEnabled: boolp(false)})
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		t.Fatalf("the author must be able to change their own setting, got %T", resp)
	}
	if ceStored(t, h.Pool, postID) {
		t.Fatalf("the author's disable did not persist")
	}
}
