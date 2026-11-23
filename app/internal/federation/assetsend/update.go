// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// An Update of an asset's mature and aiProvenance flags (#1575, spec v1
// §17.2.8).
//
// # Scope
//
// Only these two flags. The asset's tier is not synchronized by an
// Update: §6.5 refuses a restricted or embargoed activity to a peer that
// cannot encrypt, so a raised tier could not reach every peer that holds
// the asset, and an Update must not look like it did. Descriptive fields
// (#1577) are not sent at all.
//
// # When and to whom (RecordFlagUpdates)
//
// In the transaction that changes the flags (assets.Handler.UpdateAsset),
// which holds the asset row, and only when mature or aiProvenance actually
// changed. One Update per peer that BOTH
//
//   - was told a version of the asset: claims of an announcement whose
//     outbox row to that peer is sent or still queued
//     (federation_asset_announcement_claims, written as each announcement
//     is built), and
//   - holds a live original-file share of the asset now, under the rule
//     an announcement is made under (Record): peer-wide, unrevoked,
//     unexpired, the peer enabled and connected with archival-1 and
//     original-transfer-1, the asset local and public or team.
//
// A peer whose share ended gets nothing: its record is not shown there
// any more. A peer that was never told the asset existed gets nothing.
//
// # What is fixed when it is recorded
//
// The local record holds the share it rides on, the asset, the highest
// version number claimed for that peer, the asset's tier then, and the
// flags after the change. Its published is now, or one microsecond after
// the newest aa:AssetVersion or Update of the asset when that is not
// earlier (§7.5.5's rule for edits): every later aa:asset of the asset is
// ordered after every earlier one, which is what a receiver orders the
// flags by. The asset row lock the change holds orders it with the
// announcements, which hold the same row.
//
// # What is sent (Source.FlagUpdateMember, per delivery)
//
// The aa:asset member of the named version, built from its sealed rows
// (immutable once sealed, and its predecessor fixed by migration 00093)
// and the recorded tier and flags: the same bytes on every retry. The
// delivery is refused for good when the share is no longer live or its
// aa:Share was not delivered, the asset was deleted, the version was not
// claimed for that peer, or the asset's tier now differs from the one
// recorded (a different tier claim is never sent under the same id).
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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// FlagChange is the asset whose flags changed in the caller's
// transaction.
type FlagChange struct {
	AssetID uuid.UUID
	BaseURL string
	// Username resolves the owner's username ("" when it has none).
	Username func(ctx context.Context, userRef int64) string
}

