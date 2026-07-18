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
	"fmt"
	"io"
	"path"
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

	// List enumerates stored variants in lexicographic key order,
	// returning at most limit refs plus a cursor to continue from.
	// A returned cursor of "" means the enumeration is complete.
	// Passing cursor "" starts from the beginning.
	//
	// This exists for integrity sweeps (#403): without it, orphan
	// detection can only look DB->disk ("row exists, object missing")
	// and never disk->DB ("object exists, nothing references it"),
	// which is where reclaimable waste actually hides.
	//
	// Paginated rather than slurp-everything or callback-based on
	// purpose: the sweep runs as a resumable job that records
	// progress between batches, and s3 continuation tokens map onto
	// the cursor directly.
	//
	// Keys that are not well-formed object paths are skipped, not
	// reported — a sweep must never hand a delete path something it
	// cannot attribute to an object.
	List(ctx context.Context, cursor string, limit int) (refs []ObjectRef, next string, err error)
}

// ObjectRef is one stored variant as seen by the backend during
// enumeration. Size comes from the backend's own listing, so it can be
// compared against storage_variants.size_bytes without a second Stat.
type ObjectRef struct {
	Hash    string
	Variant string
	Size    int64
}

// Key returns the backend-relative key for this ref.
func (r ObjectRef) Key() string { return ObjectPath(r.Hash, r.Variant) }

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

// ParseObjectPath is the inverse of [ObjectPath]: it recovers the
// (hash, variant) pair from a backend-relative key. Enumeration (#403)
// needs this to turn "what is on disk" back into "which object is
// this", so orphan detection can compare against the DB.
//
// The layout is unambiguous even though a variant key may itself
// contain slashes ("hls/720p/seg00006.ts"): the hash occupies exactly
// the third segment and is a fixed-width 64-char hex string, so
// everything after it is the variant.
//
// Anything that is not a well-formed object path — a stray file at the
// backend root, a temp file, a truncated directory — is rejected rather
// than guessed at. Callers treat that as "not ours, leave it alone",
// which matters because the caller on the other end of this is a
// delete path.
func ParseObjectPath(rel string) (hash, variant string, err error) {
	rel = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(rel, `\`, "/")), "/")
	parts := strings.Split(rel, "/")
	if len(parts) < 4 {
		return "", "", fmt.Errorf("storage: %q is not an object path", rel)
	}
	hash = parts[2]
	if err := ValidateHash(hash); err != nil {
		return "", "", fmt.Errorf("storage: %q is not an object path: %w", rel, err)
	}
	// The shard prefix is derived from the hash, so a mismatch means
	// the key was not written by ObjectPath and we must not claim it.
	if parts[0] != hash[0:2] || parts[1] != hash[2:4] {
		return "", "", fmt.Errorf("storage: %q shard prefix does not match its hash", rel)
	}
	variant = strings.Join(parts[3:], "/")
	if err := ValidateVariantKey(variant); err != nil {
		return "", "", fmt.Errorf("storage: %q is not an object path: %w", rel, err)
	}
	return hash, variant, nil
}
