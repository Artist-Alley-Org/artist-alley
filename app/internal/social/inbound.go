// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Inbound-federation domain writes: InsertRemoteLike, UndoRemoteLike,
// InsertRemoteComment, DeleteRemoteComment and UpdateRemoteComment
// (#1572). Phase 1.22.D-a-4-dispatch.
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
	"time"

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

// ErrNoLikeActivity: an inbound Like without its activity id. Every
// Like is bound to its id (#1572), so one without it is never written.
var ErrNoLikeActivity = errors.New("social: inbound Like without its activity id")

// lockRemoteActor serialises, until tx ends, every inbound Like and Undo
// of one remote actor of one peer (#1572). Both take it FIRST, before any
// row lock (the Like's authorisation locks the target, the peer and the
// shares FOR SHARE; the likes counter trigger updates the target), so the
// two can never wait on each other in opposite orders. Serialising per
// (peer, actor) covers every (target, peer, actor) that actor touches. A
// hash collision between two actors only serialises them too.
func lockRemoteActor(ctx context.Context, tx pgx.Tx, peerID uuid.UUID, actorURI string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ' ' || $2, 1572))`,
		peerID, actorURI); err != nil {
		return fmt.Errorf("lock remote actor: %w", err)
	}
	return nil
}

// InsertRemoteLike persists an inbound Like from a remote actor, in one
// transaction with its authorisation, and after commit fires the
// post-author notification (same payload shape the local LikePost
// handler uses).
//
// The Like is bound to its activity id (#1572): one federation_remote_
// likes record per (peer, actor, activityURI). The aggregate likes row
// for (target, peer, actor) exists while at least one of that actor's
// records for the target is live, so a second, distinct Like of the
// same target by the same actor counts once. A Like whose Undo arrived
// first (a tombstone with the same peer, actor and id) is recorded
// undone: no like row, no notification.
//
// Returns true when the aggregate like row was created, false otherwise
// (already liked, retracted on arrival, or a repeat of a recorded id).
// An authorize error is returned as is and nothing is written or
// notified.
func (h *Handler) InsertRemoteLike(
	ctx context.Context,
	targetKind string,
	targetID uuid.UUID,
	peerID uuid.UUID,
	actorURI string,
	activityURI string,
	authorize InboundAuthorizer,
) (bool, error) {
	if authorize == nil {
		return false, ErrNoAuthorizer
	}
	if activityURI == "" {
		return false, ErrNoLikeActivity
	}
	var inserted bool
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := lockRemoteActor(ctx, tx, peerID, actorURI); err != nil {
			return err
		}
		if err := authorize(ctx, tx); err != nil {
			return err
		}
		var binding string
		err := tx.QueryRow(ctx, `SELECT binding FROM federation_remote_likes
			WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3 FOR UPDATE`,
			peerID, actorURI, activityURI).Scan(&binding)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_likes
				(peer_id, actor_uri, activity_uri, target_kind, target_id, binding)
				VALUES ($1, $2, $3, $4, $5, 'received')
				ON CONFLICT (peer_id, actor_uri, activity_uri) DO NOTHING`,
				peerID, actorURI, activityURI, targetKind, targetID); err != nil {
				return fmt.Errorf("record remote like: %w", err)
			}
		case err != nil:
			return fmt.Errorf("read remote like record: %w", err)
		case binding == "tombstone":
			// Undone before it arrived: record the target, stay undone.
			if _, err := tx.Exec(ctx, `UPDATE federation_remote_likes
				SET binding = 'received', target_kind = $4, target_id = $5
				WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3`,
				peerID, actorURI, activityURI, targetKind, targetID); err != nil {
				return fmt.Errorf("record retracted remote like: %w", err)
			}
			return nil
		default:
			// This id is already recorded: nothing changes.
			return nil
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

// RemoteUndoLikeInput is the typed argument for UndoRemoteLike: the
// authenticated peer and actor of the Undo, the Like activity id it
// names (its object) and its own id.
type RemoteUndoLikeInput struct {
	PeerID          uuid.UUID
	ActorURI        string
	LikeActivityURI string
	UndoActivityURI string
}

