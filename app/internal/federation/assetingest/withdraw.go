// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Withdrawal of a received asset by its origin (#1575, spec v1 §17.3).
//
// A Delete of an aa:Asset carrying aa:tombstone, from a peer whose stored
// intersection holds archival-1 and original-transfer-1 (the profiles a
// record can exist under), is applied here after
// federation.ValidateMembers: the tombstone matches §17.3 and its id is
// the envelope object. The actor's envelope signature was verified
// against that peer's actor at receipt. Here, again: the peer is enabled
// and connected with both profiles, the actor and the object are on the
// peer's origin, and the object is an asset URI of that origin.
//
// In one transaction, under the same per-asset lock bind takes (so an
// announcement and a withdrawal of one asset never interleave):
//
//  1. The tombstone is kept once per (peer, asset)
//     (federation_received_tombstones). A repeated or second tombstone of
//     the same asset changes nothing: the first is kept.
//  2. Every pending or complete record of that asset from that peer
//     becomes withdrawn: its own pins are released
//     (federation_received_asset_release_pins: no other pin owner is
//     touched), its originals are marked withdrawn, and one ledger event
//     (deaccession, success) is written. A rejected record holds nothing
//     and stays as it is.
//
// A tombstone that arrives before any announcement of the asset is kept
// the same way, with no record to change; bind refuses every later
// announcement of that asset from that peer (reason withdrawn), so a
// delayed or replayed announcement never resurrects it. An import in
// flight when the withdrawal commits finds the record withdrawn in its own
// commit (complete locks the record and requires it pending) and keeps
// nothing. The withdrawal needs no live grant: it only takes access away.
package assetingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// StateWithdrawn is a record its origin withdrew (§17.3).
const StateWithdrawn = "withdrawn"

// ReasonWithdrawn is a withdrawn record's reason, and the reason a later
// announcement of a withdrawn asset is refused.
const ReasonWithdrawn = "withdrawn"

// EventDeaccession is the ledger event of a withdrawal.
const EventDeaccession = "deaccession"

// Withdrawal is what Withdraw decided for one tombstone.
type Withdrawal struct {
	// Records are the records withdrawn now (none for a tombstone kept
	// before any announcement, or a repeated tombstone).
	Records []uuid.UUID
	// Duplicate: the asset's tombstone was already kept.
	Duplicate bool
}

// Withdraw applies the tombstone in env from peerID; see the file header.
func (s *Service) Withdraw(ctx context.Context, peerID uuid.UUID, env *federation.Envelope) (Withdrawal, error) {
	if env == nil || peerID == uuid.Nil {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New("no envelope or peer")}
	}
	p, err := s.d.Peers.ByID(ctx, peerID)
	if err != nil {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonPeerUnavailable, Err: err}
	}
	if !p.Enabled || !p.Connected {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonPeerUnavailable}
	}
	if !profilesInEffect(p.Capabilities) {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonProfile,
			Err: fmt.Errorf("%w: archival-1 and original-transfer-1 are not both negotiated", federation.ErrProfileNotNegotiated)}
	}
	if env.Type != federation.ActivityDelete {
		return Withdrawal{}, &Error{Reason: ReasonUnsupportedActivity, Err: fmt.Errorf("%s is not a withdrawal", env.Type)}
	}
	if err := federation.ValidateArchivalMembers(env, true); err != nil {
		return Withdrawal{}, &Error{Reason: ReasonArchivalInvalid, Err: err}
	}
	raw, ok := env.Extra[federation.MemberArchivalTombstone]
	if !ok {
		return Withdrawal{}, &Error{Reason: ReasonArchivalInvalid, Err: errors.New("no aa:tombstone")}
	}
	t, err := federation.ParseArchivalTombstone(raw)
	if err != nil {
		return Withdrawal{}, &Error{Reason: ReasonArchivalInvalid, Err: err}
	}
	peerOrigin := federation.OriginOf(p.InstanceURL)
	if peerOrigin == "" || federation.OriginOf(env.Actor) != peerOrigin {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonForeignOrigin, Err: errors.New("the actor is not on the peer's origin")}
	}
	if _, err := originAssetID(env.Object, peerOrigin); err != nil {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonForeignOrigin, Err: err}
	}

	var out Withdrawal
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1574))`,
			"asset "+peerID.String()+" "+env.Object); err != nil {
			return err
		}
		var reason *string
		if t.Reason != "" {
			reason = &t.Reason
		}
		tag, err := tx.Exec(ctx, `INSERT INTO federation_received_tombstones
			(peer_id, origin_asset_uri, activity_uri, actor_uri, tombstone, deleted, reason, last_version)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
			ON CONFLICT (peer_id, origin_asset_uri) DO NOTHING`,
			peerID, env.Object, env.ID, env.Actor, []byte(raw), t.Deleted, reason, t.LastVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out.Duplicate = true
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT id, attempts FROM federation_received_assets
			WHERE peer_id = $1 AND origin_asset_uri = $2 AND state IN ('pending', 'complete')
			ORDER BY id FOR UPDATE`, peerID, env.Object)
		if err != nil {
			return err
		}
		type rec struct {
			id       uuid.UUID
			attempts int32
		}
		var recs []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.attempts); err != nil {
				rows.Close()
				return err
			}
			recs = append(recs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range recs {
			if _, err := tx.Exec(ctx, `SELECT federation_received_asset_release_pins(ARRAY[$1::text])`, r.id.String()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE federation_received_assets
				SET state = 'withdrawn', reason = 'withdrawn', withdrawn_at = now(), updated_at = now()
				WHERE id = $1`, r.id); err != nil {
				return err
			}
			// After the record: an original is withdrawn only with its record.
			if _, err := tx.Exec(ctx, `UPDATE federation_received_asset_files SET state = 'withdrawn'
				WHERE received_asset_id = $1 AND role = 'original' AND state IN ('pending', 'verified')`, r.id); err != nil {
				return err
			}
			if err := s.writeEvents(ctx, tx, r.id, r.attempts, StateWithdrawn,
				event{typ: EventDeaccession, outcome: OutcomeSuccess}); err != nil {
				return err
			}
			out.Records = append(out.Records, r.id)
		}
		return nil
	})
	if err != nil {
		return Withdrawal{}, &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
	}
	for _, id := range out.Records {
		s.auditRecord(ctx, audit.EventFederationAssetIngestWithdrawn,
			recordRef{peerID: peerID, activityURI: env.ID, originAssetURI: env.Object, versionURI: t.LastVersion},
			id, ReasonWithdrawn, nil)
	}
	return out, nil
}

// tombstoneOf is the kept tombstone of (peer, asset), or nil.
func tombstoneOf(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, peerID uuid.UUID, assetURI string) (json.RawMessage, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT tombstone FROM federation_received_tombstones
		WHERE peer_id = $1 AND origin_asset_uri = $2`, peerID, assetURI).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}
