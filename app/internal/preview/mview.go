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
// The flag-0 fast path covers thumbnails, which are JPEG (LZW doesn't
// help an already-entropy-coded stream). Compressed entries get the
// LZW decoder below — a clean port of github.com/majimboo/mviewer's
// archive.rs::decompress (custom 12-bit LZW, 4096-entry dict, dict
// resets when full). That code path means we can also reach textures
// and scene.json later if a phase wants to convert mview → glb at
// ingest.

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

// ErrMviewDecompress signals a corrupt or truncated LZW stream — the
// usual cause is a malformed source file; a clean .mview from
// Marmoset never trips this path.
var ErrMviewDecompress = errors.New("mview: lzw decompression failed")

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
				return decompressLZW(entry.Data, int(entry.UncompressedSize))
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

// decompressLZW expands a Marmoset-flavoured LZW stream into output of
// uncompressed_len bytes. Ported from
// github.com/majimboo/mviewer/blob/master/src/archive.rs::decompress.
//
// Notable shape of the algorithm:
//   * 12-bit codes packed two-per-three-bytes, with the parity of the
//     code index `r` deciding which nybble lands where.
//   * Dict starts at 256 (literal range covers all bytes); each step
//     adds an entry pointing at (prev_offset, prev_length + 1).
//   * The "code == next_code" branch is the classic LZW corner case
//     where the encoder produced a code that wasn't in the decoder's
//     dict yet — the new entry is `prev + first_byte_of_prev`.
//   * Dict capacity is 4096; reset to 256 once full.
//
// We translate the Rust `bail!` calls into ErrMviewDecompress wraps so
// the upstream extractor can surface a clean error.
func decompressLZW(input []byte, outputLen int) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("%w: empty stream", ErrMviewDecompress)
	}
	out := make([]byte, outputLen)
	var (
		tableOffsets [4096]int
		tableLengths [4096]int
		nextCode     = 256
		writeIdx     = 0
		prevOffset   = 0
		prevLength   = 1
	)

	ensureRoom := func(n int) error {
		if writeIdx+n > outputLen {
			return fmt.Errorf("%w: overflow", ErrMviewDecompress)
		}
		return nil
	}

	out[writeIdx] = input[0]
	writeIdx++

	r := 1
	for {
		packedIdx := r + (r >> 1)
		if packedIdx+1 >= len(input) {
			break
		}
		m := int(input[packedIdx+1])
		n := int(input[packedIdx])
		var code int
		if r&1 == 1 {
			code = (m << 4) | (n >> 4)
		} else {
			code = ((m & 15) << 8) | n
		}

		var entryOffset, entryLength int
		switch {
		case code < nextCode:
			if code < 256 {
				if err := ensureRoom(1); err != nil {
					return nil, err
				}
				out[writeIdx] = byte(code)
				entryOffset = writeIdx
				writeIdx++
				entryLength = 1
			} else {
				entryOffset = writeIdx
				length := tableLengths[code]
				src := tableOffsets[code]
				end := src + length
				if err := ensureRoom(length); err != nil {
					return nil, err
				}
				for src < end {
					out[writeIdx] = out[src]
					writeIdx++
					src++
				}
				entryLength = length
			}
		case code == nextCode:
			entryOffset = writeIdx
			length := prevLength + 1
			src := prevOffset
			end := prevOffset + prevLength
			if err := ensureRoom(length); err != nil {
				return nil, err
			}
			for src < end {
				out[writeIdx] = out[src]
				writeIdx++
				src++
			}
			out[writeIdx] = out[prevOffset]
			writeIdx++
			entryLength = length
		default:
			// Code outside the known dict — corrupt or truncated
			// stream. Bail out and let the caller surface it.
			return nil, fmt.Errorf("%w: code %d out of range (next=%d)", ErrMviewDecompress, code, nextCode)
		}

		tableOffsets[nextCode] = prevOffset
		tableLengths[nextCode] = prevLength + 1
		nextCode++
		prevOffset = entryOffset
		prevLength = entryLength
		if nextCode >= 4096 {
			nextCode = 256
		}
		r++
	}

	if writeIdx != outputLen {
		return nil, fmt.Errorf("%w: expected %d bytes, got %d", ErrMviewDecompress, outputLen, writeIdx)
	}
	return out, nil
}
