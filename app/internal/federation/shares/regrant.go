// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package shares

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/audit"
)

// ---------------------------------------------------------------------------
// Expiry retirement, shared by the sweeper and the grant path (#1586).
//
// federation_shares_active_uniq_idx covers every row with revoked_at IS
// NULL, so an EXPIRED share keeps its grant tuple's slot until something
// revokes it. Only the hourly sweeper used to, which left a window of up
// to an hour in which a new grant for the same (grantor, object, peer,
// recipient) failed with 409. The fix is not to change the index: live
// uniqueness is right, and an expired row is still a row that has to be
// closed properly: revoked locally, with an aa:Unshare recorded in the
// local activity ledger and a federation.share.revoked audit row (reason
// "expired"). Instead the grant path does exactly what the sweeper would
// have done for that one row, first, and then grants.
//
// Telling the peer (#1571): the aa:Unshare names the grant it ends
// (aa:revokes, the share's own aa:Share activity URI) and the outbox
// resolver addresses it by the share row, revoked or not, to that share's
// peer when the peer negotiated share-lifecycle-1. The same holds for
// admin and sweeper revokes. The peer ends the grant; what it already
// received is not recalled.
//
// Expiry never AUTHORISES anything in the meantime: the inbox gate
// re-checks Share.Active(now) on every decision and the outbox resolver
// filters `expires_at > NOW()`, so this is about closing the row, not
// about access.
// ---------------------------------------------------------------------------

// expiryRetirer holds what retiring an expired share needs. The sweeper
// and the admin handler carry the same dependencies, so both build one.
type expiryRetirer struct {
	registry      *Registry
	activities    *activities.Writer
	auditRec      *audit.Recorder
	lookupPeer    PeerLookup
	instanceURLFn func(ctx context.Context) string
	usernameFn    func(ctx context.Context, userRef int64) string
}

// retire revokes one EXPIRED share in its own transaction: records an
// aa:Unshare locally (actor = the grantor, as the sweeper always has),
// flips revoked_at to point at it, writes federation.share.revoked with
// reason "expired" plus extraMeta, commits, then invalidates the
// per-object cache (cross-process via cache.Registry NOTIFY).
//
// The revoke is CONDITIONAL on the row still being expired
// (RevokeExpiredShare): the caller's read is not locked, so the row may
// have been revoked, or its expiry extended, since. Either way the UPDATE
// matches nothing, this transaction rolls back (no aa:Unshare, no audit),
// and retire returns retired=false, nil. The row is never closed twice
// and a share that is live again is never closed at all.
func (d expiryRetirer) retire(ctx context.Context, share *Share, notes string, extraMeta map[string]any) (bool, error) {
	// Peer lookup — needed for the aa:Unshare envelope addressing.
	// If the peer was defederated in between, the lookup fails and
	// the envelope is addressed to nobody; the revoked_at flip is what
	// matters.
	peer, peerErr := d.lookupPeer(ctx, share.PeerID)

	actor := emit.ActorContext{
		UserRef:  share.GrantorUserRef,
		Username: d.usernameFn(ctx, share.GrantorUserRef),
		BaseURL:  d.instanceURLFn(ctx),
	}
	targetURL := ""
	if share.TargetUserURL != nil {
		targetURL = *share.TargetUserURL
	}
	revokes, err := grantedActivityURI(ctx, d.registry.Pool, share)
	if err != nil {
		return false, err
	}
	em := emit.Unshare(actor, emit.ShareRef{
		ShareID:       share.ID,
		ObjectKind:    share.ObjectKind,
		ObjectID:      share.ObjectID,
		PeerURL:       peer.InstanceURL, // empty if lookup failed; delivery then refuses the activity (activity_not_federated, #1580)
		TargetUserURL: targetURL,
		Scope:         share.Scope,
		Notes:         notes,
	}, revokes)

	tx, err := d.registry.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	activityRow, err := d.activities.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return false, err
	}
	if _, err := New(tx).RevokeExpiredShare(ctx, RevokeExpiredShareParams{
		ID:                pgtype.UUID{Bytes: share.ID, Valid: true},
		RevokedActivityID: pgtype.UUID{Bytes: activityRow.ID, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row is no longer an unrevoked EXPIRED share: another
			// sweeper, admin revoke or regrant closed it (and committed
			// its own activity + audit), or its expiry was extended and
			// it is live again. Either way ours must not commit.
			_ = tx.Rollback(ctx)
			return false, nil
		}
		return false, err
	}

	actorRef := share.GrantorUserRef
	meta := map[string]any{
		"share_id":       share.ID.String(),
		"object_kind":    string(share.ObjectKind),
		"object_id":      share.ObjectID.String(),
		"peer_id":        share.PeerID.String(),
		"reason":         "expired",
		"correlation_id": activityRow.ID.String(),
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	if peerErr != nil {
		meta["peer_lookup_error"] = peerErr.Error()
	}
	d.auditRec.WriteInTx(ctx, audit.New(tx), audit.EventFederationShareRevoked, nil, &actorRef, meta)

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	d.registry.invalidateObject(ctx, share.ObjectKind, share.ObjectID)
	return true, nil
}

// unrevokedForTuple returns the row holding a grant tuple's unique slot
// (live or expired-but-unrevoked) and whether it has expired by the
// database clock. ErrShareNotFound when the slot is free.
func (r *Registry) unrevokedForTuple(ctx context.Context, in InsertInput) (*Share, bool, error) {
	key := ""
	if in.TargetUserURL != nil {
		key = *in.TargetUserURL
	}
	row, err := New(r.Pool).GetUnrevokedShareForTuple(ctx, GetUnrevokedShareForTupleParams{
		GrantorUserRef: in.GrantorUserRef,
		ObjectKind:     string(in.ObjectKind),
		ObjectID:       pgtype.UUID{Bytes: in.ObjectID, Valid: true},
		PeerID:         pgtype.UUID{Bytes: in.PeerID, Valid: true},
		TargetKey:      key,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrShareNotFound
		}
		return nil, false, err
	}
	return rowToShare(FederationShare{
		ID:                row.ID,
		GrantorUserRef:    row.GrantorUserRef,
		ObjectKind:        row.ObjectKind,
		ObjectID:          row.ObjectID,
		PeerID:            row.PeerID,
		TargetUserUrl:     row.TargetUserUrl,
		Scope:             row.Scope,
		ExpiresAt:         row.ExpiresAt,
		Notes:             row.Notes,
		GrantedActivityID: row.GrantedActivityID,
		GrantedAt:         row.GrantedAt,
		RevokedAt:         row.RevokedAt,
		RevokedActivityID: row.RevokedActivityID,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
		AllowOriginal:     row.AllowOriginal,
		AllowAccessCopies: row.AllowAccessCopies,
	}), row.Expired, nil
}
