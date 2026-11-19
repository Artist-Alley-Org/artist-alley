// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Verified import of bytes received from a peer (#72).
//
// UploadOriginal cannot serve a peer transfer: it takes no expected hash
// or size and records the object before anything is compared. A
// transfer arrives with a hash and size the receiver must hold the
// bytes to, so ImportVerified takes both and refuses anything else.
//
// # Order, for crash safety
//
//  1. The staged file's size and SHA-256 are recomputed here and must
//     equal the expected values. A mismatch writes nothing at all: a
//     wrong file stored under a hash key would corrupt every user of
//     that hash, so nothing reaches the backend before this check.
//  2. Already held (a storage_objects row and a backend object of the
//     expected size): no Put. One transaction adds the pin and runs the
//     caller's hook.
//  3. Otherwise the importer takes the per-hash import lock, SHARED and
//     SESSION-level, on one dedicated connection, and holds it until
//     its commit has finished. Under it: the intent row is committed,
//     THEN the backend Put, THEN one transaction (on the same
//     connection) records the object, the variant and the pin, deletes
//     the intent and runs the caller's hook (the fetch lease release
//     and its checks). If the intent is gone by then, nothing is
//     recorded.
//
// # Alive or crashed
//
// The lock is what tells a live import from a crashed one, however
// slow the Put is: while any import of a hash is alive its session
// holds the shared lock; a crashed process's session ends and releases
// it. The reconciler (ReconcileImportIntents) only TRIES the lock,
// EXCLUSIVE, per hash with an old intent. If it cannot get it, an
// import of that hash is alive and the hash is skipped: nothing is
// deleted, however old the intent. If it gets it, every intent of the
// hash belongs to a dead import, so it deletes the backend bytes when no
// storage_objects row exists and drops the intents. An importer that
// arrives meanwhile waits for the shared lock and records its intent
// only after the reconciler has finished, so its own Put comes later.
//
// # Pins
//
// The pin the caller passes is an ingest pin, never an asset pin: the
// bytes are held, not shown. Ingest (#1574) converts it into the asset's
// pin in the transaction that creates the asset row. Ingest pins that
// were never converted are released by ReleaseStalePins, and the
// existing GC removes bytes no pin holds.

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrImportMismatch: the staged file is not the expected bytes.
var ErrImportMismatch = errors.New("storage: staged file does not match the expected hash and size")

// ErrImportExistingNotRewritten: ImportRequest.KeepExistingOriginal was
// set and the hash is already recorded without matching stored bytes.
// Nothing was written or recorded (#1596).
var ErrImportExistingNotRewritten = errors.New("storage: import: an existing original is never rewritten")

// ErrImportAbandoned: the import's intent was reconciled away before it
// committed. Nothing was recorded; the caller may retry from the start.
var ErrImportAbandoned = errors.New("storage: import intent was reconciled before commit")

// DefaultImportStaleAfter is how old an intent must be before the
// reconciler treats its import as abandoned. It must exceed the longest
// Put of a staged local file by a wide margin.
const DefaultImportStaleAfter = time.Hour

// ImportRequest describes one verified import.
type ImportRequest struct {
	// TempPath is the staged file. ImportVerified never deletes it.
	TempPath    string
	Hash        string // lowercase hex SHA-256 the bytes must have
	Size        int64  // byte length the bytes must have
	ContentType string
	// OriginServerID is the peer the bytes came from (storage_objects.
	// origin_server_id). Kept as first recorded if the object exists.
	OriginServerID uuid.UUID
	// IntentID names the intent row (the fetch lease id).
	IntentID uuid.UUID
	Pin      PinRef
	// InTx, if set, runs inside the committing transaction (for
	// example, releasing the fetch lease), so it commits or rolls back
	// with the object, variant and pin.
	InTx func(ctx context.Context, tx pgx.Tx) error
	// BeforeCommit, if set, runs after the backend Put and before the
	// committing transaction opens, OUTSIDE any transaction but with the
	// live-import lock still held: the place for a last check that needs
	// the network (the fetch re-authorises the caller here). An error
	// records nothing; the intent stays for the reconciler.
	BeforeCommit func(ctx context.Context) error
	// KeepExistingOriginal (#1596, a promoted access copy) never rewrites
	// an original that is already held: an existing original row keeps
	// its content type, digest and status (a missing one is not created
	// on the dedup path), and an existing object whose stored bytes are
	// missing or the wrong size is refused instead of written again. The
	// import lock is taken EXCLUSIVELY and the hash checked again under
	// it, so an original another importer recorded meanwhile is only
	// pinned, never written over.
	KeepExistingOriginal bool
}

