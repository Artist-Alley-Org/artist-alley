// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package assetsend is the sending side of #1574: it announces one
// sealed version of a local asset, as an aa:AssetVersion carrying an
// archival-1 aa:asset member (spec v1 §17.1, §17.2), to the one peer an
// original-file share was granted to.
//
// # When
//
// Record runs inside the share grant's own transaction
// (shares.AdminHandler.insertGrant), after the aa:Share and the share row
// are written. It records an announcement only for an asset share that
// grants the original (#1573) to the peer as a whole (no recipient: the
// receiver fetches as its instance, §8.5), of a live local asset with
// bytes and a public or team tier, to an enabled, connected peer whose
// stored intersection holds BOTH archival-1 and original-transfer-1.
// Anything else records nothing, so a peer without the profiles (every
// production peer: neither is advertised) sees no change at all.
//
// # What is sent
//
// The local record holds references only: the share, its aa:Share, the
// asset and the version that was current at grant time. The aa:asset
// member is built when the delivery worker sends (Source.Member), from
// the version's SEALED file set (asset_version_files: an original whose
// stored bytes were verified for exactly its hash and size, and verified
// access copies, migrations 00084 and 00085): the identity of the asset,
// its version (number, creation time, predecessor), each file's role,
// SHA-256 and size, the declared media type and the original's base name,
// and the asset's tier, mature flag and AI provenance. Nothing a caller
// supplies, and no descriptive text, becomes a claim. A sealed file set
// never changes, so every retry of the same activity sends the same
// version and file set.
//
// # Order and refusal (Source.Member, per delivery)
//
//   - The announcement goes to a peer only after that peer's aa:Share of
//     the same grant was delivered (its outbox row is sent), so the
//     receiver holds the grant before it imports. Until then, and while
//     the version is still a draft, the delivery is retried (the outbox
//     backoff, at most its attempts).
//   - It is refused for good (federation.ErrNotFederated) once the share
//     is revoked or expired, its aa:Share failed or was refused, the asset
//     is gone, remote or no longer public or team: restricted and
//     embargoed assets are never announced (§7.5.7.4).
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// Grant is the share a Record call announces for.
type Grant struct {
	ShareID         uuid.UUID
	ShareActivityID uuid.UUID
	AssetID         uuid.UUID
	PeerID          uuid.UUID
	PeerURL         string
	BaseURL         string
	// Username resolves the owner's username ("" when it has none).
	Username func(ctx context.Context, userRef int64) string
}

// Record writes the aa:AssetVersion for g inside tx when g is eligible
// (see the package header), returning its activity id, or uuid.Nil when
// nothing is announced.
func Record(ctx context.Context, tx pgx.Tx, w *activities.Writer, g Grant) (uuid.UUID, error) {
	var (
		owner    *int64
		version  *uuid.UUID
		eligible bool
	)
	// The share as written in this transaction, the asset and the peer.
	err := tx.QueryRow(ctx, `
		SELECT a.owner_user_ref, a.current_version_id,
		       (s.object_kind = 'asset' AND s.object_id = a.id AND s.peer_id = p.id AND s.allow_original
		        AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		        AND (s.expires_at IS NULL OR s.expires_at > now()) AND s.granted_activity_id = $5
		        AND a.origin_server_id IS NULL AND a.deleted_at IS NULL AND a.file_hash IS NOT NULL
		        AND a.sensitivity IN ('public', 'team')
		        AND p.enabled AND p.status = 'connected'
		        AND p.capabilities ? $3 AND p.capabilities ? $4)
		  FROM federation_shares s, assets a, federation_peers p
		 WHERE s.id = $6 AND a.id = $1 AND p.id = $2`,
		g.AssetID, g.PeerID, federation.ProfileArchival1, federation.ProfileOriginalTransfer1, g.ShareActivityID, g.ShareID).
		Scan(&owner, &version, &eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: read the asset: %w", err)
	}
	if !eligible || owner == nil || version == nil || g.Username == nil {
		return uuid.Nil, nil
	}
	username := g.Username(ctx, *owner)
	if username == "" {
		return uuid.Nil, nil
	}
	em := emit.AssetVersion(emit.ActorContext{UserRef: *owner, Username: username, BaseURL: strings.TrimRight(g.BaseURL, "/")},
		emit.AssetVersionRef{AssetID: g.AssetID, VersionID: *version, ShareID: g.ShareID,
			ShareActivityID: g.ShareActivityID, PeerURL: g.PeerURL})
	row, err := w.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: record aa:AssetVersion: %w", err)
	}
	return row.ID, nil
}

// payload is an announcement's local record (emit.AssetVersion).
type payload struct {
	ShareID         uuid.UUID `json:"share_id"`
	ShareActivityID uuid.UUID `json:"share_activity_id"`
	AssetID         uuid.UUID `json:"asset_id"`
	VersionID       uuid.UUID `json:"version_id"`
}

// refused is a final refusal: the outbox marks the delivery not
// federated and never retries it.
func refused(format string, args ...any) error {
	return fmt.Errorf("%w: aa:AssetVersion: %s", federation.ErrNotFederated, fmt.Sprintf(format, args...))
}

// ErrNotReady: the grant is not delivered to the peer yet, or the version
// is still a draft. The outbox retries the delivery.
var ErrNotReady = errors.New("assetsend: not ready to announce")

// Source builds aa:asset members for the delivery worker.
type Source struct{ Pool *pgxpool.Pool }

