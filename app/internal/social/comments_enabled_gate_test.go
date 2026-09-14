// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21d: a post's comments-enabled setting gates ORDINARY
// comment creation, and nothing else.
//
// # What every refusal assertion reads
//
// The rows. A 409 that also inserted a row, bumped comment_count, wrote
// an activity or queued a notification would pass a status assertion,
// so every refusal below takes a snapshot of those four counters before
// the request and compares after. The snapshot is the instrument; the
// status is a label on it.
//
// # Anti-vacuity
//
// A gate that refused EVERY comment passes every 409 assertion in this
// file. So the isolation test proves the enabled post genuinely accepts
// an ordinary comment from the same caller BEFORE and AFTER the
// disabled post refuses one, and the N=1 / N>=2 fixtures are built
// through the real handler while the post was enabled, which is itself
// a proof that the handler accepts when it should.
//
// # The race
//
// TestCommentsDisabled_DisableCannotBeRaced holds the interleaving by
// hand with two transactions. Both directions are asserted: a comment
// transaction that took the lock first commits and the disable waits
// for it (that comment legitimately predates the disable); a disable
// that took the lock first makes the comment transaction wait, and when
// the disable commits the comment reads false and is refused. Neither
// half uses timing to decide the outcome; the sleeps only prove that
// the blocked side really was blocked.
//
// Skips without AA_DB_PASSWORD, like the other social integration tests.

package social_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ceCommenter is a SECOND user: an ordinary account holding
// posts.comment and nothing else, so nothing it does can be explained
// by system.admin. Returns (ref, context).
func ceCommenter(t *testing.T, f *activitiesFixture) (int64, context.Context) {
	t.Helper()
	username := "ce-commenter-" + randHex(t, 6)
	var ref int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved, actor_uri) VALUES ($1, $2, 1, $3) RETURNING ref`,
		username, "Comments Enabled Commenter", "https://test.example/users/"+username,
	).Scan(&ref); err != nil {
		t.Fatalf("insert commenter: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, ref)
		_, _ = f.pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1 OR actor_user_ref = $1`, ref)
		_, _ = f.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef:      ref,
		Username:     username,
		AuthMethod:   "session",
		Capabilities: []string{"posts.comment"},
	})
	return ref, ctx
}

// cePost plants a post authored by the fixture user at the given tier
// with comments enabled. The column is not named in the INSERT on
// purpose: that is exactly the shape of every row that existed before
// migration 00070, so the read-back below doubles as the "existing
// posts default to enabled" proof.
func cePost(t *testing.T, f *activitiesFixture, visibility string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO posts (author_user_ref, title, visibility) VALUES ($1, $2, $3) RETURNING id`,
		f.userRef, "ce post "+visibility, visibility,
	).Scan(&id); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM comments WHERE target_kind = 'post' AND target_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	if got := ceEnabled(t, f, id); !got {
		t.Fatalf("a post inserted without naming comments_enabled must default to true (migration 00070)")
	}
	return id
}

func ceSetEnabled(t *testing.T, f *activitiesFixture, post uuid.UUID, enabled bool) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE posts SET comments_enabled = $2 WHERE id = $1`, post, enabled); err != nil {
		t.Fatalf("set comments_enabled=%v: %v", enabled, err)
	}
}

