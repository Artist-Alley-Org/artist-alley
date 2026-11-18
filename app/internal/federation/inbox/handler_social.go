// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// REAL per-verb handlers for Like, Undo of a Like (#1572) and
// Create(Note as comment) per the §5.5 Q1 lock-in. Every other verb is
// rejected as unsupported_activity (registry.go, #1569). Phase
// 1.22.D-a-4-dispatch.
//
// # Authority before any write (#1570)
//
// The SocialPoster authorises each write inside the transaction that
// makes it (boot wires shares.Registry.AuthorizeInboundWrite): the verb
// and target kind are supported, the target exists and is not deleted,
// and a live share of this instance admits this peer's actor at the
// verb's scope (Like: view; comment: comment). It answers with the
// typed ErrUnknownObject or ErrUnsharedObject, which become the row's
// unknown_object / unshared_object reject reasons; any other error is a
// retryable failure and never a write. The sender's answer was given at
// receipt (202, or the stored row's 200 for a duplicate) and does not
// depend on any of this, so no outcome tells an unauthorised peer
// whether a private target exists. The actor cache is refreshed only
// after an authorised write, and notifications fire only after its
// commit.
//
// # Cross-package contract
//
// SocialPoster is the small interface the social package
// implements + this dispatcher consumes. Keeps the import edge
// one-directional (social does not import federation/inbox) so
// the existing notifications cycle (notifications imports
// social) is unaffected.

package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// SocialPoster is the contract the dispatch handlers use to
// reach back into the social package. Boot wires the methods
// to social.Handler equivalents.
type SocialPoster interface {
	// InsertRemoteLike persists an inbound Like from a remote
	// actor on a local target. Idempotent via the partial
	// UNIQUE index — a retried delivery lands as no-op.
	// Returns true when a new row was inserted, false on
	// idempotent no-op. Also fires the local notification
	// for the post author (when the target is a post) — the
	// existing emitter handles the remote-actor display path.
	//
	// activityURI is the Like's signed id: the Like is bound to it
	// (#1572), so a later Undo naming it retracts exactly this Like.
	InsertRemoteLike(ctx context.Context, targetKind string, targetID uuid.UUID, peerID uuid.UUID, actorURI, activityURI string) (inserted bool, err error)

	// UndoRemoteLike applies an inbound Undo of a Like (#1572): it
	// retracts the Like with the same peer, actor and id, or stores a
	// tombstone when there is none. It needs no share. Any error is
	// retryable; it never refuses.
	UndoRemoteLike(ctx context.Context, in RemoteUndoLikeInput) (removed bool, err error)

	// DeleteRemoteComment applies an inbound Delete of a comment
	// (#1572): it soft-deletes the comment with the same peer, actor and
	// Create id, or stores a tombstone when there is none. It needs no
	// share. Any error is retryable; it never refuses.
	DeleteRemoteComment(ctx context.Context, in RemoteDeleteCommentInput) (deleted bool, err error)

	// UpdateRemoteComment applies an inbound edit of a comment (#1572):
	// the comment with the same peer, actor and Create id takes the new
	// content when this edit is newer than the one it shows, a deleted
	// comment is never edited, and an edit whose Create has not arrived
	// is kept for it. The edit needs a live share at comment scope, so it
	// may answer ErrUnknownObject or ErrUnsharedObject; any other error is
	// retryable.
	UpdateRemoteComment(ctx context.Context, in RemoteUpdateCommentInput) (applied bool, err error)

	// InsertRemoteComment persists an inbound Create(Note) from
	// a remote actor on a local target. Idempotent via
	// activity_uri UNIQUE — a retried delivery returns the
	// existing row's ID without firing the notification a
	// second time.
	InsertRemoteComment(ctx context.Context, in RemoteCommentInput) (commentID uuid.UUID, alreadyExisted bool, err error)
}

// RemoteUndoLikeInput is the typed argument of UndoRemoteLike: the
// Undo's authenticated peer and actor, the Like id it names (its
// object, on the actor's origin) and its own id.
type RemoteUndoLikeInput struct {
	PeerID          uuid.UUID
	ActorURI        string
	LikeActivityURI string
	UndoActivityURI string
}

// RemoteDeleteCommentInput is the typed argument of DeleteRemoteComment:
// the Delete's authenticated peer and actor, the comment's Create id it
// names (its object, on the actor's origin) and its own id.
type RemoteDeleteCommentInput struct {
	PeerID            uuid.UUID
	ActorURI          string
	CreateActivityURI string
	DeleteActivityURI string
}

