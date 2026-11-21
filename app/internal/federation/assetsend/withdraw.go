// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The sending side of an asset's withdrawal (#1575, spec v1 §17.3).
//
// # What each peer was told
//
// Every aa:AssetVersion delivery records, as it builds its aa:asset member
// (Source.Member, recordClaims), what it tells its peer: the version
// number and the sealed files' wire role and sha256, in
// federation_asset_announcement_claims. It does so under a share lock of
// the live asset row, before the request. A soft delete's UPDATE of that
// row waits for such a build, and a build after the delete finds the asset
// deleted and is refused. So once a delete commits, every announcement
// that is sent or can still be sent has its claims recorded, and nothing
// else can become sent. That is the invariant the rest relies on.
//
// # When and to whom
//
// RecordWithdrawals runs inside the soft delete's own transaction (the
// delete endpoint and the scheduled delete), after the delete's UPDATE.
// It records one Delete of the asset per peer that has claims of an
// announcement whose outbox row to that peer is sent, or still queued (one
// built and possibly in flight). No other peer gets one, so no other peer
// learns the asset existed. Its local record names only the asset, the
// peer and the time: no version and no file.
//
// # What it names (Source.Tombstone, per delivery)
//
// Everything is read live from the announcements' claims and outbox rows,
// so nothing is truncated and nothing in flight can be missed:
//
//   - S is the highest version among the claims whose announcement to
//     this peer is sent: exactly what that peer was told and received.
//   - While a claimed announcement of a version above S (or of any
//     version, without S) is still queued, the delivery waits
//     (federation.ErrWaitingForDelivery: the outbox tries again later
//     without counting an attempt). Such an announcement either is sent,
//     which brings the withdrawal forward at once, or fails, bounded by its
//     own retries: after the delete its next build is refused.
//   - Then the tombstone names S, built from S's claims alone, never from
//     the asset's rows: the same member on every retry, and still after
//     the asset is purged (the claims have no foreign key to the asset).
//   - Without S, the withdrawal is refused for good (ErrNotFederated).
//
// A queued announcement without claims was never built, and after the
// delete never will be, so it is not waited for (a regrant of a version
// the peer has, or one still waiting for its aa:Share). Nothing names a
// version that was not delivered to that peer. The claims are what the
// peer was or is being sent, at most one row per announcement and peer,
// each bounded like the member (§17.2.3: at most ArchivalMaxFiles files).
//
// # Policy
//
//   - The withdrawal goes to such a peer even if the share was revoked or
//     expired since: the peer may still hold the record and its bytes, and
//     the tombstone only takes access away and names what that peer
//     already received. The resolver still requires the peer enabled,
//     connected and with both profiles.
//   - The local delete reason is never sent: it is operator text, and
//     §17.3's reason is optional.
//   - A tombstone is final at the receiver for that asset. Restoring the
//     asset here does not undo it there, and a later announcement of the
//     same asset is refused by that peer.

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

// Deleted is the asset a RecordWithdrawals call withdraws.
type Deleted struct {
	AssetID uuid.UUID
	BaseURL string
	// Username resolves the owner's username ("" when it has none).
	Username func(ctx context.Context, userRef int64) string
}

