// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"fmt"
	"time"

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
	if err := h.enrichTopComments(ctx, posts...); err != nil {
		return err
	}
	return h.enrichPreview(ctx, posts...)
}

// isAnonymousCaller reports whether the request has no authenticated
// principal behind it.
//
// Two shapes mean that, and only one of them used to be tested here
// (#1183). The usual one is a nil Identity, which is what the resolver
// leaves in the context for an unauthenticated request today. The other
// is a non-nil Identity with `AuthMethod == "anonymous"` — the shape
// `IsAnonymous()` exists for, which the read rule and ~20 handlers
// already check, and which a synthetic anonymous principal would carry.
//
// These three enrichment passes tested `== nil` alone. That is correct
// for today's resolver and silently wrong for any future that injects an
// anonymous identity: the passes would treat it as a MEMBER and hand out
// the display names that ADR 0024's hide-from-anonymous opt-out exists
// to withhold — a read rule that stayed right while the enrichment
// beside it leaked. The dead `auth.LoadAnonymousIdentity` that would
// have caused exactly that is deleted in the same change; this predicate
// is what makes reintroducing one safe rather than a regression.
func isAnonymousCaller(ctx context.Context) bool {
	id := auth.IdentityFromContext(ctx)
	return id == nil || id.IsAnonymous()
}

// TopCommentsPerPost is how many comments ride a post payload for the
// feed card's preview (#1047).
//
// TWO, which is the social convention rather than a measurement — every
// feed worth copying shows two and a "view all N" link, because two is
// the most that fits under a picture without the comments becoming the
// post. The count for the link is `comment_count`, which the card
// already had, so N is purely how many bodies travel.
//
// It is a constant and not a parameter: a client-chosen N is a client
// that can ask for a whole thread through the feed endpoint, which is
// what `GET /posts/{id}/comments` is for and is paginated for.
const TopCommentsPerPost = 2