// RemoteUpdateCommentInput is the typed argument of UpdateRemoteComment:
// the Update's authenticated peer and actor, the comment's Create id it
// names (its object), its own id, the new content and its signed
// published time (the order key, spec v1 §7.5.5).
type RemoteUpdateCommentInput struct {
	PeerID            uuid.UUID
	ActorURI          string
	CreateActivityURI string
	UpdateActivityURI string
	Content           string
	Published         time.Time
}

// RemoteCommentInput is the typed argument shape for an inbound
// comment. The dispatcher extracts these from the envelope payload
// and the SocialPoster does the domain write.
type RemoteCommentInput struct {
	TargetKind string
	TargetID   uuid.UUID
	ParentID   *uuid.UUID // nil = top-level (or ParentActivityURI)
	// ParentActivityURI names the parent by the Create activity id of a
	// comment that was itself federated (spec v1 §7.5.5, #1604); the
	// write finds it among this target's comments by activity_uri.
	ParentActivityURI string
	PeerID            uuid.UUID
	ActorURI          string
	ActivityURI       string
	Body              string
}

// EncryptionKeyInline is the parsed aa:encryptionPublicKey block
// from an inbound envelope's Extra map. Nil at the upsert
// boundary when the envelope didn't carry the block (pre-1.22.I-c
// peer, or a system-generated activity without an attributable
// user). Validated at parse time — PublicKey is always 32 bytes
// when non-nil; Version is always >= 1.
type EncryptionKeyInline struct {
	PublicKey []byte
	Version   int32
}

// RemoteActorUpserter is the contract for the remote-actor
// display cache + encryption-key store. Boot wires it to
// federation/remote's Upserter, which composes the display-info
// upsert with federation/remote.Handler.SetEncryptionKey when
// EncKey is non-nil. The dispatch handlers call it on every
// inbound activity so display fields refresh + the latest
// advertised key lands in federation_remote_actors.
type RemoteActorUpserter interface {
	Upsert(ctx context.Context,
		actorURI string,
		peerID uuid.UUID,
		displayName, avatarURL string,
		encKey *EncryptionKeyInline,
	) error
}

// SetRegistry replaces the per-verb handler map. Used at boot
// AFTER construction so handlers that need a back-reference to
// the dispatcher (LikeHandler, CommentHandler) can be built
// against the constructed instance.
func (d *Dispatcher) SetRegistry(reg map[federation.ActivityType]HandlerFn) {
	d.registry = reg
}

// SetSocialPoster wires the social-side contract. nil-safe:
// when not wired, Like + Comment handlers return OutcomeFailed
// with a clear error so misconfiguration is loud.
func (d *Dispatcher) SetSocialPoster(s SocialPoster) {
	d.social = s
}

// SetRemoteActorUpserter wires the actor-cache contract.
// nil-safe: when not wired, handlers skip the upsert (display
// info falls back to the actor_uri itself in the UI).
func (d *Dispatcher) SetRemoteActorUpserter(u RemoteActorUpserter) {
	d.actorCache = u
}

// LikeHandler is the inbound-Like handler per §5.5 Q1. Takes the
// target object from the inbox row's pre-extracted
// object_kind/object_id, has the SocialPoster authorise and insert
// the Like row scoped to the sending peer + actor in one transaction,
// then refreshes the remote-actor display row.
//
// Idempotency: the social-side INSERT uses ON CONFLICT DO
// NOTHING via the partial UNIQUE likes_remote_uniq_idx — a
// retried delivery lands as no-op.
//
// Returns OutcomeRejected with unknown_object if the inbox row
// didn't classify a target object (rare — the inbox already
// host-checked + URL-shaped it).
func LikeHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.social == nil {
			return OutcomeFailed, "", uuid.UUID{}, errors.New("LikeHandler: SocialPoster not wired")
		}
		if !row.ObjectID.Valid || row.ObjectKind == nil {
			return OutcomeRejected, federation.InboxStatusUnknownObject, uuid.UUID{},
				errors.New("Like activity missing object reference")
		}
		targetID := uuid.UUID(row.ObjectID.Bytes)
		_, err := d.social.InsertRemoteLike(ctx, *row.ObjectKind, targetID, peerID, env.Actor, env.ID)
		if outcome, reason, ok := writeRefusal(err); ok {
			return outcome, reason, uuid.UUID{}, err
		}
		// Authorised and written: now refresh the remote-actor
		// display cache (best-effort).
		d.upsertActorBestEffort(ctx, env, peerID)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// UndoHandler is the inbound handler for an Undo of a Like (#1572, spec
