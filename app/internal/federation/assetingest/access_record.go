// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Access copy outcomes (#1597, spec v1 §16.7, §17.2.1, migration 00104).
//
// A received version's access entries are kept as file rows of its record
// (00086). This file records, for one such row, what an attempt to
// receive the copy established: verified (the copy is held, pinned under
// the record) or a bounded failure reason. It changes nothing else: not
// the record, not its originals or other files, and it fetches nothing.
// The transfer itself is casfetch (class access, version-bound route);
// the bounded background worker that admits, claims and retries access
// work (access_worker.go) is these primitives' production caller, and
// runs them inside its claim-fenced transactions.
//
// # The caller's contract (the eventual worker)
//
//  1. Load the work identity from the persisted row (LoadAccessWork) and
//     build the transfer request from it (AccessWork.Request), as the
//     record's own bound signer. Only an access row of the asset's
//     current complete version is work.
//  2. Fetch with casfetch. Its Result holds the verified bytes under the
//     caller's own temporary ingest pin. Fetch's own final origin HEAD
//     authorises only Fetch's storage step: the import or reuse
//     transaction and that temporary pin. It does not authorise
//     recording the copy against the version.
//  3. Authorise once more over the network, separately, immediately
//     before the recording transaction: a signed HEAD of the exact access
//     request (asset, version, hash, size, signer), the same origin check
//     / local commit boundary as §8.5. The access worker makes it with
//     casfetch.Client.RecheckAccess. No network call and no wait is made
//     while a database lock is held.
//  4. RecordAccessCopy with the same work and the Result. In one
//     transaction it re-reads and compares the work identity, checks the
//     tombstone, the peer (enabled, connected, same origin, all four
//     derived-transfer-1 profiles, FOR SHARE) and the live access-class
//     inbound grant (AllowsTx, never the original grant), and only then
//     moves the bytes from the caller's ingest pin to the record's pin
//     and marks the row verified.
//  5. Unless the outcome says Consumed, release the Result's ingest pin
//     (storage.Service.RemovePin, on a context that is not cancelled).
//     That covers every other exit: a refusal, a duplicate success, a
//     failed or cancelled transaction, and a commit whose result was lost
//     (if it did commit, the ingest pin is already gone and the release
//     changes nothing).
//  6. For a failed attempt, RecordAccessFailure with the casfetch reason.
//
// verified is assigned once: a second success for the same row is a
// duplicate (nothing is written; the caller releases its pin), and a
// failure never overwrites it. Both refuse once the record is no longer
// the current complete version (superseded, withdrawn): a superseded
// version keeps the copies it holds, but nothing new is recorded for it.
//
// Lock order, as complete, Withdraw and a successor's activation: the
// asset's advisory lock (namespace 1574, "asset <peer> <origin asset>"),
// the record row, then (here) its file row, the tombstone, the peer row,
// the grant row, and the storage object.
package assetingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// File states of an access row (federation_received_asset_files.state).
const (
	FileStateNotTransferred = "not_transferred"
	FileStateVerified       = "verified"
	FileStateWithdrawn      = "withdrawn"
)

// Refusals of the access recorders (not persisted).
const (
	// ReasonNotCurrentVersion: the record is no longer the asset's current
	// complete version (superseded, withdrawn), or never was.
	ReasonNotCurrentVersion = "not_current_version"
	// ReasonAccessWorkChanged: the persisted record or row is not the
	// work the caller names (another record, position, hash, size,
	// checksums, version, sensitivity or signer), or the row is not an
	// access entry.
	ReasonAccessWorkChanged = "access_work_changed"
)

