// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// A post's membership in a collection — /collections/{id}/posts
// ---------------------------------------------------------------------------
//
// #882: "allow users to add other users' posts or single assets to
// their own collections. The owner can still delete it from
// everywhere." That last clause rules out a copy and mandates a
// REFERENCE, which is what `collection_posts` already is — so the
// container was right and only the routes into it were missing.
//
// The asset half of #882 shipped in #898 (collections/add_gate_test.go).
// The post half is this file, and it was not a matter of "add a gate to
// an existing endpoint": before this, `collection_posts` had exactly
// ONE writer outside seed and tests — CreatePost's optional
// `collection_id`, the context-aware upload modal dropping a NEW post
// into the collection you are standing on. There was no way to add an
// EXISTING post to a collection, no way to remove one, and no way to
// LIST one: posts.queries.sql's ListCollectionPostsPage was deleted in
// #661 for having no visibility rule and no callers.
//
// # Why these three live in `posts` and not in `collections`
//
// Every other /collections/{id}/… route is in `collections`, so this is
// the odd one out and the reason had better be good.
//
// A collection-posts LISTING has to answer the post read rule and
// return a hydrated Post — members joined, #883 placeholders applied,
// per-caller preview flags derived. Both of those live here:
// [visibility.PostReadable] is spliced from the same expression
// [ListPostsPageGated] filters the feed with, and fetchFullPost +
// enrichPreview are the hydration every other post surface uses.
// Rebuilding either inside `collections` would be a second expression
// of a rule and a second post shape for the client to drift from —
// exactly what epic #665 exists to remove, and exactly what #661's
// deletion note asked the next person to avoid: "a future
// collection-posts listing must go through the post read rule
// (posts.readRuleSQL) the way ListPostsByAssetGated does."
//
// The COLLECTION half is obtained rather than restated, in the other
// direction: [collections.ResolveMemberWrite] is the same
// canMutateCollection the asset routes apply, and
// visibility.CanSee(EntityCollection) is the same parent gate
// ListCollectionResources applies. Nothing about a collection is
// decided in this file.
//
// # The two gates on ADD, and why both are needed
//
//   - The CONTAINER: may this caller mutate this collection. Without
//     it anyone could pin anything into anyone's collection.
//   - The MEMBER: may this caller READ this post. Without it — and this
//     is the half that is easy to skip, because the container gate
//     looks like authorization — a collection owner could pin any post
//     in the instance given its UUID, and the 204-vs-404 difference
//     would confirm whether an arbitrary UUID named a real post.
//
// The member refusal is therefore the SAME 404 an absent post gets.

