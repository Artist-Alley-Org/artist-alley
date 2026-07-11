// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package raw_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/raw"
)

// minimalJPEG returns a minimum-viable JPEG (SOI + APP0 + EOI) of
// the requested length. The bytes between header + footer are
// padding — the extractor only checks the JPEG magic at the start,
// not the structural validity, so this is sufficient for "we found
// an embedded preview" assertions.
func minimalJPEG(length int) []byte {
	if length < 4 {
		length = 4
	}
	b := make([]byte, length)
	b[0], b[1] = 0xFF, 0xD8 // SOI
	b[len(b)-2], b[len(b)-1] = 0xFF, 0xD9 // EOI
	return b
}

// buildTIFFRaw constructs a TIFF-based raw with the given preview
// JPEGs each in their own SubIFD. Returns the assembled bytes +
// the byte offset where each JPEG was placed (so the test can
// verify the extractor located them by content as well as length).
//
// Layout:
//
//	[0..7]    TIFF header (II*, first-IFD-offset)
//	[8..]     IFD0 (one entry: SubIFDs → [subIFD1, subIFD2, ...])
//	          + 4-byte next-IFD-offset (0 = end)
//	[...]     SubIFD offsets array
//	[...]     For each preview: one SubIFD pointing at the JPEG
//	[...]     JPEG bytes
//
// All offsets are little-endian. tag types are LONG (size 4) for
// simplicity — both the offset + length tags can be LONG even
// when the spec allows SHORT.
func buildTIFFRaw(t *testing.T, previewLengths []int) ([]byte, []int) {
	t.Helper()
	bo := binary.LittleEndian
	var buf bytes.Buffer
	// Header: II* (little-endian, standard TIFF magic), offset to IFD0
	buf.Write([]byte{'I', 'I', 0x2A, 0x00})
	// First-IFD-offset placeholder; rewrite once we know the position.
	writeU32(&buf, bo, 0)

	ifd0Offset := uint32(buf.Len())

	// IFD0 has ONE entry: SubIFDs.
	writeU16(&buf, bo, 1) // nEntries
	// SubIFDs entry: tag=0x014A, type=LONG, count=N, value=offset-to-list
	writeU16(&buf, bo, 0x014A)
	writeU16(&buf, bo, 4) // ttLong
	writeU32(&buf, bo, uint32(len(previewLengths)))
	// Value/offset slot: placeholder, fill in after laying out array.
	subListSlotPos := buf.Len()
	writeU32(&buf, bo, 0)
	// Next IFD offset = 0 (no more IFDs in the linked list).
	writeU32(&buf, bo, 0)

	// Lay out the SubIFD-offsets array.
	subListOffset := uint32(buf.Len())
	subSlotPositions := make([]int, len(previewLengths))
	for i := range previewLengths {
		subSlotPositions[i] = buf.Len()
		writeU32(&buf, bo, 0) // placeholder
	}

	// Lay out each SubIFD + its JPEG.
	jpegOffsets := make([]int, len(previewLengths))
	for i, plen := range previewLengths {
		subIFDOff := uint32(buf.Len())
		// Rewrite the corresponding placeholder.
		patchU32(buf.Bytes(), bo, subSlotPositions[i], subIFDOff)

		// 2 entries per SubIFD: JpegIFOffset + JpegIFByteCount.
		writeU16(&buf, bo, 2) // nEntries
		// JpegIFOffset
		writeU16(&buf, bo, 0x0201)
		writeU16(&buf, bo, 4) // LONG
		writeU32(&buf, bo, 1) // count
		jpegOffPos := buf.Len()
		writeU32(&buf, bo, 0) // placeholder
		// JpegIFByteCount
		writeU16(&buf, bo, 0x0202)
		writeU16(&buf, bo, 4) // LONG
		writeU32(&buf, bo, 1) // count
		writeU32(&buf, bo, uint32(plen))
		// Next IFD offset = 0 (terminator).
		writeU32(&buf, bo, 0)

		// JPEG payload immediately after.
		jpegOff := buf.Len()
		jpegOffsets[i] = jpegOff
		patchU32(buf.Bytes(), bo, jpegOffPos, uint32(jpegOff))
		buf.Write(minimalJPEG(plen))
	}

	// Finally: backfill IFD0 offset in the TIFF header (bytes 4-7).
	patchU32(buf.Bytes(), bo, 4, ifd0Offset)
	// And the SubIFDs-list pointer in IFD0.
	patchU32(buf.Bytes(), bo, subListSlotPos, subListOffset)

	return buf.Bytes(), jpegOffsets
}

