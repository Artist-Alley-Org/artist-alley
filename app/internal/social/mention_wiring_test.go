// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package social_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// These handler-level tests prove the 1.55.X wiring end-to-end: a
// CreatePostComment whose body @-mentions a local user lands a
// mention_of_me row in `notifications`, and one that doesn't mention
// anyone lands no such row. The parse/resolve/fire internals are unit-
// tested in the mention package; this is the "the handler actually
// calls the hook after commit" seam.

// insertMentionee creates a throwaway user to be mentioned + registers
// cleanup. Returns (ref, username).
func insertMentionee(t *testing.T, f *activitiesFixture) (int64, string) {
	t.Helper()
	username := "mentionee-" + randHex(t, 6)
	var ref int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Mentionee",
	).Scan(&ref); err != nil {
		t.Fatalf("insert mentionee: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1`, ref)
		_, _ = f.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref, username
}

// insertPost creates a minimal post authored by the fixture user +
// registers cleanup. Direct SQL rather than the posts handler (which
// requires member assets) — we only need a real row for the comment's
// postExists gate.
func insertPost(t *testing.T, f *activitiesFixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO posts (author_user_ref, title) VALUES ($1, $2) RETURNING id`,
		f.userRef, "mention wiring post",
	).Scan(&id); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

func countMentionNotifs(t *testing.T, f *activitiesFixture, recipient int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE recipient_user_ref=$1 AND verb='mention_of_me'`,
		recipient,
	).Scan(&n); err != nil {
		t.Fatalf("count notifs: %v", err)
	}
	return n
}

func TestCreateComment_WithMention_FiresNotification(t *testing.T) {
	f := setupActivitiesFixture(t)
	mRef, mName := insertMentionee(t, f)
	postID := insertPost(t, f)

	ctx := f.withIdentity(f.ctx)
	resp, err := f.social.CreatePostComment(ctx, openapi.CreatePostCommentRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: &openapi.CommentCreate{Body: "hey @" + mName + " look at this"},
	})
	if err != nil {
		t.Fatalf("CreatePostComment: %v", err)
	}
	if _, ok := resp.(openapi.CreatePostComment201JSONResponse); !ok {
		t.Fatalf("expected 201, got %T", resp)
	}

	if got := countMentionNotifs(t, f, mRef); got != 1 {
		t.Fatalf("expected 1 mention_of_me notification for the mentionee, got %d", got)
	}

	// The notification points at the containing post so the bell deep-
	// links to /posts/{id}.
	var kind, tid string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT target_kind, target_id FROM notifications WHERE recipient_user_ref=$1 AND verb='mention_of_me'`,
		mRef,
	).Scan(&kind, &tid); err != nil {
		t.Fatalf("read target: %v", err)
	}
	if kind != "post" || tid != postID.String() {
		t.Fatalf("target = %s/%s, want post/%s", kind, tid, postID.String())
	}
}

func TestCreateComment_WithoutMention_FiresNoMentionNotification(t *testing.T) {
	f := setupActivitiesFixture(t)
	mRef, _ := insertMentionee(t, f)
	postID := insertPost(t, f)

	ctx := f.withIdentity(f.ctx)
	_, err := f.social.CreatePostComment(ctx, openapi.CreatePostCommentRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: &openapi.CommentCreate{Body: "a comment with no at-mentions at all"},
	})
	if err != nil {
		t.Fatalf("CreatePostComment: %v", err)
	}
	if got := countMentionNotifs(t, f, mRef); got != 0 {
		t.Fatalf("expected 0 mention notifications, got %d", got)
	}
}

func TestCreateComment_SelfMention_DoesNotNotifySelf(t *testing.T) {
	f := setupActivitiesFixture(t)
	postID := insertPost(t, f)

	// The author @-mentions themselves — the notifications.Writer gates
	// actor==recipient, so no row lands.
	ctx := f.withIdentity(f.ctx)
	_, err := f.social.CreatePostComment(ctx, openapi.CreatePostCommentRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: &openapi.CommentCreate{Body: "note to self @" + f.username},
	})
	if err != nil {
		t.Fatalf("CreatePostComment: %v", err)
	}
	if got := countMentionNotifs(t, f, f.userRef); got != 0 {
		t.Fatalf("self-mention should not notify self, got %d", got)
	}
}