// ListCollectionPosts returns the posts pinned in a collection, filtered
// by the one post read rule.
//
// Two gates, both required, for the same reason ListCollectionResources
// documents on the asset side: the parent collection may be public while
// the posts inside it are not. #882 makes that the NORMAL case rather
// than an edge one — a collection now holds other people's work, whose
// visibility is theirs and not the curator's.
//
// A post the caller may not read is ABSENT, not a placeholder. That is
// the deliberate difference from the asset listing, where #883 renders a
// restricted member as a visible placeholder so the reader can see that
// a restriction exists and ask for access (#881). There is no
// request-access flow on the post surface to attach such a placeholder
// to, and a post's own row carries the author's title and description —
// so a placeholder would be a promise we cannot keep, built out of
// fields we would have to withhold anyway.
func (h *Handler) ListCollectionPosts(
	ctx context.Context,
	req openapi.ListCollectionPostsRequestObject,
) (openapi.ListCollectionPostsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)

	// PARENT gate. Fail closed and answer 404 rather than 403 (ADR
	// 0064) — do not confirm the collection exists. The same guard
	// ListCollectionResources opens with, asking the same
	// EntityCollection predicate.
	//
	// The Caller comes from postRuleInputs, which maps an ANONYMOUS
	// identity to the anonymous caller; collections.collectionCaller
	// binds its ref instead. The two agree in practice — no user holds
	// ref 0, so the authenticated predicate's owner and ACL disjuncts
	// cannot match for it and both reduce to "public collections only" —
	// and this direction is the narrower of the two, which is the one to
	// be wrong in.
	caller, _ := postRuleInputs(id)
	visible, err := visibility.CanSee(ctx, h.Pool, visibility.EntityCollection,
		caller, uuid.UUID(req.Id))
	if err != nil || !visible {
		return openapi.ListCollectionPosts404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
		}, nil
	}

	limit := int32(50)
	if req.Params.Limit != nil {
		l := int32(*req.Params.Limit)
		if l < 1 {
			l = 1
		}
		if l > maxCollectionPostsPage {
			l = maxCollectionPostsPage
		}
		limit = l
	}

	// #1147 — the mature axis, resolved ONCE for this request and handed
	// down, the way every other listing in this package does it. The
	// admin waiver is read off the identity here rather than inside the
	// query for the reason ADR 0090 gives: the exemption is checked
	// BEFORE qualification, so it has to be an input to the rule and not
	// a branch within it.
	ids, err := h.ListCollectionPostsGated(ctx, id, uuid.UUID(req.Id), limit,
		h.resolveMature(ctx, id), id != nil && id.Can(CapSystemAdmin))
	if err != nil {
		return nil, err
	}

	items := make([]openapi.Post, 0, len(ids))
	for _, pid := range ids {
		full, err := h.fetchFullPost(ctx, pid)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // raced deletion between the list and the fetch
			}
			return nil, err
		}
		items = append(items, *full)
	}

	// preview_available (#471) and the author (#557) are per-caller —
	// derive them from the request identity, same as ListPosts and
	// GetPostsByAsset.
	ptrs := make([]*openapi.Post, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.enrichForCaller(ctx, ptrs...); err != nil {
		return nil, err
	}

	return openapi.ListCollectionPosts200JSONResponse(openapi.PostList{Items: items}), nil
}

// maxCollectionPostsPage caps ?limit. Same ceiling as the openapi
// schema declares, restated here because the wire value is caller
// input and a schema is not an enforcement point.
const maxCollectionPostsPage int32 = 200

