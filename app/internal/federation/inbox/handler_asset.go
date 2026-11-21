// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// aa:AssetVersion scheduled for the asset receiver (#1574, spec v1
// §17.1, §8.5).
//
// Reached only for a peer whose stored intersection holds archival-1
// and original-transfer-1 (registry.go profileGated, checked at receipt
// and again here at dispatch), after federation.ValidateMembers: the
// aa:asset member matches §17.2 and names the envelope object, and the
// object is on the actor's origin; the actor's envelope signature was
// verified against that peer's actor at receipt (stage 10b).
//
// The dispatcher never transfers a file. The handler hands the
// envelope to an AssetScheduler, which binds it to a received asset
// record in one short database transaction (no network): a pending
// record IS the durable import task, run later by the receiver's own
// worker with bounded concurrency and retries. One record exists per
// (peer, activity) and per live (peer, asset), so a redelivery or a
// second announcement of the same version adds no task and never
// refetches a complete record.
//
// Like the share handlers, a decision recorded on the record (pending,
// complete, or a recorded refusal such as no_inbound_grant) marks the
// inbox row processed: the record and the audit (federation.
// asset_ingest.*) hold the outcome, and the sender already had its 202.
// A refusal that stored no record (a profile, origin or member check
// failed again here) marks the row rejected with its §12.1 reason; a
// transient failure leaves it pending for the dispatcher's own retry.

package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// AssetScheduled is what the asset receiver decided for one
// announcement, without transferring anything.
type AssetScheduled struct {
	// RecordID is the received asset record the announcement is bound
	// to, or uuid.Nil when it was refused before any record existed.
	RecordID uuid.UUID
	// State is the record's state: "pending" (an import task is
	// scheduled), "complete" (already held; nothing is fetched again) or
	// "rejected". "" when no record exists.
	State string
	// Reason is the receiver's typed reason for a refusal.
	Reason string
	// Retryable means nothing was decided: the dispatcher retries.
	Retryable bool
}

// AssetScheduler binds an aa:AssetVersion to a received asset record and
// schedules its import. Production wires assetingest.Worker.
type AssetScheduler interface {
	ScheduleAsset(ctx context.Context, peerID uuid.UUID, env *federation.Envelope) (AssetScheduled, error)
}

// SetAssetScheduler wires the asset receiver. Unwired, every
// aa:AssetVersion fails (retried, then terminal): nothing is recorded as
// processed without its record.
func (d *Dispatcher) SetAssetScheduler(s AssetScheduler) { d.assetScheduler = s }

// AssetWithdrawer applies an origin's withdrawal of an asset: a Delete of
// an aa:Asset carrying aa:tombstone (#1575, spec v1 §17.3). Production
// wires assetingest.Worker.
type AssetWithdrawer interface {
	WithdrawAsset(ctx context.Context, peerID uuid.UUID, env *federation.Envelope) (AssetScheduled, error)
}

// SetAssetWithdrawer wires the withdrawal receiver. Unwired, every asset
// Delete fails (retried, then terminal): a withdrawal is never recorded as
// processed without being applied.
func (d *Dispatcher) SetAssetWithdrawer(w AssetWithdrawer) { d.assetWithdrawer = w }

// isAssetWithdrawal: a Delete carrying aa:tombstone (ValidateMembers
// admitted it only as an asset withdrawal under archival-1).
func isAssetWithdrawal(env *federation.Envelope) bool {
	if env == nil || env.Type != federation.ActivityDelete {
		return false
	}
	_, ok := env.Extra[federation.MemberArchivalTombstone]
	return ok
}

// assetWithdrawal applies one asset Delete. A tombstone kept (with or
// without a record to withdraw, or already kept) marks the row processed;
// a refusal with nothing kept rejects it with its §12.1 reason; a
// transient failure leaves it for the dispatcher's retry.
func (d *Dispatcher) assetWithdrawal(ctx context.Context, env *federation.Envelope, peerID uuid.UUID) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
	if d.assetWithdrawer == nil {
		return OutcomeFailed, "", uuid.Nil, errors.New("inbox: no asset receiver wired for an asset Delete")
	}
	res, err := d.assetWithdrawer.WithdrawAsset(ctx, peerID, env)
	switch {
	case res.Retryable:
		if err == nil {
			err = errors.New("asset receiver: retry")
		}
		return OutcomeFailed, "", uuid.Nil, err
	case err == nil:
		return OutcomeProcessed, "", uuid.Nil, nil
	}
	return OutcomeRejected, AssetRejectStatus(res.Reason), uuid.Nil, fmt.Errorf("asset receiver %s: %w", res.Reason, err)
}

// AssetVersionHandler schedules one aa:AssetVersion; see the file header.
func AssetVersionHandler(d *Dispatcher) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, _ string, _ FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if d.assetScheduler == nil {
			return OutcomeFailed, "", uuid.Nil, errors.New("inbox: no asset receiver wired for aa:AssetVersion")
		}
		res, err := d.assetScheduler.ScheduleAsset(ctx, peerID, env)
		switch {
		case res.Retryable:
			if err == nil {
				err = errors.New("asset receiver: retry")
			}
			return OutcomeFailed, "", uuid.Nil, err
		case res.RecordID != uuid.Nil:
			return OutcomeProcessed, "", uuid.Nil, nil
		}
		if err == nil {
			err = errors.New("asset receiver: refused without a reason")
		}
		return OutcomeRejected, AssetRejectStatus(res.Reason), uuid.Nil, fmt.Errorf("asset receiver %s: %w", res.Reason, err)
	}
}

// AssetRejectStatus is the §12.1 reason for an asset receiver refusal
// that stored no record. The receiver's own reason is kept in the row's
// last_error.
func AssetRejectStatus(reason string) federation.InboxStatus {
	switch reason {
	case string(federation.InboxStatusProfileNotNegotiated):
		return federation.InboxStatusProfileNotNegotiated
	case string(federation.InboxStatusArchivalInvalid):
		return federation.InboxStatusArchivalInvalid
	case string(federation.InboxStatusUnsupportedActivity):
		return federation.InboxStatusUnsupportedActivity
	case "foreign_origin":
		return federation.InboxStatusInvalidObject
	case "peer_unavailable":
		return federation.InboxStatusPeerDisabled
	case "activity_conflict":
		// The id was recorded with another payload.
		return federation.InboxStatusReplay
	}
	return federation.InboxStatusError
}
