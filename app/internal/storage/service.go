// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service is the high-level storage API the rest of the app uses. It
// orchestrates the byte plane (Backend) and the metadata plane (the
// sqlc-generated Queries). Handlers never touch Backend or Queries
// directly — they go through Service so dedup, pin counting, and GC
// scheduling stay consistent.
//
// Service is safe for concurrent use across many goroutines.
type Service struct {
	Backend Backend
	Pool    *pgxpool.Pool

	// GCGracePeriod is added to NOW() when a hash drops to zero
	// pins. Defaults to 24h (matches ADR 0008). Configurable so
	// tests can run with a short grace window.
	GCGracePeriod time.Duration

	// TempDir is where Service stages uploads before they're handed
	// to the backend. Empty = os.TempDir(). For multi-GB uploads
	// this must point at a partition with sufficient free space.
	TempDir string

	// Logger is optional and used only for best-effort operations
	// whose failure must not fail the caller — today, the split-brain
	// reconcile (#827). Nil is fine everywhere; every use goes through
	// a nil check, because Service is constructed bare in a lot of
	// tests and a background heal is not worth a panic.
	Logger *slog.Logger

	// importHooks are #72 test seams; zero in production.
	importHooks importHooks

	// variantSem bounds PutVariant's held connections; variantHooks are
	// its test seams (#1597, fixity.go).
	variantSem   variantSem
	variantHooks variantHooks
	// eventHooks are #1598 test seams; zero in production.
	eventHooks eventHooks
	// pinHooks are #1596 test seams; zero in production.
	pinHooks pinHooks
	// promoteHooks are #1596 test seams (promote.go); zero in production.
	promoteHooks promoteHooks
	// uploadHooks are #1596 test seams; zero in production.
	uploadHooks uploadHooks

	// AgentVersion is the software version recorded on local
	// preservation events (#1598, events.go). Empty omits it.
	AgentVersion string
}

// NewService constructs a storage Service with sensible defaults.
func NewService(backend Backend, pool *pgxpool.Pool) *Service {
	return &Service{
		Backend:       backend,
		Pool:          pool,
		GCGracePeriod: DefaultGCGracePeriod,
	}
}

// PinRef identifies who is keeping an object's bytes alive. The two
// strings together form a logical key (e.g., "user", "42" or
// "resource", "abc-xyz").
type PinRef struct {
	SubjectType string
	SubjectID   string
}

// UploadResult is what the upload handler returns to the client.
type UploadResult struct {
	Hash        string
	Size        int64
	ContentType string
	Deduped     bool // true if the bytes were already on the backend
	Pin         PinRef
}

