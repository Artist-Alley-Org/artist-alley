// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// GET /posts/{id}/collections: a post knows which shelves hold it (#1119)
// ---------------------------------------------------------------------------
//
// The post editor has a membership section, and a membership section
// needs a read. `collection_posts` had three callers before this (an
// inserter, a remover and a per-COLLECTION listing) and no way to ask
// the question from the post's side, so an author could put their work
// on a shelf and never find out which shelves held it.
//
// # Why it lives in `collections` and not in `posts`
//
// The mirror of the reason `/collections/{id}/posts` lives in `posts`,
// stated at the top of posts/collection_posts.go: the package that owns
// the PAYLOAD owns the endpoint. This one returns hydrated collections
// filtered by the collection read rule and each one's mutation verdict,
// and all three of those live here: [listCollectionsPageColumns],
// [visibility.CollectionReadableSQL] and [canMutateCollection]. The post
// half is a single `author_user_ref` lookup, which is a column read and
// not a rule, exactly as ADR 0091 decision 5's asset half is.
//
// Putting it in `posts` would mean a second expression of the collection
// read rule and a second answer to "may this caller mutate this
// collection", the two things epic #665 exists to remove, and the
// second of which is an authorization rule.
//
// # The disclosure shape is `/assets/{id}/posts`, turned around
//
// Same two fields, same reasoning, and deliberately not a new one. An
// author asking "where did my work end up" is one question whichever
// direction it is asked in, and the answer there was already built to be
// the most conservative shape the model allows: readable things whole,
// everything else compressed to an integer with no handle back to what
// it counted. See posts/asset_usage.go for the full argument; every
// omission it lists is omitted here too.
//
// # ⛔ `can_remove` is resolved, never derived
//
// Membership is the curator's (#882). An author holds no authority over
// somebody else's shelf, so the flag on each item is
// [canMutateCollection] applied to the row already in hand: the SAME
// predicate [ResolveMemberWrite] applies, which is what
// `DELETE /collections/{id}/posts/{post_id}` calls. It costs no extra
// query precisely because the row is already scanned.
//
// A client deriving it from `owner_user_ref` would be wrong for the two
// capability holders the predicate also admits, and would be a copy of
// an authorization rule living in a browser.
//
// # Who may ask
//
// The post's AUTHOR, plus `posts.admin` / `system.admin`. A stranger
// gets the SAME 404 a nonexistent post gets, for the reason the asset
// side gives: the alternative is a shelving oracle over every
// collection on the instance, readable or not.

// maxPostCollectionsPage caps how many memberships come back in one
// answer. Bounded with no cursor, like the asset side: a post sits on a
// handful of shelves, and `withheld_count` is deliberately independent
// of this bound so truncation can never be read as withholding.
const maxPostCollectionsPage = 200

// ListPostCollections implements GET /posts/{id}/collections.
func (h *Handler) ListPostCollections(
	ctx context.Context,
	req openapi.ListPostCollectionsRequestObject,
) (openapi.ListPostCollectionsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
		return openapi.ListPostCollections401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	postID := uuid.UUID(req.Id)

	// The authorship gate. `notFound` answers BOTH "no such post" and
	// "not yours", in that order, so the endpoint cannot be walked to
	// discover which post UUIDs exist.
	notFound := openapi.ListPostCollections404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
	}
	author, err := h.postAuthorRef(ctx, postID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound, nil
		}
		return nil, fmt.Errorf("collections: post usage: author lookup: %w", err)
	}
	isAuthor := author != 0 && author == caller.UserRef
	if !isAuthor && !caller.Can(visibility.PostsAdmin) && !caller.Can(CapSystemAdmin) {
		return notFound, nil
	}

	rows, err := h.listCollectionsHoldingPost(ctx, caller, postID, maxPostCollectionsPage)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.PostCollectionMembership, 0, len(rows))
	for _, row := range rows {
		items = append(items, openapi.PostCollectionMembership{
			Collection: rowToAPI(row),
			// The shipped predicate, on the row already scanned. Not a
			// second query and not a second rule.
			CanRemove: canMutateCollection(caller, row),
		})
	}

	// The withheld half. Every live shelf holding the post, minus the
	// ones this caller may read, floored at zero.
	//
	// ⚠️ THE SUBTRAHEND IS A COUNT, NOT len(items) (#1237 on the asset
	// side). `items` comes off a LIMIT, so subtracting its length makes
	// TRUNCATION look like withholding, and the sentence built on that
	// number ("also on N shelves you cannot see") would be false.
	// Both counts compose the same membership clause, so the two cannot
	// disagree about which rows are candidates.
	//
	// The floor is not padding: the two statements run outside one
	// snapshot, so a collection deleted between them would make the
	// subtraction negative. A race can only ever make the count too
	// SMALL, which discloses less than the truth.
	total, err := h.countCollectionsHoldingPost(ctx, nil, postID)
	if err != nil {
		return nil, err
	}
	readable, err := h.countCollectionsHoldingPost(ctx, caller, postID)
	if err != nil {
		return nil, err
	}
	withheld := total - readable
	if withheld < 0 {
		withheld = 0
	}

	return openapi.ListPostCollections200JSONResponse(openapi.PostCollectionUsage{
		Items:         items,
		WithheldCount: withheld,
	}), nil
}

// postAuthorRef reads one column off `posts`.
//
// Raw rather than sqlc because `posts` is not this package's table and
// adding it to collections/queries.sql would put a second schema's rows
// in this package's generated models for one integer. It is a column
// read and decides nothing: the authorization it feeds is spelled out at
// the call site.
func (h *Handler) postAuthorRef(ctx context.Context, postID uuid.UUID) (int64, error) {
	var ref int64
	err := h.Pool.QueryRow(ctx,
		`SELECT author_user_ref FROM posts WHERE id = $1::UUID AND deleted_at IS NULL`,
		postID,
	).Scan(&ref)
	return ref, err
}

