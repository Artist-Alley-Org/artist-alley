// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/bodgit/sevenzip"
	rardecode "github.com/nwaples/rardecode/v2"
	"github.com/ulikunitz/xz"
)

// WriteBundleZip streams every entry of `r` (a source archive in
// `format`) into `out` as a fresh ZIP. Lets a user grab the contents
// of a TAR / 7z / RAR / tar.xz in a format every OS opens natively
// without us extracting first to disk.
//
// Format-specific source readers:
//   - "zip" — needs ReaderAt; pass src + size via WriteBundleZipReaderAt.
//   - "7z" — same.
//   - "tar" / "tar.gz" / "tar.bz2" / "tar.xz" / "tgz" / "tbz2" / "txz"
//     — streams forward.
//   - "rar" — streams forward (solid-archive safe).
//
// Directories are emitted as zip entries with trailing-slash names
// so the extracted tree mirrors the source. Per-entry compression is
// zip.Deflate; the original archive's compression ratio is irrelevant
// since we're recompressing the decompressed bytes.
func WriteBundleZip(r io.Reader, format string, out *zip.Writer) error {
	switch {
	case format == "rar":
		return bundleRAR(r, out)
	case format == "tar" || strings.HasPrefix(format, "tar."):
		compressed := strings.TrimPrefix(format, "tar.")
		if compressed == "tar" {
			compressed = ""
		}
		return bundleTAR(r, compressed, out)
	case format == "tgz":
		return bundleTAR(r, "gz", out)
	case format == "tbz2":
		return bundleTAR(r, "bz2", out)
	case format == "txz":
		return bundleTAR(r, "xz", out)
	}
	return fmt.Errorf("archive.bundle: format %q needs ReaderAt — use WriteBundleZipReaderAt", format)
}

// WriteBundleZipReaderAt handles ZIP + 7z, which both need random
// access (header at end-of-file) to enumerate entries.
func WriteBundleZipReaderAt(r io.ReaderAt, size int64, format string, out *zip.Writer) error {
	switch format {
	case "zip":
		return bundleZIP(r, size, out)
	case "7z":
		return bundleSevenZip(r, size, out)
	}
	return fmt.Errorf("archive.bundle: unsupported format %q for ReaderAt path", format)
}

func bundleZIP(r io.ReaderAt, size int64, out *zip.Writer) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("archive.bundle.zip: open: %w", err)
	}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		isDir := f.FileInfo().IsDir() || strings.HasSuffix(name, "/")
		if isDir {
			if _, err := out.Create(name); err != nil {
				return fmt.Errorf("archive.bundle.zip: dir %q: %w", name, err)
			}
			continue
		}
		w, err := out.Create(name)
		if err != nil {
			return fmt.Errorf("archive.bundle.zip: entry %q: %w", name, err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("archive.bundle.zip: open %q: %w", name, err)
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return fmt.Errorf("archive.bundle.zip: copy %q: %w", name, err)
		}
		rc.Close()
	}
	return nil
}

func bundleSevenZip(r io.ReaderAt, size int64, out *zip.Writer) error {
	sr, err := sevenzip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("archive.bundle.7z: open: %w", err)
	}
	for _, f := range sr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if f.FileInfo().IsDir() {
			if _, err := out.Create(name + "/"); err != nil {
				return fmt.Errorf("archive.bundle.7z: dir %q: %w", name, err)
			}
			continue
		}
		w, err := out.Create(name)
		if err != nil {
			return fmt.Errorf("archive.bundle.7z: entry %q: %w", name, err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("archive.bundle.7z: open %q: %w", name, err)
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return fmt.Errorf("archive.bundle.7z: copy %q: %w", name, err)
		}
		rc.Close()
	}
	return nil
}

func bundleTAR(r io.Reader, compressedExt string, out *zip.Writer) error {
	var src io.Reader = r
	switch compressedExt {
	case "":
	case "gz":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("archive.bundle.tar.gz: open: %w", err)
		}
		src = gz
	case "bz2":
		src = bzip2.NewReader(r)
	case "xz":
		xr, err := xz.NewReader(r)
		if err != nil {
			return fmt.Errorf("archive.bundle.tar.xz: open: %w", err)
		}
		src = xr
	default:
		return fmt.Errorf("archive.bundle.tar: unknown compression %q", compressedExt)
	}
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive.bundle.tar: scan: %w", err)
		}
		name := strings.ReplaceAll(hdr.Name, "\\", "/")
		isDir := hdr.Typeflag == tar.TypeDir || strings.HasSuffix(name, "/")
		if isDir {
			if _, err := out.Create(name); err != nil {
				return fmt.Errorf("archive.bundle.tar: dir %q: %w", name, err)
			}
			continue
		}
		w, err := out.Create(name)
		if err != nil {
			return fmt.Errorf("archive.bundle.tar: entry %q: %w", name, err)
		}
		if _, err := io.Copy(w, tr); err != nil {
			return fmt.Errorf("archive.bundle.tar: copy %q: %w", name, err)
		}
	}
	return nil
}

func bundleRAR(r io.Reader, out *zip.Writer) error {
	rr, err := rardecode.NewReader(r)
	if err != nil {
		return fmt.Errorf("archive.bundle.rar: open: %w", err)
	}
	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive.bundle.rar: scan: %w", err)
		}
		name := strings.ReplaceAll(hdr.Name, "\\", "/")
		if hdr.IsDir {
			if _, err := out.Create(name + "/"); err != nil {
				return fmt.Errorf("archive.bundle.rar: dir %q: %w", name, err)
			}
			continue
		}
		w, err := out.Create(name)
		if err != nil {
			return fmt.Errorf("archive.bundle.rar: entry %q: %w", name, err)
		}
		if _, err := io.Copy(w, rr); err != nil {
			return fmt.Errorf("archive.bundle.rar: copy %q: %w", name, err)
		}
	}
	return nil
}
