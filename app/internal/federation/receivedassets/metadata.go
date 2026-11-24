// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The current asset-metadata-1 snapshot on a record's detail (#1577,
// spec v1 §18.4): current_metadata, separate from the claims of the
// original record, which it never replaces.
//
// It is read only after the detail's own query allowed the record
// (ViewableSQL). assetingest.Service.CurrentMetadata then chooses the
// asset's current snapshot from what is kept; that choice is not a
// permission. Before it is shown, one more statement checks again, for
// the same record and caller, that the record is still viewable, is of
// the snapshot's peer and asset and that no tombstone is kept for the
// asset (otherwise the detail is the same 404), and that the version
// the snapshot names is held here as public and verified (complete or
// superseded). When that version is not public, current_metadata is
// omitted: the newest snapshot is chosen first, so an older one is never
// shown in its place, and the original claims are never presented as
// current. The two checks are separate statements, not one serializable
// read: each is decided at the moment it runs.
//
// The kept values are projected as kept: nothing is validated against an
// aa:metadata member's bounds again, trimmed, linked or fetched.
package receivedassets

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// boundPublicSQL is true when the snapshot's own version ($5, file set
// $6) is held for r's peer and asset as public, complete or superseded:
// the origin's tier of that version is the minimum tier of the claims
// kept with it (§17.2), whatever the current version's tier is.
const boundPublicSQL = `EXISTS (SELECT 1 FROM federation_received_assets b
	                WHERE b.peer_id = r.peer_id AND b.origin_asset_uri = r.origin_asset_uri
	                  AND b.version_uri = $5 AND b.file_set_sha256 = $6
	                  AND b.state IN ('` + assetingest.StateComplete + `', '` + assetingest.StateSuperseded + `')
	                  AND b.sensitivity = 'public')`

// currentMetadata is record id's current_metadata for caller, or nil when
// none is shown. visible is false when the record is no longer viewable
// by the time the snapshot is checked (the detail then answers 404).
func (h *Handler) currentMetadata(ctx context.Context, caller int64, id, peerID uuid.UUID, assetURI string) (cm *openapi.ReceivedAssetCurrentMetadata, visible bool, err error) {
	m, err := assetingest.New(assetingest.Deps{Pool: h.Pool}).CurrentMetadata(ctx, peerID, assetURI)
	if err != nil {
		return nil, false, fmt.Errorf("receivedassets: current metadata: %w", err)
	}
	if m == nil {
		return nil, true, nil
	}
	var bound bool
	err = h.Pool.QueryRow(ctx, `SELECT `+boundPublicSQL+`
		  FROM federation_received_assets r
		 WHERE r.id = $2 AND r.peer_id = $3 AND r.origin_asset_uri = $4 AND `+ViewableSQL("r", "$1")+`
		   AND NOT EXISTS (SELECT 1 FROM federation_received_tombstones t
		                    WHERE t.peer_id = r.peer_id AND t.origin_asset_uri = r.origin_asset_uri)`,
		caller, id, m.PeerID, m.AssetURI, m.VersionURI, m.FileSetSHA256).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("receivedassets: current metadata recheck: %w", err)
	}
	if !bound {
		return nil, true, nil
	}
	return projectMetadata(m), true, nil
}

// projectMetadata is m as the API shows it, values unchanged.
func projectMetadata(m *assetingest.MetadataRevision) *openapi.ReceivedAssetCurrentMetadata {
	s := openapi.ReceivedAssetMetadataSnapshot{
		Title:       m.Snapshot.Title,
		Description: m.Snapshot.Description,
		Rights:      m.Snapshot.Rights,
	}
	if rs := m.Snapshot.RequiredStatement; rs != nil {
		s.RequiredStatement = &openapi.ReceivedAssetMetadataLabelValue{
			Label: openapi.ReceivedAssetMetadataLanguageMap(rs.Label),
			Value: openapi.ReceivedAssetMetadataLanguageMap(rs.Value),
		}
	}
	return &openapi.ReceivedAssetCurrentMetadata{
		Profile:                  openapi.AssetMetadata1,
		Snapshot:                 s,
		RevisionId:               openapi_types.UUID(m.RevisionID),
		PeerId:                   openapi_types.UUID(m.PeerID),
		OriginAssetUri:           m.AssetURI,
		ActivityUri:              m.ActivityURI,
		ActorUri:                 m.ActorURI,
		VersionUri:               m.VersionURI,
		FileSetSha256:            m.FileSetSHA256,
		Published:                m.Published,
		PublishedSeconds:         m.PublishedSeconds,
		PublishedNanos:           m.PublishedNanos,
		OriginSignerKeyId:        m.OriginSignerKeyID,
		OriginSignatureAlgorithm: m.OriginSignatureAlgorithm,
		OriginSignatureValue:     m.OriginSignatureValue,
		RecipientSignerKeyId:     m.RecipientSignerKeyID,
		ReceivedAt:               m.ReceivedAt,
	}
}
