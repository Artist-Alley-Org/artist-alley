// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// The two post list queries, with the read rule spliced in (#660).
//
// Why these are hand-built SQL rather than the sqlc queries they replace
// (ListPostsPage / ListPostsByAsset, both deleted from queries.sql):
// readRule.sql returns a runtime fragment and sqlc queries are static
// strings with fixed placeholders — the same reason every other splice
// site of visibility.Predicate is hand-built (see assets/list_page.go).
//
// The sqlc versions are DELETED rather than left unused. A second,
// ungated expression of "which posts does this list return" sitting in
// queries.sql is exactly what produced #660, #657 and #650; leaving one
// behind for a future caller to pick up would rebuild the defect.

// ListPostsPageParams carries the caller-supplied filters for one feed
// page. Visibility is a NARROWING filter only: it selects among the
// tiers the caller's identity already admits (readRule), and can never
// widen them. `?visibility=private` therefore means "the private posts I
// may read" — my own, plus everyone's for a moderator — not "everyone's
// private posts", which is what it used to mean.
type ListPostsPageParams struct {
	// IncludeDeleted is superadmin-only and enforced as such by the
	// handler. It waives the soft-delete conjunct and nothing else; the
	// read rule still applies in full.
	IncludeDeleted  *bool
	AuthorUserRef   *int64
	Visibility      *string
	Q               *string
	Tag             *string
	FeedFollowerRef *int64
	CursorPostedAt  pgtype.Timestamptz
	CursorID        pgtype.UUID
	RowLimit        int32
}

// ListPostsPageRow mirrors the SELECT list below. Order matters: rows
// scan positionally.
type ListPostsPageRow struct {
	ID                    pgtype.UUID
	AuthorUserRef         int64
	Title                 string
	Description           string
	Visibility            string
	CoverAssetID          pgtype.UUID
	CoverThumbnailAssetID pgtype.UUID
	PostedAt              pgtype.Timestamptz
	LikeCount             int64
	CommentCount          int64
	OriginServerID        pgtype.UUID
	TeamID                pgtype.UUID
	StateID               pgtype.UUID
	CreatedAt             pgtype.Timestamptz
	UpdatedAt             pgtype.Timestamptz
	DeletedAt             pgtype.Timestamptz
	DeletedReason         *string
}

// listPostsPageColumns is the SELECT list, kept identical to the one the
// sqlc query used so ListPostsPageRow keeps scanning positionally.
const listPostsPageColumns = `id, author_user_ref, title, description, visibility, cover_asset_id,
       cover_thumbnail_asset_id, posted_at, like_count, comment_count,
       origin_server_id, team_id, state_id, created_at, updated_at,
       deleted_at, deleted_reason`

// ListPostsPageGated runs the feed query for one caller. Cursor
// pagination on (posted_at DESC, id DESC). Filters:
//   - author_user_ref: limit to posts by a given user
//   - visibility: narrow to one tier WITHIN what the caller may read
//   - q: plain-text TSVECTOR search across post search_text
//   - tag: single-tag filter (intersects with q if both given)
//   - feed_follower_ref: restrict to authors the given ref follows
//     (?feed=following). EXISTS hits the user_follows PK.
//
// Placeholder discipline (ADR 0063): the builder binds $1–$9, the rule's
// fragment owns everything above, and its args are appended LAST.
func (h *Handler) ListPostsPageGated(
	ctx context.Context,
	id *auth.Identity,
	p ListPostsPageParams,
) ([]ListPostsPageRow, error) {
	args := []any{
		p.IncludeDeleted,  // $1
		p.AuthorUserRef,   // $2
		p.Visibility,      // $3
		p.Q,               // $4
		p.Tag,             // $5
		p.FeedFollowerRef, // $6
		p.CursorPostedAt,  // $7
		p.CursorID,        // $8
		p.RowLimit,        // $9
	}
	ruleFrag, ruleArgs := readRuleFor(id).sql("posts", len(args))
	args = append(args, ruleArgs...)

	var b strings.Builder
	b.WriteString(`SELECT ` + listPostsPageColumns + `
FROM posts
WHERE ($1::BOOLEAN IS TRUE OR deleted_at IS NULL)
  AND ($2::BIGINT IS NULL OR author_user_ref = $2::BIGINT)
  AND ($3::TEXT IS NULL OR visibility = $3::TEXT)
  AND ($4::TEXT IS NULL OR search_text @@ plainto_tsquery('english', $4::TEXT))
  AND ($5::TEXT IS NULL
       OR EXISTS (SELECT 1 FROM post_tags pt
                    WHERE pt.post_id = posts.id AND pt.tag = $5::TEXT))
  AND ($6::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM user_follows ff
                    WHERE ff.follower_user_ref = $6::BIGINT
                      AND ff.followee_user_ref = posts.author_user_ref))
  AND ($7::TIMESTAMPTZ IS NULL
       OR posted_at < $7::TIMESTAMPTZ
       OR (posted_at = $7::TIMESTAMPTZ AND id < $8::UUID))`)
	b.WriteString(ruleFrag)
	b.WriteString(`
ORDER BY posted_at DESC, id DESC
LIMIT $9::INTEGER`)

	rows, err := h.Pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("posts: list page: %w", err)
	}
	defer rows.Close()

	var out []ListPostsPageRow
	for rows.Next() {
		var i ListPostsPageRow
		if err := rows.Scan(
			&i.ID, &i.AuthorUserRef, &i.Title, &i.Description, &i.Visibility,
			&i.CoverAssetID, &i.CoverThumbnailAssetID, &i.PostedAt,
			&i.LikeCount, &i.CommentCount, &i.OriginServerID, &i.TeamID,
			&i.StateID, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
		); err != nil {
			return nil, fmt.Errorf("posts: list page scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: list page rows: %w", err)
	}
	return out, nil
}