// Member is the aa:asset member of one announcement to peerID, whose
// activity object is objectURI and local record rawPayload, or the
// reason it is not sent now (ErrNotReady) or ever (ErrNotFederated).
func (s Source) Member(ctx context.Context, peerID uuid.UUID, objectURI string, rawPayload []byte) (json.RawMessage, error) {
	var p payload
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.ShareID == uuid.Nil || p.ShareActivityID == uuid.Nil ||
		p.AssetID == uuid.Nil || p.VersionID == uuid.Nil {
		return nil, refused("the local record does not name its share, asset and version")
	}
	if !strings.HasSuffix(objectURI, "/assets/"+p.AssetID.String()) || federation.OriginOf(objectURI) == "" {
		return nil, refused("the object is not the announced asset")
	}

	// The grant: still live, as granted, and its aa:Share delivered first.
	var live bool
	var shareStatus string
	err := s.Pool.QueryRow(ctx, `
		SELECT (s.object_kind = 'asset' AND s.object_id = $2 AND s.peer_id = $3 AND s.allow_original
		        AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		        AND (s.expires_at IS NULL OR s.expires_at > now())),
		       COALESCE((SELECT o.status FROM federation_outbox o
		                  WHERE o.activity_id = $4 AND o.peer_id = $3 AND o.target_user_url IS NULL), '')
		  FROM federation_shares s
		 WHERE s.id = $1`, p.ShareID, p.AssetID, peerID, p.ShareActivityID).Scan(&live, &shareStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refused("the share no longer exists")
	}
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the share: %w", err)
	}
	if !live {
		return nil, refused("the share was revoked, expired or does not grant this peer the original")
	}
	switch shareStatus {
	case "sent":
	case "", "queued":
		return nil, fmt.Errorf("%w: the aa:Share is not delivered to the peer yet", ErrNotReady)
	default:
		return nil, refused("the aa:Share was not delivered (%s)", shareStatus)
	}

	// The version, as sealed.
	var (
		sensitivity, aiProvenance, basename string
		mature, local, sealed               bool
		number                              int64
		created                             time.Time
		predecessor                         *int64
	)
	err = s.Pool.QueryRow(ctx, `
		SELECT a.sensitivity, a.mature, COALESCE(a.ai_provenance, ''),
		       (a.origin_server_id IS NULL AND a.deleted_at IS NULL),
		       v.number, v.created_at, v.sealed_at IS NOT NULL, COALESCE(v.original_basename, ''), pv.number
		  FROM assets a
		  JOIN asset_versions v ON v.asset_id = a.id AND v.id = $2
		  LEFT JOIN asset_versions pv ON pv.id = v.predecessor_id
		 WHERE a.id = $1`, p.AssetID, p.VersionID).Scan(&sensitivity, &mature, &aiProvenance, &local,
		&number, &created, &sealed, &basename, &predecessor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refused("the asset or its version no longer exists")
	}
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the version: %w", err)
	}
	switch {
	case !local:
		return nil, refused("the asset is deleted or not local")
	case sensitivity != "public" && sensitivity != "team":
		return nil, refused("sensitivity_refused: the asset is %s", sensitivity)
	case !sealed:
		return nil, fmt.Errorf("%w: the version's file set is not sealed yet", ErrNotReady)
	}

	rows, err := s.Pool.Query(ctx, `SELECT role, object_hash, size_bytes, media_type FROM asset_version_files
		WHERE version_id = $1 ORDER BY role, object_hash`, p.VersionID)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the file set: %w", err)
	}
	type file struct {
		Role       string              `json:"role"`
		ContentURI string              `json:"contentUri"`
		Checksums  []map[string]string `json:"checksums"`
		Size       int64               `json:"size"`
		MediaType  string              `json:"mediaType"`
		Filename   string              `json:"originalFilename,omitempty"`
	}
	var files []file
	for rows.Next() {
		var role, hash, mediaType string
		var size int64
		if err := rows.Scan(&role, &hash, &size, &mediaType); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assetsend: read the file set: %w", err)
		}
		f := file{Role: role, ContentURI: "cas://" + hash,
			Checksums: []map[string]string{{"algorithm": "sha256", "value": hash}}, Size: size, MediaType: mediaType}
		switch role {
		case "original":
			f.Filename = basename
		case "preview", "access":
			// Both local derivative classes are the wire role access
			// (§17.2.2; migration 00085).
			f.Role = "access"
		default:
			rows.Close()
			return nil, refused("the file set lists role %q", role)
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read the file set: %w", err)
	}

	versionURI := func(n int64) string { return fmt.Sprintf("%s/versions/%d", objectURI, n) }
	member := map[string]any{
		"id": objectURI,
		"version": map[string]any{"id": versionURI(number), "number": number,
			"created": created.UTC().Format(time.RFC3339), "predecessor": nil},
		"files":       files,
		"sensitivity": sensitivity,
		"mature":      mature,
	}
	if predecessor != nil {
		member["version"].(map[string]any)["predecessor"] = versionURI(*predecessor)
	}
	if aiProvenance != "" {
		member["aiProvenance"] = aiProvenance
	}
	raw, err := validMember(member)
	if err != nil && basename != "" {
		// The stored base name is a claim the profile may not accept
		// (§17.2.2); the file is announced without it rather than not at all.
		for i := range files {
			files[i].Filename = ""
		}
		member["files"] = files
		raw, err = validMember(member)
	}
	if err != nil {
		return nil, refused("the member does not match archival-1: %v", err)
	}
	return raw, nil
}

// validMember encodes m and checks it against §17.2.
func validMember(m map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := federation.ParseArchivalAsset(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
