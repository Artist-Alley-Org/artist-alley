// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The authoritative inbound write gate (#1570): the CanPeerAccess policy
// (gate.go decide), decided inside the caller's write transaction from
// locked, uncached rows and the database clock.
//
// # Why a transactional variant
//
// CanPeerAccess reads a per-object cache and decides expiry by the app
// clock, then returns; a write made afterwards could land after a revoke
// that committed in between. CanPeerAccessTx reads the peer row and every
// share row it relies on FOR SHARE inside the transaction that also
// writes the like or comment. A revoke (UPDATE federation_shares), a peer
// disable (UPDATE federation_peers) or a soft delete of the target
// (UPDATE ... deleted_at) takes those rows FOR UPDATE, so it either
// commits first, and this decision sees it and refuses, or it waits for
// the write to commit. Expiry is judged by the database clock read after
// the share rows are loaded and locked (clock_timestamp(), never the
// transaction-start now()), so a share that expires while the decision
// waits on a lock is expired for it. There is no positive cache on this
// path.
//
// The policy is the same code: decide (direct share, then a covering
// collection share whose grantor could have shared the asset directly,
// #893), pickBestMatch (recipient, scope ladder, expiry) and the reserved
// `edit` refusal (#1524). Only where rows come from differs.

package shares

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// CanPeerAccessTx is CanPeerAccess decided inside tx. The peer state is
// read from the database (req.PeerEnabled / req.PeerConnected are
// ignored), FOR SHARE, as is every share row consulted.
func (r *Registry) CanPeerAccessTx(ctx context.Context, tx pgx.Tx, req AccessRequest) (AccessDecision, error) {
	if req.RequiredScope == federation.ShareScopeEditReserved {
		return AccessDecision{Allowed: false, Reason: RejectReasonInsufficientScope}, nil
	}
	if !req.RequiredScope.Valid() {
		return AccessDecision{}, fmt.Errorf("shares.CanPeerAccessTx: invalid required scope %q", req.RequiredScope)
	}
	if !req.ObjectKind.Valid() {
		return AccessDecision{}, fmt.Errorf("shares.CanPeerAccessTx: invalid object kind %q", req.ObjectKind)
	}
	var enabled bool
	var status string
	err := tx.QueryRow(ctx, `SELECT enabled, status FROM federation_peers WHERE id = $1 FOR SHARE`, req.PeerID).Scan(&enabled, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessDecision{Allowed: false, Reason: RejectReasonPeerDisabled}, nil
	}
	if err != nil {
		return AccessDecision{}, fmt.Errorf("shares.CanPeerAccessTx: peer: %w", err)
	}
	if !enabled {
		return AccessDecision{Allowed: false, Reason: RejectReasonPeerDisabled}, nil
	}
	if status != "connected" {
		return AccessDecision{Allowed: false, Reason: RejectReasonPeerNotConnected}, nil
	}
	return r.decide(ctx, tx, lockedShares(tx), dbClock(tx), req)
}

// dbClock reads the database's current time, clock_timestamp(), each
// time it is asked. Not now(): that is the transaction's START time, so
// a decision that waited for a row lock while a share expired would
// judge the share by a time from before the wait. decide asks it after
// each set of share rows is loaded FOR SHARE, i.e. after every wait the
// decision depends on (#1570 review).
func dbClock(tx pgx.Tx) shareClock {
	return func(ctx context.Context) (pgtype.Timestamptz, error) {
		var t pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t); err != nil {
			return pgtype.Timestamptz{}, fmt.Errorf("shares.CanPeerAccessTx: clock: %w", err)
		}
		return t, nil
	}
}

