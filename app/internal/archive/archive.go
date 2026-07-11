// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package archive parses container files (zip / tar / 7z / rar /
// etc) into a uniform Manifest shape the preview pipeline can
// cache in metadata.archive and the frontend ArchiveView can browse.
//
// Design — first cut ships two Go-stdlib parsers (ZIP + TAR) so
// the surface compiles with zero new deps. 7z + RAR slot in
// behind the same dispatch (`parserFor(ext)`) when we want them;
// they'll require external libraries.
//
// Manifest extraction is read-only and cheap for ZIP: stdlib
// `archive/zip` reads the central directory from the END of the
// file (the last ~22-65 KB). For TAR the whole file must be
// scanned once to build the entry list since TAR is stream-only;
// the preview job caches the result so subsequent reads are free.
//
// Per-entry extraction lives in extract.go (one call per click in
// the ArchiveView). Both parsers expose a `Open(entry) io.ReadCloser`
// so the caller doesn't need to know the archive kind.

package archive

import (
	"fmt"
	"strings"
	"time"
)

// Entry is one file (or directory) inside an archive. Path uses
// forward slashes regardless of the source OS so the frontend tree
// builder can split deterministically. Compressed/uncompressed
// sizes diverge for ZIP; they're equal for TAR (which doesn't
// compress inside the format).
type Entry struct {
	Path           string    `json:"path"`
	Size           int64     `json:"size"`
	CompressedSize int64     `json:"compressed_size,omitempty"`
	Modified       time.Time `json:"modified,omitempty"`
	IsDir          bool      `json:"is_dir,omitempty"`
	// Comment is the per-entry ZIP comment (often empty). Surfaced
	// in the panel only when set — most archives don't use it.
	Comment string `json:"comment,omitempty"`
}

// Manifest is what we persist into asset.metadata.archive. Format
// names the source parser ("zip" / "tar"); EntryCount mirrors
// len(Entries) but lives on the wire so a future "truncated"
// flag can report the original count without inflating Entries.
type Manifest struct {
	Format     string  `json:"format"`
	Entries    []Entry `json:"entries"`
	EntryCount int     `json:"entry_count"`
	// Truncated signals the manifest hit MaxEntries and skipped
	// the rest. The UI renders a warning + offers a "show all" path
	// that re-runs the manifest extraction with a higher cap. Empty
	// for normal archives.
	Truncated bool `json:"truncated,omitempty"`
	// TotalSize sums every entry's uncompressed bytes. Useful for
	// the "Extract all" estimate the panel surfaces.
	TotalSize int64 `json:"total_size,omitempty"`
}

// MaxEntries caps a single manifest to keep its JSON payload
// bounded. Real-world archives almost never approach 10k entries
// (a typical asset bundle has 100-1000); pathological cases (a
// scraped node_modules zip with 50k files) truncate.
const MaxEntries = 10000

// ParseFormat resolves the lower-cased extension into a parser
// name. Returns "" for unsupported formats (the caller usually
// surfaces this as "preview not available").
func ParseFormat(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "zip", "jar", "war", "ear", "apk", "ipa":
		return "zip"
	case "7z":
		return "7z"
	case "rar":
		return "rar"
	case "tar":
		return "tar"
	case "tgz":
		return "tar.gz"
	case "tbz2":
		return "tar.bz2"
	case "txz":
		return "tar.xz"
	case "gz":
		// Bare .gz is single-file compression, not an archive.
		// We don't surface those here — the file viewer can
		// transparently decompress when the user opens it.
		return ""
	}
	// Compound extensions: foo.tar.gz / foo.tar.bz2 / foo.tar.xz.
	// Returns the full compound name so the dispatcher knows which
	// decompressor to wrap around the tar reader.
	low := strings.ToLower(ext)
	for _, sfx := range []string{".tar.gz", ".tar.bz2", ".tar.xz"} {
		if strings.HasSuffix(low, sfx) {
			return strings.TrimPrefix(sfx, ".")
		}
	}
	return ""
}

// SupportedExtensions lists every extension Run can manifest.
// Surfaced by the openapi spec + the frontend's ARCHIVE_EXTS so
// the kind-routing table stays in lock-step.
func SupportedExtensions() []string {
	return []string{
		"zip", "jar", "war", "ear", "apk", "ipa",
		"7z", "rar",
		"tar", "tgz", "tbz2", "txz",
		"tar.gz", "tar.bz2", "tar.xz",
	}
}

// errUnsupported is the sentinel a caller checks when no parser
// matched. Distinct from a parse error so the dispatcher can
// decide "skip preview" vs "mark failed".
type errUnsupported struct{ ext string }

func (e *errUnsupported) Error() string {
	return fmt.Sprintf("archive: no parser for %q", e.ext)
}

// IsUnsupported reports whether err signals "format not handled".
func IsUnsupported(err error) bool {
	_, ok := err.(*errUnsupported)
	return ok
}