// v1 §7.5.4). ValidateMembers admitted it as exactly that: object_type
// "Like" and an object that is an activity URI on the actor's own
// origin, checked at receipt and again here after decryption; every
// other Undo is unsupported_activity and never reaches this handler.
//
// The object is the Like's id on the SENDER, never a local object, so
// the row carries no object reference. The retraction is bound to the
// row's authenticated peer and the envelope's verified actor: it
// matches only that actor's own Like with that id, needs no live share
// (it withdraws the actor's earlier contribution, §7.1), and is
// processed the same way whether or not the Like exists, was already
// undone or belongs to someone else. A database error is retryable.
func UndoHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.social == nil {
			return OutcomeFailed, "", uuid.UUID{}, errors.New("UndoHandler: SocialPoster not wired")
		}
		if env.Object == "" || env.Actor == "" || env.ID == "" {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{},
				errors.New("Undo activity missing object, actor or id")
		}
		if _, err := d.social.UndoRemoteLike(ctx, RemoteUndoLikeInput{
			PeerID:          peerID,
			ActorURI:        env.Actor,
			LikeActivityURI: env.Object,
			UndoActivityURI: env.ID,
		}); err != nil {
			return OutcomeFailed, "", uuid.UUID{}, err
		}
		d.upsertActorBestEffort(ctx, env, peerID)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// DeleteHandler is the inbound handler for a Delete of a comment (#1572,
// spec v1 §7.5.5). ValidateMembers admitted it as exactly that:
// object_type "Note" and an object that is the comment's Create id on
// the actor's own origin, checked at receipt and again after decryption;
// every other Delete is unsupported_activity and never reaches this
// handler.
//
// Like an Undo, it names the sender's own activity, carries no local
// object reference, and is bound to the row's authenticated peer and
// the envelope's verified actor: it deletes only that actor's comment
// delivered by that Create, needs no live share (§7.1), and is processed
// the same way whether or not the comment exists, was already deleted or
// belongs to someone else. A database error is retryable.
func DeleteHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.social == nil {
			return OutcomeFailed, "", uuid.UUID{}, errors.New("DeleteHandler: SocialPoster not wired")
		}
		if env.Object == "" || env.Actor == "" || env.ID == "" {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{},
				errors.New("Delete activity missing object, actor or id")
		}
		if _, err := d.social.DeleteRemoteComment(ctx, RemoteDeleteCommentInput{
			PeerID:            peerID,
			ActorURI:          env.Actor,
			CreateActivityURI: env.Object,
			DeleteActivityURI: env.ID,
		}); err != nil {
			return OutcomeFailed, "", uuid.UUID{}, err
		}
		d.upsertActorBestEffort(ctx, env, peerID)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// UpdateHandler is the inbound handler for an edit of a comment (#1572,
// spec v1 §7.5.5). ValidateMembers admitted it as exactly that:
// object_type "Note", the comment's Create id on the actor's own origin
// and a valid content, checked at receipt and again after decryption;
// every other Update is unsupported_activity and never reaches this
// handler.
//
// Bound like a Delete to the row's authenticated peer and the envelope's
// verified actor, it edits only that actor's comment delivered by that
// Create. Unlike a retraction it adds content, so it needs a live share
// at comment scope (unknown_object / unshared_object otherwise), checked
// in the write transaction. Edits are ordered by the signed published
// time: an older or repeated one is processed and changes nothing. A
// database error is retryable.
func UpdateHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.social == nil {
			return OutcomeFailed, "", uuid.UUID{}, errors.New("UpdateHandler: SocialPoster not wired")
		}
		if env.Object == "" || env.Actor == "" || env.ID == "" || env.Published.IsZero() {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{},
				errors.New("Update activity missing object, actor, id or published")
		}
		var content string
		if raw, ok := env.Extra[federation.MemberContent]; !ok || json.Unmarshal(raw, &content) != nil || strings.TrimSpace(content) == "" {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{},
				errors.New("Update activity missing content")
		}
		_, err := d.social.UpdateRemoteComment(ctx, RemoteUpdateCommentInput{
			PeerID:            peerID,
			ActorURI:          env.Actor,
			CreateActivityURI: env.Object,
			UpdateActivityURI: env.ID,
			Content:           content,
			Published:         env.Published,
		})
		if outcome, reason, ok := writeRefusal(err); ok {
			return outcome, reason, uuid.UUID{}, err
		}
		d.upsertActorBestEffort(ctx, env, peerID)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// CommentHandler is the inbound-Comment handler. Handles
