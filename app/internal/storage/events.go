// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

// Per-asset preservation events from the checksum sweep (#1598, ADR
// 0101, spec v1 §17.4). Every observation of a stored (object, variant)
// becomes one append-only fixity-check event on EVERY asset that
// references the object (primary file, alternate or companion), so
// deduplicated bytes shared by two assets are in both histories.
//
// RecordSweepObservation writes the sweep finding (when there is one)
// and the events in ONE transaction, before the sweep advances its
// cursor. Both writes are idempotent (migration 00083): a batch retried
// after an interruption adds nothing, while a later run, a new
// generation of the variant, or a different result adds a new event. It
// runs on the pool AFTER VerifyVariant has released the variant's lock,
// so no second connection is taken under it (#1616).
//
// The event says what was observed, never more: success only for a
// recorded digest the bytes match on a verified row; a row with no
// recorded digest (original or derived) is a warning (no_recorded_digest)
// and stays unverifiable; bytes the backend could not deliver are a
// read_error failure that carries no error text. An observation that
// fits none of the rules is refused, not recorded.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// eventHooks are #1598 test seams; zero in production.
type eventHooks struct {
	afterRefs func(objectHash string) // after the reference lookup, before the inserts
}

// FixityAgentName is the software agent of locally recorded fixity
// checks (§17.4 agents[].software.name). Service.AgentVersion supplies
// the version; empty means the version is omitted (ADR 0101).
const FixityAgentName = "artist-alley"

// RecordSweepObservation persists one observation of sweep run runID:
// its finding (if any) and one fixity-check event per asset that
// references the object. It returns how many events were newly written.
// A Gone observation (nothing was checked) records nothing.
func (s *Service) RecordSweepObservation(ctx context.Context, runID uuid.UUID, obs VariantObservation) (int64, error) {
	if obs.Gone {
		return 0, nil
	}
	outcome, reason := obs.Outcome()
	if outcome == "" {
		return 0, fmt.Errorf("storage: observation of %s/%s has no truthful outcome (finding %q, status %q)",
			short(obs.ObjectHash), obs.VariantKey, obs.Finding, obs.StatusAfter)
	}
	run := pgtype.UUID{Bytes: runID, Valid: true}
	var written int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := New(tx)
		if obs.Finding != "" {
			if err := q.RecordSweepFinding(ctx, RecordSweepFindingParams{
				ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
				RunID:      run,
				Finding:    obs.Finding,
				ObjectHash: obs.ObjectHash,
				VariantKey: obs.VariantKey,
				Detail:     obs.Detail,
			}); err != nil {
				return fmt.Errorf("record finding: %w", err)
			}
		}
		assets, err := q.ListAssetsReferencingObject(ctx, obs.ObjectHash)
		if err != nil {
			return fmt.Errorf("list referencing assets: %w", err)
		}
		if s.eventHooks.afterRefs != nil {
			s.eventHooks.afterRefs(obs.ObjectHash)
		}
		// An asset deleted since the lookup gets no event (the insert
		// reads it FOR KEY SHARE and finds nothing); every other error
		// still fails the observation.
		p := InsertFixityEventParams{
			OccurredAt:         pgtype.Timestamptz{Time: obs.ObservedAt, Valid: !obs.ObservedAt.IsZero()},
			Outcome:            outcome,
			OutcomeReason:      reason,
			ObjectHash:         obs.ObjectHash,
			VariantKey:         obs.VariantKey,
			ObservedGeneration: obs.Generation,
			RecordedSha256:     strOrNil(obs.RecordedSHA256),
			FixityStatus:       obs.StatusAfter,
			SweepRunID:         run,
			AgentName:          FixityAgentName,
			AgentVersion:       strOrNil(s.AgentVersion),
		}
		if !p.OccurredAt.Valid {
			p.OccurredAt = pgtype.Timestamptz{Time: timeNow(), Valid: true}
		}
		if obs.Found && !obs.ReadFailed {
			alg, digest, size := "sha256", obs.ObservedSHA256, obs.ObservedSize
			p.Algorithm, p.ObservedSha256, p.ObservedSizeBytes = &alg, &digest, &size
		}
		for _, assetID := range assets {
			p.AssetID = assetID
			n, err := q.InsertFixityEvent(ctx, p)
			if err != nil {
				return fmt.Errorf("insert fixity event: %w", err)
			}
			written += n
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("storage: record sweep observation %s/%s: %w", short(obs.ObjectHash), obs.VariantKey, err)
	}
	return written, nil
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
