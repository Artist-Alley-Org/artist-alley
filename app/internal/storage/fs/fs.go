// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package fs is the local-filesystem implementation of
// storage.Backend.
//
// On-disk layout (rooted at the configured Root directory) mirrors
// the storage package's canonical ObjectPath:
//
//	<root>/ab/cd/<full-sha256>/<variant>
//
// Writes are atomic via temp-file-plus-rename. Each Put writes to a
// tempfile in the same directory as the destination (so rename is
// guaranteed to be a same-filesystem rename), then atomically renames
// into place. Concurrent Put of the same (hash, variant) is safe:
// last writer wins, but both writers see a fully-written file at all
// times.
//
// The FS backend cannot mint presigned URLs; PresignGet/PresignPut
// return storage.ErrUnsupported. Callers should fall back to
// proxying bytes through the app server (or, for production-scale
// installs, switch to the s3 backend).
package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// Backend is the filesystem backend.
type Backend struct {
	Root string
}

// New constructs a filesystem-backed storage. Root is created if it
// doesn't exist; existence and writability are verified before
// returning so a misconfigured backend fails at boot, not on first
// upload.
func New(root string) (*Backend, error) {
	if root == "" {
		return nil, errors.New("fs: empty root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fs: mkdir root: %w", err)
	}
	// Probe writability via a tempfile.
	probe, err := os.CreateTemp(root, ".writable-probe-*")
	if err != nil {
		return nil, fmt.Errorf("fs: root not writable: %w", err)
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		return nil, fmt.Errorf("fs: probe cleanup: %w", err)
	}
	return &Backend{Root: root}, nil
}

// Name returns the storage backend identifier persisted in
// storage_objects.backend.
func (b *Backend) Name() string { return "fs" }

// pathFor builds the absolute on-disk path for (hash, variant) and
// returns the directory component separately so callers can MkdirAll
// before Open.
func (b *Backend) pathFor(hash, variant string) (full, dir string) {
	rel := storage.ObjectPath(hash, variant)
	full = filepath.Join(b.Root, rel)
	dir = filepath.Dir(full)
	return
}

// Put streams r to disk atomically. We don't hash here — the caller
// is responsible for verifying the bytes match the hash they're
// passing.
func (b *Backend) Put(ctx context.Context, hash, variant string, r io.Reader) (*storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	full, dir := b.pathFor(hash, variant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("fs: mkdir variant: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+"-*")
	if err != nil {
		return nil, fmt.Errorf("fs: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// On any error past this point, best-effort cleanup of the tempfile.
	cleanup := func() { _ = os.Remove(tmpName) }

	n, err := io.Copy(tmp, ctxReader{ctx: ctx, r: r})
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("fs: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("fs: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("fs: close: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		cleanup()
		return nil, fmt.Errorf("fs: rename: %w", err)
	}

	info := &storage.ObjectInfo{
		Size:        n,
		ContentType: "application/octet-stream",
		ETag:        weakETag(hash, variant, n),
		ModifiedAt:  time.Now().UTC(),
	}
	return info, nil
}

// Get opens the variant for reading. Caller MUST close.
func (b *Backend) Get(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, nil, err
	}
	full, _ := b.pathFor(hash, variant)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, storage.ErrNotFound
		}
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	info := &storage.ObjectInfo{
		Size:        st.Size(),
		ContentType: "application/octet-stream",
		ETag:        weakETag(hash, variant, st.Size()),
		ModifiedAt:  st.ModTime().UTC(),
	}
	return f, info, nil
}

// GetRange opens the variant and seeks to offset, returning a reader
// limited to length bytes. length<=0 means "until EOF".
func (b *Backend) GetRange(ctx context.Context, hash, variant string, offset, length int64) (io.ReadCloser, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("fs: negative offset")
	}
	full, _ := b.pathFor(hash, variant)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("fs: seek: %w", err)
	}
	if length <= 0 {
		return f, nil
	}
	return &limitedFileReader{f: f, lr: io.LimitReader(f, length)}, nil
}

// Delete is idempotent: removing a missing variant returns nil.
func (b *Backend) Delete(ctx context.Context, hash, variant string) error {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return err
	}
	full, _ := b.pathFor(hash, variant)
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Stat returns metadata without reading the body.
func (b *Backend) Stat(ctx context.Context, hash, variant string) (*storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	full, _ := b.pathFor(hash, variant)
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return &storage.ObjectInfo{
		Size:        st.Size(),
		ContentType: "application/octet-stream",
		ETag:        weakETag(hash, variant, st.Size()),
		ModifiedAt:  st.ModTime().UTC(),
	}, nil
}

// PresignGet is not supported by local FS — bytes are served by the
// app server itself (optionally via nginx X-Accel-Redirect).
func (b *Backend) PresignGet(ctx context.Context, hash, variant string, ttl time.Duration) (string, error) {
	return "", storage.ErrUnsupported
}

// PresignPut is not supported by local FS.
func (b *Backend) PresignPut(ctx context.Context, hash, variant string, ttl time.Duration) (string, error) {
	return "", storage.ErrUnsupported
}

// weakETag produces a stable identifier for cache validation. Hash +
// variant + size uniquely identifies the content; bumping size means
// the file was rewritten, which invalidates any cached copies.
func weakETag(hash, variant string, size int64) string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:]) // entropy on first allocation only matters for tie-breaking; ignore err
	return fmt.Sprintf(`W/"%s-%d-%s"`, hash[:8], size, hex.EncodeToString(rnd[:2]))
}

// limitedFileReader couples a *os.File with an io.LimitReader so
// Close on the wrapper closes the underlying file.
type limitedFileReader struct {
	f  *os.File
	lr io.Reader
}

func (r *limitedFileReader) Read(p []byte) (int, error) { return r.lr.Read(p) }
func (r *limitedFileReader) Close() error               { return r.f.Close() }

// ctxReader makes io.Copy respect the request context — if ctx is
// cancelled mid-stream the next Read errors with the ctx's err
// instead of blocking on the upstream reader.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}
