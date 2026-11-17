// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Original-byte transfer policy (#1573, spec v1 §7.5.7).
//
// # Byte classes are not scopes
//
// A share's scope (view < comment < annotate < remix) says what a peer
// may DO with a shared object. It never says which BYTES the peer may
// receive. Original bytes, the stored VariantOriginal, need their own
// explicit grant on the origin's row: federation_shares.allow_original,
// default false (migration 00075). No scope implies it, remix included.
//
// # What this file decides, and what it does not
//
// CanPeerFetchOriginal answers one question for the origin: may this
// peer (and, for a user-scoped share, this actor) receive the original
// bytes of this exact asset, whose stored original has this hash and
// size? It reads every input that matters from the database, in one
// read-only snapshot, and trusts nothing the caller could get wrong:
//
//   - the peer's enabled flag, status and negotiated capabilities come
//     from federation_peers, never from booleans the caller passes;
//   - the asset's hash, size, tier, owner and deletion come from assets;
//   - the candidate shares come from federation_shares directly, not
//     from the per-object cache, so a revoke committed before the call
//     is always seen.
//
// There is no byte endpoint yet. The per-asset signed fetch that will
// call this (spec §8.5) is #72; until it ships and enforces this
// decision, the original-transfer-1 profile stays unadvertised
// (peer.CapOriginalTransfer1), so no peer can negotiate it and every
// decision here answers profile_not_negotiated in production.
//
// There is deliberately no derivative counterpart. Preview and access
// derivatives have no recorded per-variant hash yet, so a derivative
// decision bound to the ORIGINAL's hash would authorise bytes nobody
// checked. #1597 records verified per-variant hashes and an explicit
// derivative key allowlist first.
//
// # Rights describe, they do not permit
//
// Rights information neither grants nor blocks a fetch, and this file
// never reads it. The grant form shows the asset's local copyright and
// credit fields as context; those are free text, not an ArchivePub
// rights URI or requiredStatement (spec v1 §17.2.5).
//
// # No recall
//
// Revoking or expiring the share ends future fetch decisions. Bytes
// already delivered are not recalled.

package shares

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// Reasons specific to original-byte decisions (#1573). The shared
// reasons (no_share_row, wrong_user, expired, insufficient_scope,
// peer_disabled, peer_not_connected, grantor_not_owner) keep their
// gate.go meaning.
const (
	// RejectReasonUnknownPeer: no federation_peers row for the peer id.
	RejectReasonUnknownPeer RejectReason = "unknown_peer"

	// RejectReasonProfileNotNegotiated: the peer's stored negotiated
	// capabilities lack original-transfer-1.
	RejectReasonProfileNotNegotiated RejectReason = "profile_not_negotiated"

	// RejectReasonActorNotOnPeer: the requesting actor URL is not on the
	// peer's own origin, so it cannot be one of that peer's actors.
	RejectReasonActorNotOnPeer RejectReason = "actor_not_on_peer"

	// RejectReasonUnknownObject: the asset does not exist, is deleted,
	// or has no stored original.
	RejectReasonUnknownObject RejectReason = "unknown_object"

	// RejectReasonNotOrigin: this instance holds the asset only as a
	// copy received from another instance (origin_server_id is set).
	// Only the origin may send its original.
	RejectReasonNotOrigin RejectReason = "not_origin"

	// RejectReasonHashMismatch: the expected hash is not the stored
	// original's hash for THIS asset. Identical bytes on another asset
	// authorise nothing here: a hash is unique only per owner.
	RejectReasonHashMismatch RejectReason = "hash_mismatch"

	// RejectReasonSizeMismatch: the expected size is not the stored
	// original's size.
	RejectReasonSizeMismatch RejectReason = "size_mismatch"

	// RejectReasonSensitivityRefused: the asset's tier is restricted,
	// embargo or unresolved. Refused even with allow_original, because
	// there is no encrypted byte transport.
	RejectReasonSensitivityRefused RejectReason = "sensitivity_refused"

	// RejectReasonOriginalNotGranted: a live share lets this peer and
	// actor see the asset, but none carries allow_original.
	RejectReasonOriginalNotGranted RejectReason = "original_not_granted"
)