// UndoRemoteLike applies an inbound Undo of a Like (#1572): the actor
// retracting its own earlier Like. It needs no share and no target, so it
// applies after a revoke, an expiry or the target's deletion.
//
// The Undo matches only the record with the same peer, actor and Like id.
// In one transaction, serialised with every Like and Undo of that actor:
//
//   - a live record is marked undone; when it was the actor's last live
//     record for its target, the aggregate like row is deleted (the
//     counter trigger decrements the count). Notifications already sent
//     are kept, as for a local unlike.
//   - an undone record (a repeated or second Undo) changes nothing.
//   - no record (the Like has not arrived, was never sent, or belongs to
//     another actor or peer) stores a tombstone for this peer, actor and
//     id, so a later matching Like is recorded undone.
//
// Every case is processed the same way, so the outcome never tells the
// sender whether another actor's Like exists. Returns whether the
// aggregate like row was removed. Any error is a database failure: it
// writes nothing and is retryable.
func (h *Handler) UndoRemoteLike(ctx context.Context, in RemoteUndoLikeInput) (bool, error) {
	if in.LikeActivityURI == "" || in.UndoActivityURI == "" || in.ActorURI == "" {
		return false, ErrNoLikeActivity
	}
	var removed bool
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := lockRemoteActor(ctx, tx, in.PeerID, in.ActorURI); err != nil {
			return err
		}
		// The binding: the record with this Undo's own peer, actor and
		// named Like id. Everything below acts on that record only.
		var (
			recActor   string
			binding    string
			targetKind *string
			targetID   pgtype.UUID
			undone     bool
		)
		err := tx.QueryRow(ctx, `SELECT actor_uri, binding, target_kind, target_id, undone_at IS NOT NULL
			FROM federation_remote_likes
			WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3 FOR UPDATE`,
			in.PeerID, in.ActorURI, in.LikeActivityURI).Scan(&recActor, &binding, &targetKind, &targetID, &undone)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_likes
				(peer_id, actor_uri, activity_uri, binding, undone_at, undo_activity_uri)
				VALUES ($1, $2, $3, 'tombstone', now(), $4)`,
				in.PeerID, in.ActorURI, in.LikeActivityURI, in.UndoActivityURI); err != nil {
				return fmt.Errorf("record undo tombstone: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("read remote like record: %w", err)
		case undone || binding == "legacy_unbound" || targetKind == nil:
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_remote_likes
			SET undone_at = now(), undo_activity_uri = $4
			WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3`,
			in.PeerID, recActor, in.LikeActivityURI, in.UndoActivityURI); err != nil {
			return fmt.Errorf("undo remote like: %w", err)
		}
		var stillLive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_remote_likes
			WHERE target_kind = $1 AND target_id = $2 AND peer_id = $3 AND actor_uri = $4
			  AND undone_at IS NULL)`,
			*targetKind, targetID, in.PeerID, recActor).Scan(&stillLive); err != nil {
			return fmt.Errorf("read live remote likes: %w", err)
		}
		if stillLive {
			return nil
		}
		tag, err := tx.Exec(ctx, `DELETE FROM likes
			WHERE target_kind = $1 AND target_id = $2 AND peer_id = $3 AND actor_uri = $4`,
			*targetKind, targetID, in.PeerID, recActor)
		if err != nil {
			return fmt.Errorf("remove remote like: %w", err)
		}
		removed = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// RemoteDeleteCommentInput is the typed argument for DeleteRemoteComment:
// the Delete's authenticated peer and actor, the comment's Create id it
// names (its object) and its own id.
type RemoteDeleteCommentInput struct {
	PeerID            uuid.UUID
	ActorURI          string
	CreateActivityURI string
	DeleteActivityURI string
}

// DeleteRemoteComment applies an inbound Delete of a comment (#1572): the
// actor retracting its own earlier comment. It needs no share, so it
// applies after a revoke, an expiry or the target's deletion.
//
// The Delete matches only the comment with the same peer, actor and
// Create id (comments.peer_id, actor_uri, activity_uri), never the Create
// id alone. In one transaction, serialised with every inbound Create,
// Like, Undo and Delete of that actor (lockRemoteActor, taken first):
//
//   - a live comment is soft-deleted exactly as a local delete does
//     (SoftDeleteComment: body cleared, deleted_at set, the target's
//     comment_count decremented by its trigger). Notifications already
//     sent are kept, and replies stay, as for a local delete; a new reply
//     to it is refused (its parent is no longer live).
//   - a comment already deleted (by the sender or by a local moderator)
//     changes nothing.
//   - no such comment (the Create has not arrived, was never sent, or
//     belongs to another actor or peer) stores a tombstone for this peer,
//     actor and Create id, so a matching Create that arrives later
//     creates nothing.
//
// Every case is processed the same way, so the outcome never tells the
// sender whether another actor's comment exists. Returns whether a live
// comment was soft-deleted. Any error is a database failure: it writes
// nothing and is retryable.
func (h *Handler) DeleteRemoteComment(ctx context.Context, in RemoteDeleteCommentInput) (bool, error) {
	if in.CreateActivityURI == "" || in.DeleteActivityURI == "" || in.ActorURI == "" {
		return false, ErrNoCommentActivity
	}
	var (
		deleted    bool
		targetKind string
		targetID   pgtype.UUID
	)
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := lockRemoteActor(ctx, tx, in.PeerID, in.ActorURI); err != nil {
			return err
		}
		// A deleted comment is never brought back by an edit (#1572): any
		// edit still waiting for its Create goes with the comment.
		if _, err := tx.Exec(ctx, `DELETE FROM federation_remote_comment_pending_edits
			WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3`,
			in.PeerID, in.ActorURI, in.CreateActivityURI); err != nil {
			return fmt.Errorf("discard pending comment edit: %w", err)
		}
		var id pgtype.UUID
		var already bool
		err := tx.QueryRow(ctx, `SELECT id, target_kind, target_id, deleted_at IS NOT NULL FROM comments
			WHERE activity_uri = $1 AND peer_id = $2 AND actor_uri = $3 FOR UPDATE`,
			in.CreateActivityURI, in.PeerID, in.ActorURI).Scan(&id, &targetKind, &targetID, &already)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_comment_tombstones
				(peer_id, actor_uri, create_activity_uri, delete_activity_uri)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (peer_id, actor_uri, create_activity_uri) DO NOTHING`,
				in.PeerID, in.ActorURI, in.CreateActivityURI, in.DeleteActivityURI); err != nil {
				return fmt.Errorf("record comment tombstone: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("read remote comment: %w", err)
		case already:
			return nil
		}
		rows, err := New(tx).SoftDeleteComment(ctx, id)
		if err != nil {
			return fmt.Errorf("soft delete remote comment: %w", err)
		}
		deleted = rows > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	if deleted && targetKind == "post" {
		h.invalidatePostCache(ctx, uuid.UUID(targetID.Bytes).String())
	}
	return deleted, nil
}

