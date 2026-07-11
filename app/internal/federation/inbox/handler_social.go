// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// REAL per-verb handlers for Like + Create(Note as comment) per
// the §5.5 Q1 lock-in. Other verbs ship stubs (handler_stub.go).
// Phase 1.22.D-a-4-dispatch.
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
	"log/slog"
	"strings"

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
	InsertRemoteLike(ctx context.Context, targetKind string, targetID uuid.UUID, peerID uuid.UUID, actorURI string) (inserted bool, err error)

	// InsertRemoteComment persists an inbound Create(Note) from
	// a remote actor on a local target. Idempotent via
	// activity_uri UNIQUE — a retried delivery returns the
	// existing row's ID without firing the notification a
	// second time.
	InsertRemoteComment(ctx context.Context, in RemoteCommentInput) (commentID uuid.UUID, alreadyExisted bool, err error)
}

// RemoteCommentInput is the typed argument shape for an inbound
// comment. The dispatcher extracts these from the envelope payload
// and the SocialPoster does the domain write.
type RemoteCommentInput struct {
	TargetKind  string
	TargetID    uuid.UUID
	ParentID    *uuid.UUID // nil = top-level
	PeerID      uuid.UUID
	ActorURI    string
	ActivityURI string
	Body        string
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

// LikeHandler is the inbound-Like handler per §5.5 Q1. Looks up
// the target object (via the inbox row's pre-extracted
// object_kind/object_id), upserts the remote-actor display row,
// then inserts a Like row scoped to the sending peer + actor.
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
		// Update the remote-actor display cache (best-effort —
		// a failure here doesn't block the Like itself).
		d.upsertActorBestEffort(ctx, env, peerID)

		targetID := uuid.UUID(row.ObjectID.Bytes)
		_, err := d.social.InsertRemoteLike(ctx, *row.ObjectKind, targetID, peerID, env.Actor)
		if err != nil {
			// Common failure: the target row doesn't exist
			// locally (the post was deleted between the
			// sender's share and now). Map to unknown_object
			// per the §12.1 catalogue distinction.
			if isMissingTargetError(err) {
				return OutcomeRejected, federation.InboxStatusUnknownObject, uuid.UUID{}, err
			}
			return OutcomeFailed, "", uuid.UUID{}, err
		}
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
			return OutcomeRejected, federation.InboxStatusInvalidType, uuid.UUID{}, err
		}
		// Target object must already be classified by the inbox
		// (object_kind + object_id non-null).
		if !row.ObjectID.Valid || row.ObjectKind == nil {
			return OutcomeRejected, federation.InboxStatusUnknownObject, uuid.UUID{},
				errors.New("Comment activity missing target object")
		}

		var parentID *uuid.UUID
		if parentURI != "" {
			// Parent URL might be local (a reply to a comment
			// we already have). If we can extract a UUID from
			// the tail of the URL, pass it through; otherwise
			// drop it (top-level comment on the target).
			if id := uuidFromURLTail(parentURI); id != (uuid.UUID{}) {
				parentID = &id
			}
		}

		d.upsertActorBestEffort(ctx, env, peerID)

		input := RemoteCommentInput{
			TargetKind:  *row.ObjectKind,
			TargetID:    uuid.UUID(row.ObjectID.Bytes),
			ParentID:    parentID,
			PeerID:      peerID,
			ActorURI:    env.Actor,
			ActivityURI: env.ID,
			Body:        body,
		}
		_, _, err = d.social.InsertRemoteComment(ctx, input)
		if err != nil {
			if isMissingTargetError(err) {
				return OutcomeRejected, federation.InboxStatusUnknownObject, uuid.UUID{}, err
			}
			return OutcomeFailed, "", uuid.UUID{}, err
		}
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
	displayName, avatarURL := extractActorDisplayHints(env)
	encKey := extractEncryptionKey(env, d.logger)
	if err := d.actorCache.Upsert(ctx, env.Actor, peerID, displayName, avatarURL, encKey); err != nil && d.logger != nil {
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

// extractCommentPayload pulls the Note `content` + `inReplyTo`
// fields from the envelope's Extra map. Both ActivityStreams
// shapes are accepted: `object` carrying a Note as JSON, OR
// inline `content` + `inReplyTo` at the top level. Our outbox
// uses the latter shape per the v1.md §7.5 vocabulary.
func extractCommentPayload(env *federation.Envelope) (body, parentURI string, err error) {
	// First try inline top-level fields.
	if raw, ok := env.Extra["content"]; ok {
		if err := json.Unmarshal(raw, &body); err != nil {
			return "", "", err
		}
	}
	if raw, ok := env.Extra["inReplyTo"]; ok {
		_ = json.Unmarshal(raw, &parentURI) // best-effort
	}
	// Fall back: object is a JSON Note.
	if body == "" {
		if raw, ok := env.Extra["object"]; ok {
			var note struct {
				Type      string `json:"type"`
				Content   string `json:"content"`
				InReplyTo string `json:"inReplyTo"`
			}
			if err := json.Unmarshal(raw, &note); err == nil {
				body = note.Content
				if parentURI == "" {
					parentURI = note.InReplyTo
				}
			}
		}
	}
	if strings.TrimSpace(body) == "" {
		return "", "", errors.New("comment payload missing content")
	}
	return body, parentURI, nil
}

// extractActorDisplayHints peeks at the envelope's Extra for
// `actorDisplayName` + `actorAvatarUrl` hints. These are NOT
// part of the v1 spec proper — the outbox dispatcher (1.22.D-b)
// adds them as convenience for receivers that don't want a
// follow-up HTTP fetch of the actor doc. Receivers that don't
// see the hints fall back to "" (UI renders the actor_uri
// itself).
func extractActorDisplayHints(env *federation.Envelope) (displayName, avatarURL string) {
	if raw, ok := env.Extra["actorDisplayName"]; ok {
		_ = json.Unmarshal(raw, &displayName)
	}
	if raw, ok := env.Extra["actorAvatarUrl"]; ok {
		_ = json.Unmarshal(raw, &avatarURL)
	}
	return displayName, avatarURL
}

// uuidFromURLTail extracts the last path segment + tries to
// parse it as a UUID. Used for inReplyTo URLs that point at
// our local /comments/<uuid>.
func uuidFromURLTail(u string) uuid.UUID {
	idx := strings.LastIndex(u, "/")
	if idx < 0 {
		return uuid.UUID{}
	}
	id, err := uuid.Parse(u[idx+1:])
	if err != nil {
		return uuid.UUID{}
	}
	return id
}

// isMissingTargetError detects the specific "the target row
// doesn't exist locally" failure. Currently a string match on
// the error message; switch to typed errors once social returns
// them.
func isMissingTargetError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "target not found") ||
		strings.Contains(s, "violates foreign key") ||
		strings.Contains(s, "no rows in result set")
}

