// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Inbound-federation domain writes — InsertRemoteLike +
// InsertRemoteComment. Phase 1.22.D-a-4-dispatch.
//
// These methods implement the inbox.SocialPoster contract +
// fire the same notification path the local Like / Comment
// handlers use. The Notifier accepts a nullable actor user_ref
// (per the existing contract) which is the natural slot for
// remote authorship — passing nil means "actor is remote";
// the notification renderer surfaces the display name from the
// federation_remote_actors cache (1.22.D-a-6 UI work) rather
// than a local user lookup.

package social

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PostTargetLookup is the minimal "who owns this post?" lookup
// the inbound Like / Comment handlers need to fire the post-
// author notification. Boot wires it to a closure over the
// posts package (posts.Handler.AuthorRefForPost-equivalent) so
// we don't pull in the whole package.
type PostTargetLookup func(ctx context.Context, postID uuid.UUID) (authorUserRef int64, found bool, err error)

// SetPostTargetLookup wires the cross-package author lookup.
// nil-safe: inbound notifications skip when not wired.
func (h *Handler) SetPostTargetLookup(fn PostTargetLookup) { h.postTargetLookup = fn }

// InsertRemoteLike persists an inbound Like from a remote actor
// + fires the post-author notification (same payload shape the
// local LikePost handler uses).
//
// Returns true when a new row was inserted, false on idempotent
// no-op (retried delivery — same actor+target).
//
// Errors:
//   - "target not found" if the target object doesn't exist
//     locally — caller maps to §12.1 unknown_object.
//   - foreign key violation on peer_id if the peer row vanished
//     mid-flight — caller treats as transient + retries.
func (h *Handler) InsertRemoteLike(
	ctx context.Context,
	targetKind string,
	targetID uuid.UUID,
	peerID uuid.UUID,
	actorURI string,
) (bool, error) {
	// Confirm the target exists. We could just attempt the
	// INSERT + catch the FK error, but the likes table has no
	// FK to posts/assets — so a missing target would land a
	// row pointing at a phantom. Explicit existence check
	// keeps the data honest.
	if err := h.verifyTargetExists(ctx, targetKind, targetID); err != nil {
		return false, err
	}

	res, err := New(h.Pool).InsertRemoteLike(ctx, InsertRemoteLikeParams{
		TargetKind: targetKind,
		TargetID:   pgtype.UUID{Bytes: targetID, Valid: true},
		PeerID:     pgtype.UUID{Bytes: peerID, Valid: true},
		ActorUri:   ptrStr(actorURI),
	})
	if err != nil {
		return false, fmt.Errorf("insert remote like: %w", err)
	}
	inserted := res > 0

	// Fire post-author notification (only for posts; assets +
	// other targets get added when their phases land).
	if inserted && targetKind == "post" && h.notifier != nil && h.postTargetLookup != nil {
		if authorRef, found, err := h.postTargetLookup(ctx, targetID); err == nil && found {
			// actor is REMOTE — pass nil for the actor int64.
			// Notification renderer pulls display name from
			// federation_remote_actors via actor_uri payload.
			payload := map[string]any{
				"target_kind":   targetKind,
				"target_id":     targetID.String(),
				"actor_uri":     actorURI,
				"actor_peer_id": peerID.String(),
				"is_remote":     true,
			}
			_ = h.notifier.Notify(ctx, authorRef, nil, "post.like", targetKind, targetID.String(), payload)
		}
	}
	return inserted, nil
}

// RemoteCommentInput is the typed argument for InsertRemoteComment.
// Mirrors inbox.RemoteCommentInput field-for-field — duplicated
// here because social can't import inbox (the inbox dispatcher
// already calls back through social.SocialPoster; reversing the
// edge would create a cycle).
type RemoteCommentInput struct {
	TargetKind  string
	TargetID    uuid.UUID
	ParentID    *uuid.UUID
	PeerID      uuid.UUID
	ActorURI    string
	ActivityURI string
	Body        string
}