// ListSharedWithMeGated returns one page of "posts somebody explicitly
// shared with me" (#875): the posts on which this caller holds a live
// post_acls grant. Newest-first on the same (posted_at, id) cursor key
// as the feed.
//
// The predicate is liveGrantSQL — the SAME fragment readRule.sql ORs in
// as its ACL disjunct, not a second copy of it. That is the whole design
// of this surface: "shared with me" and "a grant lets me read this" must
// be the same question, so a post can never appear here that
// GET /posts/{id} then refuses, and expiry/revocation drop an item off
// this page for free rather than because someone remembered to repeat
// the `expires_at` clause.
//
// No further read-rule conjunct is applied, and that is not an omission.
// ADR 0010 L6: a grant is purely additive, so a live grant alone is
// sufficient authorization to read the post at any tier — the rule's own
// disjunct sits at the top level for exactly that reason. AND-ing the
// full rule in here would be a no-op on every row this query can return.
//
// Soft-delete is the caller's axis as always: a deleted post is not
// shared content, and unlike the feed this surface has no admin
// trash-view mode, so it is filtered unconditionally.
//
// An anonymous caller cannot reach this (the handler 401s first) and
// would have no principal to match anyway.
func (h *Handler) ListSharedWithMeGated(
	ctx context.Context,
	userRef int64,
	cursorPostedAt pgtype.Timestamptz,
	cursorID pgtype.UUID,
	rowLimit int32,
) ([]ListPostsPageRow, error) {
	args := []any{
		cursorPostedAt, // $1
		cursorID,       // $2
		rowLimit,       // $3
		userRef,        // $4 — consumed by liveGrantSQL
	}

	sql := `SELECT ` + listPostsPageColumns + `
FROM posts
WHERE deleted_at IS NULL
  AND ($1::TIMESTAMPTZ IS NULL
       OR posted_at < $1::TIMESTAMPTZ
       OR (posted_at = $1::TIMESTAMPTZ AND id < $2::UUID))
  AND ` + liveGrantSQL("", 4) + `
ORDER BY posted_at DESC, id DESC
LIMIT $3::INTEGER`

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("posts: shared with me: %w", err)
	}
	defer rows.Close()

	var out []ListPostsPageRow
	for rows.Next() {
		var i ListPostsPageRow
		if err := rows.Scan(
			&i.ID, &i.AuthorUserRef, &i.Title, &i.Description, &i.Visibility,
			&i.CoverAssetID, &i.CoverThumbnailAssetID, &i.PostedAt,
			&i.LikeCount, &i.CommentCount, &i.OriginServerID, &i.TeamID,
			&i.StateID, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
		); err != nil {
			return nil, fmt.Errorf("posts: shared with me scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: shared with me rows: %w", err)
	}
	return out, nil
}

// ListPostsByAssetGated returns the ids of posts the caller may read
// whose members include the given asset (#478 slice-2, ADR 0070).
// Bounded (no cursor) — an asset lands in few posts, and the client only
// needs "redirect if one, list if several". Newest first.
//
// This used to hand-build its own tier list ({'public'} for anonymous,
// {'public','org-only'} for authenticated), which agreed with the
// single-item gate for those two tiers and silently disagreed for the
// three relationship tiers — an author could not find their OWN private
// post by asset. It now obtains the rule instead of restating it (#665),
// which both closes that gap and removes the second expression.
func (h *Handler) ListPostsByAssetGated(
	ctx context.Context,
	id *auth.Identity,
	assetID uuid.UUID,
) ([]pgtype.UUID, error) {
	args := []any{assetID} // $1
	ruleFrag, ruleArgs := readRuleFor(id).sql("p", len(args))
	args = append(args, ruleArgs...)

	sql := `SELECT p.id
FROM posts p
WHERE p.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM post_assets pa
                WHERE pa.post_id = p.id AND pa.asset_id = $1::UUID)` +
		ruleFrag + `
ORDER BY p.posted_at DESC, p.id DESC
LIMIT 200`

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("posts: by asset: %w", err)
	}
	defer rows.Close()

	var out []pgtype.UUID
	for rows.Next() {
		var pid pgtype.UUID
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("posts: by asset scan: %w", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: by asset rows: %w", err)
	}
	return out, nil
}