// RecordWithdrawals writes, inside tx, one withdrawal of d's asset per peer
// it was announced to (see the file header), returning their activity ids.
// The asset must already be soft-deleted in tx.
func RecordWithdrawals(ctx context.Context, tx pgx.Tx, w *activities.Writer, d Deleted) ([]uuid.UUID, error) {
	var (
		owner   *int64
		deleted *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT owner_user_ref, deleted_at FROM assets
		WHERE id = $1 AND origin_server_id IS NULL`, d.AssetID).Scan(&owner, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the deleted asset: %w", err)
	}
	if owner == nil || deleted == nil || d.Username == nil {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT c.peer_id, p.instance_url
		  FROM federation_asset_announcement_claims c
		  JOIN federation_outbox o ON o.activity_id = c.activity_id AND o.peer_id = c.peer_id AND o.target_user_url IS NULL
		  JOIN federation_peers p ON p.id = c.peer_id
		 WHERE c.asset_id = $1 AND o.status IN ('sent', 'queued')
		 ORDER BY c.peer_id`, d.AssetID)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the announcements: %w", err)
	}
	type target struct {
		peerID uuid.UUID
		url    string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.peerID, &t.url); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assetsend: read the announcements: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read the announcements: %w", err)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	username := d.Username(ctx, *owner)
	if username == "" {
		return nil, nil
	}
	actor := emit.ActorContext{UserRef: *owner, Username: username, BaseURL: strings.TrimRight(d.BaseURL, "/")}
	ids := make([]uuid.UUID, 0, len(targets))
	for _, t := range targets {
		em := emit.AssetWithdrawal(actor, emit.AssetWithdrawalRef{AssetID: d.AssetID, PeerID: t.peerID, PeerURL: t.url, Deleted: *deleted})
		row, err := w.RecordActivity(ctx, tx, em.Activity)
		if err != nil {
			return nil, fmt.Errorf("assetsend: record the withdrawal: %w", err)
		}
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// withdrawalPayload is a withdrawal's local record (emit.AssetWithdrawal).
type withdrawalPayload struct {
	AssetID uuid.UUID `json:"asset_id"`
	Deleted string    `json:"deleted"`
}

func refusedWithdrawal(format string, args ...any) error {
	return fmt.Errorf("%w: asset Delete: %s", federation.ErrNotFederated, fmt.Sprintf(format, args...))
}

// Tombstone is the aa:tombstone member of one withdrawal delivered to
// peerID, whose activity object is objectURI and local record rawPayload,
// or the reason it is not sent now (ErrNotReady, waiting) or ever
// (ErrNotFederated). It reads only the announcements' claims and outbox
// rows, never the asset: see the file header.
func (s Source) Tombstone(ctx context.Context, _ uuid.UUID, peerID uuid.UUID, objectURI string, rawPayload []byte) (json.RawMessage, error) {
	var p withdrawalPayload
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.AssetID == uuid.Nil || p.Deleted == "" {
		return nil, refusedWithdrawal("the local record does not name its asset and time")
	}
	if !strings.HasSuffix(objectURI, "/assets/"+p.AssetID.String()) || federation.OriginOf(objectURI) == "" {
		return nil, refusedWithdrawal("the object is not the withdrawn asset")
	}
	// One statement, so one snapshot (READ COMMITTED takes one per
	// statement): the highest delivered version and any higher one still
	// in flight are read together. A concurrent delivery that moves an
	// announcement from queued to sent is seen either before (still
	// queued: wait) or after (sent: named), never in neither read.
	var (
		number, higher int64
		files          []byte
	)
	if err := s.Pool.QueryRow(ctx, `
		WITH c AS (
		    SELECT c.version_number, c.files, o.status
		      FROM federation_asset_announcement_claims c
		      JOIN federation_outbox o ON o.activity_id = c.activity_id AND o.peer_id = c.peer_id AND o.target_user_url IS NULL
		     WHERE c.asset_id = $1 AND c.peer_id = $2 AND o.status IN ('sent', 'queued')),
		     delivered AS (
		    SELECT version_number, files FROM c WHERE status = 'sent' ORDER BY version_number DESC LIMIT 1)
		SELECT COALESCE((SELECT version_number FROM delivered), 0),
		       (SELECT files FROM delivered),
		       COALESCE((SELECT max(version_number) FROM c
		                  WHERE status = 'queued' AND version_number > COALESCE((SELECT version_number FROM delivered), 0)), 0)`,
		p.AssetID, peerID).Scan(&number, &files, &higher); err != nil {
		return nil, fmt.Errorf("assetsend: read the announcements: %w", err)
	}
	if s.afterRead != nil {
		s.afterRead()
	}
	if higher > 0 {
		return nil, fmt.Errorf("%w: %w: an announcement of version %d is not delivered to the peer yet",
			ErrNotReady, federation.ErrWaitingForDelivery, higher)
	}
	if number == 0 {
		return nil, refusedWithdrawal("no announcement of the asset reached this peer")
	}
	var claims []struct {
		Role   string `json:"role"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(files, &claims); err != nil || len(claims) == 0 {
		return nil, refusedWithdrawal("the delivered announcement's claims are unreadable")
	}
	type file struct {
		Role       string              `json:"role"`
		ContentURI string              `json:"contentUri"`
		Checksums  []map[string]string `json:"checksums"`
	}
	out := make([]file, 0, len(claims))
	for _, f := range claims {
		out = append(out, file{Role: f.Role, ContentURI: "cas://" + f.SHA256,
			Checksums: []map[string]string{{"algorithm": "sha256", "value": f.SHA256}}})
	}
	raw, err := json.Marshal(map[string]any{
		"id":          objectURI,
		"formerType":  federation.ObjectAAAsset,
		"deleted":     p.Deleted,
		"lastVersion": versionURI(objectURI, number),
		"files":       out,
	})
	if err != nil {
		return nil, err
	}
	if _, err := federation.ParseArchivalTombstone(raw); err != nil {
		return nil, refusedWithdrawal("the member does not match archival-1: %v", err)
	}
	return raw, nil
}