// ImportResult is what ImportVerified recorded.
type ImportResult struct {
	Hash    string
	Size    int64
	Deduped bool // the bytes were already held; no Put happened
	Pin     PinRef
}

// ImportVerified records the staged bytes under their hash with the
// given pin, in the crash-safe order described in this file's header.
func (s *Service) ImportVerified(ctx context.Context, req ImportRequest) (*ImportResult, error) {
	if req.Pin.SubjectType == "" || req.Pin.SubjectID == "" {
		return nil, errors.New("storage: pin SubjectType and SubjectID are required")
	}
	if err := ValidateHash(req.Hash); err != nil {
		return nil, err
	}
	if req.IntentID == uuid.Nil {
		return nil, errors.New("storage: import intent id is required")
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}
	if err := verifyStaged(req.TempPath, req.Hash, req.Size); err != nil {
		return nil, err
	}

	q := New(s.Pool)
	exists, err := q.ObjectRowExists(ctx, req.Hash)
	if err != nil {
		return nil, fmt.Errorf("storage: import: lookup: %w", err)
	}
	if exists {
		if info, statErr := s.Backend.Stat(ctx, req.Hash, VariantOriginal); statErr == nil && info != nil && info.Size == req.Size {
			if req.BeforeCommit != nil {
				if err := req.BeforeCommit(ctx); err != nil {
					return nil, fmt.Errorf("storage: import: before commit: %w", err)
				}
			}
			if err := s.commitImport(ctx, req, false); err != nil {
				return nil, err
			}
			return &ImportResult{Hash: req.Hash, Size: req.Size, Deduped: true, Pin: req.Pin}, nil
		}
		if req.KeepExistingOriginal {
			return nil, fmt.Errorf("%w: %s is recorded but its stored bytes are missing or the wrong size", ErrImportExistingNotRewritten, short(req.Hash))
		}
		// A row whose bytes are missing or the wrong size: write the
		// verified bytes again under an intent, like a miss.
	}

	// Live-import lock (session) on a dedicated connection, held until
	// the commit has finished: shared, or exclusive for an import that
	// must never write over an existing original (KeepExistingOriginal).
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: import: acquire connection: %w", err)
	}
	defer conn.Release()
	cq := New(conn)
	if req.KeepExistingOriginal {
		err = cq.LockImportHashExclusiveSession(ctx, req.Hash)
	} else {
		err = cq.LockImportHashSharedSession(ctx, req.Hash)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: import: take import lock: %w", err)
	}
	defer func() {
		// Unlock on a fresh context: a cancelled caller must still give
		// the lock back. If even that fails, release the connection
		// broken (its session ends and the lock with it).
		var uerr error
		if req.KeepExistingOriginal {
			_, uerr = cq.UnlockImportHashExclusiveSession(context.WithoutCancel(ctx), req.Hash)
		} else {
			_, uerr = cq.UnlockImportHashSharedSession(context.WithoutCancel(ctx), req.Hash)
		}
		if uerr != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()
	if s.importHooks.afterLock != nil {
		s.importHooks.afterLock()
	}
	if req.KeepExistingOriginal {
		// #1596: another importer may have recorded the hash after the
		// check above. Under the exclusive lock no other import of it can
		// be mid-way, so this answer holds until the commit: if it is
		// recorded now, nothing is written; the pin is only added.
		recorded, err := cq.ObjectRowExists(ctx, req.Hash)
		if err != nil {
			return nil, fmt.Errorf("storage: import: lookup under lock: %w", err)
		}
		if recorded {
			if req.BeforeCommit != nil {
				if err := req.BeforeCommit(ctx); err != nil {
					return nil, fmt.Errorf("storage: import: before commit: %w", err)
				}
			}
			if err := commitImportOn(ctx, conn, s.Backend.Name(), req, false); err != nil {
				return nil, err
			}
			return &ImportResult{Hash: req.Hash, Size: req.Size, Deduped: true, Pin: req.Pin}, nil
		}
	}
	if err := cq.InsertImportIntent(ctx, InsertImportIntentParams{
		LeaseID:    pgtype.UUID{Bytes: req.IntentID, Valid: true},
		ObjectHash: req.Hash,
	}); err != nil {
		return nil, fmt.Errorf("storage: import: record intent: %w", err)
	}

	f, err := os.Open(req.TempPath)
	if err != nil {
		return nil, fmt.Errorf("storage: import: reopen staged file: %w", err)
	}
	_, putErr := s.Backend.Put(ctx, req.Hash, VariantOriginal, f)
	f.Close()
	if putErr != nil {
		// The intent stays: the reconciler decides about any partial
		// bytes once this import's lock is gone.
		return nil, fmt.Errorf("storage: import: backend put: %w", putErr)
	}
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(ctx); err != nil {
			// Bytes stay under the intent; with no row, the reconciler
			// removes them once this import's lock is gone.
			return nil, fmt.Errorf("storage: import: before commit: %w", err)
		}
	}
	if err := commitImportOn(ctx, conn, s.Backend.Name(), req, true); err != nil {
		return nil, err
	}
	return &ImportResult{Hash: req.Hash, Size: req.Size, Deduped: false, Pin: req.Pin}, nil
}

