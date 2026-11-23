// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// An origin's Update of its asset's mature and aiProvenance flags (#1575,
// spec v1 §17.2.8), and the revisions every accepted aa:asset leaves.
//
// # Revisions (recordRevision)
//
// Every aa:asset this instance accepts of an asset, an aa:AssetVersion
// bound to a record that is not rejected and an Update admitted below,
// keeps one immutable revision (migration 00094): the activity's
// identity (peer, activity URI), its signed published, the version it
// named and that version's file set, and the flags it carried. The flags
// a record is shown with (receivedassets) are those of the asset's
// greatest revision by (published, activity URI). A sender makes each
// later aa:asset of an asset the greater one (assetsend), so every
// delivery order ends on the same flags. Nothing that decides access
// reads a revision: a record's tier is its own, and an Update never
// changes a record's state, so it can resurrect nothing.
//
// # Admission of an Update (ApplyFlagUpdate)
//
// After federation.ValidateMembers (aa:asset under archival-1, naming the
// envelope object, on the actor's own origin), in order:
//
//  1. The peer is enabled and connected with archival-1 and
//     original-transfer-1, and the actor and the object are on its origin
//     (validate, as for an aa:AssetVersion).
//  2. A restricted or embargoed member is refused (sensitivity_refused),
//     as for an aa:AssetVersion: neither is ever held here.
//  3. In one transaction, under the activity's and then the asset's lock
//     (bind's order, so an Update, an announcement and a withdrawal of one
//     asset never interleave):
//     - the same activity already kept: the identical Update is a
//     duplicate; anything else under that id is activity_conflict;
//     - the origin withdrew the asset (a tombstone is kept): refused
//     withdrawn, and nothing is kept;
//     - no live inbound grant for the asset's original from that peer
//     (the grant an aa:AssetVersion needs): refused no_inbound_grant;
//     - the version it names is held here (a record that is not
//     rejected) with another file set: version_conflict;
//     - otherwise the revision is kept, also before the version it names
//     arrives (the flags are the asset's; the version arriving later
//     finds them).
//
// An Update that also carries aa:metadata (asset-metadata-1, #1577,
// spec v1 §18) is admitted by the same steps, with the member validated
// first, the peer row re-read under the locks, and its snapshot kept with
// the revision in the same transaction (metadata.go). An Update without
// it is the flags-only Update above, unchanged.
package assetingest

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// FlagUpdate is what ApplyFlagUpdate decided for one Update.
type FlagUpdate struct {
	// RevisionID is the revision the Update is kept as (with its snapshot,
	// when it carried aa:metadata).
	RevisionID uuid.UUID
	// Duplicate: the Update was already kept.
	Duplicate bool
}