// AccessFailureReasons are the reasons RecordAccessFailure stores: the
// casfetch transfer reasons, exactly the set migration 00104 allows in
// access_failure_reason.
var AccessFailureReasons = []string{
	casfetch.ReasonInvalidRequest, casfetch.ReasonPeerUnavailable, casfetch.ReasonProfile,
	casfetch.ReasonForeignOrigin, casfetch.ReasonForeignSigner, casfetch.ReasonClassNotAvailable,
	casfetch.ReasonSensitivity, casfetch.ReasonTooLarge, casfetch.ReasonNoInboundGrant,
	casfetch.ReasonOriginRefused, casfetch.ReasonRedirectRefused, casfetch.ReasonHTTPStatus,
	casfetch.ReasonLengthMismatch, casfetch.ReasonFixityMismatch, casfetch.ReasonTimeout,
	casfetch.ReasonTransport, casfetch.ReasonRevokedDuringFetch, casfetch.ReasonLeaseLost,
	casfetch.ReasonStorage,
}

// AccessWork is the persisted identity of one access copy: its record
// (one received version), the entry's position and declared facts, and
// the signer the record was bound for.
type AccessWork struct {
	RecordID       uuid.UUID
	PeerID         uuid.UUID
	OriginAssetURI string
	OriginAssetID  uuid.UUID
	VersionURI     string
	VersionNumber  int64
	Sensitivity    string
	Position       int
	SHA256         string
	Size           int64
	// Checksums is the declared list as stored (sha256 included).
	Checksums      []federation.ArchivalChecksum
	MediaType      string
	SignerKeyID    string
	SignerActorURI string // "" for the instance
}

// Request is the version-bound casfetch request for w, as signer, which
// must be the record's bound signer.
func (w AccessWork) Request(signer casfetch.Signer) (casfetch.Request, error) {
	if signer.KeyID != w.SignerKeyID || signer.ActorURI != w.SignerActorURI {
		return casfetch.Request{}, &Error{Reason: casfetch.ReasonForeignSigner, Err: errors.New("not the record's bound signer")}
	}
	return casfetch.Request{
		PeerID: w.PeerID, OriginAssetURI: w.OriginAssetURI, AssetID: w.OriginAssetID,
		SHA256: w.SHA256, Size: w.Size, ContentType: w.MediaType, Checksums: extraChecksums(w.Checksums),
		AnnouncedSensitivity: w.Sensitivity, Class: cas.ClassAccess, Version: w.VersionNumber, Signer: signer,
	}, nil
}

// AccessOutcome is what a recorder established for the row.
type AccessOutcome struct {
	// State is the row's state after the call.
	State      string
	VerifiedAt time.Time // when State is verified
	Reason     string    // the stored failure reason, when State is not_transferred
	// Duplicate: the row was already verified; nothing was written.
	Duplicate bool
	// Consumed: the caller's ingest pin was moved to the record's pin in
	// the committed transaction. When false, the caller still owns it.
	Consumed bool
}

// LoadAccessWork reads the work identity of the access entry at position
// of record id. It takes no lock; the recorders compare it again under
// their locks.
func (s *Service) LoadAccessWork(ctx context.Context, id uuid.UUID, position int) (AccessWork, error) {
	w := AccessWork{RecordID: id, Position: position}
	var role, state string
	var actor *string
	var cs []byte
	err := s.d.Pool.QueryRow(ctx, `SELECT r.peer_id, r.origin_asset_uri, r.origin_asset_id, r.version_uri, r.version_number,
		    r.sensitivity, r.signer_key_id, r.signer_actor_uri, r.state,
		    f.role, f.object_hash, f.size_bytes, f.checksums, f.declared_media_type
		  FROM federation_received_assets r
		  JOIN federation_received_asset_files f ON f.received_asset_id = r.id AND f.position = $2
		 WHERE r.id = $1`, id, position).Scan(&w.PeerID, &w.OriginAssetURI, &w.OriginAssetID, &w.VersionURI,
		&w.VersionNumber, &w.Sensitivity, &w.SignerKeyID, &actor, &state, &role, &w.SHA256, &w.Size, &cs, &w.MediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessWork{}, &Error{Reason: ReasonAccessWorkChanged, Err: errors.New("no such record or position")}
	}
	if err != nil {
		return AccessWork{}, err
	}
	if role != "access" {
		return AccessWork{}, &Error{Reason: ReasonAccessWorkChanged, Err: fmt.Errorf("position %d is a %s entry", position, role)}
	}
	if state != StateComplete {
		return AccessWork{}, &Error{Reason: ReasonNotCurrentVersion, Err: fmt.Errorf("the record is %s", state)}
	}
	if actor != nil {
		w.SignerActorURI = *actor
	}
	if err := json.Unmarshal(cs, &w.Checksums); err != nil {
		return AccessWork{}, fmt.Errorf("assetingest: stored checksums: %w", err)
	}
	return w, nil
}

