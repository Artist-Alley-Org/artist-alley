// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// A metadata Update of an asset to one peer (#1577, spec v1 §18.3,
// §18.4): an Update carrying aa:asset and aa:metadata.
//
// # Scope
//
// One explicitly named peer per call. Nothing here decides that an
// asset's metadata changed, iterates peers or versions, or reacts to a
// local edit. The durable metadata work processor (metadata_work.go)
// calls it once per peer of a page; migration 00100's triggers enqueue
// that work and MetadataWorkRunner runs it under the server, but the
// profile is not advertised, so no production peer is eligible yet. The
// delivery side (MetadataUpdateMembers) is wired into the outbox worker,
// so a recorded Update is delivered under the checks below.
//
// # When (RecordMetadataUpdate)
//
// In the caller's transaction, which holds the asset row, only when all of
// these hold now:
//
//   - the asset is local, live, public or team, with an owner who has a
//     username;
//   - the peer is enabled and connected, with asset-metadata-1, archival-1
//     and original-transfer-1 in its stored intersection;
//   - a peer-wide, live, unexpired original-file share of the asset to the
//     peer exists (the oldest such one is used, as for a flags Update);
//   - a sealed version of the asset is bound for the peer: told it (its
//     announcement's peer-wide outbox row sent), or durably on its way
//     (queued, or recorded and not dispatched yet, under a share that is
//     still live, peer-wide and carries the original). The highest such
//     version for THIS peer is named, by one statement returning at most
//     one row (versionToNameSQL). A version only announced to another
//     peer, failed, refused, before the dispatch cursor with no row, a
//     draft or a later version not yet bound is not.
//
// Otherwise nothing is recorded. The snapshot is captured in the same
// transaction (CaptureMetadata) and kept in the local record as one JSON
// string, so its bytes come back exactly as captured. Its published is
// ordered after every earlier announcement and Update of the asset
// (nextAssetPublished). A rollback leaves nothing.
//
// # What is sent (Source.MetadataUpdateMembers, per delivery)
//
// The aa:asset of the named sealed version, from its sealed rows and the
// recorded tier and flags, and the recorded aa:metadata
// (Source.MetadataMember). Every check is made again first, a four-null
// snapshot included: the activity is this local Update of this asset by
// its owner; the asset is still local, live, public or team, on the
// recorded tier, owned by the activity's actor; the peer is still enabled,
// connected and holds all three profiles; the share is still a live,
// peer-wide original-file share of the asset to the peer, granted by the
// recorded aa:Share, and that aa:Share was delivered (until then the
// delivery waits); and the named version is still that sealed version of
// the asset. A failure is a final refusal of the whole activity
// (federation.ErrNotFederated), never a downgrade to a flags-only Update.
// The Update may reach the peer before the announcement of its version:
// a receiver keeps it pending until that version is held (§18.4).
//
// The checks are statements read at delivery, like the rest of the sender
// (descriptive.go): a change committed before them refuses the delivery,
// one committed after is seen by the next attempt, and nothing can recall
// bytes a peer already received.
package assetsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// MetadataChange names the asset and the one peer a metadata Update goes
// to.
type MetadataChange struct {
	AssetID uuid.UUID
	PeerID  uuid.UUID
	BaseURL string
	// Username resolves the owner's username ("" when it has none).
	Username func(ctx context.Context, userRef int64) string
}

