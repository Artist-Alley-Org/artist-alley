// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package social

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The post read gate on the social surface (#1132)
// ---------------------------------------------------------------------------
//
// # What was wrong
//
// Every handler in this package that takes a post id opened with
// `postExists` — `SELECT EXISTS (... WHERE id = $1 AND deleted_at IS
// NULL)`. That is a PRESENCE check, and presence is not readability.
// `GET /posts/{id}/comments` therefore listed the full thread of a
// post the caller could not open: org-only, followers-only, private,
// explicit-share, all of them. The listing query's only predicate is
// `deleted_at IS NULL` and it takes no viewer parameter at all, so
// nothing downstream narrowed it either.
//
// Comment bodies on a withheld post are conversation ABOUT withheld
// content — the #899/#902 class one container over. #1131's
// `comments_preview` on the feed card is safe because it rides the
// feed's own post gate; the direct endpoint was the hole.
//
// # Why the whole package moved, not just the listing
//
// `postExists` had six call sites and every one of them was the same
// mistake, so fixing only the reported one would have left five
// endpoints answering questions about a post the caller cannot read —
// including two that WRITE (a comment, a whiteboard) onto it. It is
// also, on every one of them, a UUID-existence oracle: presence and
// absence are distinguishable by status code, which is precisely what
// [visibility.PostReadable] collapses. So `postExists` is gone rather
// than kept beside its replacement; a helper that answers the weaker
// question is how the next handler acquires the same bug.
//
// The refusal is the SAME 404, same body, as an absent post. Anything
// else re-opens the oracle this closes.
//
// # One expression
//
// [visibility.PostReadable] runs the rule's own SQL (ADR 0063's
// `postReadableExpr`) as an EXISTS against one id — the identical
// expression the browse feed filters with and `GET /posts/{id}`
// answers. "Whose thread you may read" therefore cannot drift from
// "which posts you may read"; that agreement is structural, not
// maintained by hand. It is the same call `posts.postReadable` and
// `posts.AddCollectionPost`'s member gate make (#882, #922).
//
// # user_blocks is deliberately NOT consulted here
//
// Notifications, DMs and mentions filter on `user_blocks`; no comment
// read path does, before or after this change. That stays true, as a
// recorded decision rather than an oversight: a thread is a shared
// conversation with reply structure, and dropping one participant's
// rows from it silently renders replies whose parent is gone and
// changes what other readers see about a conversation they are in. A
// comment-level block model is a product question (hide the row for
// the blocker only? collapse it? does it break the tree?) and inventing
// one inside a security fix would ship an untested answer to a question
// nobody asked. Filed as a follow-up; the gate above is orthogonal to
// it and neither blocks the other.
//
// # What the sweep found and did NOT fix here
//
// The other by-target social listing in this package is
// `GET /assets/{id}/text-annotations` (with its create + update
// siblings), which opens with `assetExists` — the same presence-for-
// readability substitution, on the ASSET plane. It is not fixed in this
// change because the asset answer is a different expression: ADR 0064
// splits an asset into a row plane and a content plane, and which of
// [visibility.CanSee], [visibility.CanReadContent] and
// [visibility.FieldsReadable] an annotation body belongs behind is a
// decision, not a transcription of the post rule. Picking one silently
// inside a post-scoped fix is how a gate ends up scoped to its
// PRINCIPAL rather than its PAYLOAD. Reported for its own issue.
//
// `DELETE /comments/{id}` needs no post gate: it authorises on the
// comment's own author / moderator capability and discloses nothing
// about the containing post that the caller did not already name.

// postReadable reports whether the caller may READ this post — the
// question every handler in this package must ask before it answers
// anything about the post's likes, comments, whiteboards or thread.
//
// A nil / anonymous identity takes the rule's anonymous branch (the
// `public` tier alone), which is the narrower answer: a handler that
// loses its identity refuses rather than widens. Every social endpoint
// 401s on an anonymous caller before reaching here today, so that
// branch is defence in depth rather than a live path — but it is the
// branch a public-mode allowlist entry would activate, and it must
// already be correct on the day someone adds one (#709).
//
// Returns (false, nil) for "hidden from you" AND for "no such post"
// alike; callers map both to one 404.
func (h *Handler) postReadable(ctx context.Context, id *auth.Identity, postID uuid.UUID) (bool, error) {
	caller := visibility.NewCaller(nil)
	var caps visibility.PostCaps
	if id != nil && !id.IsAnonymous() {
		ref := id.UserRef
		caller = visibility.NewCaller(&ref)
		caps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
	}
	ok, err := visibility.PostReadable(ctx, h.Pool, caller, caps, postID)
	if err != nil {
		return false, fmt.Errorf("social: post read gate: %w", err)
	}
	return ok, nil
}

// postReadablePG is the pgtype.UUID-shaped form, since every handler
// here already holds the id in that shape for its queries.
func (h *Handler) postReadablePG(ctx context.Context, id *auth.Identity, postID pgtype.UUID) (bool, error) {
	return h.postReadable(ctx, id, uuid.UUID(postID.Bytes))
}
