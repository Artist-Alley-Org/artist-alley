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
	"os"
	"time"

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
}

// NewService constructs a storage Service with sensible defaults.
func NewService(backend Backend, pool *pgxpool.Pool) *Service {
	return &Service{
		Backend:       backend,
		Pool:          pool,
		GCGracePeriod: 24 * time.Hour,
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
	q := New(s.Pool)
	deduped := false

	// Dedup check: does an object with this hash already exist?
	existing, err := q.FindObject(ctx, hash)
	switch {
	case err == nil:
		// Hit. Skip the backend Put — bytes are already there.
		deduped = true
		// Reactivate GC if the row had been scheduled for cleanup.
		_ = q.ClearGCEligible(ctx, hash)
		// Sanity: size should match. If it doesn't, the existing
		// row is broken; we trust the hash anyway because matching
		// sha256 is far less likely than a metadata mistake.
		_ = existing
	case isNoRows(err):
		// Miss. Open the temp file and stream to the backend.
		if err := s.commitObject(ctx, hash, size, contentType, tmpName); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("storage: lookup: %w", err)
	}

	// Pin and variant rows. The variant row is upserted on every
	// upload because we may now have a richer Content-Type than
	// before (or a different content_type for a re-upload). The pin
	// row is idempotent.
	if err := q.UpsertVariant(ctx, UpsertVariantParams{
		ObjectHash:  hash,
		VariantKey:  VariantOriginal,
		SizeBytes:   size,
		ContentType: contentType,
		Metadata:    []byte("{}"),
	}); err != nil {
		return nil, fmt.Errorf("storage: variant upsert: %w", err)
	}
	if err := q.AddPin(ctx, AddPinParams{
		ObjectHash:     hash,
		PinSubjectType: pin.SubjectType,
		PinSubjectID:   pin.SubjectID,
	}); err != nil {
		return nil, fmt.Errorf("storage: add pin: %w", err)
	}

	return &UploadResult{
		Hash:        hash,
		Size:        size,
		ContentType: contentType,
		Deduped:     deduped,
		Pin:         pin,
	}, nil
}

func (s *Service) commitObject(ctx context.Context, hash string, size int64, contentType, tmpPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("storage: reopen temp: %w", err)
	}
	defer f.Close()

	if _, err := s.Backend.Put(ctx, hash, VariantOriginal, f); err != nil {
		return fmt.Errorf("storage: backend put: %w", err)
	}

	q := New(s.Pool)
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
// means the object is alive again.
func (s *Service) AddPin(ctx context.Context, pin PinRef, hash string) error {
	if pin.SubjectType == "" || pin.SubjectID == "" {
		return errors.New("storage: pin SubjectType and SubjectID are required")
	}
	q := New(s.Pool)
	if err := q.AddPin(ctx, AddPinParams{
		ObjectHash:     hash,
		PinSubjectType: pin.SubjectType,
		PinSubjectID:   pin.SubjectID,
	}); err != nil {
		return fmt.Errorf("storage: add pin: %w", err)
	}
	if err := q.ClearGCEligible(ctx, hash); err != nil {
		return fmt.Errorf("storage: clear gc: %w", err)
	}
	return nil
}

// RemovePin removes one pin from an object. If that was the last pin,
// the object is marked GC-eligible (gc_eligible_at = NOW()+grace).
// The sweeper job (not yet implemented) actually deletes the bytes
// after the grace window expires.
func (s *Service) RemovePin(ctx context.Context, pin PinRef, hash string) error {
	if pin.SubjectType == "" || pin.SubjectID == "" {
		return errors.New("storage: pin SubjectType and SubjectID are required")
	}
	q := New(s.Pool)
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
	return nil
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
