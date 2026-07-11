// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package archive

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/ulikunitz/xz"
)

// ManifestTAR scans every TAR header from the start of the stream
// (TAR is stream-only — no central directory) and projects the
// entry list. compressedExt = "gz" / "bz2" / "xz" wraps the
// reader in the matching decompressor; empty string = raw TAR.
//
// Cost: O(file size). Caller should run this in a preview job
// rather than inline so a slow disk / huge file doesn't tie up
// the HTTP path.
func ManifestTAR(r io.Reader, compressedExt string) (*Manifest, error) {
	tr, err := openTarStream(r, compressedExt)
	if err != nil {
		return nil, err
	}
	out := &Manifest{Format: "tar"}
	if compressedExt != "" {
		out.Format = "tar." + compressedExt
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Mid-stream parse error — surface what we got rather
			// than dropping a partial manifest. The frontend can
			// flag "truncated" in the panel.
			out.Truncated = true
			break
		}
		if len(out.Entries) >= MaxEntries {
			out.Truncated = true
			break
		}
		path := strings.ReplaceAll(hdr.Name, "\\", "/")
		isDir := hdr.Typeflag == tar.TypeDir || strings.HasSuffix(path, "/")
		entry := Entry{
			Path:     path,
			Size:     hdr.Size,
			Modified: hdr.ModTime,
			IsDir:    isDir,
		}
		out.Entries = append(out.Entries, entry)
		if !isDir {
			out.TotalSize += entry.Size
		}
	}
	out.EntryCount = len(out.Entries)
	return out, nil
}

// OpenTARStreamEntry scans for `path` in the stream and returns a
// ReadCloser that yields the entry's bytes. Wraps in a LimitReader
// so the caller can't read past the entry boundary into the next
// header.
//
// Stream-only — every call walks the file from byte 0. For large
// TARs serving lots of entries, the caller should consider caching
// a (path → offset) index built off the manifest. Phase B
// optimisation; current usage is one entry per user click which
// is acceptable.
func OpenTARStreamEntry(r io.Reader, compressedExt string, path string) (io.ReadCloser, *tar.Header, error) {
	tr, err := openTarStream(r, compressedExt)
	if err != nil {
		return nil, nil, err
	}
	want := strings.ReplaceAll(path, "\\", "/")
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil, fmt.Errorf("archive.tar: entry %q not found", path)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("archive.tar: scan: %w", err)
		}
		if strings.ReplaceAll(hdr.Name, "\\", "/") != want {
			continue
		}
		// Wrap the tar reader as a no-op closer so the caller can
		// `defer rc.Close()` uniformly across archive kinds.
		return io.NopCloser(tr), hdr, nil
	}
}

func openTarStream(r io.Reader, compressedExt string) (*tar.Reader, error) {
	switch compressedExt {
	case "":
		return tar.NewReader(r), nil
	case "gz":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("archive.tar.gz: open: %w", err)
		}
		return tar.NewReader(gz), nil
	case "bz2":
		return tar.NewReader(bzip2.NewReader(r)), nil
	case "xz":
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("archive.tar.xz: open: %w", err)
		}
		return tar.NewReader(xr), nil
	}
	return nil, &errUnsupported{ext: "tar." + compressedExt}
}
