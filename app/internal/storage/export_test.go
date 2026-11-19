// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"

	"github.com/google/uuid"
)

// SetImportAfterLockHook runs f once ImportVerified holds the live-import
// lock, before it records its intent. Tests only.
func (s *Service) SetImportAfterLockHook(f func()) { s.importHooks.afterLock = f }

// SetVariantAfterPutHook runs f after PutVariant's backend Put and before
// it records the digest, while the variant's write lock is held. Tests
// only (#1597).
func (s *Service) SetVariantAfterPutHook(f func(variant string)) { s.variantHooks.afterPut = f }

// VerifyChecksumBatch runs one checksum-sweep batch of run runID from
// the (cursor, cursorVariant) resume point WITHOUT Handle's checkpoint:
// exactly what a batch leaves behind when the process stops before
// AdvanceSweepRun, and what its retry repeats. Tests only (#1598).
func (h *SweepHandler) VerifyChecksumBatch(ctx context.Context, runID uuid.UUID, cursor, cursorVariant string) (nextCursor, nextVariant string, scanned, found int64, err error) {
	next, scanned, found, err := h.verifyChecksums(ctx, sweepPayload{RunID: runID, Cursor: cursor, CursorVariant: cursorVariant})
	return next.Cursor, next.CursorVariant, scanned, found, err
}

// SetEventAfterRefsHook runs f inside RecordSweepObservation's
// transaction, after it has listed the assets referencing the object and
// before it inserts their events. Tests only (#1598).
func (s *Service) SetEventAfterRefsHook(f func(objectHash string)) { s.eventHooks.afterRefs = f }
