// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// aa:Share / aa:Unshare (and the aa:RevokeShare alias) applied as
// inbound grants (#1571, spec v1 §7.5.1, §7.5.2, §16.6).
//
// Reached only for a peer whose stored intersection holds
// share-lifecycle-1 (registry.go profileGated, checked at receipt and
// again here at dispatch), after the envelope's members passed
// federation.ValidateMembers: the object is on the sender's own origin,
// target_peer is this instance, any target_user is a /users/<name> actor
// on it, the references are activity URIs on the sender's origin and
// aa:originalTransfer appears only with original-transfer-1 and
// aa:accessTransfer (exactly true, asset or collection) only when
// derived-transfer-1 is in effect with its three companions (§16.7). Both
// checks ran against the STORED negotiated intersection, at receipt and
// again at dispatch; this handler only decodes the result. The handler
// re-checks what it relies on and hands the activity to the grant record
// (inboundgrants.Store), which applies it in one transaction.
//
// The answer to the sender does not depend on the outcome: an unknown
// recipient, a tombstoned grant or a fail-closed ambiguity are all
// recorded as processed after the 202 the inbox already sent, so the
// inbox is no oracle for which users exist here. The outcome is in the
// local audit (federation.inbound_grant.*).

package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// SetInboundGrants wires the inbound grant record the share handlers
// apply to. Unwired, every share activity fails (retried, then
// terminal): nothing is recorded as processed without its domain write.
func (d *Dispatcher) SetInboundGrants(s *inboundgrants.Store) { d.inboundGrants = s }

// SetLocalBaseURL wires this instance's base URL for the dispatch-time
// member check (target_peer, target_user).
func (d *Dispatcher) SetLocalBaseURL(f func(ctx context.Context) string) { d.localBaseURL = f }

func (d *Dispatcher) localOrigin(ctx context.Context) string {
	if d.localBaseURL == nil {
		return ""
	}
	return federation.OriginOf(d.localBaseURL(ctx))
}

// shareMembers are the §7.5.1 / §7.5.2 members plus the
// share-lifecycle-1 references, decoded after validation.
type shareMembers struct {
	kind, targetPeer, targetUser, scope, expiresAt, supersedes, revokes string
	originalTransfer, accessTransfer                                    bool
}

func decodeShareMembers(env *federation.Envelope) (shareMembers, error) {
	var m shareMembers
	str := func(name string, dst *string) error {
		raw, ok := env.Extra[name]
		if !ok {
			return nil
		}
		return json.Unmarshal(raw, dst)
	}
	for name, dst := range map[string]*string{
		federation.MemberObjectKind: &m.kind, federation.MemberTargetPeer: &m.targetPeer,
		federation.MemberTargetUser: &m.targetUser, federation.MemberScope: &m.scope,
		federation.MemberExpiresAt: &m.expiresAt, federation.MemberSupersedes: &m.supersedes,
		federation.MemberRevokes: &m.revokes,
	} {
		if err := str(name, dst); err != nil {
			return m, fmt.Errorf("%w: %s: %v", federation.ErrInvalidMember, name, err)
		}
	}
	if raw, ok := env.Extra[federation.MemberOriginalTransfer]; ok {
		if err := json.Unmarshal(raw, &m.originalTransfer); err != nil {
			return m, fmt.Errorf("%w: %s: %v", federation.ErrInvalidMember, federation.MemberOriginalTransfer, err)
		}
	}
	if raw, ok := env.Extra[federation.MemberAccessTransfer]; ok {
		if err := json.Unmarshal(raw, &m.accessTransfer); err != nil {
			return m, fmt.Errorf("%w: %s: %v", federation.ErrInvalidMember, federation.MemberAccessTransfer, err)
		}
	}
	return m, nil
}

