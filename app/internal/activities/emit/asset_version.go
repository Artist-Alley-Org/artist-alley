// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// aa:AssetVersion emit helper (#1574, spec v1 §17.1).

package emit

import (
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
