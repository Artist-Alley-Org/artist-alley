// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Announcing a newly sealed version under the shares that already exist
// (#1575, spec v1 §17.2.1).
//
// A receiver accepts a later version only as the successor of the
// version it holds (the version its predecessor names, with no version it
// holds between them). A version names the asset's previous SEALED
// version as its predecessor (Source.Member), so a version that stayed a
// draft is passed over. So once a peer was announced a version, every
// later sealed version of the asset must reach it, in order, with none
// skipped.
//
// # When
//
// RecordSealed runs inside the transaction that seals a version
// (assets.VersionReconciler: a user upload's draft as it is sealed, and a
// rendition successor), which holds the asset row FOR UPDATE. For every
// share of the asset that the grant-time rule (Record) would announce
// under, live at this moment, it catches the share's peer up to the
// asset's current version. A failure rolls the seal back with it, so a
// version is never sealed without the announcements it needs, and none is
// recorded for a seal that did not commit. A grant locks the same row FOR
// SHARE, so a grant and a seal are ordered: whichever commits second sees
// the other.
//
// # What a peer holds (usableVersions)
//
// A version of the asset is usable for a peer when an aa:AssetVersion of
// it, under a share to that peer, was sent to the peer, or is still on its
// way (not dispatched yet, or queued) under a share that is still live.
// An announcement that failed, was refused or cancelled, was skipped at
// dispatch, or waits under a revoked or expired share (which refuses it
// for good) never reaches the peer, so it does not make its version
// usable. A delivered one counts whatever happened to its share since.
//
// # The chain, and what is announced (announce)
//
// A receiver accepts a version only as the successor of the one it holds,
// so what matters is not the highest usable version but the CONTIGUOUS
// chain: from the lowest usable version, each next number while it is
// usable. A usable version above a missing one does not extend it (rev657):
// v1 sent, v2 failed, v3 still queued leaves the chain at v1, since the
// receiver cannot accept v3 without v2. Everything sealed above the chain,
// up to the current version, is then announced, in order: the missing
// versions and the ones after them, also ones that are usable (a version
// delivered before its predecessor may have been refused for it by then;
// the receiver keeps a duplicate as a duplicate). When the chain reaches
// the current version nothing is announced on a seal, so repeated
// reconciles, and the grant's own announcement of a draft that is now
// sealed, add nothing. A peer with no usable version gets the current
// version alone: a receiver's first version may have any number. A grant
// always announces the current version (Record).
//
// A version that stayed a draft (superseded before it was sealed: a
// successor may follow a draft) is never announced, nor are its bytes,
// since only sealed versions are, and the chain passes over it rather
// than stopping there, so the versions after it are not announced again
// on every seal. The next sealed version names the sealed version below
// the draft as its predecessor, so a peer holding that one accepts it
// (#1575, migration 00093: a draft can never be sealed once a later
// version exists, so that predecessor never changes).
package assetsend

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// Sealed is the asset whose current version was sealed in this
// transaction.
type Sealed struct {
	AssetID uuid.UUID
	BaseURL string
	// Username resolves the owner's username ("" when it has none).
	Username func(ctx context.Context, userRef int64) string
}

// target is the share an announcement rides on, and its peer.
type target struct {
	shareID, shareActivityID uuid.UUID
	peerID                   uuid.UUID
	peerURL                  string
}