// accessRow is the locked access row and its record.
type accessRow struct {
	state      string
	verifiedAt *time.Time
	reason     *string
}

// lockAccessRow takes the asset's lock, then the record and its row at
// w.Position, and compares them with w. It refuses unless the record is
// the current complete version and the row is the access entry w names.
func lockAccessRow(ctx context.Context, tx pgx.Tx, w AccessWork) (accessRow, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1574))`,
		"asset "+w.PeerID.String()+" "+w.OriginAssetURI); err != nil {
		return accessRow{}, err
	}
	var peerID, originAssetID uuid.UUID
	var assetURI, versionURI, sensitivity, signerKey, state string
	var versionNumber int64
	var signerActor *string
	err := tx.QueryRow(ctx, `SELECT peer_id, origin_asset_uri, origin_asset_id, version_uri, version_number,
		    sensitivity, signer_key_id, signer_actor_uri, state
		  FROM federation_received_assets WHERE id = $1 FOR UPDATE`, w.RecordID).Scan(&peerID, &assetURI, &originAssetID,
		&versionURI, &versionNumber, &sensitivity, &signerKey, &signerActor, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return accessRow{}, &Error{Reason: ReasonAccessWorkChanged, Err: errors.New("the record no longer exists")}
	}
	if err != nil {
		return accessRow{}, err
	}
	actor := ""
	if signerActor != nil {
		actor = *signerActor
	}
	if peerID != w.PeerID || assetURI != w.OriginAssetURI || originAssetID != w.OriginAssetID ||
		versionURI != w.VersionURI || versionNumber != w.VersionNumber || sensitivity != w.Sensitivity ||
		signerKey != w.SignerKeyID || actor != w.SignerActorURI {
		return accessRow{}, &Error{Reason: ReasonAccessWorkChanged, Err: errors.New("the record is not the named version, asset, tier, peer or signer")}
	}
	if state == StateWithdrawn {
		return accessRow{}, &Error{Reason: ReasonWithdrawn, Err: errors.New("the origin withdrew the asset")}
	}
	if state != StateComplete {
		return accessRow{}, &Error{Reason: ReasonNotCurrentVersion, Err: fmt.Errorf("the record is %s", state)}
	}
	cs, err := json.Marshal(w.Checksums)
	if err != nil {
		return accessRow{}, err
	}
	var row accessRow
	var role, hash, mediaType string
	var size int64
	var same bool
	err = tx.QueryRow(ctx, `SELECT role, object_hash, size_bytes, declared_media_type, checksums = $3::jsonb,
		    state, verified_at, access_failure_reason
		  FROM federation_received_asset_files WHERE received_asset_id = $1 AND position = $2 FOR UPDATE`,
		w.RecordID, w.Position, cs).Scan(&role, &hash, &size, &mediaType, &same, &row.state, &row.verifiedAt, &row.reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return accessRow{}, &Error{Reason: ReasonAccessWorkChanged, Err: errors.New("no file at that position")}
	}
	if err != nil {
		return accessRow{}, err
	}
	if role != "access" || hash != w.SHA256 || size != w.Size || mediaType != w.MediaType || !same {
		return accessRow{}, &Error{Reason: ReasonAccessWorkChanged, Err: errors.New("the row is not the named access entry")}
	}
	if tomb, err := tombstoneOf(ctx, tx, w.PeerID, w.OriginAssetURI); err != nil {
		return accessRow{}, err
	} else if tomb != nil {
		return accessRow{}, &Error{Reason: ReasonWithdrawn, Err: errors.New("the origin withdrew the asset")}
	}
	return row, nil
}

func (r accessRow) outcome(duplicate, consumed bool) AccessOutcome {
	o := AccessOutcome{State: r.state, Duplicate: duplicate, Consumed: consumed}
	if r.verifiedAt != nil {
		o.VerifiedAt = *r.verifiedAt
	}
	if r.reason != nil {
		o.Reason = *r.reason
	}
	return o
}

// RecordAccessCopy records res, the verified casfetch result of w's
// access request, as the row's verified copy (see the file header for the
// caller's contract). Unless the outcome says Consumed, the caller still
// owns res.Pin and must release it.
func (s *Service) RecordAccessCopy(ctx context.Context, w AccessWork, res *casfetch.Result) (AccessOutcome, error) {
	var out AccessOutcome
	err := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.recordAccessCopyTx(ctx, tx, w, res, nil)
		return err
	})
	if err != nil {
		return AccessOutcome{}, err
	}
	return out, nil
}

// accessFence checks, inside a recorder's transaction and right after the
// record and row are locked, that the caller may still write: the access
// worker's claim token, unexpired (access_worker.go). A nil fence is no
// check (RecordAccessCopy, RecordAccessFailure).
type accessFence func(ctx context.Context, tx pgx.Tx) error

// recordAccessCopyTx is RecordAccessCopy inside tx; fence (if any) runs
// before anything is written, the duplicate outcome included.
func (s *Service) recordAccessCopyTx(ctx context.Context, tx pgx.Tx, w AccessWork, res *casfetch.Result, fence accessFence) (AccessOutcome, error) {
	if res == nil || res.Hash != w.SHA256 || res.Size != w.Size || res.IngestID == uuid.Nil ||
		res.Pin.SubjectType != casfetch.IngestPinType || res.Pin.SubjectID != res.IngestID.String() {
		return AccessOutcome{}, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New("the result is not a verified fetch of this entry under its own ingest pin")}
	}
	var out AccessOutcome
	err := func() error {
		row, err := lockAccessRow(ctx, tx, w)
		if err != nil {
			return err
		}
		if fence != nil {
			if err := fence(ctx, tx); err != nil {
				return err
			}
		}
		switch row.state {
		case FileStateVerified:
			// Single assignment: an earlier success stands.
			out = row.outcome(true, false)
			return nil
		case FileStateNotTransferred:
		default:
			return &Error{Reason: ReasonNotCurrentVersion, Err: fmt.Errorf("the access copy is %s", row.state)}
		}
		// The receiver's peer record as it is now, held until commit.
		var instanceURL, status string
		var enabled bool
		var caps []string
		if err := tx.QueryRow(ctx, `SELECT instance_url, enabled, status,
			    ARRAY(SELECT jsonb_array_elements_text(capabilities))
			  FROM federation_peers WHERE id = $1 FOR SHARE`, w.PeerID).Scan(&instanceURL, &enabled, &status, &caps); err != nil {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: fmt.Errorf("peer: %w", err)}
		}
		if !enabled || status != "connected" {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: errors.New("the peer was disabled or disconnected")}
		}
		if o := federation.OriginOf(instanceURL); o == "" || o != federation.OriginOf(w.OriginAssetURI) {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: errors.New("the peer moved off the asset's origin")}
		}
		if !federation.DerivedTransferInEffect(func(p string) bool { return slices.Contains(caps, p) }) {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch,
				Err: fmt.Errorf("%w: the derived-transfer-1 profiles are not all negotiated", federation.ErrProfileNotNegotiated)}
		}
		// The live access-copy grant, held FOR SHARE until commit.
		if _, err := s.d.Grants.AllowsTx(ctx, tx, inboundgrants.Query{PeerID: w.PeerID, ActorURI: w.SignerActorURI,
			ObjectURI: w.OriginAssetURI, Class: inboundgrants.ClassAccess}); err != nil {
			if errors.Is(err, inboundgrants.ErrNoGrant) {
				return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: fmt.Errorf("%s: %w", casfetch.ReasonNoInboundGrant, err)}
			}
			return err
		}
		// The object, still stored at this size, locked and taken off any
		// pending GC before its pins change (as complete does).
		tag, err := tx.Exec(ctx, `UPDATE storage_objects SET gc_eligible_at = NULL
			WHERE hash = $1 AND size_bytes = $2`, w.SHA256, w.Size)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: errors.New("the access copy is no longer stored")}
		}
		q := storage.New(tx)
		if err := q.AddPin(ctx, storage.AddPinParams{ObjectHash: w.SHA256, PinSubjectType: PinSubjectType,
			PinSubjectID: w.RecordID.String()}); err != nil {
			return err
		}
		// Only this caller's own ingest pin, and only if it still holds it.
		tag, err = tx.Exec(ctx, `DELETE FROM storage_pins
			WHERE object_hash = $1 AND pin_subject_type = $2 AND pin_subject_id = $3`,
			w.SHA256, res.Pin.SubjectType, res.Pin.SubjectID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &Error{Reason: casfetch.ReasonStorage, Err: errors.New("the result's ingest pin is not held")}
		}
		var verifiedAt time.Time
		if err := tx.QueryRow(ctx, `UPDATE federation_received_asset_files
			SET state = 'verified', verified_at = now(), access_failure_reason = NULL
			WHERE received_asset_id = $1 AND position = $2 AND role = 'access' AND state = 'not_transferred'
			RETURNING verified_at`, w.RecordID, w.Position).Scan(&verifiedAt); err != nil {
			return err
		}
		out = AccessOutcome{State: FileStateVerified, VerifiedAt: verifiedAt, Consumed: true}
		return nil
	}()
	if err != nil {
		return AccessOutcome{}, err
	}
	return out, nil
}

// RecordAccessFailure stores reason, one of AccessFailureReasons, as the
// row's last failure. It changes only that row, and only while it is an
// unverified access entry of the current complete version: a verified copy
// is never overwritten (Duplicate, nothing written).
func (s *Service) RecordAccessFailure(ctx context.Context, w AccessWork, reason string) (AccessOutcome, error) {
	var out AccessOutcome
	err := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.recordAccessFailureTx(ctx, tx, w, reason, nil)
		return err
	})
	if err != nil {
		return AccessOutcome{}, err
	}
	return out, nil
}

// recordAccessFailureTx is RecordAccessFailure inside tx; fence (if any)
// runs before anything is written.
func (s *Service) recordAccessFailureTx(ctx context.Context, tx pgx.Tx, w AccessWork, reason string, fence accessFence) (AccessOutcome, error) {
	if !slices.Contains(AccessFailureReasons, reason) {
		return AccessOutcome{}, &Error{Reason: casfetch.ReasonInvalidRequest, Err: fmt.Errorf("%q is not an access failure reason", reason)}
	}
	var out AccessOutcome
	err := func() error {
		row, err := lockAccessRow(ctx, tx, w)
		if err != nil {
			return err
		}
		if fence != nil {
			if err := fence(ctx, tx); err != nil {
				return err
			}
		}
		switch row.state {
		case FileStateVerified:
			out = row.outcome(true, false)
			return nil
		case FileStateNotTransferred:
		default:
			return &Error{Reason: ReasonNotCurrentVersion, Err: fmt.Errorf("the access copy is %s", row.state)}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_received_asset_files SET access_failure_reason = $3
			WHERE received_asset_id = $1 AND position = $2 AND role = 'access' AND state = 'not_transferred'`,
			w.RecordID, w.Position, reason); err != nil {
			return err
		}
		out = AccessOutcome{State: FileStateNotTransferred, Reason: reason}
		return nil
	}()
	if err != nil {
		return AccessOutcome{}, err
	}
	return out, nil
}
