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
// # What the sweep found, and how #1135 answered it
//
// The other by-target social listing in this package is
// `GET /assets/{id}/text-annotations` (with its create + update
// siblings), which opened with `assetExists` — the same presence-for-
// readability substitution, on the ASSET plane. #1132 deliberately left
// it, because the asset answer is a different expression: ADR 0064
// splits an asset into a row plane and a content plane, and which of
// [visibility.CanSee], [visibility.CanReadContent] and
// [visibility.FieldsReadable] an annotation body belongs behind is a
// decision, not a transcription of the post rule. Picking one silently
// inside a post-scoped fix is how a gate ends up scoped to its
// PRINCIPAL rather than its PAYLOAD.
//
// #1135 made that decision — see [Handler.assetContentReadable] below —
// and `assetExists` is gone the same way `postExists` went.
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

// ---------------------------------------------------------------------------
// The asset gate on the text-annotation surface (#1135)
// ---------------------------------------------------------------------------
//
// # Which plane, and why
//
// A text annotation is not metadata ABOUT an asset row — it is a
// quotation FROM the asset's content. Its anchor is a line/column range
// inside the document and its body discusses what stands at that range;
// `GET /assets/{id}/text-annotations` therefore hands back other
// people's reading of a document, one container over from handing back
// the document. That is the #899/#902 class, and it puts the annotation
// behind the CONTENT plane (ADR 0064), not mere row visibility.
//
// Row visibility alone would have gated nothing at all. EntityAsset's
// AUTHENTICATED predicate is `deleted_at IS NULL` and nothing more, so
// [visibility.CanSee] returns true for every signed-in caller against
// every undeleted asset — a gate built on it would have reviewed as if
// it worked while changing no verdict (the trap collections.go's
// mayCollectAsset already documents).
//
// [visibility.FieldsReadable] is the wrong plane in the other
// direction. It admits the FIELD plane to holders of a MUTATION
// capability — a team lead reads the title of an asset they are
// entitled to edit but not to open (#939, ADR 0064). Being allowed to
// retitle a picture is not being allowed to read what reviewers said
// about what is IN it, and routing annotations through that plane would
// scope the gate to the caller's editing rights rather than to the
// payload.
//
// So: [visibility.CanSeeAssetContent], the ROW ∧ CONTENT conjunction —
// the same call the collection and post attach gates make. "Whose
// annotations you may read" cannot drift from "whose bytes you may
// receive", because it is one expression, called from three places.
//
// # It gates the WRITES too, not just the listing
//
// Create and update both run it. An annotation is authored INTO the
// asset's thread, and a caller who may not read the document may not
// leave marginalia on it either — the write is also a read on the way
// out, since both handlers echo the stored row back. Update reaches the
// asset through the annotation's own `target_id` rather than a path
// parameter, and the moderator disjunct there (`comments.delete.any`)
// is precisely the principal who could otherwise edit — and read back —
// annotations on documents they were never admitted to.
//
// # The refusal shape
//
// Identical 404 for "hidden from you" and "no such asset", per the
// shape #1132 standardised. Anything else re-opens the UUID oracle.
//
// # The sweep for the remaining presence checks
//
// Two `SELECT EXISTS (... FROM assets ...)` pre-checks survive the
// grep and are deliberately left alone, because neither is a GATE:
// `assets.SetAITagsForAsset` (via `AssetExistsForAI`) and
// `transcribe.Writer.SetAITranscriptForAsset` are the AI bridge's
// WRITE-side entry points, called by the pipeline with no caller
// identity in scope at all. They ask "is there still a row to write
// to" so they can return `ai.ErrAssetNotFound` before allocating
// storage bytes; there is no principal to withhold anything from.
// A readability rule there would have to invent a caller, and
// inventing one is how a system job acquires a user's permissions.
//
// The other three (`search/http.go`, `search/saved/execute.go`,
// `search/feedback`) already splice the visibility predicate into
// the EXISTS — they are the shape this gate now matches, not the
// shape it replaced.
//
// A nil / anonymous identity takes the rule's anonymous branch, which
// resolves against the sensitivity tier alone — the narrower answer.
// Every annotation endpoint 401s an anonymous caller before reaching
// here; that branch is the one a public-mode allowlist entry would
// activate, and it must already be correct on the day someone adds one
// (#709).
//
// Returns (false, nil) for "hidden from you" AND "no such asset" alike.
func (h *Handler) assetContentReadable(ctx context.Context, id *auth.Identity, assetID uuid.UUID) (bool, error) {
	caller := visibility.NewCaller(nil)
	var caps visibility.CapabilityChecker
	if id != nil && !id.IsAnonymous() {
		ref := id.UserRef
		caller = visibility.NewCaller(&ref)
		caps = func(code string) bool { return id.Can(code) }
	}
	ok, err := visibility.CanSeeAssetContent(ctx, h.Pool, caller, caps, assetID)
	if err != nil {
		return false, fmt.Errorf("social: asset content gate: %w", err)
	}
	return ok, nil
}

// assetContentReadablePG is the pgtype.UUID-shaped form, since the
// annotation handlers already hold the id in that shape for their
// queries.
func (h *Handler) assetContentReadablePG(ctx context.Context, id *auth.Identity, assetID pgtype.UUID) (bool, error) {
	return h.assetContentReadable(ctx, id, uuid.UUID(assetID.Bytes))
}
