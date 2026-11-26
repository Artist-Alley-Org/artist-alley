// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Access-copy transfer policy (#1597, spec v1 §7.5.7 "Access copies",
// §8.5.1, §16.7; proposed ADR 0103).
//
// # Its own grant
//
// An access copy is a file with a local derivative role (preview or
// access, both wire role access) in a sealed version's file set. A peer
// may receive one only under federation_shares.allow_access_copies,
// default false (migration 00102). That grant is independent of scope and
// of allow_original: no scope implies it, the original-file grant does not
// imply it, and it does not imply the original-file grant. Both decisions
// share one share matcher (matchByteGrant), which reads only the requested
// class's flag.
//
// # What this file decides, and what it does not
//
// CanPeerFetchAccessCopy answers one question for the origin: may this
// peer (and, for a user-scoped share, this actor) receive the access copy
// with this hash and size from version n of this exact asset? It reads
// every input from the database, in one read-only snapshot, never from
// the caller and never from a cache:
//
//   - the peer's enabled flag, status and stored negotiated capabilities
//     (all of archival-1, original-transfer-1, share-lifecycle-1 and
//     derived-transfer-1);
//   - the asset's owner, tier, deletion and origin, but not its current
//     file_hash: an access copy is never authorised by the asset's
//     current original;
//   - the sealed version's own file row, the copy's own storage object and
//     its recorded fixity, verified now at that SHA-256 and size;
//   - this origin's record that it announced that file of that version to
//     this peer (federation_asset_announcement_claims, written before
//     sending; it is never a delivery acknowledgement and never a grant on
//     its own);
//   - the candidate shares, read from federation_shares directly.
//
// The signed version-bound route (federation/cas, access.go, §8.5.1) calls
// this for every GET and HEAD. There is no wire member and no receiver yet
// (spec Implementation status S4), and the derived-transfer-1 token is not
// advertised (peer.CapDerivedTransfer1), so no production peer's
// intersection can satisfy step 1. The reasons are internal: the route
// passes on only the peer-level ones and answers every refusal that
// concerns the asset, its version, its file or a grant with one 403
// not_available (§8.5.1, §12.4).
//
// # No recall
//
// Revoking or expiring the share ends future decisions. Bytes already
// delivered are not recalled.

package shares

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// Reasons specific to access-copy decisions (#1597). The original-file
// reasons (unknown_peer, profile_not_negotiated, actor_not_on_peer,
// unknown_object, not_origin, sensitivity_refused) and the shared share
// reasons keep their meaning.
const (
	// RejectReasonVersionNotFound: the asset has no sealed version with
	// the requested number.
	RejectReasonVersionNotFound RejectReason = "version_not_found"

	// RejectReasonCopyNotListed: that sealed version lists no access copy
	// with exactly this hash and size. The same bytes on another asset,
	// another version or another role authorise nothing here.
	RejectReasonCopyNotListed RejectReason = "copy_not_listed"

	// RejectReasonCopyNotVerified: the listed copy's own storage object is
	// missing, has another size, or its recorded fixity is not verified
	// at this SHA-256 and size now.
	RejectReasonCopyNotVerified RejectReason = "copy_not_verified"

	// RejectReasonCopyNotAnnounced: this origin has no record that it
	// announced that file of that version to this peer.
	RejectReasonCopyNotAnnounced RejectReason = "copy_not_announced"

	// RejectReasonAccessCopyNotGranted: a live share lets this peer and
	// actor see the asset, but none carries allow_access_copies.
	RejectReasonAccessCopyNotGranted RejectReason = "access_copy_not_granted"
)

// accessCopyProfiles is every profile that must be in the stored
// intersection for derived-transfer-1 to be in effect (§16.7).
var accessCopyProfiles = []peer.PeerCapability{
	peer.CapArchival1,
	peer.CapOriginalTransfer1,
	peer.CapShareLifecycle1,
	peer.CapDerivedTransfer1,
}

// AccessCopyFetchRequest names who asks for which access copy.
//
// PeerID and ActorURL must come from verified signatures, as for
// OriginalFetchRequest: ActorURL is "" for an instance-level request,
// which only a share without a named recipient can satisfy. The asset is
// named by its local id, the version by its number (§17.2.1).
type AccessCopyFetchRequest struct {
	PeerID   uuid.UUID
	ActorURL string
	AssetID  uuid.UUID
	Version  int64  // the sealed version's number, >= 1
	SHA256   string // the access copy's SHA-256, hex
	Size     int64  // the access copy's size, in bytes
}

// AccessCopyFetchDecision is the typed result. Share is the share that
// authorised the fetch (set only when Allowed), for the per-fetch audit.
type AccessCopyFetchDecision struct {
	Allowed bool
	Reason  RejectReason
	Share   *Share
}

func denyAccessCopy(r RejectReason) AccessCopyFetchDecision {
	return AccessCopyFetchDecision{Allowed: false, Reason: r}
}

