// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// An Update of an asset carrying its asset-metadata-1 snapshot (#1577,
// spec v1 §18): the same Update as one of its flags (AssetFlagUpdate),
// with the captured snapshot and the sealed version it names added to
// the local record.

package emit

import (
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// AssetMetadataUpdateRef names what a metadata Update tells one peer.
// Everything in it is fixed when the Update is recorded: the flags
// Update's fields (the share it rides on, the version, the tier then, the
// flags then and the published time), the id of the sealed version whose
// files the aa:asset names, and the captured snapshot.
type AssetMetadataUpdateRef struct {
	AssetFlagUpdateRef
	// VersionID is the sealed version VersionNumber names; the delivery
	// worker builds the aa:asset from its sealed rows only.
	VersionID uuid.UUID
	// Capture is the JSON of the captured snapshot (assetsend), kept as
	// one string so that its bytes come back exactly as recorded. It is
	// local only: the delivery worker builds aa:metadata from it.
	Capture string
}

// AssetMetadataUpdate emits an Update of ref's asset to one peer carrying
// its metadata snapshot. The actor is the asset's owner, whose key signs
// it (#1526). No notification.
func AssetMetadataUpdate(actor ActorContext, ref AssetMetadataUpdateRef) Emission {
	em := AssetFlagUpdate(actor, ref.AssetFlagUpdateRef)
	em.Activity.Payload["version_id"] = ref.VersionID.String()
	em.Activity.Payload[federation.MetadataCaptureKey] = ref.Capture
	return em
}