// OriginalFetchRequest names who asks for which original bytes.
//
// PeerID and ActorURL must come from verified signatures: PeerID from
// the instance HTTP signature, ActorURL from the per-actor signature
// (#1526), or "" for an instance-level request, which only a share
// without a named recipient can satisfy. The asset is named by its
// local id; a caller holding an object URI resolves it with the same
// local object-ref parsing the inbox uses before calling this.
type OriginalFetchRequest struct {
	PeerID   uuid.UUID
	ActorURL string
	AssetID  uuid.UUID
	SHA256   string // expected hash of the stored original, lowercase hex
	Size     int64  // expected size of the stored original, in bytes
}

// OriginalFetchDecision is the typed result. Share is the share that
// authorised the fetch (set only when Allowed), for the per-fetch audit.
type OriginalFetchDecision struct {
	Allowed bool
	Reason  RejectReason
	Share   *Share
}

func deny(r RejectReason) OriginalFetchDecision {
	return OriginalFetchDecision{Allowed: false, Reason: r}
}

// OriginalTierAllowed reports whether an asset tier permits original
// transfer at all: public and team only. Restricted, embargo and any
// value this build does not know (unresolved) are refused. Used by the
// grant handler and by CanPeerFetchOriginal, so both apply one rule.
func OriginalTierAllowed(tier string) bool {
	return tier == "public" || tier == "team"
}