// importHooks are test seams (set through export_test.go).
type importHooks struct {
	afterLock func() // after the live-import lock is held, before the intent
}

// txBeginner is a pool or a single connection.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// commitImport records object, variant and pin in one transaction on the
// pool (the dedup path: no intent, no Put).
func (s *Service) commitImport(ctx context.Context, req ImportRequest, withIntent bool) error {
	return commitImportOn(ctx, s.Pool, s.Backend.Name(), req, withIntent)
}

// commitImportOn records object, variant and pin in one transaction on
// db, deleting the intent when one was written.
func commitImportOn(ctx context.Context, db txBeginner, backend string, req ImportRequest, withIntent bool) error {
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		tq := New(tx)
		if withIntent {
			n, err := tq.DeleteImportIntent(ctx, pgtype.UUID{Bytes: req.IntentID, Valid: true})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrImportAbandoned
			}
		}
		origin := pgtype.UUID{Valid: false}
		if req.OriginServerID != uuid.Nil {
			origin = pgtype.UUID{Bytes: req.OriginServerID, Valid: true}
		}
		if err := tq.InsertObject(ctx, InsertObjectParams{
			Hash:           req.Hash,
			SizeBytes:      req.Size,
			ContentType:    req.ContentType,
			Backend:        backend,
			OriginServerID: origin,
		}); err != nil {
			return err
		}
		// #1597: with an intent, this import Put the staged bytes, which
		// verifyStaged re-hashed against req.Hash, so the original's
		// digest is recorded. Without one (the dedup path) nothing was
		// written and the stored bytes' row is only touched.
		// KeepExistingOriginal (#1596): an original row that is already
		// there is left exactly as it is.
		var verr error
		switch {
		case withIntent && req.KeepExistingOriginal:
			verr = tq.RecordOriginalVariantIfAbsent(ctx, RecordOriginalVariantIfAbsentParams{ObjectHash: req.Hash, SizeBytes: req.Size, ContentType: req.ContentType})
		case withIntent:
			verr = tq.RecordOriginalVariant(ctx, RecordOriginalVariantParams{ObjectHash: req.Hash, SizeBytes: req.Size, ContentType: req.ContentType})
		case req.KeepExistingOriginal:
		default:
			verr = tq.TouchOriginalVariant(ctx, TouchOriginalVariantParams{ObjectHash: req.Hash, SizeBytes: req.Size, ContentType: req.ContentType})
		}
		if verr != nil {
			return verr
		}
		// The object row is already locked by InsertObject's upsert above;
		// addPinTx clears GC and inserts the pin in that order (#1596).
		if err := addPinTx(ctx, tq, req.Pin, req.Hash); err != nil {
			return err
		}
		if req.InTx != nil {
			return req.InTx(ctx, tx)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrImportAbandoned) {
			return err
		}
		return fmt.Errorf("storage: import: commit: %w", err)
	}
	return nil
}