// enrichTopComments attaches `comments_preview` — the head of each
// post's thread — to a page of posts, in ONE query (#1047).
//
// # The composition, and what it deliberately does not invent
//
// The feed card wants a couple of comments under the description. The
// tempting implementation is to call the comments endpoint per post,
// and it is wrong twice over: it is an N+1 on a browse surface, and
// that endpoint's gate is `signed in AND the post row exists` — it does
// NOT apply `postReadableExpr`. Composing it here would have imported
// that weakness into the feed.
//
// So this composes the FEED's rule instead, which is the stronger one
// and is already applied: every post reaching this pass came out of
// ListPostsPageGated, i.e. through `postReadableExpr` (author, tier,
// followers, ACL grant) for THIS caller. The preview inherits exactly
// that and adds only what the thread query itself adds — top-level
// rows, not soft-deleted, newest first.
//
// ⚠️ WHAT IS NOT HERE, and is not an oversight: there is no
// comment-level visibility rule in this codebase to compose. Comment
// text is gated by nothing but `deleted_at IS NULL`, and `user_blocks`
// is consulted by notifications, DMs and mention resolution and by
// NOTHING on any comment read path — so a blocked user's comments are
// visible to the blocker everywhere today. Making the feed preview the
// one surface that filtered them would be a comment-visibility model
// invented in a card, disagreeing with the thread the card links to.
// Recorded in #1047's handoff rather than fixed here.
//
// # The identity is the part that IS governed
//
// A commenter's name is disclosed by the same expression a post
// author's is — users.LookupAuthors — so the ADR 0024 opt-out and the
// authenticated-only real-name rung hold without being restated, and a
// withheld commenter is an OMISSION from the map rather than a redacted
// entry. Their words still ride, exactly as an opted-out author's post
// still rides: the opt-out is about the identity.
//
// # One query, bounded
//
// A window function over the whole page rather than a LATERAL per post
// or a query per post: `row_number()` partitioned by target ranks each
// post's comments and the outer filter keeps the first N, so adding
// posts to the page cannot add queries and the rows returned are capped
// at N × page size. Per-caller (the identities are), so it runs here on
// the way out and never into the cross-caller post cache — the reason
// enrichAuthors spells out at length.
func (h *Handler) enrichTopComments(ctx context.Context, posts ...*openapi.Post) error {
	ids := make([]uuid.UUID, 0, len(posts))
	for _, p := range posts {
		if p == nil {
			continue
		}
		// Reset first: the post came off a cross-caller cache and this
		// must be THIS caller's answer or nothing.
		p.CommentsPreview = nil
		ids = append(ids, uuid.UUID(p.Id))
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := h.Pool.Query(ctx, `
		SELECT c.target_id,
		       c.id,
		       c.body,
		       c.created_at,
		       c.author_user_ref,
		       COALESCE(fra.display_name, '') AS remote_display_name
		  FROM (
		        SELECT id, target_id, body, created_at, author_user_ref, actor_uri,
		               row_number() OVER (PARTITION BY target_id ORDER BY created_at DESC, id ASC) AS rn
		          FROM comments
		         WHERE target_kind = 'post'
		           AND target_id = ANY($1::UUID[])
		           AND parent_id IS NULL
		           AND deleted_at IS NULL
		       ) c
		  LEFT JOIN federation_remote_actors fra ON fra.actor_uri = c.actor_uri
		 WHERE c.rn <= $2::int
		 ORDER BY c.target_id, c.created_at DESC, c.id ASC`, ids, TopCommentsPerPost)
	if err != nil {
		return fmt.Errorf("posts: comment preview: %w", err)
	}
	defer rows.Close()

	type previewRow struct {
		target   uuid.UUID
		entry    openapi.PostCommentPreview
		authorID *int64
	}
	scanned := make([]previewRow, 0, len(ids)*TopCommentsPerPost)
	refSet := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var (
			target    uuid.UUID
			id        uuid.UUID
			body      string
			createdAt time.Time
			authorRef *int64
			remote    string
		)
		if err := rows.Scan(&target, &id, &body, &createdAt, &authorRef, &remote); err != nil {
			return fmt.Errorf("posts: comment preview scan: %w", err)
		}
		e := openapi.PostCommentPreview{
			Id:        id,
			Body:      body,
			CreatedAt: createdAt,
		}
		if remote != "" {
			r := remote
			e.RemoteDisplayName = &r
		}
		if authorRef != nil {
			refSet[*authorRef] = struct{}{}
		}
		scanned = append(scanned, previewRow{target: target, entry: e, authorID: authorRef})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("posts: comment preview rows: %w", err)
	}
	if len(scanned) == 0 {
		return nil
	}

	// One identity lookup for every commenter on the page, under the
	// same rule the post's own author header uses.
	authors := map[int64]openapi.PostAuthor{}
	if len(refSet) > 0 {
		refs := make([]int64, 0, len(refSet))
		for ref := range refSet {
			refs = append(refs, ref)
		}
		anonymous := isAnonymousCaller(ctx)
		authors, err = users.LookupAuthors(ctx, h.Pool, refs, anonymous)
		if err != nil {
			return err
		}
	}

	byTarget := make(map[uuid.UUID][]openapi.PostCommentPreview, len(ids))
	for _, r := range scanned {
		e := r.entry
		if r.authorID != nil {
			// Absent from the map = withheld or gone. Writing only what
			// the map contains is what makes the failure mode of any bug
			// above "no name", never "the wrong person's name".
			if a, ok := authors[*r.authorID]; ok {
				author := a
				e.Author = &author
			}
		}
		byTarget[r.target] = append(byTarget[r.target], e)
	}

	for _, p := range posts {
		if p == nil {
			continue
		}
		if list, ok := byTarget[uuid.UUID(p.Id)]; ok {
			entries := list
			p.CommentsPreview = &entries
		}
	}
	return nil
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
	anonymous := isAnonymousCaller(ctx)

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
	if caller.IsAnonymous() {
		// Anonymous holds no likes, and reading `caller.UserRef` for one
		// would query ref 0. Nil is the usual shape of an
		// unauthenticated caller here; a non-nil anonymous identity is
		// the other one, and both mean the same thing.
		caller = nil
	}

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
