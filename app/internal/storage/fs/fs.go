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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

	// Read the mtime back off the renamed file rather than using
	// time.Now(): the validator returned here must equal the one a
	// later Stat produces, or the first conditional request after an
	// upload misses for no reason.
	mod := time.Now().UTC()
	if st, statErr := os.Stat(full); statErr == nil {
		mod = st.ModTime().UTC()
	}
	info := &storage.ObjectInfo{
		Size:        n,
		ContentType: "application/octet-stream",
		ETag:        weakETag(hash, variant, n, mod),
		ModifiedAt:  mod,
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
		ETag:        weakETag(hash, variant, st.Size(), st.ModTime()),
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
		ETag:        weakETag(hash, variant, st.Size(), st.ModTime()),
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

// weakETag builds the backend's validator for a stored variant.
//
// It used to mix in two random bytes, which made it useless as an HTTP
// validator: a fresh value on every call means a conditional request can
// never match, so If-None-Match always misses and the bytes are always
// re-sent. That was latent rather than harmful only because nothing
// compared it — the asset routes shipped their own (path-derived) ETag
// instead (#620).
//
// Now derived from size + modification time, which is stable across
// reads of unchanged bytes and changes when a variant is re-rendered.
// Still WEAK: it is not a digest of the content, so a rewrite that
// preserved both size and mtime to the nanosecond would not be
// detected. That does not occur for a re-render, and a weak validator
// is the correct HTTP shape for "semantically the same bytes".
func weakETag(hash, variant string, size int64, mod time.Time) string {
	return fmt.Sprintf(`W/"%s-%d-%d"`, hash[:8], size, mod.UTC().UnixNano())
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

// List enumerates stored variants in lexicographic key order (#403).
// Implements storage.Backend.List.
//
// Ordering is deliberate rather than incidental. filepath.WalkDir's
// depth-first order is NOT globally lexicographic: given variants
// "a.b" and "a/b" under one object, '.' (0x2E) sorts below '/' (0x2F),
// so "a.b" < "a/b" as keys — but the walk descends into directory "a"
// first and emits "a/b" earlier. Paging on a cursor derived from that
// order would skip keys permanently. Since this feeds orphan
// detection, and orphan detection feeds a delete path, "usually
// ordered" is not good enough.
//
// So the walk is structured around the layout instead. The first three
// levels ("ab/cd/<hash>") are fixed-width hex and cannot collide
// ambiguously, so they are iterated in sorted order and skipped
// wholesale when they sort past the cursor. The variants beneath a
// single object are few (bounded by the preview pipeline), so that
// subtree is materialised and sorted properly, which is where the
// ambiguity lives.
//
// Keys that don't parse as object paths (the writability probe, an
// operator's stray file) are skipped — see storage.ParseObjectPath.
func (b *Backend) List(ctx context.Context, cursor string, limit int) ([]storage.ObjectRef, string, error) {
	if limit <= 0 {
		limit = 1000
	}
	refs := make([]storage.ObjectRef, 0, limit)

	shards, err := sortedDirNames(b.Root)
	if err != nil {
		return nil, "", fmt.Errorf("fs: list: %w", err)
	}
	for _, s1 := range shards {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if cursor != "" && s1 < cursor[:min(2, len(cursor))] {
			continue
		}
		subs, err := sortedDirNames(filepath.Join(b.Root, s1))
		if err != nil {
			return nil, "", fmt.Errorf("fs: list: %w", err)
		}
		for _, s2 := range subs {
			prefix2 := s1 + "/" + s2
			if cursor != "" && prefix2 < cursor[:min(len(prefix2), len(cursor))] {
				continue
			}
			hashes, err := sortedDirNames(filepath.Join(b.Root, s1, s2))
			if err != nil {
				return nil, "", fmt.Errorf("fs: list: %w", err)
			}
			for _, h := range hashes {
				objDir := filepath.Join(b.Root, s1, s2, h)
				objPrefix := prefix2 + "/" + h
				if cursor != "" && objPrefix < cursor[:min(len(objPrefix), len(cursor))] {
					continue
				}
				// Materialise this one object's variants and sort them
				// properly — this is the only place ordering is subtle.
				objRefs, err := listObjectVariants(ctx, objDir, objPrefix)
				if err != nil {
					return nil, "", fmt.Errorf("fs: list: %w", err)
				}
				for _, r := range objRefs {
					if cursor != "" && r.Key() <= cursor {
						continue
					}
					if len(refs) == limit {
						// A full page with more to come: resume after
						// the last key we actually returned.
						return refs, refs[len(refs)-1].Key(), nil
					}
					refs = append(refs, r)
				}
			}
		}
	}
	return refs, "", nil
}

// listObjectVariants collects every variant beneath one object
// directory, sorted by full key. The subtree is small (one object's
// renditions), so materialising it is cheap and lets us sort by the
// real key rather than trusting traversal order.
func listObjectVariants(ctx context.Context, objDir, objPrefix string) ([]storage.ObjectRef, error) {
	var out []storage.ObjectRef
	err := filepath.WalkDir(objDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // vanished mid-walk against a live store
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(objDir, p)
		if rerr != nil {
			return nil
		}
		key := objPrefix + "/" + filepath.ToSlash(rel)
		hash, variant, perr := storage.ParseObjectPath(key)
		if perr != nil {
			return nil // not an object we wrote
		}
		info, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, os.ErrNotExist) {
				return nil
			}
			return ierr
		}
		out = append(out, storage.ObjectRef{Hash: hash, Variant: variant, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// sortedDirNames returns the subdirectory names of dir, sorted. A
// missing dir yields no entries rather than an error, so enumerating a
// backend that has never been written to is a clean empty result.
func sortedDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
