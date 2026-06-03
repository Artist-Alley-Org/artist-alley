package archive

import (
	"fmt"
	"io"
	"strings"

	"github.com/bodgit/sevenzip"
)

// ManifestSevenZip reads a 7z archive via the bodgit/sevenzip
// library. 7z stores its header at the END of the file (similar
// to ZIP), so manifest extraction is bounded by the header size,
// not the archive size — typically <1 MB even for multi-GB 7z
// archives.
//
// Caller passes io.ReaderAt + size the same way as the ZIP path.
func ManifestSevenZip(r io.ReaderAt, size int64) (*Manifest, error) {
	sr, err := sevenzip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("archive.7z: open: %w", err)
	}
	out := &Manifest{Format: "7z"}
	for i, f := range sr.File {
		if i >= MaxEntries {
			out.Truncated = true
			break
		}
		path := strings.ReplaceAll(f.Name, "\\", "/")
		entry := Entry{
			Path:           path,
			Size:           int64(f.UncompressedSize),
			CompressedSize: 0, // sevenzip exposes a stream-level
			//                    compressed size, not per-entry
			Modified: f.Modified,
			IsDir:    f.FileInfo().IsDir(),
		}
		out.Entries = append(out.Entries, entry)
		if !entry.IsDir {
			out.TotalSize += entry.Size
		}
	}
	out.EntryCount = len(out.Entries)
	return out, nil
}

// OpenSevenZipEntry locates `path` and returns a ReadCloser
// positioned at the start of its decompressed bytes. Mirrors
// OpenZIPEntry's contract — returns (nil, hdr, nil) for
// directory entries.
func OpenSevenZipEntry(r io.ReaderAt, size int64, path string) (io.ReadCloser, *sevenzip.File, error) {
	sr, err := sevenzip.NewReader(r, size)
	if err != nil {
		return nil, nil, fmt.Errorf("archive.7z: open: %w", err)
	}
	want := strings.ReplaceAll(path, "\\", "/")
	for _, f := range sr.File {
		fp := strings.ReplaceAll(f.Name, "\\", "/")
		if fp != want {
			continue
		}
		if f.FileInfo().IsDir() {
			return nil, f, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("archive.7z: open entry: %w", err)
		}
		return rc, f, nil
	}
	return nil, nil, fmt.Errorf("archive.7z: entry %q not found", path)
}