// pendingEdit is an edit that arrived before its comment's Create.
type pendingEdit struct {
	uri, content string
	published    time.Time
}

// RemoteUpdateCommentInput is the typed argument for UpdateRemoteComment:
// the Update's authenticated peer and actor, the comment's Create id (its
// object), its own id, the new content and its signed published time.
// Authorize builds the write's authorisation for the comment's target,
// once the comment is found; nil refuses.
type RemoteUpdateCommentInput struct {
	PeerID            uuid.UUID
	ActorURI          string
	CreateActivityURI string
	UpdateActivityURI string
	Content           string
	Published         time.Time
	Authorize         func(targetKind string, targetID uuid.UUID) InboundAuthorizer
}

// UpdateRemoteComment applies an inbound edit of a comment (#1572): the
// actor changing its own comment's text. It is bound like a Delete, to
// the comment with the same peer, actor and Create id, never the Create
// id alone, and serialised with every inbound write of that actor
// (lockRemoteActor, taken first).
//
// Edits are ordered by (published, Update id): the signed published time
// a sender keeps strictly increasing per comment, with the id breaking a
// tie a conforming sender never produces. So every delivery order ends
// on the same text.
//
//   - a live comment takes the content, and edited_at = the edit's
//     published time, when this edit's key is greater than the one it
//     shows; an older or repeated edit changes nothing. Counts and
//     notifications never change.
//   - a deleted comment is never edited: deletion wins, whatever arrives
//     after it.
//   - no such comment: if a Delete of it was recorded (a tombstone), the
//     edit is dropped; otherwise the newest edit is kept for the Create,
//     which is then stored with it.
//
// An edit adds content, so unlike a retraction it needs the share a new
// comment needs: Authorize (the comment-scope check of #1570) runs inside
// the transaction once the target is known. Its refusal is returned as
// is; any other error is a database failure, writes nothing and is
// retryable. Returns whether the comment's text changed.
func (h *Handler) UpdateRemoteComment(ctx context.Context, in RemoteUpdateCommentInput) (bool, error) {
	if in.CreateActivityURI == "" || in.UpdateActivityURI == "" || in.ActorURI == "" || in.Published.IsZero() {
		return false, ErrNoCommentActivity
	}
	published := in.Published.UTC().Truncate(time.Microsecond) // the column's precision
	var (
		applied    bool
		targetKind string
		targetID   pgtype.UUID
	)
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := lockRemoteActor(ctx, tx, in.PeerID, in.ActorURI); err != nil {
			return err
		}
		var (
			id       pgtype.UUID
			deleted  bool
			shownAt  pgtype.Timestamptz
			shownURI string
		)
		err := tx.QueryRow(ctx, `SELECT c.id, c.target_kind, c.target_id, c.deleted_at IS NOT NULL,
				e.published, coalesce(e.update_activity_uri, '')
			FROM comments c LEFT JOIN federation_remote_comment_edits e ON e.comment_id = c.id
			WHERE c.activity_uri = $1 AND c.peer_id = $2 AND c.actor_uri = $3 FOR UPDATE OF c`,
			in.CreateActivityURI, in.PeerID, in.ActorURI).Scan(&id, &targetKind, &targetID, &deleted, &shownAt, &shownURI)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			var tombstoned bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_remote_comment_tombstones
				WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3)`,
				in.PeerID, in.ActorURI, in.CreateActivityURI).Scan(&tombstoned); err != nil {
				return fmt.Errorf("read remote comment tombstone: %w", err)
			}
			if tombstoned {
				return nil
			}
			if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_comment_pending_edits AS p
				(peer_id, actor_uri, create_activity_uri, update_activity_uri, content, published)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (peer_id, actor_uri, create_activity_uri) DO UPDATE
				   SET update_activity_uri = EXCLUDED.update_activity_uri, content = EXCLUDED.content,
				       published = EXCLUDED.published, received_at = now()
				 WHERE (EXCLUDED.published, EXCLUDED.update_activity_uri) > (p.published, p.update_activity_uri)`,
				in.PeerID, in.ActorURI, in.CreateActivityURI, in.UpdateActivityURI, in.Content, published); err != nil {
				return fmt.Errorf("keep pending comment edit: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("read remote comment: %w", err)
		case deleted:
			return nil
		}
		if in.Authorize == nil {
			return ErrNoAuthorizer
		}
		if err := in.Authorize(targetKind, uuid.UUID(targetID.Bytes))(ctx, tx); err != nil {
			return err
		}
		if shownAt.Valid {
			shown := shownAt.Time.UTC()
			if published.Before(shown) || (published.Equal(shown) && in.UpdateActivityURI <= shownURI) {
				return nil // older or repeated: the comment keeps the newer text
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE comments SET body = $2, body_html = '', edited_at = $3, updated_at = now()
			WHERE id = $1`, id, in.Content, published); err != nil {
			return fmt.Errorf("edit remote comment: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_comment_edits (comment_id, update_activity_uri, published)
			VALUES ($1, $2, $3)
			ON CONFLICT (comment_id) DO UPDATE SET update_activity_uri = EXCLUDED.update_activity_uri,
			    published = EXCLUDED.published, applied_at = now()`, id, in.UpdateActivityURI, published); err != nil {
			return fmt.Errorf("record remote comment edit: %w", err)
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if applied && targetKind == "post" {
		h.invalidatePostCache(ctx, uuid.UUID(targetID.Bytes).String())
	}
	return applied, nil
}

// ErrNoCommentActivity: an inbound comment Delete without its ids.
var ErrNoCommentActivity = errors.New("social: inbound comment Delete without its activity ids")

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
// The write takes lockRemoteActor first (#1572), like every inbound Like,
// Undo and Delete of the same actor. A Delete that arrived first left a
// tombstone with this peer, actor and Create id: the Create is then
// processed with no comment row and no notification, reported as
// alreadyExisted with a zero id.
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
	var retracted bool
	var edited *pendingEdit
	err = pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if err := lockRemoteActor(ctx, tx, in.PeerID, in.ActorURI); err != nil {
			return err
		}
		if err := in.Authorize(ctx, tx); err != nil {
			return err
		}
		// Deleted before it arrived (#1572): exactly this peer, actor and
		// Create id. A tombstone from anyone else does not match.
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_remote_comment_tombstones
			WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3)`,
			in.PeerID, in.ActorURI, in.ActivityURI).Scan(&retracted); err != nil {
			return fmt.Errorf("read remote comment tombstone: %w", err)
		}
		if retracted {
			return nil
		}
		// An edit that arrived before this Create (#1572): the comment is
		// stored with that edit's content and order key.
		var pendingURI, pendingContent string
		var pendingPublished time.Time
		err := tx.QueryRow(ctx, `DELETE FROM federation_remote_comment_pending_edits
			WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3
			RETURNING update_activity_uri, content, published`,
			in.PeerID, in.ActorURI, in.ActivityURI).Scan(&pendingURI, &pendingContent, &pendingPublished)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read pending comment edit: %w", err)
		default:
			edited = &pendingEdit{uri: pendingURI, content: pendingContent, published: pendingPublished}
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
		if edited != nil {
			if _, err := tx.Exec(ctx, `UPDATE comments SET body = $2, body_html = '', edited_at = $3 WHERE id = $1`,
				row.ID, edited.content, edited.published); err != nil {
				return fmt.Errorf("apply pending comment edit: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO federation_remote_comment_edits (comment_id, update_activity_uri, published)
				VALUES ($1, $2, $3)`, row.ID, edited.uri, edited.published); err != nil {
				return fmt.Errorf("record pending comment edit: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return uuid.UUID{}, false, err
	}
	if retracted {
		return uuid.UUID{}, true, nil
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
