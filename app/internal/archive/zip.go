// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package archive

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

// ManifestZIP reads the ZIP central directory and projects every
// entry onto the unified shape. r must support io.ReaderAt; size
// is the full-file byte count (passed in so we don't have to seek
// to end + back).
//
// Cost: O(entries) — the central directory parse is a single read
// of the trailing ~22-65 KB. Per-entry data isn't touched.
func ManifestZIP(r io.ReaderAt, size int64) (*Manifest, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("archive.zip: open: %w", err)
	}
	out := &Manifest{Format: "zip"}
	for i, f := range zr.File {
		if i >= MaxEntries {
			out.Truncated = true
			break
		}
		// archive/zip already normalises to forward slashes per the
		// ZIP spec, but defensively re-normalise so an OS-specific
		// quirk (cp437 + backslash-paths from old DOS zips) can't
		// leak through.
		path := strings.ReplaceAll(f.Name, "\\", "/")
		entry := Entry{
			Path:           path,
			Size:           int64(f.UncompressedSize64),
			CompressedSize: int64(f.CompressedSize64),
			Modified:       f.Modified,
			IsDir:          f.FileInfo().IsDir(),
			Comment:        f.Comment,
		}
		out.Entries = append(out.Entries, entry)
		if !entry.IsDir {
			out.TotalSize += entry.Size
		}
	}
	out.EntryCount = len(out.Entries)
	return out, nil
}

// OpenZIPEntry locates `path` inside the archive and returns a
// ReadCloser positioned at the start of its decompressed bytes.
// Path is the exact entry path (forward-slash normalised) the
// manifest reported.
//
// Returns (nil, nil) when the path matches a directory entry —
// the caller (extract endpoint) should respond with 404 in that
// case rather than serving an empty body.
func OpenZIPEntry(r io.ReaderAt, size int64, path string) (io.ReadCloser, *zip.File, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, nil, fmt.Errorf("archive.zip: open: %w", err)
	}
	want := strings.ReplaceAll(path, "\\", "/")
	for _, f := range zr.File {
		fp := strings.ReplaceAll(f.Name, "\\", "/")
		if fp != want {
			continue
		}
		if f.FileInfo().IsDir() {
			return nil, f, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("archive.zip: open entry: %w", err)
		}
		return rc, f, nil
	}
	return nil, nil, fmt.Errorf("archive.zip: entry %q not found", path)
}