// InsertRemoteComment persists an inbound Create(Note) from a
// remote actor as a comment + fires the post-author notification.
//
// Idempotent via activity_uri UNIQUE — a retried delivery
// returns (existing.id, true, nil) so the dispatcher can mark
// the inbox row processed without firing notifications a
// second time.
func (h *Handler) InsertRemoteComment(ctx context.Context, in RemoteCommentInput) (commentID uuid.UUID, alreadyExisted bool, err error) {
	// Idempotency: check the activity_uri UNIQUE constraint
	// FIRST so a retry doesn't re-fire the notification.
	existing, err := New(h.Pool).GetCommentByActivityURI(ctx, ptrStr(in.ActivityURI))
	if err == nil {
		return uuid.UUID(existing.ID.Bytes), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.UUID{}, false, err
	}
	if err := h.verifyTargetExists(ctx, in.TargetKind, in.TargetID); err != nil {
		return uuid.UUID{}, false, err
	}

	// Resolve parent_id / root_id / depth. Top-level comments
	// have parent_id NULL + root_id = self + depth 0.
	newID := uuid.New()
	rootID := newID
	depth := int32(0)
	parentPG := pgtype.UUID{}
	if in.ParentID != nil {
		parentPG = pgtype.UUID{Bytes: *in.ParentID, Valid: true}
		parent, err := New(h.Pool).GetComment(ctx, parentPG)
		if err != nil {
			// Parent not found — treat as a top-level comment
			// rather than failing. Sender's view may have
			// included a reply target we don't have a row for
			// (post was deleted, e.g.).
			parentPG = pgtype.UUID{}
		} else {
			rootID = uuid.UUID(parent.RootID.Bytes)
			depth = parent.Depth + 1
		}
	}

	row, err := New(h.Pool).InsertRemoteComment(ctx, InsertRemoteCommentParams{
		ID:          pgtype.UUID{Bytes: newID, Valid: true},
		TargetKind:  in.TargetKind,
		TargetID:    pgtype.UUID{Bytes: in.TargetID, Valid: true},
		ParentID:    parentPG,
		RootID:      pgtype.UUID{Bytes: rootID, Valid: true},
		Depth:       depth,
		Body:        in.Body,
		BodyHtml:    "", // local CreateComment passes "" too; renderer reads body, not body_html
		PeerID:      pgtype.UUID{Bytes: in.PeerID, Valid: true},
		ActorUri:    ptrStr(in.ActorURI),
		ActivityUri: ptrStr(in.ActivityURI),
	})
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("insert remote comment: %w", err)
	}

	// Fire post-author notification.
	if in.TargetKind == "post" && h.notifier != nil && h.postTargetLookup != nil {
		if authorRef, found, err := h.postTargetLookup(ctx, in.TargetID); err == nil && found {
			payload := map[string]any{
				"target_kind":   in.TargetKind,
				"target_id":     in.TargetID.String(),
				"comment_id":    newID.String(),
				"actor_uri":     in.ActorURI,
				"actor_peer_id": in.PeerID.String(),
				"is_remote":     true,
			}
			_ = h.notifier.Notify(ctx, authorRef, nil, "post.comment", in.TargetKind, in.TargetID.String(), payload)
		}
	}
	return uuid.UUID(row.ID.Bytes), false, nil
}

// verifyTargetExists is a cheap exists-check for the inbound
// handlers' "target not found" failure mode. Currently only
// supports post targets — extends with other kinds as their
// phases land.
func (h *Handler) verifyTargetExists(ctx context.Context, targetKind string, targetID uuid.UUID) error {
	switch targetKind {
	case "post":
		var n int
		err := h.Pool.QueryRow(ctx,
			`SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL`,
			targetID,
		).Scan(&n)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("target not found")
			}
			return err
		}
		return nil
	}
	// Other target kinds: assume exists (extends per phase).
	return nil
}

func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