// UploadOriginal stages the reader to a temp file (hashing in the
// same pass), then either dedups against an existing storage_object
// or commits the bytes to the backend. Either way, a pin is added so
// the caller's PinRef keeps the object alive.
//
// Service intentionally does the hashing itself — callers cannot be
// trusted to hand us a correct sha256. The whole content-addressing
// model rests on the hash matching the bytes.
func (s *Service) UploadOriginal(ctx context.Context, r io.Reader, contentType string, pin PinRef) (*UploadResult, error) {
	if pin.SubjectType == "" || pin.SubjectID == "" {
		return nil, errors.New("storage: pin SubjectType and SubjectID are required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Stage to a temp file, hashing on the fly.
	tmpDir := s.TempDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "aa-upload-*")
	if err != nil {
		return nil, fmt.Errorf("storage: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)
	size, err := io.Copy(mw, r)
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("storage: stage: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("storage: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("storage: close: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	deduped := false

	// #1596: the existence check, the Put, the rows and the pin run under
	// the per-hash import lock, SHARED (as every importer takes it), held
	// on ONE dedicated connection that does all of this upload's database
	// work. Uploads and ordinary imports of a hash still run side by side;
	// a promotion (ImportRequest.KeepExistingOriginal) takes the lock
	// EXCLUSIVELY, so it can never be between its re-check and its Put
	// while an upload is between its check and its record, nor the other
	// way round. Whichever runs second finds the row and writes no bytes.
	//
	// The connection is held across the backend Put, so the upload first
	// takes one of the bounded put slots (acquireVariantSlot), BEFORE it
	// asks the pool for a connection (no hold-and-wait, #1616); nothing
	// inside takes a second connection.
	releaseSlot, err := s.acquireVariantSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseSlot()
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: upload: acquire connection: %w", err)
	}
	defer conn.Release()
	q := New(conn)
	if err := q.LockImportHashSharedSession(ctx, hash); err != nil {
		return nil, fmt.Errorf("storage: upload: take import lock: %w", err)
	}
	defer func() {
		if _, err := q.UnlockImportHashSharedSession(context.WithoutCancel(ctx), hash); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()

	// Dedup check: does an object with this hash already exist?
	existing, err := q.FindObject(ctx, hash)
	if s.uploadHooks.afterCheck != nil {
		s.uploadHooks.afterCheck(hash)
	}
	switch {
	case err == nil:
		// Hit. Skip the backend Put — bytes are already there.
		deduped = true
		// A pending GC mark is cleared by the pin below, in the pin's
		// own transaction (addPinTx, #1596).
		// Sanity: size should match. If it doesn't, the existing
		// row is broken; we trust the hash anyway because matching
		// sha256 is far less likely than a metadata mistake.
		_ = existing
	case isNoRows(err):
		// Miss. Open the temp file and stream to the backend.
		if err := s.commitObject(ctx, q, hash, size, contentType, tmpName); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("storage: lookup: %w", err)
	}

	// Pin and variant rows. The variant row is written on every upload
	// because we may now have a richer Content-Type than before. Its
	// digest (#1597) is recorded only when THIS upload wrote the bytes:
	// an original's digest is its hash, computed above from the stream
	// and checked again against what commitObject handed the backend. A
	// deduplicated upload wrote nothing, so it changes the content type
	// and leaves the stored bytes' digest and status as they were. The
	// pin row is idempotent.
	if deduped {
		err = q.TouchOriginalVariant(ctx, TouchOriginalVariantParams{ObjectHash: hash, SizeBytes: size, ContentType: contentType})
	} else {
		err = q.RecordOriginalVariant(ctx, RecordOriginalVariantParams{ObjectHash: hash, SizeBytes: size, ContentType: contentType})
	}
	if err != nil {
		return nil, fmt.Errorf("storage: variant row: %w", err)
	}
	// The pin clears any pending GC mark and commits with it, the object
	// row locked first (addPinTx, #1596), for a new and a deduplicated
	// object alike: on the same connection, still under the lock.
	if err := inPinTx(ctx, conn, "add pin", func(tx pgx.Tx) error {
		return addPinTx(ctx, New(tx), pin, hash)
	}); err != nil {
		return nil, err
	}

	return &UploadResult{
		Hash:        hash,
		Size:        size,
		ContentType: contentType,
		Deduped:     deduped,
		Pin:         pin,
	}, nil
}

// commitObject Puts the staged bytes and records the object row, on the
// caller's connection (UploadOriginal's, under the import lock).
func (s *Service) commitObject(ctx context.Context, q *Queries, hash string, size int64, contentType, tmpPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("storage: reopen temp: %w", err)
	}
	defer f.Close()

	// Hash what the backend actually reads (#1597): it must be the staged
	// bytes the address was computed from.
	sum := sha256.New()
	if _, err := s.Backend.Put(ctx, hash, VariantOriginal, io.TeeReader(f, sum)); err != nil {
		return fmt.Errorf("storage: backend put: %w", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != hash {
		return fmt.Errorf("storage: backend put: streamed bytes hash to %s, staged %s", short(got), short(hash))
	}

	if err := q.InsertObject(ctx, InsertObjectParams{
		Hash:           hash,
		SizeBytes:      size,
		ContentType:    contentType,
		Backend:        s.Backend.Name(),
		BackendBucket:  nil, // s3 backend can populate this in a follow-up
		OriginServerID: pgtype.UUID{Valid: false},
	}); err != nil {
		return fmt.Errorf("storage: insert object: %w", err)
	}
	return nil
}

// Download opens a variant for reading. Caller MUST close. Wraps
// Backend.Get with no additional behaviour today; in a later phase
// this is where access-log writes and last_used_at touches will land.
func (s *Service) Download(ctx context.Context, hash, variant string) (io.ReadCloser, *ObjectInfo, error) {
	return s.Backend.Get(ctx, hash, variant)
}

// Stat reports a stored variant's size, content type and modification
// time without opening it. Used by the HTTP layer to build a
// content-derived cache validator before deciding whether the byte
// transfer is needed at all (#620).
func (s *Service) Stat(ctx context.Context, hash, variant string) (*ObjectInfo, error) {
	return s.Backend.Stat(ctx, hash, variant)
}

// DownloadRange opens a byte range of a variant.
func (s *Service) DownloadRange(ctx context.Context, hash, variant string, offset, length int64) (io.ReadCloser, error) {
	return s.Backend.GetRange(ctx, hash, variant, offset, length)
}

// AddPin attaches a fresh pin to an existing storage object without
// touching the bytes. Used by the asset entity handler to claim a
// previously uploaded blob under `asset:<uuid>`. Idempotent on the
// underlying primary key (subject_type, subject_id, hash) — re-adding
// is a no-op.
//
// Also clears any pending gc_eligible_at on the object: a fresh pin
// means the object is alive again. The clear and the pin commit
// together, the object row locked first (addPinTx, #1596).
func (s *Service) AddPin(ctx context.Context, pin PinRef, hash string) error {
	if pin.SubjectType == "" || pin.SubjectID == "" {
		return errors.New("storage: pin SubjectType and SubjectID are required")
	}
	return inPinTx(ctx, s.Pool, "add pin", func(tx pgx.Tx) error {
		if err := addPinTx(ctx, New(tx), pin, hash); err != nil {
			return err
		}
		if s.pinHooks.afterAdd != nil {
			return s.pinHooks.afterAdd(hash)
		}
		return nil
	})
}

// RemovePin removes one pin from an object. If that was the last pin,
// the object is marked GC-eligible (gc_eligible_at = NOW()+grace).
// The sweeper job (not yet implemented) actually deletes the bytes
// after the grace window expires. Removing a pin that does not exist
// changes nothing but still marks an object left with no pin.
//
// #1596: one transaction, the object row locked BEFORE the pin row
// changes, and the "last pin?" check run afterwards as its own
// statement. Under READ COMMITTED each statement has a fresh snapshot,
// so a concurrent pin that held the lock has committed and is seen. A
// single UPDATE ... WHERE NOT EXISTS (pins) that waits on that lock
// re-checks the row with its OLD snapshot and would schedule an object
// that has just been pinned.
func (s *Service) RemovePin(ctx context.Context, pin PinRef, hash string) error {
	if pin.SubjectType == "" || pin.SubjectID == "" {
		return errors.New("storage: pin SubjectType and SubjectID are required")
	}
	return inPinTx(ctx, s.Pool, "remove pin", func(tx pgx.Tx) error {
		q := New(tx)
		if err := q.LockObjectForPinChange(ctx, hash); err != nil {
			return fmt.Errorf("storage: lock object: %w", err)
		}
		if err := q.RemovePin(ctx, RemovePinParams{
			ObjectHash:     hash,
			PinSubjectType: pin.SubjectType,
			PinSubjectID:   pin.SubjectID,
		}); err != nil {
			return fmt.Errorf("storage: remove pin: %w", err)
		}
		// Was that the last pin?
		if err := q.MarkGCEligibleIfOrphaned(ctx, MarkGCEligibleIfOrphanedParams{
			Hash:    hash,
			Column2: pgIntervalFromDuration(s.GCGracePeriod),
		}); err != nil {
			return fmt.Errorf("storage: mark gc: %w", err)
		}
		if s.pinHooks.beforeRemoveCommit != nil {
			return s.pinHooks.beforeRemoveCommit(hash)
		}
		return nil
	})
}

// addPinTx pins hash for pin inside the caller's transaction (#1596).
// Its first statement clears any pending GC, which also locks the object
// row, and only then is the pin row inserted: the order casfetch and
// migration 00084's asset_version_pin use, so a concurrent removal's GC
// mark either waits for this pin or is cleared by it. A missing object
// fails on the pin's foreign key, as before.
func addPinTx(ctx context.Context, q *Queries, pin PinRef, hash string) error {
	if err := q.ClearGCEligible(ctx, hash); err != nil {
		return fmt.Errorf("storage: clear gc: %w", err)
	}
	if err := q.AddPin(ctx, AddPinParams{
		ObjectHash:     hash,
		PinSubjectType: pin.SubjectType,
		PinSubjectID:   pin.SubjectID,
	}); err != nil {
		return fmt.Errorf("storage: add pin: %w", err)
	}
	return nil
}

// inPinTx runs fn in one transaction on db: the pool (one connection),
// or a connection the caller already holds (UploadOriginal's). Nothing
// inside takes a second connection (#1616). A refused commit, such as
// migration 00084 refusing to unpin a live asset version, is reported
// as "storage: <what>: commit: ...".
func inPinTx(ctx context.Context, db txBeginner, what string, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: %s: begin: %w", what, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: %s: commit: %w", what, err)
	}
	return nil
}

// uploadHooks are #1596 test seams; zero in production.
type uploadHooks struct {
	afterCheck func(hash string) // under the import lock, after the existence check
}

// pinHooks are #1596 test seams; zero in production.
type pinHooks struct {
	afterAdd           func(hash string) error // inside AddPin's transaction, after the pin
	beforeRemoveCommit func(hash string) error // inside RemovePin's transaction, after the GC mark
}

// DefaultGCGracePeriod is how long an object left without any pin waits
// before it is GC-eligible, unless the service is configured otherwise.
// Migration 00084's version-pin release uses the same value as its
// default (aa.storage_gc_grace unset), so a purge and RemovePin agree.
const DefaultGCGracePeriod = 24 * time.Hour

// UploadReceiptTTL is how long an upload receipt (#1595) stays usable by
// POST /assets. Both browser callers create the asset straight after
// the bytes land; a day covers a slow queue and a paused tab.
const UploadReceiptTTL = 24 * time.Hour

// uploadReceiptSweepBatch bounds the expired receipts one upload deletes.
// Each upload adds one receipt and removes up to this many expired ones,
// so expired rows cannot pile up faster than they are swept.
const uploadReceiptSweepBatch = 100

// IssueUploadReceipt records one upload's receipt (#1595): an opaque id
// bound to the uploading user, the exact hash and the base name the
// request carried (nil means the request carried none, source unknown).
// basename must already be a safe base name (provenance.FromUploadHeader).
//
// The receipt is per UPLOAD, not per object: storage_objects and
// storage_pins are shared by every upload of the same bytes, so two
// uploads of one file under two names get two receipts with their own
// names, and neither touches the other.
//
// Expired receipts are swept here, best effort: a failed sweep is logged
// and the upload still gets its receipt.
func (s *Service) IssueUploadReceipt(ctx context.Context, userRef int64, hash string, basename *string) (pgtype.UUID, error) {
	q := New(s.Pool)
	if n, err := q.SweepExpiredUploadReceipts(ctx, uploadReceiptSweepBatch); err != nil {
		if s.Logger != nil {
			s.Logger.LogAttrs(ctx, slog.LevelWarn, "storage.upload_receipt.sweep_failed", slog.String("err", err.Error()))
		}
	} else if n > 0 && s.Logger != nil {
		s.Logger.LogAttrs(ctx, slog.LevelDebug, "storage.upload_receipt.swept", slog.Int64("rows", n))
	}
	source := "unknown"
	if basename != nil {
		source = "upload_request"
	}
	id, err := q.InsertUploadReceipt(ctx, InsertUploadReceiptParams{
		UserRef:                userRef,
		FileHash:               hash,
		OriginalBasename:       basename,
		OriginalBasenameSource: source,
		Ttl:                    pgIntervalFromDuration(UploadReceiptTTL),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("storage: issue upload receipt: %w", err)
	}
	return id, nil
}

// isNoRows reports whether err is pgx's "no rows in result set". The
// import path differs by pgx version; we use a string match so we
// don't pin any particular import here.
func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}

// pgIntervalFromDuration converts a Go duration into a pg interval
// suitable for parameter binding. sqlc maps INTERVAL to
// pgtype.Interval; constructing it manually keeps the SQL signature
// the same for tests and prod.
func pgIntervalFromDuration(d time.Duration) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: d.Microseconds(),
		Days:         0,
		Months:       0,
		Valid:        true,
	}
}
