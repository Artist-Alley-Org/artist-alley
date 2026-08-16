// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1047 — the feed card's comments preview, and the three properties
// that make it safe to put on a browse payload.
//
// 1. IT IS THE HEAD OF THE THREAD, bounded. The card shows
//    TopCommentsPerPost top-level comments, newest first, skipping
//    replies and soft-deleted rows — the same rows in the same order
//    `GET /posts/{id}/comments` puts at the top of its thread. If the
//    preview had its own idea of "which comments matter", the card and
//    the post it links to would disagree the first time either changed.
//
// 2. IT RESPECTS THE ANONYMOUS OPT-OUT ON THE COMMENTER. The identity
//    is resolved by the same expression a post's own author uses, so
//    ADR 0024's opt-out and ADR 0070 §3's authenticated-only real-name
//    rung hold here too — and the assertion is a SAME-COMMENT,
//    TWO-CALLERS pair, because an equality assertion against one caller
//    passes on a rule that is uniformly wrong.
//
//    The withholding is on the IDENTITY, not the words: an opted-out
//    commenter's comment still appears with no name, exactly as an
//    opted-out author's POST still appears with no header. The body
//    assertion is therefore deliberate, not an oversight — see the
//    handoff note on comment visibility.
//
// 3. IT IS ONE QUERY PER PAGE. Adding posts to a page must not add
//    queries; the preview would otherwise be the N+1 that the author
//    object exists to have removed.
//
// Skips without AA_DB_PASSWORD, like the other posts integration tests.

