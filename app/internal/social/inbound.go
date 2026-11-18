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

// InboundAuthorizer decides, inside the write transaction tx, whether an
// inbound write may happen (#1570): the target exists and a live share
// admits the sender. It must hold what it relied on until tx ends, so a
// concurrent revoke or delete either committed first (and it refuses) or
// waits for this write. A non-nil error means no write. Boot wires it to
// shares.Registry.AuthorizeInboundWrite.
type InboundAuthorizer func(ctx context.Context, tx pgx.Tx) error

// ErrNoAuthorizer: an inbound write was attempted without an authorizer.
// Fail closed: nothing is written.
var ErrNoAuthorizer = errors.New("social: inbound write without an authorizer")

// ErrRemoteParentNotFound: an inbound reply names a parent comment that
// is not a live comment on the same target. The reply is refused, never
// re-parented to top level.
var ErrRemoteParentNotFound = errors.New("social: inbound reply parent is not a live comment on the target")

// InsertRemoteLike persists an inbound Like from a remote actor, in one
// transaction with its authorisation, and after commit fires the
// post-author notification (same payload shape the local LikePost
// handler uses).
//
// Returns true when a new row was inserted, false on idempotent no-op
// (retried delivery: same actor and target). An authorize error is
// returned as is and nothing is written or notified.
func (h *Handler) InsertRemoteLike(
	ctx context.Context,
	targetKind string,
	targetID uuid.UUID,
	peerID uuid.UUID,
	actorURI string,
	authorize InboundAuthorizer,
) (bool, error) {
	if authorize == nil {
		return false, ErrNoAuthorizer
	}
	var inserted bool
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx); err != nil {
			return err
		}
		res, err := New(tx).InsertRemoteLike(ctx, InsertRemoteLikeParams{
			TargetKind: targetKind,
			TargetID:   pgtype.UUID{Bytes: targetID, Valid: true},
			PeerID:     pgtype.UUID{Bytes: peerID, Valid: true},
			ActorUri:   ptrStr(actorURI),
		})
		if err != nil {
			return fmt.Errorf("insert remote like: %w", err)
		}
		inserted = res > 0
		return nil
	})
	if err != nil {
		return false, err
	}

	// Fire post-author notification (only for posts; assets +
	// other targets get added when their phases land). After the
	// commit: a refused or rolled-back write notifies nobody.
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
	TargetKind string
	TargetID   uuid.UUID
	ParentID   *uuid.UUID
	// ParentActivityURI names the parent by the Create activity id of a
	// federated comment (#1604), looked up among this target's comments.
	ParentActivityURI string
	PeerID            uuid.UUID
	ActorURI          string
	ActivityURI       string
	Body              string
	// Authorize runs inside the write transaction (#1570); nil refuses.
	Authorize InboundAuthorizer
}

// InsertRemoteComment persists an inbound Create(Note) from a remote
// actor as a comment, in one transaction with its authorisation, and
// after commit fires the post-author notification.
//
// Idempotent via activity_uri UNIQUE — a retried delivery of an
// activity already saved returns (existing.id, true, nil) without a
// second notification. A reply's parent must be a live comment on the
// same target (ErrRemoteParentNotFound otherwise): a missing parent is
// refused, never re-parented to top level.
func (h *Handler) InsertRemoteComment(ctx context.Context, in RemoteCommentInput) (commentID uuid.UUID, alreadyExisted bool, err error) {
	if in.Authorize == nil {
		return uuid.UUID{}, false, ErrNoAuthorizer
	}
	// Idempotency: check the activity_uri UNIQUE constraint
	// FIRST so a retry doesn't re-fire the notification.
	existing, err := New(h.Pool).GetCommentByActivityURI(ctx, ptrStr(in.ActivityURI))
	if err == nil {
		return uuid.UUID(existing.ID.Bytes), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.UUID{}, false, err
	}

	newID := uuid.New()
	var rowID uuid.UUID
	err = pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := in.Authorize(ctx, tx); err != nil {
			return err
		}
		// Resolve parent_id / root_id / depth. Top-level comments
		// have parent_id NULL + root_id = self + depth 0. A parent is
		// read FOR SHARE: it must be a live comment on this target
		// until the reply commits.
		rootID := newID
		depth := int32(0)
		parentPG := pgtype.UUID{}
		if in.ParentID != nil || in.ParentActivityURI != "" {
			var parentID, parentRoot pgtype.UUID
			var parentDepth int32
			var err error
			if in.ParentID != nil {
				err = tx.QueryRow(ctx, `SELECT id, root_id, depth FROM comments
					WHERE id = $1 AND target_kind = $2 AND target_id = $3 AND deleted_at IS NULL
					FOR SHARE`, *in.ParentID, in.TargetKind, in.TargetID).Scan(&parentID, &parentRoot, &parentDepth)
			} else {
				err = tx.QueryRow(ctx, `SELECT id, root_id, depth FROM comments
					WHERE activity_uri = $1 AND target_kind = $2 AND target_id = $3 AND deleted_at IS NULL
					FOR SHARE`, in.ParentActivityURI, in.TargetKind, in.TargetID).Scan(&parentID, &parentRoot, &parentDepth)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRemoteParentNotFound
			}
			if err != nil {
				return err
			}
			parentPG = parentID
			rootID = uuid.UUID(parentRoot.Bytes)
			depth = parentDepth + 1
		}
		row, err := New(tx).InsertRemoteComment(ctx, InsertRemoteCommentParams{
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
			return fmt.Errorf("insert remote comment: %w", err)
		}
		rowID = uuid.UUID(row.ID.Bytes)
		return nil
	})
	if err != nil {
		return uuid.UUID{}, false, err
	}

	// Fire post-author notification, after the commit.
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
	return rowID, false, nil
}

func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
