// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Later versions of a received asset (#1575, spec v1 §17.2.1).
//
// Every version of an asset this instance receives is its own immutable
// record. The asset's versions are its records that are not rejected:
// at most one complete (the version shown), superseded ones (earlier
// versions a verified successor replaced, kept with their originals
// verified and pinned) and pending ones (being received, or waiting).
// The database holds the chain (migration 00092): one record per version
// number and per version id, one complete, one first version, one
// successor per version, and a successor links the record one number
// below it with the version id it names.
//
// # Where an announced version goes (bind, place)
//
//  1. Its version id is taken: the same immutable version (number, file
//     set, predecessor and creation time, sameVersion) is a duplicate (the
//     stored record is the outcome, whatever its state); anything else is
//     version_conflict (§17.2.1: a version is immutable once announced,
//     and a sender never sends a different file set under a version id it
//     already announced). Descriptive fields are not part of the version
//     (§17.2.1: an Update that changes only them keeps the version), so a
//     new announcement differing only in them is a duplicate, and the
//     stored record is kept as it was.
//  2. Its number is taken by another version id: version_conflict.
//  3. The asset has no version here: it is the first version received
//     (any number; a share may start at version 3), with no predecessor.
//  4. Older than the first version held: version_stale. Nothing ever
//     goes back to an older version. When the first version was refused
//     and only waiting successors remain, an older version becomes the
//     new first version instead.
//  5. Otherwise it is a successor: it follows the version named by its
//     predecessor, which has a lower number. A sender passes over a
//     version that stayed a draft (never announced), so the two numbers
//     may be more than one apart (00093). The chain must stay consistent
//     with what is held, or the successor is version_conflict:
//     - a held predecessor is lower than the successor, and no held
//     version lies between the two;
//     - while the predecessor is not held, no other version is held one
//     number below the successor (the sender's predecessor would be it);
//     - no held later version follows the same predecessor, or follows a
//     held version below the successor (passing over it), and one that
//     follows the successor has nothing held between the two.
//     If the predecessor is complete, the successor is received like a
//     first version; if not (still being received, or not announced
//     yet), it is recorded pending, awaiting_predecessor.
//
// The tier, size and grant checks then apply as to any announcement.
//
// # Waiting, and activation
//
// A waiting successor fetches nothing and uses no attempt (awaitPredecessor,
// before an attempt is counted). It is checked again after a backoff (as
// awaiting_grant is), woken when its predecessor completes (wakeSuccessor,
// in that completion's transaction), and rejected predecessor_missing
// when Config.PredecessorWait from received_at runs out.
//
// A successor is fetched while the current version stays complete and
// readable. Its completion (complete, under the asset's lock) checks
// again that the predecessor is still the asset's complete version
// (lockPredecessor), then in ONE transaction marks the predecessor
// superseded and the successor complete, linked to it. A withdrawal,
// a revoked grant or a failed fetch before that leaves the current
// version as it was; a withdrawal withdraws every version.
package assetingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// version is one record of the asset's versions.
type version struct {
	rec            record
	uri            string
	number         int64
	digest         string
	predecessorURI string
	// The version's own claims as recorded (aa:asset version.predecessor
	// and version.created), "" when absent.
	claimedPredecessor, created string
}

// sameVersion reports whether the announced version is the recorded one:
// the same number, file set, predecessor and creation time. created is
// compared as an instant (both are validated RFC 3339 times), so the same
// time written another way is the same claim.
func sameVersion(v version, an *announced) bool {
	pred := ""
	if p := an.asset.Version.Predecessor; p != nil {
		pred = *p
	}
	if v.number != an.asset.Version.Number || v.digest != an.digest || v.claimedPredecessor != pred {
		return false
	}
	if v.created == an.asset.Version.Created {
		return true
	}
	a, errA := time.Parse(time.RFC3339Nano, v.created)
	b, errB := time.Parse(time.RFC3339Nano, an.asset.Version.Created)
	return errA == nil && errB == nil && a.Equal(b)
}

// placement is where bind puts an announced version.
type placement struct {
	// duplicate: the version is already recorded; its record decides.
	duplicate *record
	// refusal: recorded as rejected with this reason.
	refusal string
	// predecessorURI: the version the new record follows ("" for a first
	// version); predecessorReady: that version is complete here.
	predecessorURI   string
	predecessorReady bool
}

