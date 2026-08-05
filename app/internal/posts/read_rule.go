// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The post read rule — obtained, not restated
// ---------------------------------------------------------------------------
//
// #660: `GET /posts?visibility=private` returned other people's private
// posts to any signed-in caller. The list query took the caller-supplied
// tier and applied it as a bare `visibility = $1` with no author or
// relationship conjunct, while the single-item gate did the real work in
// Go. Two expressions of one rule; one of them was wrong, and the
// comments on both asserted the other one's behaviour.
//
// The fix was one expression. It lived HERE, as an unexported readRule,
// which fixed the three post surfaces and left search composing
// visibility.Filter's coarser EntityPost branch — so an org-only post
// you could read while browsing did not exist as far as /search,
// /search/facets and /search/suggest were concerned (#873). The rule was
// package-private, so those three could not have called it even if they
// had wanted to.
//
// So it moved to where ADR 0063 says the read rule lives:
// visibility.postReadableExpr, rendered by visibility.Predicate.ToSQL.
// This file is now the posts-side adapter — it turns an *auth.Identity
// into the (Caller, PostCaps) the shared package takes — plus the
// single-item gate. There is no post read rule in this package any more,
// and that is the property #665 is about: a rule that CAN be written
// twice eventually is.
//
// The fragment feeds every post read path — ListPostsPageGated (the
// browse feed), ListPostsByAssetGated, postReadable (the single-item
// gate), and now the three search surfaces — so the list can never
// become wider than GET /posts/{id} the way it did in #660. That
// agreement is asserted directly by
// TestListPosts_NeverExceedsSingleItemGate, which enumerates the tiers
// from the database's own CHECK constraint so a tier added later is
// covered without anyone remembering to extend a list.

// readRuleSQL renders the post read rule for one caller as a SQL
// WHERE-fragment against `alias` ("" for an un-aliased FROM). The
// fragment always begins with " AND (…)", so callers concatenate it into
// an existing WHERE clause with no pre-processing, and it binds its
// placeholders starting at argOffset+1 — the shared package's contract,
// unchanged.
//
// Soft-delete is NOT in the fragment, and IncludeSoftDeleted is how that
// is stated to the shared package rather than an oversight. It is an
// orthogonal axis owned by each caller here: the list path has an
// admin-only include_deleted flag it applies as its own conjunct, and
// ListPostsByAssetGated and postReadable filter `deleted_at IS NULL`
// unconditionally. The option waives ONLY the soft-delete conjunct, so
// the admin trash view still gets the authorization disjunction in full
// — it cannot accidentally waive a read conjunct along with the
// soft-delete one.
//
// A nil or anonymous identity takes the anonymous branch (public tier
// only), reachable today via /posts/by-asset, the one posts route on the
// public-mode allowlist (auth.publicmode).
func readRuleSQL(
	ctx context.Context,
	id *auth.Identity,
	alias string,
	argOffset int,
) (fragment string, args []any, err error) {
	caller := visibility.NewCaller(nil)
	var caps visibility.PostCaps
	if id != nil && !id.IsAnonymous() {
		ref := id.UserRef
		caller = visibility.NewCaller(&ref)
		caps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
	}
	pred, err := visibility.Filter(ctx, visibility.EntityPost, caller,
		visibility.IncludeSoftDeleted(), visibility.WithPostCaps(caps))
	if err != nil {
		return "", nil, fmt.Errorf("posts: read rule: %w", err)
	}
	frag, fragArgs := pred.ToSQL(alias, argOffset)
	return frag, fragArgs, nil
}

// postReadable reports whether the caller may read one post, by id. It
// runs the rule's own fragment as an EXISTS probe, so the single-item
// answer is produced by the same SQL the list path filters with —
// agreement by construction rather than by two implementations kept in
// step by hand.
//
// Returns (false, nil) both for "row hidden" and "row missing": callers
// map both to their own 403/404 choice, and the two collapsing here
// keeps this helper enumeration-safe by default.
//
// Soft-deleted rows are excluded — a deleted post is not readable
// content on either path.
func (h *Handler) postReadable(ctx context.Context, id *auth.Identity, postID uuid.UUID) (bool, error) {
	frag, args, err := readRuleSQL(ctx, id, "", 1)
	if err != nil {
		return false, err
	}
	sql := "SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL" + frag + ")"
	var ok bool
	if err := h.Pool.QueryRow(ctx, sql, append([]any{postID}, args...)...).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("posts: read gate: %w", err)
	}
	return ok, nil
}
