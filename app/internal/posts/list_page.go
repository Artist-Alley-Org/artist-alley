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
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The two post list queries, with the read rule spliced in (#660).
//
// Why these are hand-built SQL rather than the sqlc queries they replace
// (ListPostsPage / ListPostsByAsset, both deleted from queries.sql):
// readRuleSQL returns a runtime fragment and sqlc queries are static
// strings with fixed placeholders — the same reason every other splice
// site of visibility.Predicate is hand-built (see assets/list_page.go).
//
// The sqlc versions are DELETED rather than left unused. A second,
// ungated expression of "which posts does this list return" sitting in
// queries.sql is exactly what produced #660, #657 and #650; leaving one
// behind for a future caller to pick up would rebuild the defect.

// ListPostsPageParams carries the caller-supplied filters for one feed
// page. Visibility is a NARROWING filter only: it selects among the
// tiers the caller's identity already admits (the read rule), and can never
// widen them. `?visibility=private` therefore means "the private posts I
// may read" — my own, plus everyone's for a moderator — not "everyone's
// private posts", which is what it used to mean.
type ListPostsPageParams struct {
	// IncludeDeleted is superadmin-only and enforced as such by the
	// handler. It waives the soft-delete conjunct and nothing else; the
	// read rule still applies in full.
	IncludeDeleted *bool
	AuthorUserRef  *int64
	Visibility     *string
	Q              *string
	Tag            *string
	// FeedFollowerRef is ?feed=following, and "following" is the UNION
	// of the two follow graphs (#1048): posts by an author this ref
	// follows, OR posts belonging to a team it follows. It had only the
	// author arm, which made the filter return nothing at all for the
	// very common account that follows studios and no people — and made
	// it structurally unreachable for a read-only account, since
	// following a *user* is a write such an account is refused while
	// following a team is not.
	//
	// The union is what the page's own vocabulary promises: the rail
	// above the grid lists the teams you follow and the only filter
	// offered says "Following". It is still one NARROWING conjunct —
	// see the header note above — so widening it from one graph to two
	// widens the SELECTION, never the read rule, which is ANDed on
	// after it and consults neither table.
	FeedFollowerRef *int64
	// TeamID scopes the page to one team's posts (#684). NARROWING ONLY:
	// it is a plain conjunct beside the read rule, never a disjunct with
	// it, so a team page shows the caller exactly the subset of that
	// team's posts browse would already have shown them. Membership of
	// the team grants nothing here — the read rule never consults
	// team_id (see visibility/post_rule.go), and this filter must not
	// become the place that starts.
	TeamID         pgtype.UUID
	CursorPostedAt pgtype.Timestamptz
	CursorID       pgtype.UUID
	RowLimit       int32
	// Ascending flips the feed to oldest-first (?dir=asc). It moves the
	// ORDER BY and the keyset predicate TOGETHER — see feedOrder.
	Ascending bool
}

// feedOrder is the (posted_at, id) keyset in one direction, expressed
// once (#868).
//
// The ORDER BY and the cursor comparison are the SAME fact stated
// twice, and the whole bug class this closes is what happens when only
// one of them moves. The feed's toggle used to send `?dir=asc` to a
// server that declared no such parameter, so nothing moved at all and
// "Oldest" quietly rendered newest. The obvious half-fix — flip the
// ORDER BY, leave the predicate — is strictly worse than the bug it
// replaces: `posted_at < cursor` walking an ascending scan asks for
// rows BEHIND the ones just returned, so page 2 re-serves page 1's
// window and everything past it is unreachable.
//
// So the direction is a single struct with a single constructor, and
// both strings come out of it. There is no way to hold one without the
// other.
type feedOrder struct {
	// cmp is the strict inequality that advances the scan: `<` walking
	// down, `>` walking up.
	cmp string
	// dir is the SQL keyword for both ORDER BY terms.
	dir string
}

func newFeedOrder(ascending bool) feedOrder {
	if ascending {
		return feedOrder{cmp: ">", dir: "ASC"}
	}
	return feedOrder{cmp: "<", dir: "DESC"}
}

// keysetSQL renders the "strictly past the cursor" predicate for the
// (posted_at, id) key, with the timestamp at $tsN and the tiebreak id
// at $idN. NULL cursor means "first page", which admits every row.
//
// The id tiebreak carries the same comparison as the timestamp on
// purpose: it is the low-order digit of one composite key, not a
// separate ordering. Pinning it to `<` while the timestamp flipped
// would make posts that share a posted_at (the seeded case, and any
// bulk import) the only ones that paginate wrongly — a defect that
// hides completely behind timestamps that happen to be distinct.
func (o feedOrder) keysetSQL(col, idCol string, tsN, idN int) string {
	return fmt.Sprintf(
		`($%d::TIMESTAMPTZ IS NULL
       OR %s %s $%d::TIMESTAMPTZ
       OR (%s = $%d::TIMESTAMPTZ AND %s %s $%d::UUID))`,
		tsN, col, o.cmp, tsN, col, tsN, idCol, o.cmp, idN,
	)
}

// orderBySQL renders the matching ORDER BY clause.
func (o feedOrder) orderBySQL(col, idCol string) string {
	return fmt.Sprintf("ORDER BY %s %s, %s %s", col, o.dir, idCol, o.dir)
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
// pagination on the (posted_at, id) keyset, in whichever direction
// `Ascending` selects — both halves of that key move together via
// feedOrder, which is the point of it. Filters:
//   - author_user_ref: limit to posts by a given user
//   - visibility: narrow to one tier WITHIN what the caller may read
//   - q: plain-text TSVECTOR search across post search_text
//   - tag: single-tag filter (intersects with q if both given)
//   - feed_follower_ref: restrict to what the given ref follows
//     (?feed=following) — authors OR teams OR tags. Three EXISTS,
//     hitting the user_follows PK, the team_follows PK and the
//     tag_follows PK respectively (#1123).
//   - team_id: restrict to one team's posts (#684)
//
// Every one of those NARROWS. The read rule is ANDed onto the result,
// never ORed into it, so no filter here can surface a row the caller
// could not already read — least obviously `team_id`, which looks like
// it ought to mean "the team's space" and does not. It means "the part
// of the team's space this caller can already see".
//
// ⚠️ The tag arm is where that sentence stops being an abstraction.
// A post's author chooses its tags, so `tag_follows` is the one follow
// source whose matching side is written by the party the read rule is
// protecting against: anybody may tag their own restricted post
// `fantasy` and every follower of `fantasy` would then have it in
// their Following feed — IF this disjunction were ORed with the read
// rule. It is not. The three EXISTS are ORed with EACH OTHER inside a
// single conjunct, and `ruleFrag` is appended as a separate ANDed
// conjunct below, so the follow set can only ever remove rows from a
// page the caller could already see. The acceptance test for #1123 is
// exactly that pair: one restricted post carrying a followed tag,
// visible to the caller who may read it and absent for the one who may
// not.
//
// The tag EXISTS joins `post_tags` on the tag STRING because that is
// what the corpus is keyed by — see migration 00050 for why there is no
// id to join on, and why matching is exact rather than case-folded.
//
// Placeholder discipline (ADR 0063): the builder binds $1–$10, the rule's
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
		p.TeamID,          // $10
	}
	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "posts", len(args))
	if err != nil {
		return nil, err
	}
	args = append(args, ruleArgs...)

	order := newFeedOrder(p.Ascending)

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
                      AND ff.followee_user_ref = posts.author_user_ref)
       OR EXISTS (SELECT 1 FROM team_follows tf
                    WHERE tf.user_ref = $6::BIGINT
                      AND tf.team_id = posts.team_id)
       OR EXISTS (SELECT 1 FROM tag_follows gf
                    JOIN post_tags pgt ON pgt.tag = gf.tag
                    WHERE gf.user_ref = $6::BIGINT
                      AND pgt.post_id = posts.id))
  AND ($10::UUID IS NULL OR team_id = $10::UUID)
  AND ` + order.keysetSQL("posted_at", "id", 7, 8))
	b.WriteString(ruleFrag)
	b.WriteString(`
` + order.orderBySQL("posted_at", "id") + `
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
// The predicate is visibility.PostLiveGrantSQL — the SAME fragment the
// post read rule ORs in as its ACL disjunct, not a second copy of it.
// It moved to the shared package with the rest of the rule (#873) and is
// exported for this one caller. That is the whole design
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
		userRef,        // $4 — consumed by PostLiveGrantSQL
	}

	sql := `SELECT ` + listPostsPageColumns + `
FROM posts
WHERE deleted_at IS NULL
  AND ($1::TIMESTAMPTZ IS NULL
       OR posted_at < $1::TIMESTAMPTZ
       OR (posted_at = $1::TIMESTAMPTZ AND id < $2::UUID))
  AND ` + visibility.PostLiveGrantSQL("", 4) + `
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
	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "p", len(args))
	if err != nil {
		return nil, err
	}
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