// ListCollectionPostsGated returns the ids of the posts pinned in a
// collection that the caller may read, in the curator's order.
//
// This is the listing #661 deleted, rebuilt the way that deletion note
// specified: the read rule is OBTAINED from readRuleSQL rather than
// restated, exactly as ListPostsByAssetGated does it. The version that
// was deleted had `p.deleted_at IS NULL` as its only post-side
// condition, which would have listed every private post in a collection
// the day somebody wired it up.
//
// Ordering is the curator's `sort_order ASC, added_at ASC` — the same
// order the resources listing returns, because a collection is "an
// ordered, optionally-shared set" (ADR 0009) and the arrangement is the
// curation. Bounded by `limit` with no cursor: the membership of one
// collection is small and the resources listing's cursor exists for a
// page size this surface does not need yet. When it does, the cursor
// belongs on the (sort_order, added_at) pair, like its asset twin.
//
// The expired-membership conjunct is the resources query's, restated
// here because it is a property of `collection_posts` and not of the
// read rule: a membership row past its `expires_at` is not a member.
// # The mature axis (#1147)
//
// This was the THIRD of the three sibling listings and the one #1116
// missed. [Handler.ListSharedWithMeGated] and
// [Handler.ListPostsByAssetGated] both carry the conjunct with an
// argument written out at each; this one is the same rule reached
// through a collection, and the gap was worth more than either of theirs
// because the rows it returns go on to `fetchFullPost` +
// `enrichForCaller` — so a disqualified viewer got the mature post's
// cover ids AND its members' thumbhashes, which is a picture rather than
// a listing.
//
// The conjunct is on the ROW plane, not the picture plane: absent, not
// placeheld. `assets.ListAssetsPageGated` states the argument in full —
// a restricted asset stays listed because #881's request-access flow
// hangs off the placeholder, and a mature one has no such flow, only a
// preference to change.
func (h *Handler) ListCollectionPostsGated(
	ctx context.Context,
	id *auth.Identity,
	collectionID uuid.UUID,
	limit int32,
	mature visibility.MatureViewer,
	matureAdmin bool,
) ([]pgtype.UUID, error) {
	args := []any{collectionID, limit} // $1, $2
	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "p", len(args))
	if err != nil {
		return nil, err
	}
	args = append(args, ruleArgs...)

	// The owner placeholder goes on the END, above whatever the read rule
	// bound, and ONLY when the conjunct names it: MatureFilterSQL folds
	// to "" for a qualified viewer and for an admin, and a bound
	// parameter no statement mentions is 42P18 — an error on every
	// request by exactly the readers who qualify. Same trap
	// posts/list_page.go documents at its own splice.
	matureFrag := visibility.MatureFilterSQL("p", visibility.MatureOwnerColPost,
		"$"+strconv.Itoa(len(args)+1), mature, matureAdmin)
	if matureFrag != "" {
		args = append(args, matureOwnerArg(id))
	}

	sql := `SELECT p.id
FROM collection_posts cp
JOIN posts p ON p.id = cp.post_id
WHERE cp.collection_id = $1::UUID
  AND cp.pinned = TRUE
  AND (cp.expires_at IS NULL OR cp.expires_at > NOW())
  AND p.deleted_at IS NULL` + ruleFrag + matureFrag + `
ORDER BY cp.sort_order ASC, cp.added_at ASC
LIMIT $2::INTEGER`

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("posts: list collection posts: %w", err)
	}
	defer rows.Close()

	var out []pgtype.UUID
	for rows.Next() {
		var pid pgtype.UUID
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("posts: list collection posts scan: %w", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: list collection posts rows: %w", err)
	}
	return out, nil
}

// AddCollectionPost pins an EXISTING post into a collection — the
// endpoint #882 asked for and the one that did not exist.
//
// What lands is a row in `collection_posts`: a reference. The post is
// not copied, does not change author, and does not gain a second
// lifecycle. `collection_posts_post_id_fkey … ON DELETE CASCADE`
// (baseline migration :4887) is what makes the owner's half of the
// owner's sentence true — delete the post and the reference vanishes
// from every collection that held it, including collections belonging
// to people the author has never heard of.
func (h *Handler) AddCollectionPost(
	ctx context.Context,
	req openapi.AddCollectionPostRequestObject,
) (openapi.AddCollectionPostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddCollectionPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddCollectionPost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	target, err := collections.ResolveMemberWrite(ctx, h.Pool, caller, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, collections.ErrCollectionUnreachable) {
			return openapi.AddCollectionPost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: add to collection: %w", err)
	}

	postID := uuid.UUID(req.Body.PostId)

	// The MEMBER gate. Everything above authorises the COLLECTION;
	// this is the separate question of whether the caller may reach the
	// post at all. Without it, holding a collection is enough to pin
	// any post in the instance and to learn from the status code
	// whether an arbitrary UUID names one.
	//
	// visibility.PostReadable and not a tier list: it runs the SAME
	// expression GET /posts/{id} and the browse feed run, so "what you
	// may collect" cannot drift from "what you may read". It answers
	// false for a hidden post and for an absent one alike, which is why
	// the refusal below is a single branch.
	ruleCaller, ruleCaps := postRuleInputs(caller)
	readable, err := visibility.PostReadable(ctx, h.Pool, ruleCaller, ruleCaps, postID)
	if err != nil {
		return nil, fmt.Errorf("posts: add to collection: member gate: %w", err)
	}
	if !readable {
		// Deliberately the SAME 404 an absent post gets — same status,
		// same body. Anything else (a 403, a distinct message) makes
		// this endpoint a UUID-existence probe, which is the whole
		// reason the gate is here.
		return openapi.AddCollectionPost404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}

	// Gold-standard path: Add(object=post, target=collection) per AP
	// §6.6 / §7.8, the post-kind twin of the asset path's emission.
	em := emit.AddToCollection(
		h.collectionActorContext(ctx, caller),
		activities.ObjectKindPost,
		postID.String(),
		target.ID.String(),
		target.Name,
	)
	if err := h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return New(tx).AddCollectionPost(ctx, AddCollectionPostParams{
			CollectionID: pgtype.UUID{Bytes: target.ID, Valid: true},
			PostID:       pgtype.UUID{Bytes: postID, Valid: true},
			SortOrder:    int32Or(req.Body.SortOrder, 0),
			Pinned:       boolOr(req.Body.Pinned, true),
			ExpiresAt:    pgTimestamptzFromPtr(req.Body.ExpiresAt),
		})
	}); err != nil {
		return nil, fmt.Errorf("posts: add to collection: %w", err)
	}

	// Nothing to evict. The post's cached copy is the post itself,
	// which this did not touch, and the collections by-id cache holds
	// the collection ROW (name, visibility, owner) — also untouched by
	// a membership write. The contents listing is uncached.
	return openapi.AddCollectionPost204Response{}, nil
}