// RecordMetadataUpdate writes, inside tx, one metadata Update of c's asset
// to c's peer when it is eligible (file header), returning its activity
// id, or uuid.Nil when nothing is recorded. The caller holds the asset row
// in tx and decides that the asset's metadata is to be sent.
func RecordMetadataUpdate(ctx context.Context, tx pgx.Tx, w *activities.Writer, c MetadataChange) (uuid.UUID, error) {
	var (
		owner       *int64
		sensitivity string
		flags       emit.AssetFlags
		local       bool
	)
	err := tx.QueryRow(ctx, `SELECT owner_user_ref, sensitivity, mature, COALESCE(ai_provenance, ''),
		       (origin_server_id IS NULL AND deleted_at IS NULL)
		  FROM assets WHERE id = $1`, c.AssetID).Scan(&owner, &sensitivity, &flags.Mature, &flags.AIProvenance, &local)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: read the asset: %w", err)
	}
	if !local || owner == nil || (sensitivity != "public" && sensitivity != "team") || c.Username == nil {
		return uuid.Nil, nil
	}
	username := c.Username(ctx, *owner)
	if username == "" {
		return uuid.Nil, nil
	}
	var (
		shareID, shareActivityID uuid.UUID
		peerURL                  string
	)
	err = tx.QueryRow(ctx, `
		SELECT s.id, s.granted_activity_id, p.instance_url
		  FROM federation_shares s
		  JOIN federation_peers p ON p.id = s.peer_id AND p.enabled AND p.status = 'connected'
		                         AND p.capabilities ? $3 AND p.capabilities ? $4 AND p.capabilities ? $5
		 WHERE s.peer_id = $2 AND s.object_kind = 'asset' AND s.object_id = $1
		   AND s.allow_original AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		   AND (s.expires_at IS NULL OR s.expires_at > now()) AND s.granted_activity_id IS NOT NULL
		 ORDER BY s.created_at, s.id
		 LIMIT 1`, c.AssetID, c.PeerID,
		federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1).
		Scan(&shareID, &shareActivityID, &peerURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: read the peer's share: %w", err)
	}
	var (
		versionID uuid.UUID
		number    int64
	)
	err = tx.QueryRow(ctx, versionToNameSQL, c.AssetID, c.PeerID).Scan(&versionID, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: read the version to name: %w", err)
	}
	capture, err := CaptureMetadata(ctx, tx, c.AssetID)
	if err != nil {
		return uuid.Nil, err
	}
	record, err := json.Marshal(capture)
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: encode the metadata capture: %w", err)
	}
	published, err := nextAssetPublished(ctx, tx, c.AssetID)
	if err != nil {
		return uuid.Nil, err
	}
	actor := emit.ActorContext{UserRef: *owner, Username: username, BaseURL: strings.TrimRight(c.BaseURL, "/")}
	em := emit.AssetMetadataUpdate(actor, emit.AssetMetadataUpdateRef{
		AssetFlagUpdateRef: emit.AssetFlagUpdateRef{AssetID: c.AssetID, ShareID: shareID, ShareActivityID: shareActivityID,
			PeerURL: peerURL, VersionNumber: number, Sensitivity: sensitivity, Flags: flags, Published: published},
		VersionID: versionID,
		Capture:   string(record),
	})
	row, err := w.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: record the metadata Update: %w", err)
	}
	return row.ID, nil
}