// RecordSealed catches every eligible share's peer up to the asset's
// current version, inside the sealing transaction tx (see the file
// header). It returns the announcements recorded.
func RecordSealed(ctx context.Context, tx pgx.Tx, w *activities.Writer, s Sealed) ([]uuid.UUID, error) {
	// The shares the grant-time rule announces under, live now, oldest
	// first per peer; the asset row is already held by the seal.
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.granted_activity_id, s.peer_id, p.instance_url, a.owner_user_ref, a.current_version_id
		  FROM federation_shares s
		  JOIN assets a ON a.id = s.object_id
		  JOIN federation_peers p ON p.id = s.peer_id
		 WHERE s.object_kind = 'asset' AND s.object_id = $1 AND s.allow_original
		   AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		   AND (s.expires_at IS NULL OR s.expires_at > now())
		   AND a.origin_server_id IS NULL AND a.deleted_at IS NULL AND a.file_hash IS NOT NULL
		   AND a.sensitivity IN ('public', 'team')
		   AND p.enabled AND p.status = 'connected'
		   AND p.capabilities ? $2 AND p.capabilities ? $3
		 ORDER BY s.peer_id, s.created_at, s.id`,
		s.AssetID, federation.ProfileArchival1, federation.ProfileOriginalTransfer1)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the shares: %w", err)
	}
	type eligible struct {
		to      target
		owner   *int64
		version *uuid.UUID
	}
	var shares []eligible
	for rows.Next() {
		var e eligible
		if err := rows.Scan(&e.to.shareID, &e.to.shareActivityID, &e.to.peerID, &e.to.peerURL, &e.owner, &e.version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assetsend: read the shares: %w", err)
		}
		shares = append(shares, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read the shares: %w", err)
	}
	var out []uuid.UUID
	for _, e := range shares {
		if e.owner == nil || e.version == nil || s.Username == nil {
			continue
		}
		username := s.Username(ctx, *e.owner)
		if username == "" {
			continue
		}
		actor := emit.ActorContext{UserRef: *e.owner, Username: username, BaseURL: strings.TrimRight(s.BaseURL, "/")}
		ids, err := announce(ctx, tx, w, actor, s.AssetID, *e.version, e.to, false)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return out, nil
}

// announce records, inside tx, the aa:AssetVersion activities that catch
// to.peerID up to version current of assetID (file header), in version
// order. always also announces current when the chain already reaches it
// (the grant).
func announce(ctx context.Context, tx pgx.Tx, w *activities.Writer, actor emit.ActorContext, assetID, current uuid.UUID, to target, always bool) ([]uuid.UUID, error) {
	var currentNumber int64
	if err := tx.QueryRow(ctx, `SELECT number FROM asset_versions WHERE id = $1 AND asset_id = $2`, current, assetID).
		Scan(&currentNumber); err != nil {
		return nil, fmt.Errorf("assetsend: read the current version: %w", err)
	}
	// The flags every announcement recorded now carries (#1575): read
	// under the caller's lock of the asset row, so a flag change commits
	// before or after this, never between.
	var flags emit.AssetFlags
	if err := tx.QueryRow(ctx, `SELECT mature, COALESCE(ai_provenance, '') FROM assets WHERE id = $1`, assetID).
		Scan(&flags.Mature, &flags.AIProvenance); err != nil {
		return nil, fmt.Errorf("assetsend: read the asset's flags: %w", err)
	}
	// The descriptive members every announcement recorded now carries
	// (#1577, descriptive.go), read under the same lock.
	descriptive, err := captureDescriptive(ctx, tx, assetID)
	if err != nil {
		return nil, err
	}
	usable, err := usableVersions(ctx, tx, assetID, to.peerID)
	if err != nil {
		return nil, err
	}
	// The versions to announce, sealed, above the chain and below the
	// current one; the current one is decided after.
	var versions []uuid.UUID
	chain := int64(0) // the chain's last number; 0 when the peer holds nothing
	if len(usable) > 0 {
		lowest := int64(-1)
		for n := range usable {
			if lowest < 0 || n < lowest {
				lowest = n
			}
		}
		chain = lowest
		rows, err := tx.Query(ctx, `SELECT number, id, sealed_at IS NOT NULL FROM asset_versions
			WHERE asset_id = $1 AND number > $2 AND number <= $3 ORDER BY number`, assetID, lowest, currentNumber)
		if err != nil {
			return nil, fmt.Errorf("assetsend: read the versions to catch up: %w", err)
		}
		type ver struct {
			number int64
			id     uuid.UUID
			sealed bool
		}
		var above []ver
		for rows.Next() {
			var v ver
			if err := rows.Scan(&v.number, &v.id, &v.sealed); err != nil {
				rows.Close()
				return nil, fmt.Errorf("assetsend: read the versions to catch up: %w", err)
			}
			above = append(above, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("assetsend: read the versions to catch up: %w", err)
		}
		// Extend the chain while each next number is usable, or a draft
		// below the current version (never announceable: passed over).
		for _, v := range above {
			if v.number != chain+1 || !(usable[v.number] || (!v.sealed && v.number < currentNumber)) {
				break
			}
			chain = v.number
		}
		for _, v := range above {
			if v.number > chain && v.number < currentNumber && v.sealed {
				versions = append(versions, v.id)
			}
		}
	}
	if always || chain < currentNumber {
		versions = append(versions, current)
	}
	ids := make([]uuid.UUID, 0, len(versions))
	for _, v := range versions {
		em := emit.AssetVersion(actor, emit.AssetVersionRef{AssetID: assetID, VersionID: v, ShareID: to.shareID,
			ShareActivityID: to.shareActivityID, PeerURL: to.peerURL, Flags: &flags, Descriptive: descriptive})
		row, err := w.RecordActivity(ctx, tx, em.Activity)
		if err != nil {
			return nil, fmt.Errorf("assetsend: record aa:AssetVersion: %w", err)
		}
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// usableVersions is the set of version numbers of assetID that are usable
// for peerID (file header).
func usableVersions(ctx context.Context, tx pgx.Tx, assetID, peerID uuid.UUID) (map[int64]bool, error) {
	rows, err := tx.Query(ctx, `
		WITH cursor AS (
		    SELECT a.created_at, a.id FROM federation_dispatch_state d
		      JOIN activities a ON a.id = d.last_dispatched_activity_id
		     WHERE d.id = 1)
		SELECT DISTINCT v.number
		  FROM activities act
		  JOIN federation_shares s ON s.id::text = act.payload->>'share_id' AND s.peer_id = $2
		  JOIN asset_versions v ON v.id::text = act.payload->>'version_id' AND v.asset_id = $1
		 WHERE act.activity_type = 'aa:AssetVersion' AND act.source = 'local'
		   AND act.object_kind = 'asset' AND act.object_local_id = $1::text
		   AND (EXISTS (SELECT 1 FROM federation_outbox o
		                 WHERE o.activity_id = act.id AND o.peer_id = $2 AND o.status = 'sent')
		        OR (s.revoked_at IS NULL AND (s.expires_at IS NULL OR s.expires_at > now())
		            AND (EXISTS (SELECT 1 FROM federation_outbox o
		                          WHERE o.activity_id = act.id AND o.peer_id = $2 AND o.status = 'queued')
		                 -- Not dispatched yet: no row for the peer, after the cursor.
		                 OR (NOT EXISTS (SELECT 1 FROM federation_outbox o WHERE o.activity_id = act.id AND o.peer_id = $2)
		                     AND (NOT EXISTS (SELECT 1 FROM cursor)
		                          OR (act.created_at, act.id) > (SELECT created_at, id FROM cursor))))))`,
		assetID, peerID)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read what the peer was announced: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("assetsend: read what the peer was announced: %w", err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assetsend: read what the peer was announced: %w", err)
	}
	return out, nil
}
