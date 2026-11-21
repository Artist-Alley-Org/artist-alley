// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// aa:AssetVersion emit helper (#1574, spec v1 §17.1), and the asset's
// withdrawal, a Delete carrying aa:tombstone (#1575, §17.3).

package emit

import (
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// AssetVersionRef names what an aa:AssetVersion announces: one sealed
// version of a local asset, to the one peer an original-file share
// went to. The aa:asset member itself is not in the local record: the
// delivery worker builds it from the version's sealed rows when it
// sends (federation/assetsend), so nothing a caller supplies becomes a
// file claim.
type AssetVersionRef struct {
	AssetID   uuid.UUID
	VersionID uuid.UUID
	// ShareID is the federation_shares row the announcement rides on; the
	// resolver addresses it to that share's peer only.
	ShareID uuid.UUID
	// ShareActivityID is the activities row of that share's aa:Share: the
	// delivery worker sends this announcement to a peer only after that
	// aa:Share was delivered to it.
	ShareActivityID uuid.UUID
	PeerURL         string
}

// AssetVersion emits an aa:AssetVersion announcing ref's version. The
// actor is the asset's owner (actor's UserRef and Username), whose key
// signs it (#1526). No notification: an announcement is not a social
// event.
func AssetVersion(actor ActorContext, ref AssetVersionRef) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindAsset, ref.AssetID.String())
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAAAssetVersion,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindAsset,
				LocalID: ref.AssetID.String(),
			},
			To: []string{ref.PeerURL},
			Payload: map[string]any{
				"share_id":          ref.ShareID.String(),
				"share_activity_id": ref.ShareActivityID.String(),
				"asset_id":          ref.AssetID.String(),
				"version_id":        ref.VersionID.String(),
			},
		},
	}
}

// AssetWithdrawalRef names what an asset's withdrawal tells one peer: the
// asset and when it was deleted. Which version it names, and that
// version's files, are not in the local record: the delivery worker reads
// them when it sends, from what each delivery of the asset's announcements
// told that peer (federation/assetsend).
type AssetWithdrawalRef struct {
	AssetID uuid.UUID
	// PeerID and PeerURL are the one peer it goes to.
	PeerID  uuid.UUID
	PeerURL string
	// Deleted is when the asset was deleted here (the tombstone's
	// deleted).
	Deleted time.Time
}

// AssetWithdrawal emits a Delete of ref's asset to one peer. The actor is
// the asset's owner, whose key signs it (#1526). No notification.
func AssetWithdrawal(actor ActorContext, ref AssetWithdrawalRef) Emission {
	actorRef := actor.UserRef
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityDelete,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     actor.ObjectURI(activities.ObjectKindAsset, ref.AssetID.String()),
				Kind:    activities.ObjectKindAsset,
				LocalID: ref.AssetID.String(),
			},
			To: []string{ref.PeerURL},
			Payload: map[string]any{
				"asset_id":       ref.AssetID.String(),
				"target_peer_id": ref.PeerID.String(),
				"deleted":        ref.Deleted.UTC().Format(time.RFC3339),
			},
		},
	}
}