func ceEnabled(t *testing.T, f *activitiesFixture, post uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT comments_enabled FROM posts WHERE id = $1`, post).Scan(&v); err != nil {
		t.Fatalf("read comments_enabled: %v", err)
	}
	return v
}

// ceSnapshot is the persisted state a refused comment must not move.
type ceSnapshot struct {
	rows          int // live comments rows on the post
	count         int // posts.comment_count
	activities    int // activities rows by the commenter
	notifications int // notifications rows addressed to the post author
}

func ceTake(t *testing.T, f *activitiesFixture, post uuid.UUID, commenter int64) ceSnapshot {
	t.Helper()
	c := context.Background()
	var s ceSnapshot
	if err := f.pool.QueryRow(c,
		`SELECT COUNT(*) FROM comments WHERE target_kind = 'post' AND target_id = $1 AND deleted_at IS NULL`,
		post).Scan(&s.rows); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if err := f.pool.QueryRow(c, `SELECT comment_count FROM posts WHERE id = $1`, post).Scan(&s.count); err != nil {
		t.Fatalf("read comment_count: %v", err)
	}
	if err := f.pool.QueryRow(c, `SELECT COUNT(*) FROM activities WHERE actor_user_ref = $1`, commenter).Scan(&s.activities); err != nil {
		t.Fatalf("count activities: %v", err)
	}
	if err := f.pool.QueryRow(c, `SELECT COUNT(*) FROM notifications WHERE recipient_user_ref = $1`, f.userRef).Scan(&s.notifications); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return s
}

func ceComment(t *testing.T, f *activitiesFixture, ctx context.Context, post uuid.UUID, body string, parent *uuid.UUID) openapi.CreatePostCommentResponseObject {
	t.Helper()
	in := &openapi.CommentCreate{Body: body}
	if parent != nil {
		p := openapi_types.UUID(*parent)
		in.ParentId = &p
	}
	resp, err := f.social.CreatePostComment(ctx, openapi.CreatePostCommentRequestObject{
		Id:   openapi_types.UUID(post),
		Body: in,
	})
	if err != nil {
		t.Fatalf("CreatePostComment returned a transport error: %v", err)
	}
	return resp
}

// ceAccepted asserts a 201 and returns the new comment's id, read back
// from the ROW rather than trusted from the echo.
func ceAccepted(t *testing.T, f *activitiesFixture, resp openapi.CreatePostCommentResponseObject, post uuid.UUID, body string) uuid.UUID {
	t.Helper()
	if _, ok := resp.(openapi.CreatePostComment201JSONResponse); !ok {
		t.Fatalf("expected 201, got %T (%+v)", resp, resp)
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM comments WHERE target_kind = 'post' AND target_id = $1 AND body = $2 AND deleted_at IS NULL`,
		post, body).Scan(&id); err != nil {
		t.Fatalf("the accepted comment %q must exist as a row: %v", body, err)
	}
	return id
}

// ceRefused asserts the 409 with the stable error value and that the
// snapshot did not move: no row, no count, no activity, no notification.
func ceRefused(t *testing.T, f *activitiesFixture, resp openapi.CreatePostCommentResponseObject, post uuid.UUID, commenter int64, before ceSnapshot, what string) {
	t.Helper()
	r, ok := resp.(openapi.CreatePostComment409JSONResponse)
	if !ok {
		t.Fatalf("%s: expected 409, got %T (%+v)", what, resp, resp)
	}
	if r.Error != "comments_disabled" {
		t.Fatalf("%s: 409 error = %q, want the stable value %q", what, r.Error, "comments_disabled")
	}
	after := ceTake(t, f, post, commenter)
	if after != before {
		t.Fatalf("%s: a refused comment moved persisted state\n  before %+v\n  after  %+v", what, before, after)
	}
}

func ceListed(t *testing.T, f *activitiesFixture, ctx context.Context, post uuid.UUID) []openapi.Comment {
	t.Helper()
	resp, err := f.social.ListPostComments(ctx, openapi.ListPostCommentsRequestObject{
		Id: openapi_types.UUID(post),
	})
	if err != nil {
		t.Fatalf("ListPostComments: %v", err)
	}
	list, ok := resp.(openapi.ListPostComments200JSONResponse)
	if !ok {
		t.Fatalf("ListPostComments: expected 200, got %T", resp)
	}
	return list.Items
}

// ---------------------------------------------------------------------------