// RecordFlagUpdates writes, inside tx, one Update of the asset's flags
// per peer it goes to (file header), returning their activity ids. The
// caller has changed the flags in tx, holding the asset row.
func RecordFlagUpdates(ctx context.Context, tx pgx.Tx, w *activities.Writer, c FlagChange) ([]uuid.UUID, error) {
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
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the asset: %w", err)
	}
	// A restricted or embargoed asset is never announced (§7.5.7.4), so
	// no Update of it is either.
	if !local || owner == nil || (sensitivity != "public" && sensitivity != "team") || c.Username == nil {
		return nil, nil
	}
	username := c.Username(ctx, *owner)
	if username == "" {
		return nil, nil
	}
	type peerTarget struct {
		peerID, shareID, shareActivityID uuid.UUID
		peerURL                          string
		version                          int64
	}
	rows, err := tx.Query(ctx, `
		WITH told AS (
		    SELECT c.peer_id, max(c.version_number) AS version_number
		      FROM federation_asset_announcement_claims c
		      JOIN federation_outbox o ON o.activity_id = c.activity_id AND o.peer_id = c.peer_id
		                              AND o.target_user_url IS NULL
		     WHERE c.asset_id = $1 AND o.status IN ('sent', 'queued')
		     GROUP BY c.peer_id)
		SELECT DISTINCT ON (t.peer_id) t.peer_id, t.version_number, s.id, s.granted_activity_id, p.instance_url
		  FROM told t
		  JOIN federation_shares s ON s.peer_id = t.peer_id AND s.object_kind = 'asset' AND s.object_id = $1
		                          AND s.allow_original AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		                          AND (s.expires_at IS NULL OR s.expires_at > now())
		  JOIN federation_peers p ON p.id = t.peer_id AND p.enabled AND p.status = 'connected'
		                         AND p.capabilities ? $2 AND p.capabilities ? $3
		 ORDER BY t.peer_id, s.created_at, s.id`,
		c.AssetID, federation.ProfileArchival1, federation.ProfileOriginalTransfer1)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the peers to update: %w", err)
	}
	var targets []peerTarget
	for rows.Next() {
		var t peerTarget
		if err := rows.Scan(&t.peerID, &t.version, &t.shareID, &t.shareActivityID, &t.peerURL); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assetsend: read the peers to update: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read the peers to update: %w", err)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	published, err := nextAssetPublished(ctx, tx, c.AssetID)
	if err != nil {
		return nil, err
	}
	actor := emit.ActorContext{UserRef: *owner, Username: username, BaseURL: strings.TrimRight(c.BaseURL, "/")}
	ids := make([]uuid.UUID, 0, len(targets))
	for _, t := range targets {
		em := emit.AssetFlagUpdate(actor, emit.AssetFlagUpdateRef{AssetID: c.AssetID, ShareID: t.shareID,
			ShareActivityID: t.shareActivityID, PeerURL: t.peerURL, VersionNumber: t.version,
			Sensitivity: sensitivity, Flags: flags, Published: published})
		row, err := w.RecordActivity(ctx, tx, em.Activity)
		if err != nil {
			return nil, fmt.Errorf("assetsend: record the asset Update: %w", err)
		}
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// nextAssetPublished is the published time of a new Update of the asset:
// now, or one microsecond after the newest aa:AssetVersion or Update of it
// when that is not earlier (a clock step back, or two in one microsecond).
func nextAssetPublished(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) (time.Time, error) {
	var last pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT max(published_at) FROM activities
		WHERE source = 'local' AND object_kind = 'asset' AND object_local_id = $1
		  AND activity_type IN ($2, $3)`, assetID.String(),
		string(federation.ActivityAAAssetVersion), string(federation.ActivityUpdate)).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("assetsend: the asset's last announcement: %w", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if last.Valid && !now.After(last.Time) {
		now = last.Time.UTC().Add(time.Microsecond)
	}
	return now, nil
}

// flagPayload is an Update's local record (emit.AssetFlagUpdate).
type flagPayload struct {
	ShareID         uuid.UUID `json:"share_id"`
	ShareActivityID uuid.UUID `json:"share_activity_id"`
	AssetID         uuid.UUID `json:"asset_id"`
	VersionNumber   *int64    `json:"version_number"`
	Sensitivity     string    `json:"sensitivity"`
	Mature          *bool     `json:"mature"`
	AIProvenance    *string   `json:"ai_provenance"`
}

// refusedUpdate is a final refusal of an asset Update's delivery.
func refusedUpdate(format string, args ...any) error {
	return fmt.Errorf("%w: asset Update: %s", federation.ErrNotFederated, fmt.Sprintf(format, args...))
}

// FlagUpdateMember is the aa:asset member of one asset Update delivery to
// peerID (file header), or the reason it is not sent now (ErrNotReady) or
// ever (ErrNotFederated).
func (s Source) FlagUpdateMember(ctx context.Context, _ uuid.UUID, peerID uuid.UUID, objectURI string, rawPayload []byte) (json.RawMessage, error) {
	var p flagPayload
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.ShareID == uuid.Nil || p.ShareActivityID == uuid.Nil ||
		p.AssetID == uuid.Nil || p.VersionNumber == nil || p.Mature == nil || p.Sensitivity == "" {
		return nil, refusedUpdate("the local record does not name its share, asset, version, tier and flags")
	}
	if !strings.HasSuffix(objectURI, "/assets/"+p.AssetID.String()) || federation.OriginOf(objectURI) == "" {
		return nil, refusedUpdate("the object is not the updated asset")
	}
	if err := s.shareDelivered(ctx, p.ShareID, p.AssetID, peerID, p.ShareActivityID); err != nil {
		return nil, err
	}
	var (
		sensitivity, basename string
		local, sealed, told   bool
		versionID             uuid.UUID
		created               time.Time
		predecessor           *int64
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT a.sensitivity, (a.origin_server_id IS NULL AND a.deleted_at IS NULL),
		       v.id, v.created_at, v.sealed_at IS NOT NULL, COALESCE(v.original_basename, ''),
		       COALESCE((SELECT max(s.number) FROM asset_versions s
		                  WHERE s.asset_id = a.id AND s.number < v.number AND s.sealed_at IS NOT NULL), pv.number),
		       EXISTS (SELECT 1 FROM federation_asset_announcement_claims c
		                WHERE c.asset_id = a.id AND c.peer_id = $3 AND c.version_number = v.number)
		  FROM assets a
		  JOIN asset_versions v ON v.asset_id = a.id AND v.number = $2
		  LEFT JOIN asset_versions pv ON pv.id = v.predecessor_id
		 WHERE a.id = $1`, p.AssetID, *p.VersionNumber, peerID).Scan(&sensitivity, &local,
		&versionID, &created, &sealed, &basename, &predecessor, &told)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refusedUpdate("the asset or its version no longer exists")
	}
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the version: %w", err)
	}
	switch {
	case !local:
		return nil, refusedUpdate("the asset is deleted or not local")
	case sensitivity != p.Sensitivity:
		return nil, refusedUpdate("the asset's tier changed (%s, recorded %s)", sensitivity, p.Sensitivity)
	case sensitivity != "public" && sensitivity != "team":
		return nil, refusedUpdate("sensitivity_refused: the asset is %s", sensitivity)
	case !sealed || !told:
		return nil, refusedUpdate("version %d was not announced to the peer", *p.VersionNumber)
	}
	sealedFiles, err := versionFiles(ctx, s.Pool, versionID)
	if err != nil {
		return nil, err
	}
	aiProvenance := ""
	if p.AIProvenance != nil {
		aiProvenance = *p.AIProvenance
	}
	// No descriptive member: §17.2.8 keeps an Update to mature and
	// aiProvenance (#1577 does not change that).
	return versionMember(objectURI, *p.VersionNumber, created, predecessor, basename, sealedFiles, p.Sensitivity, *p.Mature, aiProvenance, nil)
}
