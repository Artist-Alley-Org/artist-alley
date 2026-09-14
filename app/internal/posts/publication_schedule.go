// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// ---------------------------------------------------------------------------
// Scheduled publication, the author's half (#1119 sprint 21e)
// ---------------------------------------------------------------------------
//
// An author scheduling their own post is the same instruction #1238
// gave operators, a `change_state` action on a post fired through
// MovePostPublication, reached through a different door. The door is
// the scheduled-action package's author HTTP seam; what it asks THIS
// package is the one question only the publication core can answer:
// may this caller schedule this post's publication right now?
//
// It is asked HERE and not re-derived beside the table because every
// gate in it already exists in this file's neighbour, publication.go,
// and a second spelling of "who may publish" is how the two surfaces
// come to disagree. The ordering is the endpoints' own: read gate first
// (a 404 that says nothing about whether the id exists), then
// authorship, then the instance's `posts.publish` policy, then the
// post's current state.
//
// Two of those gates are NEW to the author surface and are not the
// generic engine's behaviour: only the author (not a posts.admin who
// could publish by hand) may schedule, and only a current draft may be
// scheduled. Both are decided at schedule time so a refusal arrives
// while the author is watching (#1238's own reason for validating
// pairs at insert). They do not replace the fire-time gates: the
// action still publishes through movePublicationAs with a freshly
// loaded identity, and a capability revoked in between stops it there.

// PublicationScheduleGate decides whether caller may act on the
// author publication schedule of postID.
//
// forCreate=true is the gate for MAKING a schedule and applies all
// four checks. forCreate=false is the gate for READING or CANCELLING
// one and stops after authorship: an author whose `posts.publish` was
// revoked after scheduling must still be able to see and withdraw
// their instruction, and a post that has meanwhile been published by
// hand still has a schedule row its author may want to clear.
func (h *Handler) PublicationScheduleGate(
	ctx context.Context,
	caller *auth.Identity,
	postID uuid.UUID,
	forCreate bool,
) (status int, message string, err error) {
	if caller == nil || caller.IsAnonymous() {
		return 401, "authentication required", nil
	}

	// READ GATE FIRST, same as movePublicationAs and for the same
	// reason: an unreadable post and a nonexistent one answer alike.
	// The rule excludes soft-deleted rows and admits a draft to its
	// author.
	readable, err := h.postReadable(ctx, caller, postID)
	if err != nil {
		return 0, "", fmt.Errorf("posts: publication schedule read gate: %w", err)
	}
	if !readable {
		return 404, "post not found", nil
	}

	pgID := pgtype.UUID{Bytes: postID, Valid: true}
	cur, err := New(h.Pool).GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 404, "post not found", nil
		}
		return 0, "", fmt.Errorf("posts: publication schedule load: %w", err)
	}

	// AUTHORSHIP, and strictly authorship. canWidenPostAccess would
	// also admit a global posts.admin, who may publish by hand; a
	// standing instruction that publishes in somebody's name and
	// federates as them is the author's to give, not a curator's.
	if caller.UserRef == 0 || cur.AuthorUserRef == 0 || caller.UserRef != cur.AuthorUserRef {
		return 403, "scheduling publication is reserved to the post's author", nil
	}
	if !forCreate {
		return 200, "", nil
	}

	// INSTANCE POLICY. The same capability the wip -> published edge
	// carries and CreatePost checks for a born-published post; checked
	// now so an author who cannot publish today is told so today,
	// rather than by a failed action next week. It is checked AGAIN at
	// fire time by the workflow service, against the database as it is
	// then.
	if !caller.Can(CapPostsPublish) {
		return 403, "publishing requires the posts.publish capability", nil
	}

	// CURRENT DRAFT ONLY. A published post has nothing to schedule;
	// #1238's "already published" no-op cell exists for a race the
	// schedule lost, not for an instruction that was pointless when
	// given.
	states, err := h.publicationStates(ctx)
	if err != nil {
		return 0, "", err
	}
	if !isDraftState(cur.StateID, states.published) {
		return 409, "post is already published", nil
	}
	return 200, "", nil
}