// verifyStaged recomputes the staged file's size and SHA-256.
func verifyStaged(path, wantHash string, wantSize int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("storage: import: open staged file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, wantSize+1))
	if err != nil {
		return fmt.Errorf("storage: import: read staged file: %w", err)
	}
	if n != wantSize || hex.EncodeToString(h.Sum(nil)) != wantHash {
		return ErrImportMismatch
	}
	return nil
}

// ReconcileResult reports one reconcile pass.
type ReconcileResult struct {
	HashesSeen     int
	SkippedAlive   int // an import of the hash is alive: nothing touched
	BytesDeleted   int // backend objects removed (no row, no live import)
	IntentsDropped int64
}

// ReconcileImportIntents resolves hashes with an intent older than
// staleAfter. For each, it TRIES the exclusive import lock: if an import
// of the hash is alive the hash is skipped, whatever the intent's age.
// Otherwise every intent of the hash is dead: the backend bytes are
// deleted when no storage_objects row exists, and the intents dropped.
func (s *Service) ReconcileImportIntents(ctx context.Context, staleAfter time.Duration, maxHashes int32) (ReconcileResult, error) {
	var res ReconcileResult
	if staleAfter <= 0 {
		staleAfter = DefaultImportStaleAfter
	}
	if maxHashes <= 0 {
		maxHashes = 500
	}
	hashes, err := New(s.Pool).ListStaleImportIntentHashes(ctx, ListStaleImportIntentHashesParams{
		StaleAfter: pgIntervalFromDuration(staleAfter), MaxRows: maxHashes,
	})
	if err != nil {
		return res, fmt.Errorf("storage: reconcile intents: list: %w", err)
	}
	for _, h := range hashes {
		res.HashesSeen++
		var skipped, deleted bool
		var dropped int64
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			tq := New(tx)
			got, err := tq.TryLockImportHashExclusive(ctx, h)
			if err != nil {
				return err
			}
			if !got {
				skipped = true
				return nil
			}
			hasRow, err := tq.ObjectRowExists(ctx, h)
			if err != nil {
				return err
			}
			if !hasRow {
				if err := s.Backend.Delete(ctx, h, VariantOriginal); err != nil && !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("backend delete: %w", err)
				}
				deleted = true
			}
			dropped, err = tq.DeleteImportIntentsForHash(ctx, h)
			return err
		})
		if err != nil {
			return res, fmt.Errorf("storage: reconcile intents: %s: %w", h, err)
		}
		switch {
		case skipped:
			res.SkippedAlive++
		default:
			if deleted {
				res.BytesDeleted++
			}
			res.IntentsDropped += dropped
		}
	}
	return res, nil
}

// ReleaseStalePins removes pins of one subject type older than
// staleAfter (for example federation_ingest pins ingest never
// converted) and marks objects left with no pin GC-eligible.
func (s *Service) ReleaseStalePins(ctx context.Context, subjectType string, staleAfter time.Duration, maxRows int32) (int, error) {
	if subjectType == "" {
		return 0, errors.New("storage: subject type is required")
	}
	if maxRows <= 0 {
		maxRows = 500
	}
	rows, err := New(s.Pool).ListStalePinsOfType(ctx, ListStalePinsOfTypeParams{
		PinSubjectType: subjectType,
		StaleAfter:     pgIntervalFromDuration(staleAfter),
		MaxRows:        maxRows,
	})
	if err != nil {
		return 0, fmt.Errorf("storage: stale pins: %w", err)
	}
	for _, r := range rows {
		if err := s.RemovePin(ctx, PinRef{SubjectType: subjectType, SubjectID: r.PinSubjectID}, r.ObjectHash); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
