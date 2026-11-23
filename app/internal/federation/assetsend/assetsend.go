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
// RecordSealed (sealed.go, #1575) runs inside the transaction that seals
// a later version, and announces it, under the same rule, to the peer of
// every such share that is live then, catching each peer up in order.
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
// and the asset's tier, mature flag and AI provenance. Since #1577 it also
// carries the baseline descriptive members captured when the announcement
// was recorded, under the peer emission policy in descriptive.go, and
// re-checked against current privacy on every delivery. Nothing a caller
// supplies becomes a claim. A sealed file set never changes, so every
// retry of the same activity sends the same version, file set and
// descriptive bytes.
//
// The predecessor is the asset's previous SEALED version (the greatest
// sealed number below this one), not the version it was created over: a
// version that stayed a draft is never announced, so naming it would
// leave the receiver waiting for a version that never comes (#1575). The
// numbers then skip the draft's. Only the latest version is ever sealed
// (migration 00093), so once this version exists the sealed versions
// below it are final, and every announcement of it names the same
// predecessor. With no sealed version below it the version it was
// created over is named, as before: a receiver holding nothing of the
// asset takes it as its first version.
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
//   - A member that is built is recorded first (recordClaims, #1575):
//     what this delivery tells the peer, under a share lock of the live
//     asset row, so that a later withdrawal names exactly what was
//     delivered (withdraw.go). A deleted asset's build is refused there.
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
// (see the package header), returning the activity id of the current
// version's announcement, or uuid.Nil when nothing is announced.
//
// The current version is always announced, also when it was announced to
// the peer before (a receiver that refused or never completed it gets it
// again; the same version is a duplicate there otherwise). Before it, the
// peer is caught up (#1575, sealed.go): every sealed version between the
// highest version it holds or will receive and the current one, in order,
// so the receiver's chain stays contiguous.
func Record(ctx context.Context, tx pgx.Tx, w *activities.Writer, g Grant) (uuid.UUID, error) {
	var (
		owner    *int64
		version  *uuid.UUID
		eligible bool
	)
	// The share as written in this transaction, the asset and the peer.
	// The asset row is held FOR SHARE (#1575): a version being sealed or
	// moved to holds it FOR UPDATE, so a grant reads the current version
	// after that change commits, and a seal after this grant sees the
	// share (RecordSealed), and no version is announced to neither.
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
		 WHERE s.id = $6 AND a.id = $1 AND p.id = $2
		   FOR SHARE OF a`,
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
	to := target{shareID: g.ShareID, shareActivityID: g.ShareActivityID, peerID: g.PeerID, peerURL: g.PeerURL}
	actor := emit.ActorContext{UserRef: *owner, Username: username, BaseURL: strings.TrimRight(g.BaseURL, "/")}
	ids, err := announce(ctx, tx, w, actor, g.AssetID, *version, to, true)
	if err != nil {
		return uuid.Nil, err
	}
	if len(ids) == 0 {
		return uuid.Nil, nil
	}
	return ids[len(ids)-1], nil
}

// payload is an announcement's local record (emit.AssetVersion).
type payload struct {
	ShareID         uuid.UUID `json:"share_id"`
	ShareActivityID uuid.UUID `json:"share_activity_id"`
	AssetID         uuid.UUID `json:"asset_id"`
	VersionID       uuid.UUID `json:"version_id"`
	// The flags when the announcement was recorded (#1575); absent on a
	// record from before then.
	Mature       *bool   `json:"mature"`
	AIProvenance *string `json:"ai_provenance"`
	// The descriptive snapshot (#1577, descriptive.go); absent on a record
	// from before then, which then sends no descriptive member.
	Descriptive json.RawMessage `json:"descriptive"`
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
type Source struct {
	Pool *pgxpool.Pool
	// afterRead, when set (tests only, export_test.go), runs right after
	// Tombstone's read of the announcements, where a concurrent delivery
	// may change one (#1575).
	afterRead func()
}

// Member is the aa:asset member of one announcement to peerID, whose
// activity object is objectURI and local record rawPayload, or the
// reason it is not sent now (ErrNotReady) or ever (ErrNotFederated).
func (s Source) Member(ctx context.Context, activityID, peerID uuid.UUID, objectURI string, rawPayload []byte) (json.RawMessage, error) {
	var p payload
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.ShareID == uuid.Nil || p.ShareActivityID == uuid.Nil ||
		p.AssetID == uuid.Nil || p.VersionID == uuid.Nil {
		return nil, refused("the local record does not name its share, asset and version")
	}
	if !strings.HasSuffix(objectURI, "/assets/"+p.AssetID.String()) || federation.OriginOf(objectURI) == "" {
		return nil, refused("the object is not the announced asset")
	}

	// The grant: still live, as granted, and its aa:Share delivered first.
	if err := s.shareDelivered(ctx, p.ShareID, p.AssetID, peerID, p.ShareActivityID); err != nil {
		return nil, err
	}

	// The version, as sealed.
	var (
		sensitivity, aiProvenance, basename string
		mature, local, sealed               bool
		number                              int64
		created                             time.Time
		predecessor                         *int64
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT a.sensitivity, a.mature, COALESCE(a.ai_provenance, ''),
		       (a.origin_server_id IS NULL AND a.deleted_at IS NULL),
		       v.number, v.created_at, v.sealed_at IS NOT NULL, COALESCE(v.original_basename, ''),
		       COALESCE((SELECT max(s.number) FROM asset_versions s
		                  WHERE s.asset_id = a.id AND s.number < v.number AND s.sealed_at IS NOT NULL), pv.number)
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

	if p.Mature != nil {
		// The flags as recorded (#1575), so a change after the
		// announcement was recorded never changes what a redelivery of it
		// claims. A record from before then has none: the live ones.
		mature, aiProvenance = *p.Mature, ""
		if p.AIProvenance != nil {
			aiProvenance = *p.AIProvenance
		}
	}

	sealedFiles, err := versionFiles(ctx, s.Pool, p.VersionID)
	if err != nil {
		return nil, err
	}
	// The recorded descriptive members, re-checked against current
	// privacy (#1577): a member no longer shareable refuses the activity.
	descriptive, err := s.materializeDescriptive(ctx, p.AssetID, p.Descriptive)
	if err != nil {
		return nil, err
	}
	raw, err := versionMember(objectURI, number, created, predecessor, basename, sealedFiles, sensitivity, mature, aiProvenance, descriptive)
	if err != nil {
		return nil, err
	}
	if err := s.recordClaims(ctx, activityID, peerID, p.AssetID, number, sealedFiles); err != nil {
		return nil, err
	}
	return raw, nil
}

// shareDelivered checks, at send time, that the share an asset activity
// rides on still grants peerID the original of assetID as a whole, live
// and unexpired, and that its aa:Share was delivered to that peer: an
// ErrNotReady error while it is on its way, a final refusal otherwise.
func (s Source) shareDelivered(ctx context.Context, shareID, assetID, peerID, shareActivityID uuid.UUID) error {
	var live bool
	var shareStatus string
	err := s.Pool.QueryRow(ctx, `
		SELECT (s.object_kind = 'asset' AND s.object_id = $2 AND s.peer_id = $3 AND s.allow_original
		        AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		        AND (s.expires_at IS NULL OR s.expires_at > now())),
		       COALESCE((SELECT o.status FROM federation_outbox o
		                  WHERE o.activity_id = $4 AND o.peer_id = $3 AND o.target_user_url IS NULL), '')
		  FROM federation_shares s
		 WHERE s.id = $1`, shareID, assetID, peerID, shareActivityID).Scan(&live, &shareStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return refused("the share no longer exists")
	}
	if err != nil {
		return fmt.Errorf("assetsend: read the share: %w", err)
	}
	if !live {
		return refused("the share was revoked, expired or does not grant this peer the original")
	}
	switch shareStatus {
	case "sent":
		return nil
	case "", "queued":
		return fmt.Errorf("%w: the aa:Share is not delivered to the peer yet", ErrNotReady)
	}
	return refused("the aa:Share was not delivered (%s)", shareStatus)
}

// versionMember builds and checks the aa:asset member of one sealed
// version: its identity and predecessor, its sealed files (the original
// with its base name, dropped when §17.2.2 refuses it), the tier and flags
// given, and, for an aa:AssetVersion only, its recorded descriptive
// members (#1577). An Update (FlagUpdateMember) passes nil: §17.2.8 keeps
// it to mature and aiProvenance.
func versionMember(objectURI string, number int64, created time.Time, predecessor *int64, basename string,
	sealedFiles []versionFile, sensitivity string, mature bool, aiProvenance string, descriptive *descriptiveMembers) (json.RawMessage, error) {
	type file struct {
		Role       string              `json:"role"`
		ContentURI string              `json:"contentUri"`
		Checksums  []map[string]string `json:"checksums"`
		Size       int64               `json:"size"`
		MediaType  string              `json:"mediaType"`
		Filename   string              `json:"originalFilename,omitempty"`
	}
	files := make([]file, 0, len(sealedFiles))
	for _, vf := range sealedFiles {
		f := file{Role: vf.role, ContentURI: "cas://" + vf.hash,
			Checksums: []map[string]string{{"algorithm": "sha256", "value": vf.hash}}, Size: vf.size, MediaType: vf.mediaType}
		if vf.role == "original" {
			f.Filename = basename
		}
		files = append(files, f)
	}

	member := map[string]any{
		"id": objectURI,
		"version": map[string]any{"id": versionURI(objectURI, number), "number": number,
			"created": created.UTC().Format(time.RFC3339), "predecessor": nil},
		"files":       files,
		"sensitivity": sensitivity,
		"mature":      mature,
	}
	if predecessor != nil {
		member["version"].(map[string]any)["predecessor"] = versionURI(objectURI, *predecessor)
	}
	if aiProvenance != "" {
		member["aiProvenance"] = aiProvenance
	}
	descriptive.put(member)
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

// versionURI is the id of an asset's version number n (§17.2.1).
func versionURI(assetURI string, n int64) string { return fmt.Sprintf("%s/versions/%d", assetURI, n) }

// versionFile is one file of a sealed version, with its wire role.
type versionFile struct {
	role, hash, mediaType string
	size                  int64
}

// versionFiles is versionID's sealed file set in (role, hash) order, each
// with its wire role: both local derivative classes are the wire role
// access (§17.2.2; migration 00085). Any other role refuses the delivery.
func versionFiles(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, versionID uuid.UUID) ([]versionFile, error) {
	rows, err := q.Query(ctx, `SELECT role, object_hash, size_bytes, media_type FROM asset_version_files
		WHERE version_id = $1 ORDER BY role, object_hash`, versionID)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the file set: %w", err)
	}
	defer rows.Close()
	var out []versionFile
	for rows.Next() {
		var f versionFile
		if err := rows.Scan(&f.role, &f.hash, &f.size, &f.mediaType); err != nil {
			return nil, fmt.Errorf("assetsend: read the file set: %w", err)
		}
		switch f.role {
		case "original":
		case "preview", "access":
			f.role = "access"
		default:
			return nil, fmt.Errorf("%w: the file set lists role %q", federation.ErrNotFederated, f.role)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read the file set: %w", err)
	}
	return out, nil
}

// recordClaims keeps what this delivery of announcement activityID tells
// peerID (#1575): the version number and the sealed files' wire role and
// sha256, as federation_asset_announcement_claims, so the asset's
// withdrawal can name the highest version actually delivered to that
// peer, also after the asset is purged (withdraw.go).
//
// It runs under a share lock of the asset row, and only while the asset
// is live and local: a soft delete's UPDATE of that row waits for it, and
// a build that starts after the delete committed finds the asset deleted
// and is refused. So every announcement that can still be sent after a
// delete has its claims committed before the delete does (the delete's
// RecordWithdrawals sees them), and nothing is sent without its claims.
func (s Source) recordClaims(ctx context.Context, activityID, peerID, assetID uuid.UUID, number int64, sealed []versionFile) error {
	type claim struct {
		Role   string `json:"role"`
		SHA256 string `json:"sha256"`
	}
	files := make([]claim, 0, len(sealed))
	for _, f := range sealed {
		files = append(files, claim{Role: f.role, SHA256: f.hash})
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var live bool
		err := tx.QueryRow(ctx, `SELECT TRUE FROM assets WHERE id = $1 AND deleted_at IS NULL AND origin_server_id IS NULL FOR SHARE`,
			assetID).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) {
			return refused("the asset is deleted or not local")
		}
		if err != nil {
			return fmt.Errorf("assetsend: lock the asset: %w", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO federation_asset_announcement_claims (activity_id, peer_id, asset_id, version_number, files)
			SELECT $1::uuid, $2::uuid, $3::uuid, $4::bigint, $5::jsonb
			 WHERE EXISTS (SELECT 1 FROM activities WHERE id = $1::uuid AND activity_type = $6 AND object_local_id = $3::uuid::text)
			ON CONFLICT (activity_id, peer_id) DO NOTHING`,
			activityID, peerID, assetID, number, raw, string(federation.ActivityAAAssetVersion))
		if err != nil {
			return fmt.Errorf("assetsend: record the announcement's claims: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM federation_asset_announcement_claims
				WHERE activity_id = $1 AND peer_id = $2 AND asset_id = $3 AND version_number = $4`,
				activityID, peerID, assetID, number).Scan(&n); err != nil {
				return fmt.Errorf("assetsend: read the announcement's claims: %w", err)
			}
			if n != 1 {
				return refused("the delivery is not this asset's announcement %s", activityID)
			}
		}
		return nil
	})
}