// Create(Note) where the Note's `inReplyTo` carries the local
// target object URL.
//
// Idempotency: the social-side INSERT uses ON CONFLICT (on
// activity_uri) DO NOTHING — a retried delivery returns the
// existing row's ID + alreadyExisted=true so we don't fire the
// notification twice.
func CommentHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.social == nil {
			return OutcomeFailed, "", uuid.UUID{}, errors.New("CommentHandler: SocialPoster not wired")
		}
		// Extract the Note payload from the envelope.
		body, parentURI, err := extractCommentPayload(env)
		if err != nil {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, err
		}
		// Target object must already be classified by the inbox
		// (object_kind + object_id non-null).
		if !row.ObjectID.Valid || row.ObjectKind == nil {
			return OutcomeRejected, federation.InboxStatusUnknownObject, uuid.UUID{},
				errors.New("Comment activity missing target object")
		}

		// inReplyTo names the parent comment (spec v1 §7.5.5, #1604):
		// a comment of THIS instance (<origin>/comments/<uuid>), or the
		// Create activity id of a comment that was federated to it.
		// Either way the write checks it is a live comment on the same
		// target. Absent, or equal to the target (what senders before
		// #1604 sent for a top-level comment), is a top-level comment.
		// Anything else is refused, never dropped to top level: a reply
		// is never re-parented as a side effect of a missing parent.
		var parentID *uuid.UUID
		var parentActivity string
		if parentURI != "" && parentURI != env.Object {
			if federation.OriginOf(parentURI) == d.localOrigin(ctx) && d.localOrigin(ctx) != "" {
				id, status, err := d.localCommentID(ctx, parentURI)
				if err != nil {
					if status == "" {
						return OutcomeFailed, "", uuid.UUID{}, err
					}
					return OutcomeRejected, status, uuid.UUID{}, err
				}
				parentID = &id
			} else if d.localOrigin(ctx) == "" {
				return OutcomeFailed, "", uuid.UUID{}, errors.New("inbox: the local base URL is not wired; cannot resolve inReplyTo")
			} else {
				parentActivity = parentURI
			}
		}

		input := RemoteCommentInput{
			TargetKind:        *row.ObjectKind,
			TargetID:          uuid.UUID(row.ObjectID.Bytes),
			ParentID:          parentID,
			ParentActivityURI: parentActivity,
			PeerID:            peerID,
			ActorURI:          env.Actor,
			ActivityURI:       env.ID,
			Body:              body,
		}
		_, _, err = d.social.InsertRemoteComment(ctx, input)
		if outcome, reason, ok := writeRefusal(err); ok {
			return outcome, reason, uuid.UUID{}, err
		}
		d.upsertActorBestEffort(ctx, env, peerID)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// --- helpers -----------------------------------------------------------

// upsertActorBestEffort refreshes the remote-actor display row +
// optionally the encryption-key columns. Failures log + swallow —
// the dispatch decision shouldn't hinge on a display-cache write.
//
// Phase 1.22.I-c — also parses the aa:encryptionPublicKey block
// out of env.Extra (when present) so federation_remote_actors
// gains the recipient key I-e/I-f need.
func (d *Dispatcher) upsertActorBestEffort(ctx context.Context, env *federation.Envelope, peerID uuid.UUID) {
	if d.actorCache == nil || env.Actor == "" {
		return
	}
	// No display name or avatar: v1 defines no hint members for them
	// (#1580); a remote actor's name and icon come from its actor
	// document (#1526).
	encKey := extractEncryptionKey(env, d.logger)
	if err := d.actorCache.Upsert(ctx, env.Actor, peerID, "", "", encKey); err != nil && d.logger != nil {
		d.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.dispatcher.actor_upsert.error",
			slog.String("err", err.Error()),
		)
	}
}

