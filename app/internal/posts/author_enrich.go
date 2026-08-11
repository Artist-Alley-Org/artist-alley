// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// enrichForCaller applies EVERY per-caller derivation a Post needs
// before it leaves a handler. One entry point, called from every read
// and write path that returns a post.
//
// It exists because there are now two such derivations and seven call
// sites. Adding the author pass beside each `enrichPreview` call would
// have been seven chances to miss one, and a missed one is invisible:
// the post renders, just without an author header, on whichever single
// surface was forgotten. Handlers ask for "the post, for this caller"
// and get all of it.
//
// Tests still call the individual passes directly — they are testing
// one rule at a time, which is the case where the split is useful.
func (h *Handler) enrichForCaller(ctx context.Context, posts ...*openapi.Post) error {
	if err := h.enrichAuthors(ctx, posts...); err != nil {
		return err
	}
	if err := h.enrichOrigins(ctx, posts...); err != nil {
		return err
	}
	if err := h.enrichLiked(ctx, posts...); err != nil {
		return err
	}
	return h.enrichPreview(ctx, posts...)
}

// enrichAuthors stamps `post.author` — the renderable identity behind
// `author_user_ref` — onto the given posts for the request's caller
// (#557). ONE query for the whole batch, keyed on the DISTINCT set of
// author refs across every post handed in.
//
// # Why this is a per-request pass and not a JOIN on the list query
//
// The obvious implementation — widen ListPostsPageGated's SELECT with a
// LEFT JOIN onto "user" — cannot work here, and the reason is the shape
// of the feed path rather than anything about SQL. ListPosts uses the
// list query only to decide WHICH posts and in what order; the payload
// for each one then comes out of `h.byID`, the cross-caller post cache,
// via fetchFullPost. A column joined onto the list row would be
// discarded on the very next line.
//
// Putting it into postRowToAPI instead — i.e. into the cached object —
// is worse than not shipping it. The identity is PER-CALLER twice over:
// whether it appears at all depends on the caller being anonymous, and
// so does the display-name ladder. Baking it into a cache shared by
// every caller means the first authenticated reader of a post populates
// the entry, and the next ANONYMOUS reader gets a cache hit carrying an
// identity they are not entitled to. The opt-out would then hold or fail
// depending on who read the post first — intermittently, at a minority
// of users, and invisibly to any test that exercises one caller against
// a cold cache. That is the same trap preview_available fell into
// (#471) and members' `restricted` after it (#883), so this follows the
// same rule they settled on: per-caller facts are derived after the
// cache, on the way out.
//
// # Cache safety
//
// `posts` here are POINTERS INTO the caller's own []openapi.Post — the
// slice ListPosts built by copying each cached Post by value. Assigning
// to `p.Author` writes to that copy. Unlike enrichPreview, which had to
// clone the Members slice because a slice header aliases the cached
// backing array, `Author` is a plain pointer FIELD on the struct: the
// value copy already detached it, and the openapi.PostAuthor it points
// at is freshly allocated per request by users.LookupAuthors. Nothing
// reaches back into h.byID.
//
// # Withheld authors
//
// A ref missing from the lookup gets NO author object, and the two ways
// that happens are both correct as an absence:
//
//   - the author set `hide_from_anonymous` and the caller is anonymous.
//     LookupAuthors omits them rather than returning a redacted entry,
//     so there is nothing here to accidentally render.
//   - the "user" row is gone (a hard-deleted account).
//
// Fail-closed by construction: this writes only what the map contains,
// so the failure mode of any bug above is a missing author header, never
// a disclosed one. `p.Author` is also RESET to nil before the lookup —
// posts arrive from a cache that must never have carried one, but
// clearing it costs nothing and makes "the map decides" true rather than
// merely expected.
func (h *Handler) enrichAuthors(ctx context.Context, posts ...*openapi.Post) error {
	anonymous := auth.IdentityFromContext(ctx) == nil

	refSet := make(map[int64]struct{}, len(posts))
	for _, p := range posts {
		if p == nil {
			continue
		}
		p.Author = nil
		refSet[p.AuthorUserRef] = struct{}{}
	}
	if len(refSet) == 0 {
		return nil
	}
	refs := make([]int64, 0, len(refSet))
	for ref := range refSet {
		refs = append(refs, ref)
	}

	authors, err := users.LookupAuthors(ctx, h.Pool, refs, anonymous)
	if err != nil {
		return err
	}

	for _, p := range posts {
		if p == nil {
			continue
		}
		if a, ok := authors[p.AuthorUserRef]; ok {
			author := a
			p.Author = &author
		}
	}
	return nil
}

