package preview

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Marmoset Viewer (.mview) archive parser.
//
// The format is a custom TAR-like container, not ZIP. Each entry is
// laid out sequentially as:
//
//   name              null-terminated string (UTF-8)
//   type              null-terminated string (MIME-ish, e.g. "image/jpeg")
//   flags             uint32 little-endian
//                     bit 0: data is LZ77-compressed
//   compressed_size   uint32 little-endian
//   uncompressed_size uint32 little-endian
//   data              compressed_size bytes
//
// The file is parsed top-to-bottom — no central directory. Marmoset's
// own fetchThumbnail() relies on this: 'thumbnail.jpg' lives early in
// the archive so a partial 64KB GET is enough to extract it.
//
// We don't need the LZ77 decompressor for thumbnails: JPEG is already
// compressed, so the thumbnail entry is invariably stored with flag 0
// (uncompressed). The handler refuses compressed entries with a clear
// error rather than embedding a parser for Marmoset's custom LZ77
// variant — that can land as a follow-up if a sample ever shows up.

const (
	// mviewMaxStringLen caps null-terminated string reads so a
	// malformed file can't get us to gobble unbounded bytes looking
	// for a terminator. 256 bytes is plenty for any sane filename or
	// MIME-ish type field.
	mviewMaxStringLen = 256

	// mviewMaxEntrySize caps a single entry's data length. 32MB is
	// well above any reasonable thumbnail size and also blocks
	// memory-bomb shapes.
	mviewMaxEntrySize = 32 * 1024 * 1024

	// mviewFlagCompressed is the bit in flags that says the entry's
	// data is LZ77-compressed.
	mviewFlagCompressed = 0x1
)

// MviewEntry is one named blob inside a .mview archive.
type MviewEntry struct {
	Name             string
	Type             string
	Flags            uint32
	CompressedSize   uint32
	UncompressedSize uint32
	Data             []byte
}

// ErrMviewEntryCompressed is returned when the requested entry has
// the LZ77 flag set. The thumbnail is invariably stored uncompressed;
// any other compressed entry is a follow-up to support.
var ErrMviewEntryCompressed = errors.New("mview: entry is compressed (LZ77 decompressor not implemented)")

// ExtractMviewThumbnail streams the archive in `r` and returns the
// first entry whose name is "thumbnail.jpg". Reads sequentially so
// it works on a partial download — once the thumbnail entry is fully
// read we stop without consuming the rest of the archive.
func ExtractMviewThumbnail(r io.Reader) ([]byte, error) {
	return ExtractMviewEntry(r, "thumbnail.jpg")
}

// ExtractMviewEntry returns the first entry whose name matches `want`.
// On success the entry's data is in memory; on a match-not-found case
// you get io.EOF, which lets the caller distinguish from a malformed-
// archive failure.
func ExtractMviewEntry(r io.Reader, want string) ([]byte, error) {
	br := bufio.NewReader(r)
	for {
		entry, err := readMviewEntry(br)
		if err != nil {
			return nil, err
		}
		if entry.Name == want {
			if entry.Flags&mviewFlagCompressed != 0 {
				return nil, ErrMviewEntryCompressed
			}
			return entry.Data, nil
		}
		// Not the entry we want — body is already consumed by
		// readMviewEntry, just loop around for the next one.
	}
}

func readMviewEntry(br *bufio.Reader) (*MviewEntry, error) {
	name, err := readCString(br, mviewMaxStringLen)
	if err != nil {
		return nil, fmt.Errorf("read name: %w", err)
	}
	typ, err := readCString(br, mviewMaxStringLen)
	if err != nil {
		return nil, fmt.Errorf("read type: %w", err)
	}
	var hdr struct {
		Flags            uint32
		CompressedSize   uint32
		UncompressedSize uint32
	}
	if err := binary.Read(br, binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if hdr.CompressedSize > mviewMaxEntrySize {
		return nil, fmt.Errorf("entry %q exceeds size cap: %d > %d",
			name, hdr.CompressedSize, mviewMaxEntrySize)
	}
	data := make([]byte, hdr.CompressedSize)
	if _, err := io.ReadFull(br, data); err != nil {
		return nil, fmt.Errorf("read data for %q: %w", name, err)
	}
	return &MviewEntry{
		Name:             name,
		Type:             typ,
		Flags:            hdr.Flags,
		CompressedSize:   hdr.CompressedSize,
		UncompressedSize: hdr.UncompressedSize,
		Data:             data,
	}, nil
}

// readCString reads up to maxLen bytes looking for a null terminator.
// Returns the string (without the terminator) or an error if the
// terminator isn't found within maxLen bytes.
func readCString(br *bufio.Reader, maxLen int) (string, error) {
	var buf bytes.Buffer
	for buf.Len() <= maxLen {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if b == 0 {
			return buf.String(), nil
		}
		_ = buf.WriteByte(b)
	}
	return "", fmt.Errorf("cstring exceeded %d bytes without null", maxLen)
}