// CanPeerFetchAccessCopy decides whether the peer (and actor) may receive
// one access copy of a sealed version. Order, each step with its reason:
//
//  1. Peer, from its row: exists (unknown_peer), enabled (peer_disabled),
//     connected (peer_not_connected), and archival-1, original-transfer-1,
//     share-lifecycle-1 and derived-transfer-1 all in the stored
//     negotiated capabilities (profile_not_negotiated).
//  2. Actor, if named: on the peer's own origin (actor_not_on_peer).
//  3. Asset, from its row: exists and is not deleted (unknown_object);
//     this instance is its origin (not_origin).
//  4. The copy: version n is sealed (version_not_found); it lists a
//     preview or access file with exactly this hash and size
//     (copy_not_listed); that file's own storage object has this size and
//     its recorded fixity is verified now at this SHA-256 and size
//     (copy_not_verified); this origin recorded announcing that version
//     to this peer with this file as wire role access
//     (copy_not_announced).
//  5. Tier: public or team (sensitivity_refused), the same rule as for
//     originals.
//  6. A current share, exactly as for originals but carrying
//     allow_access_copies: a direct asset share, or a share of a
//     collection containing the asset, for this peer, live, naming no
//     recipient or exactly this actor, whose grantor owns the asset or
//     holds system.admin now. A share that passes everything but the
//     flag gives access_copy_not_granted; otherwise the most specific
//     shared reason.
//
// A database error is returned as err, never as an allow.
func (r *Registry) CanPeerFetchAccessCopy(ctx context.Context, req AccessCopyFetchRequest) (AccessCopyFetchDecision, error) {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AccessCopyFetchDecision{}, fmt.Errorf("shares.CanPeerFetchAccessCopy: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)

	// 1. Peer state, from the row.
	peerRow, err := q.GetPeerForOriginalFetch(ctx, pgtype.UUID{Bytes: req.PeerID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return denyAccessCopy(RejectReasonUnknownPeer), nil
		}
		return AccessCopyFetchDecision{}, fmt.Errorf("shares.CanPeerFetchAccessCopy: peer: %w", err)
	}
	if !peerRow.Enabled {
		return denyAccessCopy(RejectReasonPeerDisabled), nil
	}
	if peerRow.Status != "connected" {
		return denyAccessCopy(RejectReasonPeerNotConnected), nil
	}
	for _, p := range accessCopyProfiles {
		negotiated, err := capabilitiesContain(peerRow.Capabilities, string(p))
		if err != nil {
			return AccessCopyFetchDecision{}, fmt.Errorf("shares.CanPeerFetchAccessCopy: peer capabilities: %w", err)
		}
		if !negotiated {
			return denyAccessCopy(RejectReasonProfileNotNegotiated), nil
		}
	}

	// 2. The actor must be one of this peer's.
	if req.ActorURL != "" && !sameOrigin(req.ActorURL, peerRow.InstanceUrl) {
		return denyAccessCopy(RejectReasonActorNotOnPeer), nil
	}

	// 3. The asset.
	asset, err := q.GetAssetForAccessCopyFetch(ctx, pgtype.UUID{Bytes: req.AssetID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return denyAccessCopy(RejectReasonUnknownObject), nil
		}
		return AccessCopyFetchDecision{}, fmt.Errorf("shares.CanPeerFetchAccessCopy: asset: %w", err)
	}
	if asset.Deleted {
		return denyAccessCopy(RejectReasonUnknownObject), nil
	}
	if asset.FromPeer {
		return denyAccessCopy(RejectReasonNotOrigin), nil
	}

	// 4. The sealed version's own file, its verified object, and the
	// announcement to this peer.
	if req.Version < 1 {
		return denyAccessCopy(RejectReasonVersionNotFound), nil
	}
	facts, err := q.GetAccessCopyFacts(ctx, GetAccessCopyFactsParams{
		AssetID:       pgtype.UUID{Bytes: req.AssetID, Valid: true},
		VersionNumber: req.Version,
		ObjectHash:    strings.ToLower(req.SHA256),
		SizeBytes:     req.Size,
		PeerID:        pgtype.UUID{Bytes: req.PeerID, Valid: true},
	})
	if err != nil {
		return AccessCopyFetchDecision{}, fmt.Errorf("shares.CanPeerFetchAccessCopy: copy: %w", err)
	}
	switch {
	case !facts.VersionSealed:
		return denyAccessCopy(RejectReasonVersionNotFound), nil
	case !facts.CopyListed:
		return denyAccessCopy(RejectReasonCopyNotListed), nil
	case !facts.CopyVerified:
		return denyAccessCopy(RejectReasonCopyNotVerified), nil
	case !facts.Announced:
		return denyAccessCopy(RejectReasonCopyNotAnnounced), nil
	}

	// 5. Tier cap.
	if !OriginalTierAllowed(asset.Sensitivity) {
		return denyAccessCopy(RejectReasonSensitivityRefused), nil
	}

	// 6. A current share that carries the access-copy grant.
	if asset.OwnerUserRef == nil {
		return denyAccessCopy(RejectReasonGrantorNotOwner), nil
	}
	share, reason, err := r.matchByteGrant(ctx, tx, req.PeerID, req.ActorURL, req.AssetID, *asset.OwnerUserRef, accessCopyGrant)
	if err != nil {
		return AccessCopyFetchDecision{}, err
	}
	if share == nil {
		return denyAccessCopy(reason), nil
	}
	return AccessCopyFetchDecision{Allowed: true, Share: share}, nil
}