// lockedShares loads an object's unrevoked shares inside tx, FOR SHARE,
// never from the cache.
func lockedShares(tx pgx.Tx) shareLoader {
	return func(ctx context.Context, kind federation.ShareObjectKind, id uuid.UUID) ([]Share, error) {
		rows, err := tx.Query(ctx, `SELECT id, grantor_user_ref, object_kind, object_id,
		       peer_id, target_user_url, scope, expires_at, notes,
		       granted_activity_id, granted_at, revoked_at,
		       revoked_activity_id, created_at, updated_at, allow_original
		  FROM federation_shares
		 WHERE object_kind = $1 AND object_id = $2 AND revoked_at IS NULL
		 ORDER BY id
		   FOR SHARE`, string(kind), pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []FederationShare
		for rows.Next() {
			var s FederationShare
			if err := rows.Scan(&s.ID, &s.GrantorUserRef, &s.ObjectKind, &s.ObjectID,
				&s.PeerID, &s.TargetUserUrl, &s.Scope, &s.ExpiresAt, &s.Notes,
				&s.GrantedActivityID, &s.GrantedAt, &s.RevokedAt,
				&s.RevokedActivityID, &s.CreatedAt, &s.UpdatedAt, &s.AllowOriginal); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return rowsToShares(out), nil
	}
}

// Inbound write verbs and the targets they apply to (#1570). A like is
// a `view` reaction (spec v1 §7.5.1 scope ladder); a comment needs
// `comment`. Only these target kinds hold likes or comments; any other
// kind has no such target here.
var inboundWriteTargets = map[federation.ActivityType]struct {
	scope federation.ShareScope
	kinds map[federation.ShareObjectKind]string // kind -> table
}{
	federation.ActivityLike: {federation.ShareScopeView, map[federation.ShareObjectKind]string{
		federation.ShareObjectKindPost: "posts", federation.ShareObjectKindAsset: "assets",
	}},
	federation.ActivityCreate: {federation.ShareScopeComment, map[federation.ShareObjectKind]string{
		federation.ShareObjectKindPost: "posts", federation.ShareObjectKindAsset: "assets",
		federation.ShareObjectKindCollection: "collections",
	}},
}

// InboundWriteSupported reports whether verb writes onto a target of
// kind, and the scope it needs.
func InboundWriteSupported(verb federation.ActivityType, kind federation.ShareObjectKind) (federation.ShareScope, bool) {
	t, ok := inboundWriteTargets[verb]
	if !ok {
		return "", false
	}
	if _, ok := t.kinds[kind]; !ok {
		return "", false
	}
	return t.scope, true
}

// Inbound write refusals. The inbox records them as unknown_object and
// unshared_object; neither is ever told to the sender, whose answer was
// already sent at receipt.
var (
	ErrInboundUnknownTarget = errors.New("shares: inbound write target does not exist or does not take this activity")
	ErrInboundUnshared      = errors.New("shares: no live share admits this inbound write")
)

// InboundWrite is one inbound like or comment to authorise.
type InboundWrite struct {
	Verb       federation.ActivityType
	PeerID     uuid.UUID
	ActorURI   string
	TargetKind federation.ShareObjectKind
	TargetID   uuid.UUID
}

// AuthorizeInboundWrite is the one decision for every applied inbound
// write (#1570), made inside the transaction that will write: the verb
// and target kind are supported, the target exists now and is not
// deleted (its row held FOR SHARE until the write commits), and a live
// share of this instance admits this peer's actor at the verb's scope
// (CanPeerAccessTx). It returns nil, ErrInboundUnknownTarget,
// ErrInboundUnshared (wrapping the reject reason), or a database error,
// which never authorises.
func (r *Registry) AuthorizeInboundWrite(ctx context.Context, tx pgx.Tx, w InboundWrite) error {
	scope, ok := InboundWriteSupported(w.Verb, w.TargetKind)
	if !ok {
		return fmt.Errorf("%w: %s on %q", ErrInboundUnknownTarget, w.Verb, w.TargetKind)
	}
	table := inboundWriteTargets[w.Verb].kinds[w.TargetKind]
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM `+table+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, w.TargetID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s %s", ErrInboundUnknownTarget, w.TargetKind, w.TargetID)
	}
	if err != nil {
		return fmt.Errorf("shares: inbound target: %w", err)
	}
	d, err := r.CanPeerAccessTx(ctx, tx, AccessRequest{
		PeerID: w.PeerID, UserURL: w.ActorURI, ObjectKind: w.TargetKind, ObjectID: w.TargetID, RequiredScope: scope,
	})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fmt.Errorf("%w: %s", ErrInboundUnshared, d.Reason)
	}
	return nil
}