// recordRevision keeps an's revision, inside the binding transaction tx
// (file header). A redelivery of the same activity keeps nothing more.
// inserted reports whether this INSERT wrote the row (its command tag),
// not whether a row for the activity exists afterwards.
func recordRevision(ctx context.Context, tx pgx.Tx, an *announced) (inserted bool, err error) {
	mature := an.asset.Mature != nil && *an.asset.Mature
	var ai *string
	if an.asset.AIProvenance != "" {
		ai = &an.asset.AIProvenance
	}
	tag, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_revisions
		(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256,
		 mature, ai_provenance, record)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		ON CONFLICT (peer_id, activity_uri) DO NOTHING`,
		an.peer.ID, an.env.Object, an.env.ID, string(an.env.Type), an.env.Actor, an.env.Published,
		an.asset.Version.ID, an.digest, mature, ai, []byte(an.raw))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ApplyFlagUpdate admits one Update of an asset's flags (file header).
func (s *Service) ApplyFlagUpdate(ctx context.Context, a Announcement) (FlagUpdate, error) {
	an, err := s.validate(ctx, a, federation.ActivityUpdate)
	if err != nil {
		return FlagUpdate{}, s.refuseUpdate(ctx, a, err)
	}
	if an.env.Published.IsZero() {
		return FlagUpdate{}, s.refuseUpdate(ctx, a, &Error{Reason: ReasonArchivalInvalid, Err: errors.New("an Update of an asset needs its published time")})
	}
	if an.asset.Sensitivity != "public" && an.asset.Sensitivity != "team" {
		return FlagUpdate{}, s.refuseUpdate(ctx, a, &Error{Reason: casfetch.ReasonSensitivity, Err: errors.New("the asset is " + an.asset.Sensitivity)})
	}
	// #1577: an aa:metadata member is validated before anything is kept.
	meta, err := s.validateMetadata(an)
	if err != nil {
		return FlagUpdate{}, s.refuseUpdate(ctx, a, err)
	}
	var metaSnapshot []byte
	var metaSeconds int64
	var metaNanos int32
	if meta != nil {
		metaSnapshot, metaSeconds, metaNanos = meta.snapshot, meta.seconds, meta.nanos
	}
	var out FlagUpdate
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		for _, key := range []string{"activity " + an.peer.ID.String() + " " + an.env.ID,
			"asset " + an.peer.ID.String() + " " + an.env.Object} {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1574))`, key); err != nil {
				return err
			}
		}
		// The same activity is a duplicate only with the same aa:asset and,
		// when either delivery carries aa:metadata, the same snapshot and
		// published to the nanosecond (#1577). Its first revision, snapshot
		// and signature stay as kept.
		var same, hasMeta, sameMeta bool
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT r.id, (r.activity_type = $3 AND r.actor_uri = $4 AND r.origin_asset_uri = $5
		        AND r.published = $6 AND r.record = $7::jsonb),
		        m.revision_id IS NOT NULL,
		        COALESCE(m.snapshot = $8::jsonb AND m.published_seconds = $9 AND m.published_nanos = $10, FALSE)
		  FROM federation_received_asset_revisions r
		  LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.activity_uri = $2`,
			an.peer.ID, an.env.ID, string(federation.ActivityUpdate), an.env.Actor, an.env.Object, an.env.Published,
			[]byte(an.raw), metaSnapshot, metaSeconds, metaNanos).Scan(&id, &same, &hasMeta, &sameMeta)
		if err == nil {
			same = same && hasMeta == (meta != nil) && (meta == nil || sameMeta)
		}
		switch {
		case err == nil && same:
			out = FlagUpdate{RevisionID: id, Duplicate: true}
			return nil
		case err == nil:
			return &Error{Reason: ReasonActivityConflict, Err: errors.New("the activity id was recorded with another payload")}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var announced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2)`,
			an.peer.ID, an.env.ID).Scan(&announced); err != nil {
			return err
		}
		if announced {
			return &Error{Reason: ReasonActivityConflict, Err: errors.New("the activity id was recorded as an announcement")}
		}
		if tomb, err := tombstoneOf(ctx, tx, an.peer.ID, an.env.Object); err != nil {
			return err
		} else if tomb != nil {
			return &Error{Reason: ReasonWithdrawn, Err: errors.New("the origin withdrew the asset")}
		}
		if err := inboundgrants.LockPeerTx(ctx, tx, an.peer.ID); err != nil {
			return err
		}
		if meta != nil {
			if err := recheckMetadataPeer(ctx, tx, an); err != nil {
				return err
			}
		}
		if _, err := s.d.Grants.AllowsTx(ctx, tx, s.grantQuery(an)); err != nil {
			if errors.Is(err, inboundgrants.ErrNoGrant) {
				return &Error{Reason: casfetch.ReasonNoInboundGrant, Err: err}
			}
			return err
		}
		var held string
		err = tx.QueryRow(ctx, `SELECT file_set_sha256 FROM federation_received_assets
			WHERE peer_id = $1 AND origin_asset_uri = $2 AND version_uri = $3 AND state <> 'rejected'
			ORDER BY version_number LIMIT 1`, an.peer.ID, an.env.Object, an.asset.Version.ID).Scan(&held)
		switch {
		case err == nil && held != an.digest:
			return &Error{Reason: ReasonVersionConflict, Err: errors.New("the version it names is held here with another file set")}
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		inserted, err := recordRevision(ctx, tx, an)
		if err != nil {
			return err
		}
		// The revision and its snapshot are one admission: a snapshot is
		// attached only to the revision this INSERT wrote, and a failed
		// snapshot insert rolls both back.
		if meta != nil && !inserted {
			return &Error{Reason: ReasonActivityConflict, Err: errors.New("the revision for the activity was not written by this delivery")}
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
			an.peer.ID, an.env.ID).Scan(&out.RevisionID); err != nil {
			return err
		}
		if meta == nil {
			return nil
		}
		return insertMetadata(ctx, tx, out.RevisionID, meta)
	})
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
		}
		if e.Retryable {
			return FlagUpdate{}, e
		}
		return FlagUpdate{}, s.refuseUpdate(ctx, a, e)
	}
	return out, nil
}

// refuseUpdate audits a refused Update and returns err.
func (s *Service) refuseUpdate(ctx context.Context, a Announcement, err error) error {
	meta := map[string]any{"peer_id": a.PeerID.String(), "reason": ReasonOf(err), "error": err.Error(), "activity_type": "Update"}
	if a.Envelope != nil {
		meta["activity_uri"] = a.Envelope.ID
		meta["origin_asset_uri"] = a.Envelope.Object
	}
	s.audit(ctx, audit.EventFederationAssetIngestRejected, meta)
	return err
}