func writeU16(b *bytes.Buffer, bo binary.ByteOrder, v uint16) {
	var tmp [2]byte
	bo.PutUint16(tmp[:], v)
	b.Write(tmp[:])
}
func writeU32(b *bytes.Buffer, bo binary.ByteOrder, v uint32) {
	var tmp [4]byte
	bo.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}
func patchU32(b []byte, bo binary.ByteOrder, off int, v uint32) {
	bo.PutUint32(b[off:off+4], v)
}

func TestSupports(t *testing.T) {
	e := raw.New()
	cases := map[string]bool{
		"image/x-canon-cr2":     true,
		"image/x-nikon-nef":     true,
		"image/x-adobe-dng":     true,
		"image/x-sony-arw":      true,
		"image/x-panasonic-rw2": true,
		"IMAGE/X-CANON-CR2":     true, // case insensitive
		"image/jpeg":            false,
		"application/pdf":       false,
		"":                      false,
	}
	for mime, want := range cases {
		if got := e.Supports(mime); got != want {
			t.Errorf("Supports(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestMimeTypeForExt(t *testing.T) {
	cases := map[string]string{
		"cr2":  "image/x-canon-cr2",
		"CR2":  "image/x-canon-cr2",
		".cr2": "image/x-canon-cr2",
		"nef":  "image/x-nikon-nef",
		"dng":  "image/x-adobe-dng",
		"arw":  "image/x-sony-arw",
		"rw2":  "image/x-panasonic-rw2",
		"jpg":  "",
		"raf":  "", // RAF is deliberately not in this phase
	}
	for ext, want := range cases {
		if got := raw.MimeTypeForExt(ext); got != want {
			t.Errorf("MimeTypeForExt(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestExtract_PicksLargestPreview(t *testing.T) {
	// Three previews: a small thumbnail (256 bytes), a medium
	// (2048 bytes), and a large (8192 bytes). Extractor must pick
	// the largest — that's what the variant pipeline wants.
	src, jpegOffsets := buildTIFFRaw(t, []int{256, 2048, 8192})

	res, err := raw.New().Extract(context.Background(), bytes.NewReader(src), "image/x-canon-cr2")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := len(res.PreviewImageBytes); got != 8192 {
		t.Fatalf("PreviewImageBytes length = %d, want 8192", got)
	}
	// Sanity: first two bytes are the JPEG SOI marker.
	if res.PreviewImageBytes[0] != 0xFF || res.PreviewImageBytes[1] != 0xD8 {
		t.Errorf("Preview doesn't start with JPEG SOI: %x %x",
			res.PreviewImageBytes[0], res.PreviewImageBytes[1])
	}
	// And the largest preview is the third one we wrote.
	want := src[jpegOffsets[2] : jpegOffsets[2]+8192]
	if !bytes.Equal(res.PreviewImageBytes, want) {
		t.Errorf("Preview bytes don't match the largest fixture preview")
	}
}

func TestExtract_BigEndianAlsoWorks(t *testing.T) {
	// Build a big-endian (MM) fixture with one preview. Real Nikon
	// NEFs from some bodies are big-endian; we don't want to
	// silently only-decode little-endian sources. We put the JPEG
	// pair directly on IFD0 (no SubIFD) to keep the byte-count math
	// trivial — the IFD walker is byte-order-independent so either
	// shape exercises the same code.
	bo := binary.BigEndian
	var buf bytes.Buffer
	buf.Write([]byte{'M', 'M', 0x00, 0x2A})
	writeU32(&buf, bo, 8) // IFD0 starts at byte 8

	// IFD0 with two entries: JpegIFOffset + JpegIFByteCount.
	writeU16(&buf, bo, 2)
	writeU16(&buf, bo, 0x0201)
	writeU16(&buf, bo, 4) // ttLong
	writeU32(&buf, bo, 1)
	jpegOffPos := buf.Len()
	writeU32(&buf, bo, 0) // placeholder
	writeU16(&buf, bo, 0x0202)
	writeU16(&buf, bo, 4)
	writeU32(&buf, bo, 1)
	writeU32(&buf, bo, 512)
	writeU32(&buf, bo, 0) // next-IFD = 0

	jpegStart := buf.Len()
	patchU32(buf.Bytes(), bo, jpegOffPos, uint32(jpegStart))
	buf.Write(minimalJPEG(512))

	res, err := raw.New().Extract(context.Background(), bytes.NewReader(buf.Bytes()), "image/x-nikon-nef")
	if err != nil {
		t.Fatalf("Extract big-endian: %v", err)
	}
	if len(res.PreviewImageBytes) != 512 {
		t.Errorf("PreviewImageBytes length = %d, want 512", len(res.PreviewImageBytes))
	}
}

func TestExtract_NoPreview_ReturnsErrNoMetadata(t *testing.T) {
	// Valid TIFF header with one IFD that carries no JPEG preview
	// tags + no SubIFDs. The extractor should return ErrNoMetadata,
	// not a malformed-file error — the upload is fine, there's just
	// nothing for the embedded-preview pipeline to grab.
	bo := binary.LittleEndian
	var buf bytes.Buffer
	buf.Write([]byte{'I', 'I', 0x2A, 0x00})
	writeU32(&buf, bo, 8)
	writeU16(&buf, bo, 0) // 0 entries
	writeU32(&buf, bo, 0) // next-IFD

	_, err := raw.New().Extract(context.Background(), bytes.NewReader(buf.Bytes()), "image/x-adobe-dng")
	if !errors.Is(err, metadata.ErrNoMetadata) {
		t.Errorf("err = %v, want ErrNoMetadata", err)
	}
}

func TestExtract_UnsupportedMIME(t *testing.T) {
	res, err := raw.New().Extract(context.Background(), bytes.NewReader([]byte("ignored")), "image/jpeg")
	if !errors.Is(err, metadata.ErrUnsupportedFormat) {
		t.Errorf("err = %v, want ErrUnsupportedFormat", err)
	}
	if len(res.PreviewImageBytes) != 0 {
		t.Errorf("PreviewImageBytes populated on unsupported MIME")
	}
}

func TestExtract_GarbageInput(t *testing.T) {
	res, err := raw.New().Extract(context.Background(), bytes.NewReader([]byte("\x00\x00\x00\x00\x00\x00\x00\x00")), "image/x-canon-cr2")
	if !errors.Is(err, metadata.ErrMalformedFile) {
		t.Errorf("err = %v, want ErrMalformedFile (wrapped)", err)
	}
	if res.Format != "image/x-canon-cr2" {
		t.Errorf("Format = %q, want image/x-canon-cr2", res.Format)
	}
}

func TestExtract_PreviewOffsetBeyondFile(t *testing.T) {
	// Build a TIFF where JpegIFOffset points outside the file. The
	// extractor must refuse to read out-of-bounds and return
	// ErrMalformedFile rather than slicing into uninitialised memory.
	bo := binary.LittleEndian
	var buf bytes.Buffer
	buf.Write([]byte{'I', 'I', 0x2A, 0x00})
	writeU32(&buf, bo, 8)
	writeU16(&buf, bo, 2)
	// JpegIFOffset = 999999 (way past file end)
	writeU16(&buf, bo, 0x0201)
	writeU16(&buf, bo, 4)
	writeU32(&buf, bo, 1)
	writeU32(&buf, bo, 999999)
	// JpegIFByteCount = 100
	writeU16(&buf, bo, 0x0202)
	writeU16(&buf, bo, 4)
	writeU32(&buf, bo, 1)
	writeU32(&buf, bo, 100)
	writeU32(&buf, bo, 0) // next-IFD

	_, err := raw.New().Extract(context.Background(), bytes.NewReader(buf.Bytes()), "image/x-adobe-dng")
	if !errors.Is(err, metadata.ErrMalformedFile) {
		t.Errorf("err = %v, want ErrMalformedFile", err)
	}
}

func TestExtract_RW2MagicAccepted(t *testing.T) {
	// RW2 uses 0x0055 in bytes 2-3 instead of standard 0x002A. Walker
	// must accept it.
	bo := binary.LittleEndian
	var buf bytes.Buffer
	buf.Write([]byte{'I', 'I', 0x55, 0x00}) // RW2 magic
	writeU32(&buf, bo, 8)
	// Single IFD with one JPEG preview
	writeU16(&buf, bo, 2)
	writeU16(&buf, bo, 0x0201)
	writeU16(&buf, bo, 4)
	writeU32(&buf, bo, 1)
	jpegOffPos := buf.Len()
	writeU32(&buf, bo, 0)
	writeU16(&buf, bo, 0x0202)
	writeU16(&buf, bo, 4)
	writeU32(&buf, bo, 1)
	writeU32(&buf, bo, 64)
	writeU32(&buf, bo, 0)
	jpegStart := buf.Len()
	patchU32(buf.Bytes(), bo, jpegOffPos, uint32(jpegStart))
	buf.Write(minimalJPEG(64))

	res, err := raw.New().Extract(context.Background(), bytes.NewReader(buf.Bytes()), "image/x-panasonic-rw2")
	if err != nil {
		t.Fatalf("RW2 Extract: %v", err)
	}
	if len(res.PreviewImageBytes) != 64 {
		t.Errorf("len = %d, want 64", len(res.PreviewImageBytes))
	}
}
