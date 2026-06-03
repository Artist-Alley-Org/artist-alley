package archive

import (
	"fmt"
	"io"
	"strings"

	rardecode "github.com/nwaples/rardecode/v2"
)

// ManifestRAR scans every RAR header from the start of the stream.
// RAR doesn't ship a centralised directory we can read in O(1)
// (RAR5 has an end-of-archive locator but the Go library doesn't
// expose it); both manifest extraction and per-entry reads are
// O(file size). Caller should run this from the preview job.
//
// Multi-volume archives (.rar / .r00 / .r01 ...) aren't handled
// here — those need the rardecode "Volume" hook with all part
// streams concatenated. Single-volume covers the common case.
func ManifestRAR(r io.Reader) (*Manifest, error) {
	rr, err := rardecode.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("archive.rar: open: %w", err)
	}
	out := &Manifest{Format: "rar"}
	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Truncated = true
			break
		}
		if len(out.Entries) >= MaxEntries {
			out.Truncated = true
			break
		}
		path := strings.ReplaceAll(hdr.Name, "\\", "/")
		entry := Entry{
			Path:           path,
			Size:           hdr.UnPackedSize,
			CompressedSize: hdr.PackedSize,
			Modified:       hdr.ModificationTime,
			IsDir:          hdr.IsDir,
		}
		out.Entries = append(out.Entries, entry)
		if !entry.IsDir {
			out.TotalSize += entry.Size
		}
	}
	out.EntryCount = len(out.Entries)
	return out, nil
}

// OpenRARStreamEntry walks the stream looking for `path` and
// returns a ReadCloser that yields its decompressed bytes.
// Stream-only: every call re-opens the archive from byte 0. A
// future optimisation could maintain a path→offset index from
// the manifest pass, but RAR's solid-archive mode makes random
// access unreliable (the decompressor depends on prior entries),
// so streaming is the safest baseline.
func OpenRARStreamEntry(r io.Reader, path string) (io.ReadCloser, *rardecode.FileHeader, error) {
	rr, err := rardecode.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("archive.rar: open: %w", err)
	}
	want := strings.ReplaceAll(path, "\\", "/")
	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			return nil, nil, fmt.Errorf("archive.rar: entry %q not found", path)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("archive.rar: scan: %w", err)
		}
		if strings.ReplaceAll(hdr.Name, "\\", "/") != want {
			continue
		}
		// rardecode's Reader keeps state inside the archive — we
		// wrap as NopCloser so the caller can defer rc.Close()
		// uniformly across archive kinds.
		return io.NopCloser(rr), hdr, nil
	}
}
