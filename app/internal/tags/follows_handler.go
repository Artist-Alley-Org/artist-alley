// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package tags owns the tag-follow bookmark model (#1123).
//
// # A follow is a bookmark
//
// Nothing in this package participates in authorization. Following
// #fantasy does not widen a single row of what the caller can see, and
// no visibility plane consults `tag_follows`. What it does is put a `#`
// chip in the reader's browse rail and add a third source to the
// Following feed — a NARROWING conjunct ANDed beside the read rule in
// posts.ListPostsPageGated, never a disjunct with it.
//
// That sentence carries more weight here than it does for teams, and
// migration 00050 spells out why: a tag's right-hand side is
// ATTACKER-CHOSEN. Anyone can tag their own restricted post `fantasy`.
// If the Following feed's third EXISTS were ever ORed into the read
// rule, tagging a private post with a popular tag would publish it to
// every follower of that tag. It is ANDed. Do not "simplify" that.
//
// # Why there is no capability gate beyond authentication
//
// Team follows require `teams.read`, because that capability is what
// lets the caller see a team at all and a bookmark of an invisible
// thing is incoherent. Tags have no such capability and need none: a
// tag is a free string, following one reveals nothing about the corpus
// (the write path deliberately never confirms whether the tag exists —
// see 00050), and the feed the follow feeds is already gated by the
// post read rule. So the gate is "is there a caller", which is the
// honest floor, rather than a capability borrowed from a neighbouring
// domain for the look of it.
//
// # No counts, no unread, no notifications
//
// As team follows. A follow is a bookmark, not a subscription; #520's
// arc owns notifications and picks its own schema.
package tags

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// maxTagLen mirrors the CHECK constraint in migration 00050. Kept in
// both places on purpose: the constraint is what makes a pathological
// write impossible, and this is what turns it into a 400 instead of a
// 500. If they ever disagree, the database wins and the caller gets an
// opaque error — so they must not disagree.
const maxTagLen = 200

// Handler serves the tag-follow endpoints.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// NewHandler builds the handler. No cache registry: the followed-tag
// list is one indexed read of a handful of short rows on the PK, and a
// cache would exist only to be invalidated by the two endpoints beside
// it.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Logger: logger}
}

// normalizeTag applies the corpus's own rule and nothing more.
//
// `posts.dedupeTags` stores tags TRIMMED and otherwise verbatim, and
// `ListPostsPageGated` filters with `pt.tag = $5::TEXT` — an exact
// match. So trimming is the whole normalisation, and lowercasing here
// would be a bug rather than a tidy-up: a reader following `fantasy`
// would get a rail chip whose `?tag=` filter finds nothing while their
// Following feed fills with `Fantasy` posts. The chip and the feed have
// to agree, and the only way to agree with an exact-match corpus is to
// match it exactly.
//
// Case-folding the corpus is #789's decision, to be made once for
// post_tags, the rail, search and this table together.
func normalizeTag(raw string) string {
	return strings.TrimSpace(raw)
}

// FollowTag bookmarks a tag into the caller's browse rail.
//
// # Idempotent
//
// A tag already followed is a 204, not a 409 — the insert is ON
// CONFLICT DO NOTHING, so the second press of a double-tapped button
// and a retried request produce the outcome the caller asked for.
//
// # No liveness probe, and that is the security-relevant half
//
// FollowTeam probes that its target is live before writing, because a
// soft-deleted team satisfies the FK. There is no analogue here and
// there must not be one: a tag is not a row, so the only "does this
// exist" question available is "does any post carry it" — asked against
// a corpus that spans posts the caller cannot read. Answering it would
// turn this endpoint into an oracle enumerating the tags of private
// work one guess at a time. So the write is unconditional, and a follow
// of a tag nobody uses is inert until somebody does.
func (h *Handler) FollowTag(
	ctx context.Context,
	req openapi.FollowTagRequestObject,
) (openapi.FollowTagResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.FollowTag401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	tag := normalizeTag(req.Tag)
	if tag == "" || len(tag) > maxTagLen {
		return openapi.FollowTag400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "tag must be 1-200 characters"},
		}, nil
	}

	if err := New(h.Pool).FollowTag(ctx, FollowTagParams{
		UserRef: caller.UserRef,
		Tag:     tag,
	}); err != nil {
		return nil, fmt.Errorf("tags: follow: %w", err)
	}
	return openapi.FollowTag204Response{}, nil
}

// UnfollowTag drops the caller's bookmark.
//
// Idempotent for the same reason follow is: unfollowing something you
// do not follow has already achieved what you asked for. The tag is
// trimmed on the way in so that a follow and its unfollow normalise to
// the same key — otherwise a client that round-trips the tag through a
// field with trailing whitespace could write a row it can never delete.
//
// Deliberately NO length refusal here, unlike the write path. An
// over-long tag cannot be in the table (the CHECK constraint saw to
// that), so the DELETE matches nothing and 204s — which is the correct
// answer to "make sure I am not following this" and one query cheaper
// than reasoning about it.
func (h *Handler) UnfollowTag(
	ctx context.Context,
	req openapi.UnfollowTagRequestObject,
) (openapi.UnfollowTagResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UnfollowTag401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	if _, err := New(h.Pool).UnfollowTag(ctx, UnfollowTagParams{
		UserRef: caller.UserRef,
		Tag:     normalizeTag(req.Tag),
	}); err != nil {
		return nil, fmt.Errorf("tags: unfollow: %w", err)
	}
	return openapi.UnfollowTag204Response{}, nil
}

// GetMyFollowedTags returns the caller's followed tags for the rail.
func (h *Handler) GetMyFollowedTags(
	ctx context.Context,
	req openapi.GetMyFollowedTagsRequestObject,
) (openapi.GetMyFollowedTagsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetMyFollowedTags401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	rows, err := New(h.Pool).ListFollowedTags(ctx, caller.UserRef)
	if err != nil {
		return nil, fmt.Errorf("tags: list followed tags: %w", err)
	}

	// Built with an explicit length so an empty follow set marshals as
	// `[]` and not `null`. The rail's client store spreads this into an
	// array on every read; `null` would make that the one payload that
	// throws instead of rendering an empty strip.
	items := make([]openapi.FollowedTag, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.FollowedTag{
			Tag:        r.Tag,
			FollowedAt: r.CreatedAt.Time,
		})
	}
	return openapi.GetMyFollowedTags200JSONResponse(items), nil
}