package posts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// seedComment plants one comment on a post. `parent` non-nil makes it a
// REPLY, which the preview must skip; `deleted` soft-deletes it, which
// the preview must skip too. `at` pins created_at so the ordering
// assertions do not depend on insert speed.
func seedComment(
	t *testing.T,
	pool *pgxpool.Pool,
	post uuid.UUID,
	author int64,
	body string,
	at time.Time,
	parent *uuid.UUID,
	deleted bool,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	root := id
	depth := 0
	if parent != nil {
		root = *parent
		depth = 1
	}
	var del *time.Time
	if deleted {
		now := time.Now()
		del = &now
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO comments (id, target_kind, target_id, parent_id, root_id, depth,
		                       author_user_ref, body, body_html, created_at, updated_at, deleted_at)
		 VALUES ($1,'post',$2,$3,$4,$5,$6,$7,$7,$8,$8,$9)`,
		id, post, parent, root, depth, author, body, at, del); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, id)
	})
	return id
}

func previewOf(t *testing.T, p *openapi.Post) []openapi.PostCommentPreview {
	t.Helper()
	if p.CommentsPreview == nil {
		return nil
	}
	return *p.CommentsPreview
}

// TestCommentPreview_IsTheBoundedHeadOfTheThread pins property 1, and
// pins it against the three rows that must NOT appear rather than only
// the ones that must: a reply, a soft-deleted comment, and the third
// top-level comment that falls off the end of the bound. A test that
// only asserted "the newest comment is there" would pass on an
// implementation that shipped the whole thread.
func TestCommentPreview_IsTheBoundedHeadOfTheThread(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ref, _ := seedAuthor(t, pool, "", "Commenter", "", false)
	post := seedAuthoredPost(t, pool, ref)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	oldest := seedComment(t, pool, post, ref, "oldest top-level", base, nil, false)
	middle := seedComment(t, pool, post, ref, "middle top-level", base.Add(time.Minute), nil, false)
	newest := seedComment(t, pool, post, ref, "newest top-level", base.Add(2*time.Minute), nil, false)
	seedComment(t, pool, post, ref, "a reply", base.Add(3*time.Minute), &newest, false)
	seedComment(t, pool, post, ref, "a deleted comment", base.Add(4*time.Minute), nil, true)

	p, err := h.fetchFullPost(context.Background(), pgtype.UUID{Bytes: post, Valid: true})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichTopComments(context.Background(), p); err != nil {
		t.Fatalf("enrichTopComments: %v", err)
	}
	got := previewOf(t, p)

	if len(got) != TopCommentsPerPost {
		t.Fatalf("preview carried %d comments, want the bound of %d: %+v", len(got), TopCommentsPerPost, got)
	}
	if got[0].Id != newest || got[1].Id != middle {
		t.Errorf("preview is not the newest-first head of the thread: got %v, %v; want %v, %v",
			got[0].Id, got[1].Id, newest, middle)
	}

	// The three exclusions, asserted against the WHOLE serialised
	// payload rather than the ids: a body that arrived on some other
	// field would still be a body that arrived.
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, unwanted := range []string{"a reply", "a deleted comment", "oldest top-level", oldest.String()} {
		if strings.Contains(string(body), unwanted) {
			t.Errorf("preview payload leaked %q; the head of the thread is top-level, live, and bounded", unwanted)
		}
	}
}

// TestCommentPreview_TheCommenterOptOutHolds is property 2, as the
// same-comment-two-callers pair.
//
// Two commenters on ONE post, so a single fetch exercises both rungs
// the ladder can get wrong:
//
//   - `hidden` opted out. An anonymous reader gets NO author object on
//     their entry — not a redacted one, not a placeholder — while an
//     authenticated reader gets the full identity. That difference is
//     the assertion; either leg alone proves nothing.
//   - `open` did not opt out and has no profile display_name, so their
//     display string falls back a rung. An anonymous reader must land
//     on the USERNAME and never on `user.fullname`, which is
//     authenticated-only (ADR 0070 §3). This is the half a test that
//     only checked the opt-out would miss.
//
// And the deliberate NON-withholding: both bodies ride in both cases.
// The opt-out is about the identity, and a preview that deleted an
// opted-out person's words from the feed would be a comment-visibility
// model this codebase does not have.
func TestCommentPreview_TheCommenterOptOutHolds(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const (
		hiddenReal    = "Hidden Comment Real Name"
		hiddenDisplay = "Hidden Comment Display Name"
		openReal      = "Open Comment Real Name"
	)
	hiddenRef, hiddenUser := seedAuthor(t, pool, hiddenReal, hiddenDisplay, "", true)
	openRef, openUser := seedAuthor(t, pool, openReal, "", "", false)

	poster, _ := seedAuthor(t, pool, "", "Poster", "", false)
	post := seedAuthoredPost(t, pool, poster)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedComment(t, pool, post, openRef, "the open commenter spoke", base, nil, false)
	seedComment(t, pool, post, hiddenRef, "the hidden commenter spoke", base.Add(time.Minute), nil, false)

	load := func(caller *auth.Identity) (*openapi.Post, string) {
		t.Helper()
		ctx := context.Background()
		if caller != nil {
			ctx = auth.WithIdentity(ctx, caller)
		}
		p, err := h.fetchFullPost(ctx, pgtype.UUID{Bytes: post, Valid: true})
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if err := h.enrichTopComments(ctx, p); err != nil {
			t.Fatalf("enrichTopComments: %v", err)
		}
		return p, authorBodyJSON(t, p)
	}

	authed, authedJSON := load(&auth.Identity{UserRef: poster, AuthMethod: "session"})
	anon, anonJSON := load(nil)

	names := func(p *openapi.Post) map[string]string {
		out := map[string]string{}
		for _, c := range previewOf(t, p) {
			if c.Author != nil {
				out[c.Body] = c.Author.DisplayName
			} else {
				out[c.Body] = ""
			}
		}
		return out
	}
	a := names(authed)
	n := names(anon)

	// CONSTRUCTIBILITY: without this leg, an implementation that never
	// resolved any commenter would pass every assertion below.
	if a["the hidden commenter spoke"] != hiddenDisplay {
		t.Fatalf("control: an authenticated reader must see the hidden commenter as %q, got %q; the assertions below would prove nothing",
			hiddenDisplay, a["the hidden commenter spoke"])
	}
	if a["the open commenter spoke"] != openReal {
		t.Fatalf("control: an authenticated reader must reach the real-name rung for the open commenter (%q), got %q",
			openReal, a["the open commenter spoke"])
	}

	// THE PAIR. Same comment, opposite verdicts.
	if n["the hidden commenter spoke"] != "" {
		t.Errorf("a commenter who opted out of anonymous exposure was named to an anonymous reader as %q",
			n["the hidden commenter spoke"])
	}
	if n["the open commenter spoke"] != openUser {
		t.Errorf("anonymous reader saw the open commenter as %q, want the username %q — rung 2 is authenticated-only",
			n["the open commenter spoke"], openUser)
	}

	// The whole payload, not just the fields we thought to check: a
	// field added to PostAuthor later that happens to carry the name
	// fails here rather than shipping quietly.
	for _, leaked := range []string{hiddenReal, hiddenDisplay, hiddenUser, openReal} {
		if strings.Contains(anonJSON, leaked) {
			t.Errorf("anonymous comment preview leaked %q", leaked)
		}
	}
	if !strings.Contains(authedJSON, hiddenDisplay) {
		t.Errorf("control: the authenticated payload should contain %q", hiddenDisplay)
	}

	// THE NON-WITHHOLDING, asserted so a future change that starts
	// dropping withheld people's comments is a test failure and a
	// conversation rather than a silent deletion.
	for _, body := range []string{"the hidden commenter spoke", "the open commenter spoke"} {
		if _, ok := n[body]; !ok {
			t.Errorf("an anonymous reader lost the comment %q entirely; the opt-out withholds the IDENTITY, not the words", body)
		}
	}
}

// TestCommentPreview_OneQueryForNPosts is property 3, measured on the
// database traffic pgx actually issues rather than on a code shape a
// refactor could walk past. Two queries, not one: the comment rows and
// the batched identity lookup, both keyed on the whole page.
func TestCommentPreview_OneQueryForNPosts(t *testing.T) {
	pool, tracer := tracedPool(t)
	h := peHandler(pool)

	ref, _ := seedAuthor(t, pool, "", "Commenter", "", false)
	const n = 6
	posts := make([]*openapi.Post, 0, n)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < n; i++ {
		id := seedAuthoredPost(t, pool, ref)
		seedComment(t, pool, id, ref, "a comment", base.Add(time.Duration(i)*time.Minute), nil, false)
		p, err := h.fetchFullPost(context.Background(), pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		posts = append(posts, p)
	}

	tracer.start()
	if err := h.enrichTopComments(context.Background(), posts...); err != nil {
		t.Fatalf("enrichTopComments: %v", err)
	}
	if got := tracer.stop(); got > 2 {
		t.Errorf("enriching %d posts issued %d queries; the pass is batched and must not scale with the page", n, got)
	}
	for i, p := range posts {
		if len(previewOf(t, p)) != 1 {
			t.Errorf("post %d got %d preview comments, want 1", i, len(previewOf(t, p)))
		}
	}
}