// versionToNameSQL selects, in one statement returning at most one row,
// the highest sealed version of asset $1 that peer $2 was told or will
// durably be told: a local aa:AssetVersion of the asset naming that
// version, on a share to the peer, whose outbox row to the peer as a
// whole (no target user) is
//
//   - sent: told, whatever the share's state now; or
//   - queued, or absent with the announcement after the dispatch cursor
//     (or no cursor): on its way, and only while its share is live,
//     unexpired, peer-wide and carries the original file.
//
// A failed or refused row, an announcement before the cursor with no
// row, a draft and another peer's announcement never qualify. The new
// Update itself rides on the current live share the recorder selected.
const versionToNameSQL = `
	WITH cursor AS (
	    SELECT a.created_at, a.id FROM federation_dispatch_state d
	      JOIN activities a ON a.id = d.last_dispatched_activity_id
	     WHERE d.id = 1)
	SELECT v.id, v.number
	  FROM asset_versions v
	 WHERE v.asset_id = $1 AND v.sealed_at IS NOT NULL
	   AND EXISTS (
	       SELECT 1
	         FROM activities act
	         JOIN federation_shares s ON s.id::text = act.payload->>'share_id' AND s.peer_id = $2
	        WHERE act.activity_type = 'aa:AssetVersion' AND act.source = 'local'
	          AND act.object_kind = 'asset' AND act.object_local_id = v.asset_id::text
	          AND act.payload->>'version_id' = v.id::text
	          AND (EXISTS (SELECT 1 FROM federation_outbox o
	                        WHERE o.activity_id = act.id AND o.peer_id = $2 AND o.target_user_url IS NULL AND o.status = 'sent')
	               OR (s.object_kind = 'asset' AND s.object_id = v.asset_id AND s.allow_original AND s.target_user_url IS NULL
	                   AND s.revoked_at IS NULL AND (s.expires_at IS NULL OR s.expires_at > now())
	                   AND (EXISTS (SELECT 1 FROM federation_outbox o
	                                 WHERE o.activity_id = act.id AND o.peer_id = $2 AND o.target_user_url IS NULL AND o.status = 'queued')
	                        OR (NOT EXISTS (SELECT 1 FROM federation_outbox o WHERE o.activity_id = act.id AND o.peer_id = $2)
	                            AND (NOT EXISTS (SELECT 1 FROM cursor)
	                                 OR (act.created_at, act.id) > (SELECT created_at, id FROM cursor)))))))
	 ORDER BY v.number DESC
	 LIMIT 1`

// metadataUpdatePayload is a metadata Update's local record
// (emit.AssetMetadataUpdate).
type metadataUpdatePayload struct {
	flagPayload
	VersionID uuid.UUID `json:"version_id"`
	Capture   *string   `json:"metadata_capture"`
}

// refusedMetadataUpdate is a final refusal of a metadata Update's
// delivery.
func refusedMetadataUpdate(format string, args ...any) error {
	return fmt.Errorf("%w: metadata Update: %s", federation.ErrNotFederated, fmt.Sprintf(format, args...))
}

