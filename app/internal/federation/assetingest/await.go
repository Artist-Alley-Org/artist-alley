// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// Waiting for a grant (#1574 slice 3).
//
// The sender announces an asset only after its aa:Share was delivered,
// but this instance's inbox dispatches the two independently, so the
// announcement can be bound before the share is applied (a transient
// dispatch failure, or a reordered batch). The import is fetched as this
// instance, so the decision is, in order:
//
//  1. Authorised: inboundgrants.AllowsTx accepts the original through
//     either a live, unexpired peer-wide asset grant (kind asset, no
//     target user, no recipient) carrying allow_original, or a live,
//     unexpired peer-wide collection grant carrying allow_original whose
//     recorded membership lists the asset. The import proceeds.
//  2. Refused: neither authorises, but the peer has a direct row of the
//     peer-wide asset tuple for the asset (a live grant without the
//     original, or one that is revoked, superseded, expired or
//     tombstoned). That is a decision the origin already made, so it is
//     refused at once (no_inbound_grant), as before.
//  3. Waiting: neither authorises and there is no such direct row. The
//     record waits for its asset grant:
//       - reason awaiting_grant, nothing fetched, no attempt counted;
//       - checked again by the worker after a backoff (half the time it
//         has waited, from 1 minute to 1 hour), never later than
//         received_at + Config.GrantWait, and each check starts again
//         from step 1 (so a collection grant with a recorded membership
//         that has since arrived authorises it then);
//       - woken at once only when ApplyShare records a row of the
//         peer-wide asset tuple for exactly that peer and asset
//         (WakeAwaiting, installed as the grant store's ArrivalHook),
//         live or ended on arrival;
//       - rejected as no_inbound_grant when GrantWait runs out.
//
// A grant to one local user, or a direct row of another kind for the same
// URI, neither ends the wait nor wakes it. The decision is taken under the
// per-peer lock ApplyShare holds (LockPeerTx), so a grant committed
// meanwhile is either seen by the decision or finds the record waiting
// and wakes it: no wake is lost.

// awaitNextSQL is the next check of a waiting record ($2 = GrantWait in
// seconds), for an UPDATE of federation_received_assets.
const awaitNextSQL = `LEAST(now() + GREATEST(LEAST((now() - received_at) / 2, interval '1 hour'), interval '1 minute'),
	received_at + make_interval(secs => $2))`

// grantRowExists reports whether the peer has a direct row of the
// peer-wide asset tuple for objectURI (kind asset, no target user, no
// recipient), in any state. Asked only when AllowsTx authorised nothing:
// such a row then refuses the import (step 2 above), and its absence makes
// the record wait (step 3).
func grantRowExists(ctx context.Context, tx pgx.Tx, peerID uuid.UUID, objectURI string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_inbound_grants
		WHERE peer_id = $1 AND object_uri = $2 AND object_kind = 'asset'
		  AND target_user_uri IS NULL AND recipient_user_ref IS NULL)`,
		peerID, objectURI).Scan(&ok)
	return ok, err
}

// awaitGrant decides, before an attempt is counted, whether a pending
// record waits for its grant (steps 1 to 3 above). decided is false when the grant allows the
// fetch or a grant row exists (the normal path then runs); otherwise out
// and err are the outcome: still waiting, rejected because the wait ran
// out, or the record as another attempt left it.
func (s *Service) awaitGrant(ctx context.Context, st stored, id uuid.UUID, signer casfetch.Signer) (out Outcome, decided bool, err error) {
	q := inboundgrants.Query{PeerID: st.ref.peerID, ActorURI: signer.ActorURI, ObjectURI: st.ref.originAssetURI,
		Class: inboundgrants.ClassOriginal}
	const (
		proceed = iota
		wait
		timedOut
		notPending
	)
	verdict := proceed
	var attempts int32
	terr := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		verdict = proceed
		if err := inboundgrants.LockPeerTx(ctx, tx, st.ref.peerID); err != nil {
			return err
		}
		if _, err := s.d.Grants.AllowsTx(ctx, tx, q); err == nil {
			return nil
		} else if !errors.Is(err, inboundgrants.ErrNoGrant) {
			return err
		}
		exists, err := grantRowExists(ctx, tx, st.ref.peerID, st.ref.originAssetURI)
		if err != nil || exists {
			return err
		}
		if s.inAwait != nil {
			s.inAwait()
		}
		var reason string
		var expired bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(reason, ''), attempts, received_at + make_interval(secs => $2) <= now()
			  FROM federation_received_assets WHERE id = $1 AND state = 'pending' FOR UPDATE`,
			id, s.cfg.GrantWait.Seconds()).Scan(&reason, &attempts, &expired)
		if errors.Is(err, pgx.ErrNoRows) {
			verdict = notPending
			return nil
		}
		if err != nil {
			return err
		}
		if expired {
			verdict = timedOut
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_received_assets
			SET reason = $3, next_attempt_at = `+awaitNextSQL+`, updated_at = now()
			WHERE id = $1`, id, s.cfg.GrantWait.Seconds(), ReasonAwaitingGrant); err != nil {
			return err
		}
		verdict = wait
		// One ledger row when the wait begins, not one per check.
		if reason != ReasonAwaitingGrant {
			return s.writeEvents(ctx, tx, id, attempts, StatePending,
				event{typ: EventIngestion, outcome: OutcomeWarning, reason: ReasonAwaitingGrant})
		}
		return nil
	})
	if terr != nil {
		return Outcome{RecordID: id, State: StatePending}, true,
			&Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: terr}
	}
	switch verdict {
	case wait:
		return Outcome{RecordID: id, State: StatePending, Reason: ReasonAwaitingGrant}, true,
			&Error{Reason: ReasonAwaitingGrant, Retryable: true, Err: errors.New("waiting for the inbound grant")}
	case timedOut:
		out, err := s.fail(ctx, st.ref, id, attempts, casfetch.ReasonNoInboundGrant, false,
			errors.New("no inbound grant arrived within the wait"), nil)
		return out, true, err
	case notPending:
		out, err := s.current(ctx, id)
		return out, true, err
	}
	return Outcome{}, false, nil
}

// WakeAwaiting makes the records waiting for the arrived grant's tuple due
// now, inside tx (the transaction recording that grant). It is the grant
// store's ArrivalHook.InTx and is exact: only a row of the peer-wide asset
// tuple wakes anything (a grant to one local user, or of another kind,
// wakes nothing), and only that peer's records for that asset that are
// waiting for their grant. The worker's Wake (ArrivalHook.After) then runs
// them.
func WakeAwaiting(ctx context.Context, tx pgx.Tx, a inboundgrants.Arrival) error {
	if a.ObjectKind != string(federation.ShareObjectKindAsset) || a.TargetUserURI != "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE federation_received_assets SET next_attempt_at = now(), updated_at = now()
		WHERE state = 'pending' AND reason = $3 AND peer_id = $1 AND origin_asset_uri = $2`,
		a.PeerID, a.ObjectURI, ReasonAwaitingGrant)
	return err
}