// enrichOrigins resolves `post.origin` — the peer a remote post came
// from — for the given posts. One batched query, and only when at least
// one post is remote, so a purely-local install (which is every install
// until it federates) pays nothing.
//
// The post-level twin of assets/cardfields.go's ListAssetOrigins pass,
// and deliberately the same shape: a post carries `origin_server_id`,
// which is a bare UUID, and the card has to print a NAME. ContentOrigin
// exists precisely because answering "whose is this?" with an identifier
// answers nothing.
//
// Not per-caller — provenance is a fact about the row, the same for
// everyone who can see it. It lives here rather than on the cached Post
// for a plainer reason: the peer's display_name is the OPERATOR's, and
// they can rename a peer at any time. Nothing on that path invalidates a
// post cache, so a renamed peer would keep showing its old name on
// cached posts until an unrelated write evicted them — the same
// "server has it, client can't see it" shape #648 hit with thumbhash.
// One join per page is cheaper than a cross-domain invalidation.
//
// A local post is simply absent from the result and keeps `origin` nil,
// so "no row" reads as "ours" without a sentinel.
func (h *Handler) enrichOrigins(ctx context.Context, posts ...*openapi.Post) error {
	idSet := make(map[uuid.UUID]struct{}, len(posts))
	for _, p := range posts {
		if p == nil {
			continue
		}
		p.Origin = nil
		if p.OriginServerId != nil {
			idSet[uuid.UUID(*p.OriginServerId)] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	peerIDs := make([]uuid.UUID, 0, len(idSet))
	for id := range idSet {
		peerIDs = append(peerIDs, id)
	}

	rows, err := h.Pool.Query(ctx,
		`SELECT id, display_name, instance_url
		   FROM federation_peers
		  WHERE id = ANY($1::UUID[])`, peerIDs)
	if err != nil {
		return fmt.Errorf("posts: origin enrich: %w", err)
	}
	defer rows.Close()

	peers := make(map[uuid.UUID]openapi.ContentOrigin, len(peerIDs))
	for rows.Next() {
		var (
			id          uuid.UUID
			displayName string
			instanceURL string
		)
		if err := rows.Scan(&id, &displayName, &instanceURL); err != nil {
			return fmt.Errorf("posts: origin enrich scan: %w", err)
		}
		url := instanceURL
		peers[id] = openapi.ContentOrigin{
			PeerId:      id,
			DisplayName: displayName,
			InstanceUrl: &url,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("posts: origin enrich rows: %w", err)
	}

	for _, p := range posts {
		if p == nil || p.OriginServerId == nil {
			continue
		}
		if o, ok := peers[uuid.UUID(*p.OriginServerId)]; ok {
			origin := o
			p.Origin = &origin
		}
	}
	return nil
}

// enrichLiked sets `post.liked` — whether THIS caller has liked each
// post — in ONE query for the whole page (#557).
//
// The feed card's heart has to be drawn in its correct state on first
// paint. `GET /posts/{id}/like` answers that for one post, which is
// right for the modal and useless for a page of twenty: it is the same
// N+1 the author object exists to remove, in a different field. So the
// answer rides the payload, resolved for every post at once against the
// `likes` table's (target_kind, target_id, user_ref) key.
//
// Anonymous callers are false without a query — there is no identity to
// have liked with, and the endpoints would 401 anyway.
//
// Per-caller by definition, so it lives here and not on the cached Post
// for exactly the reason spelled out on enrichAuthors: one reader's
// answer must never become the next reader's.
func (h *Handler) enrichLiked(ctx context.Context, posts ...*openapi.Post) error {
	caller := auth.IdentityFromContext(ctx)

	ids := make([]uuid.UUID, 0, len(posts))
	for _, p := range posts {
		if p == nil {
			continue
		}
		// Reset first: the value came off a cross-caller cache and must
		// be this caller's answer or nothing.
		f := false
		p.Liked = &f
		if caller != nil {
			ids = append(ids, uuid.UUID(p.Id))
		}
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := h.Pool.Query(ctx,
		`SELECT target_id
		   FROM likes
		  WHERE target_kind = 'post'
		    AND user_ref = $1::BIGINT
		    AND target_id = ANY($2::UUID[])`, caller.UserRef, ids)
	if err != nil {
		return fmt.Errorf("posts: liked enrich: %w", err)
	}
	defer rows.Close()

	liked := make(map[uuid.UUID]struct{}, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("posts: liked enrich scan: %w", err)
		}
		liked[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("posts: liked enrich rows: %w", err)
	}

	for _, p := range posts {
		if p == nil {
			continue
		}
		if _, ok := liked[uuid.UUID(p.Id)]; ok {
			v := true
			p.Liked = &v
		}
	}
	return nil
}