// N=1: one existing comment, then the author disables. The comment
// stays listed; a new root and a new reply are both refused; nothing
// about the thread moves. The fixture's own identity carries
// system.admin and is refused too.
func TestCommentsDisabled_N1_RootAndReplyRefused_ExistingKept(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)

	// The one existing comment, through the real handler while enabled:
	// that is also the proof the commenter is otherwise permitted.
	existing := ceAccepted(t, f, ceComment(t, f, cctx, post, "before the disable", nil), post, "before the disable")

	ceSetEnabled(t, f, post, false)
	before := ceTake(t, f, post, commenter)
	if before.rows != 1 || before.count != 1 {
		t.Fatalf("precondition: N=1 fixture has rows=%d count=%d", before.rows, before.count)
	}

	ceRefused(t, f, ceComment(t, f, cctx, post, "new root while disabled", nil), post, commenter, before, "top-level while disabled")
	ceRefused(t, f, ceComment(t, f, cctx, post, "new reply while disabled", &existing), post, commenter, before, "reply while disabled")

	// system.admin bypasses the capability check and NOT the setting.
	adminBefore := ceTake(t, f, post, f.userRef)
	ceRefused(t, f, ceComment(t, f, f.withIdentity(f.ctx), post, "admin root while disabled", nil), post, f.userRef, adminBefore, "system.admin top-level while disabled")
	ceRefused(t, f, ceComment(t, f, f.withIdentity(f.ctx), post, "admin reply while disabled", &existing), post, f.userRef, adminBefore, "system.admin reply while disabled")

	// The existing comment is still readable where the thread is.
	items := ceListed(t, f, cctx, post)
	if len(items) != 1 || uuid.UUID(items[0].Id) != existing {
		t.Fatalf("disabling must not hide the existing comment: listed %d item(s), want the one at %s", len(items), existing)
	}

	// Re-enable: the same request now lands.
	ceSetEnabled(t, f, post, true)
	ceAccepted(t, f, ceComment(t, f, cctx, post, "after re-enable", &existing), post, "after re-enable")
	after := ceTake(t, f, post, commenter)
	if after.rows != 2 || after.count != 2 {
		t.Fatalf("after re-enable rows=%d count=%d, want 2/2", after.rows, after.count)
	}
}

// N=0: nothing to keep, and still nothing lands.
func TestCommentsDisabled_N0_NothingLands(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)
	ceSetEnabled(t, f, post, false)

	before := ceTake(t, f, post, commenter)
	if before.rows != 0 || before.count != 0 {
		t.Fatalf("precondition: N=0 fixture has rows=%d count=%d", before.rows, before.count)
	}
	ceRefused(t, f, ceComment(t, f, cctx, post, "first ever, refused", nil), post, commenter, before, "N=0")
	if len(ceListed(t, f, cctx, post)) != 0 {
		t.Fatalf("N=0: the thread must still be empty")
	}
}

// N>=2: two roots and a reply survive the disable, all three are
// listed, and no ordinary row is added at any depth.
func TestCommentsDisabled_N2_AllRowsRetained(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)

	root1 := ceAccepted(t, f, ceComment(t, f, cctx, post, "root one", nil), post, "root one")
	reply1 := ceAccepted(t, f, ceComment(t, f, cctx, post, "reply to one", &root1), post, "reply to one")
	root2 := ceAccepted(t, f, ceComment(t, f, cctx, post, "root two", nil), post, "root two")

	ceSetEnabled(t, f, post, false)
	before := ceTake(t, f, post, commenter)
	if before.rows != 3 || before.count != 3 {
		t.Fatalf("precondition: N>=2 fixture has rows=%d count=%d, want 3/3", before.rows, before.count)
	}

	ceRefused(t, f, ceComment(t, f, cctx, post, "root three", nil), post, commenter, before, "third root while disabled")
	ceRefused(t, f, ceComment(t, f, cctx, post, "reply to two", &root2), post, commenter, before, "reply to a root while disabled")
	ceRefused(t, f, ceComment(t, f, cctx, post, "reply to the reply", &reply1), post, commenter, before, "nested reply while disabled")

	want := map[uuid.UUID]bool{root1: true, reply1: true, root2: true}
	for _, it := range ceListed(t, f, cctx, post) {
		delete(want, uuid.UUID(it.Id))
	}
	if len(want) != 0 {
		t.Fatalf("disabling must keep every existing row listed; missing %v", want)
	}
}

// Two posts, one caller: the setting is per post, and the enabled post
// proves the caller is permitted before the disabled one is asked.
func TestCommentsDisabled_PerPostIsolation(t *testing.T) {
	f := setupActivitiesFixture(t)
	postA := cePost(t, f, "org-only")
	postB := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)
	ceSetEnabled(t, f, postA, false)
	if ceEnabled(t, f, postB) != true {
		t.Fatalf("precondition: disabling A must not touch B")
	}

	// Anti-vacuity FIRST: B accepts an ordinary comment from this caller.
	ceAccepted(t, f, ceComment(t, f, cctx, postB, "B accepts before", nil), postB, "B accepts before")

	beforeA := ceTake(t, f, postA, commenter)
	ceRefused(t, f, ceComment(t, f, cctx, postA, "A refuses", nil), postA, commenter, beforeA, "A while B is enabled")

	// And B still accepts AFTER A refused, from the same caller.
	ceAccepted(t, f, ceComment(t, f, cctx, postB, "B accepts after", nil), postB, "B accepts after")
	if got := ceTake(t, f, postB, commenter); got.rows != 2 || got.count != 2 {
		t.Fatalf("B rows=%d count=%d, want 2/2", got.rows, got.count)
	}
	if got := ceTake(t, f, postA, commenter); got.rows != 0 || got.count != 0 {
		t.Fatalf("A rows=%d count=%d, want 0/0", got.rows, got.count)
	}
}