// MetadataUpdateMembers are the aa:asset and aa:metadata members of one
// metadata Update delivery of activityID to peerID (file header), or the
// reason they are not sent now (ErrNotReady) or ever (ErrNotFederated).
func (s Source) MetadataUpdateMembers(ctx context.Context, activityID, peerID uuid.UUID, objectURI string, rawPayload []byte) (asset, metadata json.RawMessage, err error) {
	var p metadataUpdatePayload
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.ShareID == uuid.Nil || p.ShareActivityID == uuid.Nil ||
		p.AssetID == uuid.Nil || p.VersionID == uuid.Nil || p.VersionNumber == nil || p.Mature == nil || p.Sensitivity == "" ||
		p.Capture == nil || *p.Capture == "" {
		return nil, nil, refusedMetadataUpdate("the local record does not name its share, asset, version, tier, flags and capture")
	}
	if !strings.HasSuffix(objectURI, "/assets/"+p.AssetID.String()) || federation.OriginOf(objectURI) == "" {
		return nil, nil, refusedMetadataUpdate("the object is not the updated asset")
	}
	var (
		isThis, assetOK, ownedByActor, peerOK, shareOK bool
		sensitivity                                    string
		holds                                          [3]bool
	)
	// The activity's actor, object kind and object id may be NULL in the
	// table; a NULL is not this Update (IS TRUE), never a scan error.
	err = s.Pool.QueryRow(ctx, `
		SELECT (act.activity_type = $5 AND act.source = 'local' AND act.object_kind = 'asset' AND act.object_local_id = a.id::text) IS TRUE,
		       (a.origin_server_id IS NULL AND a.deleted_at IS NULL),
		       (a.owner_user_ref = act.actor_user_ref) IS TRUE,
		       a.sensitivity,
		       EXISTS (SELECT 1 FROM federation_peers p WHERE p.id = $3 AND p.enabled AND p.status = 'connected'),
		       EXISTS (SELECT 1 FROM federation_peers p WHERE p.id = $3 AND p.capabilities ? $7),
		       EXISTS (SELECT 1 FROM federation_peers p WHERE p.id = $3 AND p.capabilities ? $8),
		       EXISTS (SELECT 1 FROM federation_peers p WHERE p.id = $3 AND p.capabilities ? $9),
		       EXISTS (SELECT 1 FROM federation_shares sh WHERE sh.id = $4 AND sh.granted_activity_id = $6
		                 AND sh.peer_id = $3 AND sh.object_kind = 'asset' AND sh.object_id = $2)
		  FROM activities act, assets a
		 WHERE act.id = $1 AND a.id = $2`, activityID, p.AssetID, peerID, p.ShareID,
		string(federation.ActivityUpdate), p.ShareActivityID,
		federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1).
		Scan(&isThis, &assetOK, &ownedByActor, &sensitivity, &peerOK, &holds[0], &holds[1], &holds[2], &shareOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, refusedMetadataUpdate("the activity or the asset no longer exists")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("assetsend: read the metadata Update: %w", err)
	}
	switch {
	case !isThis:
		return nil, nil, refusedMetadataUpdate("the activity is not a local Update of the asset")
	case !assetOK:
		return nil, nil, refusedMetadataUpdate("the asset is deleted or not local")
	case !ownedByActor:
		return nil, nil, refusedMetadataUpdate("the asset is no longer owned by the Update's actor")
	case sensitivity != p.Sensitivity:
		return nil, nil, refusedMetadataUpdate("the asset's tier changed (%s, recorded %s)", sensitivity, p.Sensitivity)
	case sensitivity != "public" && sensitivity != "team":
		return nil, nil, refusedMetadataUpdate("sensitivity_refused: the asset is %s", sensitivity)
	case !peerOK:
		return nil, nil, refusedMetadataUpdate("the peer is not enabled and connected")
	}
	for i, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		if !holds[i] {
			return nil, nil, fmt.Errorf("%w: %w: metadata Update: the peer does not hold %s", federation.ErrNotFederated,
				federation.ErrProfileNotNegotiated, token)
		}
	}
	switch {
	case !shareOK:
		return nil, nil, refusedMetadataUpdate("the share is not the recorded grant of the asset to the peer")
	}
	if err := s.shareDelivered(ctx, p.ShareID, p.AssetID, peerID, p.ShareActivityID); err != nil {
		return nil, nil, err
	}
	var (
		basename    string
		created     time.Time
		predecessor *int64
	)
	err = s.Pool.QueryRow(ctx, `
		SELECT v.created_at, COALESCE(v.original_basename, ''),
		       COALESCE((SELECT max(s.number) FROM asset_versions s
		                  WHERE s.asset_id = v.asset_id AND s.number < v.number AND s.sealed_at IS NOT NULL), pv.number)
		  FROM asset_versions v
		  LEFT JOIN asset_versions pv ON pv.id = v.predecessor_id
		 WHERE v.id = $1 AND v.asset_id = $2 AND v.number = $3 AND v.sealed_at IS NOT NULL`,
		p.VersionID, p.AssetID, *p.VersionNumber).Scan(&created, &basename, &predecessor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, refusedMetadataUpdate("version %d is not the recorded sealed version of the asset", *p.VersionNumber)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("assetsend: read the version: %w", err)
	}
	sealedFiles, err := versionFiles(ctx, s.Pool, p.VersionID)
	if err != nil {
		return nil, nil, err
	}
	aiProvenance := ""
	if p.AIProvenance != nil {
		aiProvenance = *p.AIProvenance
	}
	// No descriptive member in the aa:asset: aa:metadata carries them
	// (§18.2).
	asset, err = versionMember(objectURI, *p.VersionNumber, created, predecessor, basename, sealedFiles, p.Sensitivity, *p.Mature, aiProvenance, nil)
	if err != nil {
		return nil, nil, err
	}
	metadata, err = s.MetadataMember(ctx, p.AssetID, json.RawMessage(*p.Capture))
	if err != nil {
		return nil, nil, err
	}
	return asset, metadata, nil
}