// postMembershipClause is what "this collection currently holds this
// post" means, and it is [posts.Handler.ListCollectionPostsGated]'s
// clause with the sides swapped.
//
// Restated here rather than imported because it is a property of
// `collection_posts` rather than of any read rule, and because `posts`
// be imported from this package without a cycle. The three conjuncts
// are the same three: pinned, not expired, and a live post.
const postMembershipClause = `EXISTS (
         SELECT 1
           FROM collection_posts cp
           JOIN posts p ON p.id = cp.post_id
          WHERE cp.collection_id = c.id
            AND cp.post_id = $1::UUID
            AND cp.pinned = TRUE
            AND (cp.expires_at IS NULL OR cp.expires_at > NOW())
            AND p.deleted_at IS NULL
       )`

// listCollectionsHoldingPost returns the collections holding the post
// that `caller` may read, newest shelf first.
//
// The read rule is OBTAINED from [visibility.CollectionReadableSQL], not
// restated. It is the same fragment `GET /collections` and the collections
// autocomplete compose, so "which shelves I can see here" cannot drift
// from "which shelves I can see anywhere".
//
// ⚠️ `c.deleted_at IS NULL` IS INLINE ON PURPOSE. That fragment is EMPTY
// for a system.admin (an admin must not pay for a predicate that would
// only narrow them), and on that arm nothing else states the tombstone
// rule. Its own doc says a caller that must not surface soft-deleted
// rows has to say so itself. This is such a caller: a tombstoned shelf
// is not somewhere the post appears, and offering one would put a
// removal control on a row the curator has already thrown away.
//
// Placeholder discipline (ADR 0063): this builder's own placeholders are
// $1 and $2, the predicate's fragment numbers from there, and its args
// are appended LAST.
func (h *Handler) listCollectionsHoldingPost(
	ctx context.Context,
	caller *auth.Identity,
	postID uuid.UUID,
	limit int,
) ([]Collection, error) {
	args := []any{postID, limit} // $1, $2
	frag, fragArgs, err := visibility.CollectionReadableSQL(ctx, "c",
		visibility.NewCaller(&caller.UserRef), collectionCaps(caller), len(args))
	if err != nil {
		return nil, fmt.Errorf("collections: post usage: read rule: %w", err)
	}
	args = append(args, fragArgs...)

	var b strings.Builder
	b.WriteString(`SELECT ` + listCollectionsPageColumns + `
FROM collections c
WHERE c.deleted_at IS NULL
  AND ` + postMembershipClause)
	b.WriteString(frag)
	b.WriteString(`
ORDER BY c.created_at DESC, c.id DESC
LIMIT $2::INTEGER`)

	rows, err := h.Pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("collections: post usage: list: %w", err)
	}
	defer rows.Close()

	var out []Collection
	for rows.Next() {
		var i Collection
		if err := scanCollection(rows, &i); err != nil {
			return nil, fmt.Errorf("collections: post usage: scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collections: post usage: rows: %w", err)
	}
	return out, nil
}

// countCollectionsHoldingPost counts the shelves holding the post,
// UNBOUNDED, with the read rule applied when `caller` is non-nil and
// omitted when it is nil.
//
// The nil arm is the TOTAL, and it is the one place in this file that
// deliberately runs no visibility rule: it produces a number, never a
// row, and that number is the entire disclosure `withheld_count` makes.
// Passing an identity here instead would make the remainder always zero
// and quietly retire the field.
func (h *Handler) countCollectionsHoldingPost(
	ctx context.Context,
	caller *auth.Identity,
	postID uuid.UUID,
) (int64, error) {
	args := []any{postID} // $1
	frag := ""
	if caller != nil {
		f, fragArgs, err := visibility.CollectionReadableSQL(ctx, "c",
			visibility.NewCaller(&caller.UserRef), collectionCaps(caller), len(args))
		if err != nil {
			return 0, fmt.Errorf("collections: post usage: count read rule: %w", err)
		}
		frag = f
		args = append(args, fragArgs...)
	}

	sql := `SELECT COUNT(*)
FROM collections c
WHERE c.deleted_at IS NULL
  AND ` + postMembershipClause + frag

	var n int64
	if err := h.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("collections: post usage: count: %w", err)
	}
	return n, nil
}

// scanCollection reads one [listCollectionsPageColumns] row.
//
// Extracted so this file and list_page.go cannot drift apart in the one
// way a positional scan can go wrong silently: the column list is a
// shared constant, and a column added to it that only one scanner reads
// is a mismatch the compiler cannot see.
func scanCollection(rows pgx.Rows, i *Collection) error {
	return rows.Scan(
		&i.ID, &i.OwnerUserRef, &i.Name, &i.Description, &i.Visibility, &i.Membership,
		&i.ExpiresAt, &i.Purpose, &i.OriginServerID,
		&i.CreatedAt, &i.UpdatedAt, &i.SearchText, &i.SmartQuery,
		&i.DeletedAt, &i.DeletedReason, &i.DeletedByUserRef, &i.CoverAssetID,
		&i.FeaturedCoverAssetID, &i.FeaturedCoverFocalX, &i.FeaturedCoverFocalY,
		&i.CoverFocalX, &i.CoverFocalY,
		&i.FeaturedCoverZoom, &i.CoverZoom,
	)
}