// checkShareEnvelope re-checks, at dispatch, what applying relies on:
// the store is wired, this instance's origin is known, the sender's
// actor and object are on the peer's own origin, and target_peer is
// this instance. ValidateMembers already ran with the same context; a
// failure here is a refusal, never a partial apply.
func (d *Dispatcher) checkShareEnvelope(ctx context.Context, env *federation.Envelope, peerURL string, m shareMembers) (federation.InboxStatus, error) {
	local := d.localOrigin(ctx)
	if d.inboundGrants == nil || local == "" {
		return "", errors.New("inbox: the inbound grant record or the local base URL is not wired")
	}
	peerOrigin := federation.OriginOf(peerURL)
	if peerOrigin == "" || federation.OriginOf(env.Actor) != peerOrigin || federation.OriginOf(env.Object) != peerOrigin {
		return federation.InboxStatusInvalidMember, fmt.Errorf("%w: a peer can share only its own objects, as its own actors", federation.ErrInvalidMember)
	}
	if federation.OriginOf(m.targetPeer) != local {
		return federation.InboxStatusInvalidMember, fmt.Errorf("%w: target_peer is not this instance", federation.ErrInvalidMember)
	}
	return "", nil
}

// ShareHandler applies an aa:Share as an inbound grant.
func ShareHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		m, err := decodeShareMembers(env)
		if err != nil {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, err
		}
		if status, err := d.checkShareEnvelope(ctx, env, peerURL, m); err != nil {
			if status == "" {
				return OutcomeFailed, "", uuid.UUID{}, err
			}
			return OutcomeRejected, status, uuid.UUID{}, err
		}
		var expires *time.Time
		if m.expiresAt != "" {
			t, err := time.Parse(time.RFC3339Nano, m.expiresAt)
			if err != nil {
				return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, fmt.Errorf("%w: expires_at", federation.ErrInvalidMember)
			}
			expires = &t
		}
		res, err := d.inboundGrants.ApplyShare(ctx, inboundgrants.Share{
			PeerID:          peerID,
			ActivityURI:     env.ID,
			GrantorActorURI: env.Actor,
			ObjectKind:      federation.ShareObjectKind(m.kind),
			ObjectURI:       env.Object,
			TargetUserURI:   m.targetUser,
			Scope:           federation.ShareScope(m.scope),
			AllowOriginal:   m.originalTransfer,
			// Independent of AllowOriginal; absent means false, also on a
			// successor (nothing is inherited from the grant it replaces).
			AllowAccessCopies: m.accessTransfer,
			ExpiresAt:         expires,
			Supersedes:        m.supersedes,
		})
		if errors.Is(err, inboundgrants.ErrInvalid) {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, err
		}
		if err != nil {
			return OutcomeFailed, "", uuid.UUID{}, err
		}
		d.logShare(ctx, "inbox.share.applied", env, res)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// UnshareHandler applies an aa:Unshare / aa:RevokeShare: it ends the
// grant aa:revokes names, or tombstones it if it has not arrived.
func UnshareHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		m, err := decodeShareMembers(env)
		if err != nil {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, err
		}
		if status, err := d.checkShareEnvelope(ctx, env, peerURL, m); err != nil {
			if status == "" {
				return OutcomeFailed, "", uuid.UUID{}, err
			}
			return OutcomeRejected, status, uuid.UUID{}, err
		}
		res, err := d.inboundGrants.ApplyUnshare(ctx, inboundgrants.Unshare{
			PeerID:        peerID,
			ActivityURI:   env.ID,
			ActorURI:      env.Actor,
			ObjectKind:    federation.ShareObjectKind(m.kind),
			ObjectURI:     env.Object,
			TargetUserURI: m.targetUser,
			Revokes:       m.revokes,
		})
		if errors.Is(err, inboundgrants.ErrInvalid) {
			return OutcomeRejected, federation.InboxStatusInvalidMember, uuid.UUID{}, err
		}
		if err != nil {
			return OutcomeFailed, "", uuid.UUID{}, err
		}
		d.logShare(ctx, "inbox.unshare.applied", env, res)
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

func (d *Dispatcher) logShare(ctx context.Context, msg string, env *federation.Envelope, res inboundgrants.Result) {
	if d.logger == nil {
		return
	}
	d.logger.LogAttrs(ctx, slog.LevelInfo, msg,
		slog.String("activity_uri", env.ID),
		slog.String("outcome", string(res.Outcome)),
		slog.String("reason", res.Reason),
		slog.String("grant_id", res.GrantID.String()),
		slog.Bool("tuple_mismatch", res.TupleMismatch),
	)
}