// CanPeerFetchOriginal decides whether the peer (and actor) may receive
// the original bytes of the asset. Order, each step with its reason:
//
//  1. Peer, from its row: exists (unknown_peer), enabled
//     (peer_disabled), connected (peer_not_connected), original-transfer-1
//     in the stored negotiated capabilities (profile_not_negotiated).
//  2. Actor, if named: on the peer's own origin (actor_not_on_peer).
//  3. Asset, from its row: exists, not deleted, has a stored original
//     (unknown_object); this instance is its origin, not a holder of a
//     copy from a peer (not_origin); the stored hash, and the size of
//     the stored object, equal the expected ones (hash_mismatch,
//     size_mismatch).
//  4. Tier: public or team (sensitivity_refused).
//  5. A current share: a direct asset share, or a share of a collection
//     containing the asset, for this peer, live (not revoked, not
//     expired), naming no recipient or exactly this actor, whose grantor
//     owns the asset or holds system.admin, and which carries
//     allow_original. A share that passes everything but the flag gives
//     original_not_granted; otherwise the most specific shared reason.
//
// A database error is returned as err, never as an allow.
func (r *Registry) CanPeerFetchOriginal(ctx context.Context, req OriginalFetchRequest) (OriginalFetchDecision, error) {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OriginalFetchDecision{}, fmt.Errorf("shares.CanPeerFetchOriginal: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)

	// 1. Peer state, from the row.
	peerRow, err := q.GetPeerForOriginalFetch(ctx, pgtype.UUID{Bytes: req.PeerID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deny(RejectReasonUnknownPeer), nil
		}
		return OriginalFetchDecision{}, fmt.Errorf("shares.CanPeerFetchOriginal: peer: %w", err)
	}
	if !peerRow.Enabled {
		return deny(RejectReasonPeerDisabled), nil
	}
	if peerRow.Status != "connected" {
		return deny(RejectReasonPeerNotConnected), nil
	}
	negotiated, err := capabilitiesContain(peerRow.Capabilities, string(peer.CapOriginalTransfer1))
	if err != nil {
		return OriginalFetchDecision{}, fmt.Errorf("shares.CanPeerFetchOriginal: peer capabilities: %w", err)
	}
	if !negotiated {
		return deny(RejectReasonProfileNotNegotiated), nil
	}

	// 2. The actor must be one of this peer's.
	if req.ActorURL != "" && !sameOrigin(req.ActorURL, peerRow.InstanceUrl) {
		return deny(RejectReasonActorNotOnPeer), nil
	}

	// 3. The asset and its stored original.
	asset, err := q.GetAssetForOriginalFetch(ctx, pgtype.UUID{Bytes: req.AssetID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deny(RejectReasonUnknownObject), nil
		}
		return OriginalFetchDecision{}, fmt.Errorf("shares.CanPeerFetchOriginal: asset: %w", err)
	}
	if asset.Deleted || asset.FileHash == nil || *asset.FileHash == "" || asset.StoredSize == nil {
		return deny(RejectReasonUnknownObject), nil
	}
	if asset.FromPeer {
		return deny(RejectReasonNotOrigin), nil
	}
	if !strings.EqualFold(*asset.FileHash, req.SHA256) {
		return deny(RejectReasonHashMismatch), nil
	}
	if *asset.StoredSize != req.Size {
		return deny(RejectReasonSizeMismatch), nil
	}

	// 4. Tier cap.
	if !OriginalTierAllowed(asset.Sensitivity) {
		return deny(RejectReasonSensitivityRefused), nil
	}

	// 5. A current share that carries the grant.
	if asset.OwnerUserRef == nil {
		// No owner to have granted anything on their behalf.
		return deny(RejectReasonGrantorNotOwner), nil
	}
	share, reason, err := r.matchOriginalShare(ctx, tx, req, *asset.OwnerUserRef)
	if err != nil {
		return OriginalFetchDecision{}, err
	}
	if share == nil {
		return deny(reason), nil
	}
	return OriginalFetchDecision{Allowed: true, Share: share}, nil
}

// matchOriginalShare finds a current share that authorises the original
// for this asset: direct asset shares first, then shares of every
// collection containing it. Every candidate, direct or container, must
// have been the grantor's to make (owner of the asset, or system.admin),
// checked against the owner as it is NOW, so a grant made before an
// ownership change confers nothing afterwards.
func (r *Registry) matchOriginalShare(ctx context.Context, db DBTX, req OriginalFetchRequest, ownerRef int64) (*Share, RejectReason, error) {
	q := New(db)
	access := AccessRequest{
		PeerID:        req.PeerID,
		UserURL:       req.ActorURL,
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      req.AssetID,
		RequiredScope: federation.ShareScopeView,
	}
	adminSeen := make(map[int64]bool, 1)
	best := RejectReasonNoShareRow
	sawWithoutFlag := false

	consider := func(kind federation.ShareObjectKind, id uuid.UUID) (*Share, error) {
		rows, err := q.ListActiveSharesByObject(ctx, ListActiveSharesByObjectParams{
			ObjectKind: string(kind),
			ObjectID:   pgtype.UUID{Bytes: id, Valid: true},
		})
		if err != nil {
			return nil, err
		}
		var forPeer, granted []Share
		for _, s := range rowsToShares(rows) {
			if s.PeerID != req.PeerID {
				continue
			}
			ok, err := r.grantorAuthority(ctx, db, s.GrantorUserRef, ownerRef, adminSeen)
			if err != nil {
				return nil, err
			}
			if !ok {
				best = bumpReason(best, RejectReasonGrantorNotOwner)
				continue
			}
			forPeer = append(forPeer, s)
			if s.AllowOriginal {
				granted = append(granted, s)
			}
		}
		if len(forPeer) == 0 {
			return nil, nil
		}
		// Pick among the flagged shares only, so a higher-ranked share
		// without the flag never masks a lower-ranked one that has it.
		if len(granted) > 0 {
			hit, reason, err := pickBestMatch(granted, access)
			if err != nil {
				return nil, err
			}
			if hit != nil {
				return hit, nil
			}
			best = bumpReason(best, reason)
		}
		hit, reason, err := pickBestMatch(forPeer, access)
		if err != nil {
			return nil, err
		}
		if hit != nil {
			// Live, right peer and actor, enough scope: only the flag is
			// missing.
			sawWithoutFlag = true
			return nil, nil
		}
		best = bumpReason(best, reason)
		return nil, nil
	}

	hit, err := consider(federation.ShareObjectKindAsset, req.AssetID)
	if err != nil {
		return nil, "", err
	}
	if hit != nil {
		return hit, "", nil
	}
	containers, err := q.FindContainingCollections(ctx, pgtype.UUID{Bytes: req.AssetID, Valid: true})
	if err != nil {
		return nil, "", err
	}
	for _, c := range containers {
		hit, err := consider(federation.ShareObjectKindCollection, uuid.UUID(c.Bytes))
		if err != nil {
			return nil, "", err
		}
		if hit != nil {
			return hit, "", nil
		}
	}
	if sawWithoutFlag {
		return nil, RejectReasonOriginalNotGranted, nil
	}
	return nil, best, nil
}

// capabilitiesContain reports whether the stored capabilities JSON array
// holds token. A malformed column is an error, never a match.
func capabilitiesContain(raw []byte, token string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var caps []string
	if err := json.Unmarshal(raw, &caps); err != nil {
		return false, err
	}
	for _, c := range caps {
		if c == token {
			return true, nil
		}
	}
	return false, nil
}

// sameOrigin reports whether two absolute URLs share scheme, host and
// port. Anything unparseable or relative is not the same origin.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil || ua.Scheme == "" || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil || ub.Scheme == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}