// place decides where an announced version goes among the asset's
// versions (steps 1 to 5 of the file header). It runs in bind's
// transaction, under the asset's lock.
func (s *Service) place(ctx context.Context, tx pgx.Tx, an *announced) (placement, error) {
	rows, err := tx.Query(ctx, `SELECT `+recordCols+`, version_uri, version_number, file_set_sha256,
		    COALESCE(predecessor_version_uri, ''),
		    COALESCE(record->'version'->>'predecessor', ''), COALESCE(record->'version'->>'created', '')
		  FROM federation_received_assets
		 WHERE peer_id = $1 AND origin_asset_uri = $2 AND state <> 'rejected'
		 ORDER BY version_number`, an.peer.ID, an.env.Object)
	if err != nil {
		return placement{}, err
	}
	var vs []version
	for rows.Next() {
		var v version
		r, err := scanRecord(rows, &v.uri, &v.number, &v.digest, &v.predecessorURI, &v.claimedPredecessor, &v.created)
		if err != nil {
			rows.Close()
			return placement{}, err
		}
		v.rec = r
		vs = append(vs, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return placement{}, err
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].number < vs[j].number })
	n := an.asset.Version.Number
	for _, v := range vs {
		if v.uri == an.asset.Version.ID {
			if sameVersion(v, an) {
				r := v.rec
				return placement{duplicate: &r}, nil
			}
			return placement{refusal: ReasonVersionConflict}, nil
		}
	}
	for _, v := range vs {
		if v.number == n {
			return placement{refusal: ReasonVersionConflict}, nil
		}
	}
	if len(vs) == 0 {
		return placement{predecessorReady: true}, nil
	}
	var first *version
	for i := range vs {
		if vs[i].predecessorURI == "" {
			first = &vs[i]
		}
	}
	switch {
	case first != nil && n < first.number:
		return placement{refusal: ReasonVersionStale}, nil
	case first == nil && n < vs[0].number:
		// Only waiting successors remain (their first version was
		// refused): an older version becomes the first version.
		return placement{predecessorReady: true}, nil
	}
	pred := an.asset.Version.Predecessor
	if pred == nil || *pred == "" {
		// ValidateArchivalMembers requires a predecessor above version 1,
		// and every number here is above the first version's.
		return placement{refusal: ReasonArchivalInvalid}, nil
	}
	held := func(uri string) *version {
		for i := range vs {
			if vs[i].uri == uri {
				return &vs[i]
			}
		}
		return nil
	}
	pl := placement{predecessorURI: *pred}
	if p := held(*pred); p != nil {
		if p.number >= n {
			return placement{refusal: ReasonVersionConflict}, nil
		}
		for _, v := range vs {
			if v.number > p.number && v.number < n {
				return placement{refusal: ReasonVersionConflict}, nil
			}
		}
		pl.predecessorReady = p.rec.state == StateComplete
	} else {
		for _, v := range vs {
			if v.number == n-1 {
				return placement{refusal: ReasonVersionConflict}, nil
			}
		}
	}
	for _, v := range vs {
		if v.number <= n || v.predecessorURI == "" {
			continue
		}
		switch v.predecessorURI {
		case an.asset.Version.ID:
			for _, w := range vs {
				if w.number > n && w.number < v.number {
					return placement{refusal: ReasonVersionConflict}, nil
				}
			}
		case *pred:
			return placement{refusal: ReasonVersionConflict}, nil
		default:
			if q := held(v.predecessorURI); q != nil && q.number < n {
				return placement{refusal: ReasonVersionConflict}, nil
			}
		}
	}
	return pl, nil
}