// extractEncryptionKey pulls the optional aa:encryptionPublicKey
// block out of env.Extra. Returns nil if the field is absent OR
// malformed (with a logged warning); a malformed block does NOT
// block dispatch — the activity still flows, the receiver just
// can't encrypt back to the sender until the next envelope
// carries a clean key.
//
// Expected shape (per vocab.go):
//
//	"aa:encryptionPublicKey": {
//	  "type": "aa:X25519PublicKey",
//	  "publicKeyBase64": "<44 chars, 32-byte key>",
//	  "version": 1
//	}
func extractEncryptionKey(env *federation.Envelope, logger *slog.Logger) *EncryptionKeyInline {
	raw, ok := env.Extra[federation.PropEncryptionPublicKey]
	if !ok {
		return nil
	}
	var block struct {
		Type            string `json:"type"`
		PublicKeyBase64 string `json:"publicKeyBase64"`
		Version         int32  `json:"version"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		if logger != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "inbox.encryption_key.parse_error",
				slog.String("actor", env.Actor),
				slog.String("err", err.Error()),
			)
		}
		return nil
	}
	if block.Type != "" && block.Type != federation.TypeX25519PublicKey {
		// Unknown algorithm token; defer to the future when we
		// dispatch on it. For v1 we just skip + log so the
		// inbound activity isn't blocked by a forward-compat
		// hiccup.
		if logger != nil {
			logger.LogAttrs(context.Background(), slog.LevelInfo, "inbox.encryption_key.unknown_type",
				slog.String("actor", env.Actor),
				slog.String("type", block.Type),
			)
		}
		return nil
	}
	if block.Version < 1 {
		return nil
	}
	keyBytes, err := base64.StdEncoding.DecodeString(block.PublicKeyBase64)
	if err != nil || len(keyBytes) != 32 {
		if logger != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "inbox.encryption_key.malformed",
				slog.String("actor", env.Actor),
				slog.Int("len", len(keyBytes)),
			)
		}
		return nil
	}
	return &EncryptionKeyInline{PublicKey: keyBytes, Version: block.Version}
}

// extractCommentPayload reads a comment Create's content and optional
// inReplyTo (spec v1 §7.5, #1580). ValidateMembers has already
// checked both, before the envelope was stored and again after any
// decryption; the error here is defence in depth.
func extractCommentPayload(env *federation.Envelope) (body, parentURI string, err error) {
	if raw, ok := env.Extra[federation.MemberContent]; ok {
		if err := json.Unmarshal(raw, &body); err != nil {
			return "", "", err
		}
	}
	if raw, ok := env.Extra[federation.MemberInReplyTo]; ok {
		if err := json.Unmarshal(raw, &parentURI); err != nil {
			return "", "", err
		}
	}
	if strings.TrimSpace(body) == "" {
		return "", "", errors.New("comment payload missing content")
	}
	return body, parentURI, nil
}

// localCommentID resolves an inReplyTo URI to a comment id on this
// instance: <local origin>/comments/<uuid>. Another origin or shape is
// invalid_member; an unknown local origin is a retryable failure (fail
// closed, never guessed).
func (d *Dispatcher) localCommentID(ctx context.Context, uri string) (uuid.UUID, federation.InboxStatus, error) {
	local := d.localOrigin(ctx)
	if local == "" {
		return uuid.UUID{}, "", errors.New("inbox: the local base URL is not wired; cannot resolve inReplyTo")
	}
	u, err := url.Parse(uri)
	if err != nil || federation.OriginOf(uri) != local || u.RawQuery != "" || u.Fragment != "" {
		return uuid.UUID{}, federation.InboxStatusInvalidMember, fmt.Errorf("%w: inReplyTo is not a comment on this instance", federation.ErrInvalidMember)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 2 || segs[0] != "comments" {
		return uuid.UUID{}, federation.InboxStatusInvalidMember, fmt.Errorf("%w: inReplyTo is not a comment on this instance", federation.ErrInvalidMember)
	}
	id, err := uuid.Parse(segs[1])
	if err != nil {
		return uuid.UUID{}, federation.InboxStatusInvalidMember, fmt.Errorf("%w: inReplyTo is not a comment id", federation.ErrInvalidMember)
	}
	return id, "", nil
}

// writeRefusal maps a SocialPoster error to the dispatch outcome: nil is
// not a refusal (ok=false); ErrUnknownObject and ErrUnsharedObject are
// terminal rejections; anything else (a database error, an unwired
// authorizer) is a retryable failure. None of them wrote anything.
func writeRefusal(err error) (DispatchOutcome, federation.InboxStatus, bool) {
	switch {
	case err == nil:
		return 0, "", false
	case errors.Is(err, ErrUnknownObject):
		return OutcomeRejected, federation.InboxStatusUnknownObject, true
	case errors.Is(err, ErrUnsharedObject):
		return OutcomeRejected, federation.InboxStatusUnsharedObject, true
	}
	return OutcomeFailed, "", true
}
