// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package storage is the artist-alley content-addressed storage layer.
//
// The full design is in docs/adr/0008-storage-architecture.md. In
// short: every blob is keyed by sha256(contents). Backends (fs, s3,
// future gcs) implement the Backend interface; nothing else in the
// codebase references concrete backends. The accompanying Postgres
// schema (storage_objects, storage_variants, storage_pins) tracks
// metadata, variants, and reference counts.
//
// This package only owns the byte plane. Pin/object/variant rows are
// the responsibility of the asset service that wraps it (Phase 1.4.C
// onward). Keeping the boundary clean means backend implementations
// stay narrow and easy to test.
package storage

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Backend is the contract every storage implementation satisfies.
//
// All methods take a (hash, variant) pair. `hash` is the lowercase hex
// sha256 of the object's content (validated via [ValidateHash]).
// `variant` is the key under the object — "original", "preview_2048",
// "hls/seg00001.ts", etc. — validated via [ValidateVariantKey].
//
// Implementations must be safe for concurrent use by multiple
// goroutines.
type Backend interface {
	// Name returns the stable identifier we record in
	// storage_objects.backend ("fs", "s3", ...).
	Name() string

	// Put streams r into the backend at (hash, variant). Returns the
	// ObjectInfo as seen by the backend after the write completes.
	// Implementations must be atomic — if Put errors, the caller can
	// safely retry.
	Put(ctx context.Context, hash, variant string, r io.Reader) (*ObjectInfo, error)

	// Get returns a stream of the full object. The caller MUST close
	// the returned ReadCloser. The returned *ObjectInfo is a snapshot
	// taken at open time.
	Get(ctx context.Context, hash, variant string) (io.ReadCloser, *ObjectInfo, error)

	// GetRange returns a stream of [offset, offset+length). Used by
	// HLS scrubbing and partial 3D loads. Implementations may
	// truncate length to the end of the object; callers must accept
	// short reads.
	GetRange(ctx context.Context, hash, variant string, offset, length int64) (io.ReadCloser, error)

	// Delete removes a variant. Removing the "original" of an object
	// is allowed; the higher-level GC ensures all variants are
	// removed together. Idempotent — deleting a missing variant
	// returns nil.
	Delete(ctx context.Context, hash, variant string) error

	// Stat returns the variant's metadata without opening a body.
	// Returns ErrNotFound when the variant doesn't exist.
	Stat(ctx context.Context, hash, variant string) (*ObjectInfo, error)

	// PresignGet returns a URL that lets a client fetch the variant
	// directly from the backend (S3, GCS, …) without proxying through
	// the app. Backends that can't presign (local FS) return
	// ErrUnsupported.
	PresignGet(ctx context.Context, hash, variant string, ttl time.Duration) (string, error)

	// PresignPut returns a URL that lets a client upload directly to
	// the backend. As with PresignGet, FS returns ErrUnsupported.
	PresignPut(ctx context.Context, hash, variant string, ttl time.Duration) (string, error)
}

// ObjectInfo is the per-variant metadata a backend can describe
// without reading the bytes themselves.
type ObjectInfo struct {
	Size        int64
	ContentType string
	ETag        string
	ModifiedAt  time.Time
}

// VariantOriginal is the canonical variant key for an object's
// untouched bytes — what was uploaded. Every storage_objects row has
// (or once had) an "original" variant.
const VariantOriginal = "original"

// ErrNotFound is returned by Get/Stat/GetRange when the variant
// doesn't exist. Wrap with %w so callers can use errors.Is.
var ErrNotFound = errors.New("storage: not found")

// ErrUnsupported is returned by capabilities the backend doesn't
// implement — typically PresignGet/PresignPut on the local FS backend.
var ErrUnsupported = errors.New("storage: unsupported by this backend")

// --- input validation -------------------------------------------------------
//
// Hash and variant strings travel from external input (uploads, URL
// params) into filesystem paths and S3 keys. Strict validation here
// keeps the backend layer simple and trustworthy.

var (
	hashRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	variantRe = regexp.MustCompile(`^[A-Za-z0-9._-][A-Za-z0-9._/-]{0,254}$`)
)

// ValidateHash returns nil if h is a valid lowercase hex sha256.
func ValidateHash(h string) error {
	if !hashRe.MatchString(h) {
		return errors.New("storage: invalid object hash; want lowercase hex sha256")
	}
	return nil
}

// ValidateVariantKey returns nil if v is a syntactically valid variant
// key. Allows letters, digits, dot, hyphen, underscore, and forward
// slash (so "hls/seg00001.ts" works), bans leading slash and dot
// segments to keep path traversal impossible.
func ValidateVariantKey(v string) error {
	if v == "" || !variantRe.MatchString(v) {
		return errors.New("storage: invalid variant key")
	}
	// No "..", no doubled slashes, no trailing slash.
	if strings.Contains(v, "..") || strings.Contains(v, "//") {
		return errors.New("storage: variant key contains forbidden path segment")
	}
	if strings.HasSuffix(v, "/") {
		return errors.New("storage: variant key may not end with a slash")
	}
	return nil
}

// ValidatePair runs both validators; backends call this at the top of
// every operation so they never see junk input.
func ValidatePair(hash, variant string) error {
	if err := ValidateHash(hash); err != nil {
		return err
	}
	return ValidateVariantKey(variant)
}

// ObjectPath returns the canonical layout-shared path for a
// (hash, variant) pair: "ab/cd/<full-hash>/<variant>". Used by every
// backend so the on-disk and in-bucket layouts agree.
func ObjectPath(hash, variant string) string {
	return hash[0:2] + "/" + hash[2:4] + "/" + hash + "/" + variant
}