// awaitPredecessor decides, before an attempt is counted, whether a
// pending successor waits for its predecessor (file header). decided is
// false when the predecessor is complete here (the attempt runs);
// otherwise out and err are the outcome: still waiting, rejected because
// the wait ran out or the chain moved past it, or the record as another
// attempt left it.
func (s *Service) awaitPredecessor(ctx context.Context, st stored, id uuid.UUID) (out Outcome, decided bool, err error) {
	const (
		proceed = iota
		wait
		timedOut
		passed
		notPending
	)
	verdict := proceed
	var attempts int32
	terr := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		verdict = proceed
		var predState string
		err := tx.QueryRow(ctx, `SELECT state FROM federation_received_assets
			WHERE peer_id = $1 AND origin_asset_uri = $2 AND version_uri = $3 AND version_number < $4
			  AND state <> 'rejected'`,
			st.ref.peerID, st.ref.originAssetURI, st.predecessorURI, st.versionNumber).Scan(&predState)
		switch {
		case err == nil && predState == StateComplete:
			return nil
		case err == nil && predState == StateSuperseded:
			// Another version already followed it: this one never will.
			verdict = passed
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var reason string
		var expired bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(reason, ''), attempts, received_at + make_interval(secs => $2) <= now()
			  FROM federation_received_assets WHERE id = $1 AND state = 'pending' FOR UPDATE`,
			id, s.cfg.PredecessorWait.Seconds()).Scan(&reason, &attempts, &expired)
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
			WHERE id = $1`, id, s.cfg.PredecessorWait.Seconds(), ReasonAwaitingPredecessor); err != nil {
			return err
		}
		verdict = wait
		// One ledger row when the wait begins, not one per check.
		if reason != ReasonAwaitingPredecessor {
			return s.writeEvents(ctx, tx, id, attempts, StatePending,
				event{typ: EventIngestion, outcome: OutcomeWarning, reason: ReasonAwaitingPredecessor})
		}
		return nil
	})
	if terr != nil {
		return Outcome{RecordID: id, State: StatePending}, true,
			&Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: terr}
	}
	switch verdict {
	case wait:
		return Outcome{RecordID: id, State: StatePending, Reason: ReasonAwaitingPredecessor}, true,
			&Error{Reason: ReasonAwaitingPredecessor, Retryable: true, Err: errors.New("waiting for the predecessor version")}
	case timedOut:
		out, err := s.fail(ctx, st.ref, id, attempts, ReasonPredecessorMissing, false,
			errors.New("the predecessor version did not complete within the wait"), nil)
		return out, true, err
	case passed:
		out, err := s.fail(ctx, st.ref, id, attempts, ReasonVersionConflict, false,
			fmt.Errorf("version %s was already followed by another version", st.predecessorURI), nil)
		return out, true, err
	case notPending:
		out, err := s.current(ctx, id)
		return out, true, err
	}
	return Outcome{}, false, nil
}

// lockPredecessor returns the record of the successor's predecessor,
// locked, inside the completing transaction: it must still be the asset's
// complete version. A predecessor that is not complete (any longer)
// keeps the successor pending, waiting; one another version followed
// refuses it.
func lockPredecessor(ctx context.Context, tx pgx.Tx, an *announced) (uuid.UUID, error) {
	var id uuid.UUID
	var state string
	err := tx.QueryRow(ctx, `SELECT id, state FROM federation_received_assets
		WHERE peer_id = $1 AND origin_asset_uri = $2 AND version_uri = $3 AND version_number < $4
		  AND state <> 'rejected'
		FOR UPDATE`, an.peer.ID, an.env.Object, an.predecessorURI, an.asset.Version.Number).Scan(&id, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, &Error{Reason: ReasonAwaitingPredecessor, Retryable: true, Err: errors.New("the predecessor version is not held")}
	case err != nil:
		return uuid.Nil, err
	case state == StateSuperseded:
		return uuid.Nil, &Error{Reason: ReasonVersionConflict, Err: errors.New("the predecessor version was already followed by another version")}
	case state != StateComplete:
		return uuid.Nil, &Error{Reason: ReasonAwaitingPredecessor, Retryable: true, Err: fmt.Errorf("the predecessor version is %s", state)}
	}
	return id, nil
}

// wakeSuccessor makes the successor waiting for the version an just
// completed due now, inside that completion's transaction. The worker
// claims again after every record it runs, so it runs next.
func wakeSuccessor(ctx context.Context, tx pgx.Tx, an *announced) error {
	_, err := tx.Exec(ctx, `UPDATE federation_received_assets SET next_attempt_at = now(), updated_at = now()
		WHERE state = 'pending' AND reason = $3 AND peer_id = $1 AND origin_asset_uri = $2
		  AND version_number > $4 AND predecessor_version_uri = $5`,
		an.peer.ID, an.env.Object, ReasonAwaitingPredecessor, an.asset.Version.Number, an.asset.Version.ID)
	return err
}