// Gate order: an unreadable post answers 404 whether or not comments
// are disabled on it, so the setting never leaks through a 409.
func TestCommentsDisabled_UnreadablePostStays404(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "private")
	_, cctx := ceCommenter(t, f)
	ceSetEnabled(t, f, post, false)

	resp := ceComment(t, f, cctx, post, "should be 404 not 409", nil)
	r, ok := resp.(openapi.CreatePostComment404JSONResponse)
	if !ok {
		t.Fatalf("private + disabled must answer 404 to a stranger, got %T", resp)
	}
	if r.Error != "post not found" {
		t.Fatalf("404 body = %q, want the read gate's own %q", r.Error, "post not found")
	}
}

// Whiteboards are a separate path and stay open while ordinary
// comments are closed. The same post, the same caller, in the same
// state: the whiteboard lands, the comment does not.
func TestCommentsDisabled_WhiteboardPathPreserved(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)
	ceSetEnabled(t, f, post, false)

	before := ceTake(t, f, post, commenter)
	ceRefused(t, f, ceComment(t, f, cctx, post, "ordinary, refused", nil), post, commenter, before, "ordinary comment beside a whiteboard")

	title := "sketch while comments are off"
	wb, err := f.social.CreatePostWhiteboard(cctx, openapi.CreatePostWhiteboardRequestObject{
		Id: openapi_types.UUID(post),
		Body: &openapi.WhiteboardCreate{
			Title:   &title,
			Content: openapi.WhiteboardContent{SourceW: 800, SourceH: 600},
		},
	})
	if err != nil {
		t.Fatalf("CreatePostWhiteboard: %v", err)
	}
	created, ok := wb.(openapi.CreatePostWhiteboard201JSONResponse)
	if !ok {
		t.Fatalf("whiteboards must not be gated by comments_enabled: got %T (%+v)", wb, wb)
	}

	// The row, and its kind.
	var kind *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT annotation_type FROM comments WHERE id = $1 AND deleted_at IS NULL`,
		uuid.UUID(created.Id)).Scan(&kind); err != nil {
		t.Fatalf("whiteboard row must exist: %v", err)
	}
	if kind == nil || *kind != "whiteboard" {
		t.Fatalf("annotation_type = %v, want whiteboard", kind)
	}
	list, err := f.social.ListPostWhiteboards(cctx, openapi.ListPostWhiteboardsRequestObject{Id: openapi_types.UUID(post)})
	if err != nil {
		t.Fatalf("ListPostWhiteboards: %v", err)
	}
	items, ok := list.(openapi.ListPostWhiteboards200JSONResponse)
	if !ok || len(items) != 1 || items[0].Id != created.Id {
		t.Fatalf("the whiteboard must be listed on the disabled post: %T len=%d", list, len(items))
	}

	// And ordinary comments are STILL refused after the whiteboard landed,
	// including a reply threaded under the whiteboard itself.
	wbID := uuid.UUID(created.Id)
	after := ceTake(t, f, post, commenter)
	ceRefused(t, f, ceComment(t, f, cctx, post, "reply under the sketch", &wbID), post, commenter, after, "ordinary reply under a whiteboard")
}

// The race, held by hand in both directions. See the file comment.
func TestCommentsDisabled_DisableCannotBeRaced(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	commenter, cctx := ceCommenter(t, f)
	ctx := context.Background()

	// ── Direction 1: the comment transaction holds the lock first. ──
	// The disable must WAIT for it, and the comment it commits is a
	// comment that legitimately predates the disable.
	tx1, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	var enabled bool
	if err := tx1.QueryRow(ctx,
		`SELECT comments_enabled FROM posts WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`,
		post).Scan(&enabled); err != nil {
		t.Fatalf("tx1 gate read: %v", err)
	}
	if !enabled {
		t.Fatalf("precondition: tx1 must read enabled")
	}

	disableDone := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(ctx, `UPDATE posts SET comments_enabled = false WHERE id = $1`, post)
		disableDone <- err
	}()
	select {
	case err := <-disableDone:
		t.Fatalf("the disable must block behind the open comment transaction; it returned early with %v", err)
	case <-time.After(300 * time.Millisecond):
		// Blocked, as the row lock requires.
	}

	firstID := uuid.New()
	if _, err := tx1.Exec(ctx,
		`INSERT INTO comments (id, target_kind, target_id, parent_id, root_id, depth, author_user_ref, body, body_html)
		 VALUES ($1, 'post', $2, NULL, $1, 0, $3, 'committed before the disable', '')`,
		firstID, post, commenter); err != nil {
		t.Fatalf("tx1 insert: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}
	select {
	case err := <-disableDone:
		if err != nil {
			t.Fatalf("disable after tx1 commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the disable must proceed once the comment transaction commits")
	}
	if ceEnabled(t, f, post) {
		t.Fatalf("the disable must have committed")
	}
	snap := ceTake(t, f, post, commenter)
	if snap.rows != 1 || snap.count != 1 {
		t.Fatalf("the comment that took the lock first must exist: rows=%d count=%d", snap.rows, snap.count)
	}
	// And now that the disable is committed, the real handler refuses.
	ceRefused(t, f, ceComment(t, f, cctx, post, "after a committed disable", nil), post, commenter, snap, "handler after direction 1")

	// ── Direction 2: the disable holds the lock first (uncommitted). ──
	// The REAL handler must block on it, and when the disable commits
	// the handler must read false and refuse. This is the interleaving a
	// check-then-insert gets wrong: a pre-transaction read would have
	// seen true and inserted.
	ceSetEnabled(t, f, post, true)
	tx2, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	if _, err := tx2.Exec(ctx, `UPDATE posts SET comments_enabled = false WHERE id = $1`, post); err != nil {
		t.Fatalf("tx2 disable: %v", err)
	}

	handlerDone := make(chan openapi.CreatePostCommentResponseObject, 1)
	go func() {
		resp, err := f.social.CreatePostComment(cctx, openapi.CreatePostCommentRequestObject{
			Id:   openapi_types.UUID(post),
			Body: &openapi.CommentCreate{Body: "started before the disable committed"},
		})
		if err != nil {
			t.Errorf("handler during direction 2: %v", err)
			handlerDone <- nil
			return
		}
		handlerDone <- resp
	}()
	select {
	case resp := <-handlerDone:
		t.Fatalf("the comment handler must block behind the uncommitted disable; it returned %T", resp)
	case <-time.After(300 * time.Millisecond):
		// Blocked on the row lock inside its transaction.
	}
	before := ceTake(t, f, post, commenter)
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}
	select {
	case resp := <-handlerDone:
		if resp == nil {
			t.Fatalf("handler errored; see above")
		}
		ceRefused(t, f, resp, post, commenter, before, "handler that waited out the disable")
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler must return once the disable commits")
	}
	if got := ceTake(t, f, post, commenter); got.rows != 1 {
		t.Fatalf("exactly the pre-disable comment must exist, rows=%d", got.rows)
	}
}

// Deleted between the read gate and the lock: still the read gate's
// 404, never a 500. Exercised by soft-deleting the post from a second
// transaction that holds the lock while the handler waits.
func TestCommentsDisabled_PostGoneUnderLockIs404(t *testing.T) {
	f := setupActivitiesFixture(t)
	post := cePost(t, f, "org-only")
	_, cctx := ceCommenter(t, f)
	ctx := context.Background()

	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE posts SET deleted_at = NOW() WHERE id = $1`, post); err != nil {
		t.Fatalf("soft delete in tx: %v", err)
	}
	done := make(chan openapi.CreatePostCommentResponseObject, 1)
	go func() {
		resp, err := f.social.CreatePostComment(cctx, openapi.CreatePostCommentRequestObject{
			Id:   openapi_types.UUID(post),
			Body: &openapi.CommentCreate{Body: "post vanishes under me"},
		})
		if err != nil {
			t.Errorf("handler: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		t.Fatalf("handler must block behind the open delete; returned %T", resp)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	select {
	case resp := <-done:
		r, ok := resp.(openapi.CreatePostComment404JSONResponse)
		if !ok {
			t.Fatalf("expected 404 for a post deleted under the lock, got %T", resp)
		}
		if r.Error != "post not found" {
			t.Fatalf("404 body = %q", r.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("handler must return once the delete commits")
	}
}