// RemoveCollectionPost un-pins a post from a collection.
//
// Removal is the curator's alone: it deletes one `collection_posts`
// row and touches neither the post nor any other collection that
// references it. That is the property #882 needs on the way out — you
// can drop someone else's work from YOUR collection without affecting
// their work or anyone else's shelf.
//
// Not gated on readability, deliberately, and the reasoning is the
// asset route's (see TestRemoveCollectionResource_NeedsNoAssetGate): a
// post whose visibility narrowed after it was pinned would otherwise be
// stranded — permanently present in a collection whose owner is no
// longer allowed to name it. Removing is not a read; it leaks nothing,
// because DELETE answers 204 whether or not the membership existed.
func (h *Handler) RemoveCollectionPost(
	ctx context.Context,
	req openapi.RemoveCollectionPostRequestObject,
) (openapi.RemoveCollectionPostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveCollectionPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	target, err := collections.ResolveMemberWrite(ctx, h.Pool, caller, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, collections.ErrCollectionUnreachable) {
			return openapi.RemoveCollectionPost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: remove from collection: %w", err)
	}

	postID := uuid.UUID(req.PostId)
	em := emit.RemoveFromCollection(
		h.collectionActorContext(ctx, caller),
		activities.ObjectKindPost,
		postID.String(),
		target.ID.String(),
		target.Name,
	)
	if err := h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return New(tx).RemoveCollectionPost(ctx, RemoveCollectionPostParams{
			CollectionID: pgtype.UUID{Bytes: target.ID, Valid: true},
			PostID:       pgtype.UUID{Bytes: postID, Valid: true},
		})
	}); err != nil {
		return nil, fmt.Errorf("posts: remove from collection: %w", err)
	}
	return openapi.RemoveCollectionPost204Response{}, nil
}

// collectionActorContext builds the emit.ActorContext these two writes
// need. Same three fields every other emission site in this package
// assembles inline; named here because two call sites in one file
// assembling the same literal is how they come to disagree.
func (h *Handler) collectionActorContext(ctx context.Context, id *auth.Identity) emit.ActorContext {
	return emit.ActorContext{
		UserRef:  id.UserRef,
		Username: id.Username,
		BaseURL:  h.baseURLFn(ctx),
	}
}

// boolOr / pgTimestamptzFromPtr mirror the collections helpers of the
// same name — the membership columns are the same columns, so the
// defaults have to be the same defaults. int32Or already exists in this
// package (handler.go).
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func pgTimestamptzFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
