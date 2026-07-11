// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ExtractICCProfile pulls the ICC colour profile bytes out of an
// image source. Returns nil + nil if the source has no ICC profile
// (the normal case for screenshots, web images, AI-generated
// content); returns ([]byte, nil) when present.
//
// Per-format chunk shapes:
//
//   - JPEG: APP2 marker (0xFFE2) with payload prefix
//     "ICC_PROFILE\x00" + chunk_seq (1 byte) + chunk_count (1 byte)
//     + profile bytes. Profiles >64 KB span multiple APP2 chunks;
//     this function concatenates them in chunk_seq order.
//
//   - PNG: iCCP chunk with payload = profile_name + 0x00 +
//     compression_method (0=deflate) + zlib-compressed ICC bytes.
//     For round-trip purposes we return the raw COMPRESSED bytes
//     prefixed by the profile name + null byte + compression byte
//     so the variant writer can splice them back into a derivative
//     PNG verbatim. (We DON'T inflate the profile — chunk-copy
//     preserves byte-identical fidelity; AA's variant pipeline
//     doesn't need to read the profile's contents.)
//
//   - WebP: ICCP chunk inside the RIFF container. Payload is the
//     raw ICC profile bytes (no name prefix, no compression).
//
// Returns ErrMalformedFile only when the container's structural
// integrity is wrong (truncated chunk-length field, etc.); a
// missing ICC profile in an otherwise-valid file is NOT an error.
func ExtractICCProfile(raw []byte, mimeType string) ([]byte, error) {
	switch mimeType {
	case "image/jpeg", "image/jpg":
		return extractICCFromJPEG(raw)
	case "image/png":
		return extractICCFromPNG(raw)
	case "image/webp":
		return extractICCFromWebP(raw)
	}
	// TIFF + others don't have a standard ICC chunk position;
	// callers asking those formats get nil/nil (not an error —
	// just "no extractable profile").
	return nil, nil
}

// ---------------------------------------------------------------------------
// JPEG ICC (APP2 marker with multi-chunk concat)
// ---------------------------------------------------------------------------

const (
	jpegSOI    = 0xFFD8
	jpegEOI    = 0xFFD9
	jpegAPP2   = 0xFFE2
	jpegPrefix = "ICC_PROFILE\x00"
)

func extractICCFromJPEG(raw []byte) ([]byte, error) {
	if len(raw) < 4 || binary.BigEndian.Uint16(raw[:2]) != jpegSOI {
		return nil, fmt.Errorf("%w: not a JPEG (SOI marker missing)", ErrMalformedFile)
	}
	// Per-chunk: (seq, total, payload_bytes)
	type chunk struct {
		seq, total int
		bytes      []byte
	}
	var chunks []chunk
	i := 2
	for i+4 <= len(raw) {
		if raw[i] != 0xFF {
			return nil, fmt.Errorf("%w: lost JPEG marker alignment at offset %d", ErrMalformedFile, i)
		}
		marker := binary.BigEndian.Uint16(raw[i : i+2])
		i += 2
		if marker == jpegEOI {
			break
		}
		// SOS (0xFFDA) starts compressed image data — beyond it
		// there are no more APP markers worth scanning, just
		// entropy-coded bytes + the EOI. Stop here.
		if marker == 0xFFDA {
			break
		}
		// Length field includes its own 2 bytes but NOT the marker.
		if i+2 > len(raw) {
			return nil, fmt.Errorf("%w: truncated marker length at offset %d", ErrMalformedFile, i)
		}
		segLen := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if segLen < 2 || i+segLen > len(raw) {
			return nil, fmt.Errorf("%w: invalid segment length %d at offset %d", ErrMalformedFile, segLen, i)
		}
		payload := raw[i+2 : i+segLen]
		i += segLen
		if marker != jpegAPP2 {
			continue
		}
		if len(payload) < len(jpegPrefix)+2 || string(payload[:len(jpegPrefix)]) != jpegPrefix {
			continue
		}
		seq := int(payload[len(jpegPrefix)])
		total := int(payload[len(jpegPrefix)+1])
		profileBytes := payload[len(jpegPrefix)+2:]
		chunks = append(chunks, chunk{seq: seq, total: total, bytes: profileBytes})
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	// Sort by seq + concatenate. Multi-chunk profiles use 1-based
	// seq numbering (1..total).
	expected := chunks[0].total
	for _, c := range chunks {
		if c.total != expected {
			return nil, fmt.Errorf("%w: ICC chunks disagree on total count (%d vs %d)", ErrMalformedFile, expected, c.total)
		}
	}
	// Single-chunk profile (overwhelming majority): skip the sort.
	if len(chunks) == 1 && chunks[0].seq == 1 {
		return chunks[0].bytes, nil
	}
	// Multi-chunk: bucket by seq, then concat.
	if len(chunks) != expected {
		return nil, fmt.Errorf("%w: have %d ICC chunks, expected %d", ErrMalformedFile, len(chunks), expected)
	}
	out := make([]byte, 0)
	for seq := 1; seq <= expected; seq++ {
		found := false
		for _, c := range chunks {
			if c.seq == seq {
				out = append(out, c.bytes...)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: missing ICC chunk seq=%d", ErrMalformedFile, seq)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// PNG ICC (iCCP chunk; bytes returned in chunk-payload form)
// ---------------------------------------------------------------------------

var pngSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func extractICCFromPNG(raw []byte) ([]byte, error) {
	if len(raw) < len(pngSignature) || !bytes.Equal(raw[:len(pngSignature)], pngSignature) {
		return nil, fmt.Errorf("%w: not a PNG (signature missing)", ErrMalformedFile)
	}
	i := len(pngSignature)
	for i+8 <= len(raw) {
		chunkLen := int(binary.BigEndian.Uint32(raw[i : i+4]))
		chunkType := string(raw[i+4 : i+8])
		i += 8
		if i+chunkLen+4 > len(raw) {
			return nil, fmt.Errorf("%w: PNG chunk %q truncated (len %d)", ErrMalformedFile, chunkType, chunkLen)
		}
		data := raw[i : i+chunkLen]
		i += chunkLen + 4 // skip CRC32
		if chunkType == "iCCP" {
			// data = profile_name + 0x00 + compression_method + zlib-compressed ICC
			// Returned verbatim — chunk-copy preserves byte-fidelity
			// without inflating the profile.
			return append([]byte(nil), data...), nil
		}
		if chunkType == "IDAT" {
			// Once we hit IDAT we're past the metadata; iCCP must
			// appear before IDAT per the PNG spec.
			break
		}
		if chunkType == "IEND" {
			break
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// WebP ICC (RIFF ICCP chunk)
// ---------------------------------------------------------------------------

func extractICCFromWebP(raw []byte) ([]byte, error) {
	if len(raw) < 12 {
		return nil, fmt.Errorf("%w: WebP truncated", ErrMalformedFile)
	}
	if string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		return nil, fmt.Errorf("%w: not a WebP RIFF container", ErrMalformedFile)
	}
	i := 12
	for i+8 <= len(raw) {
		chunkID := string(raw[i : i+4])
		chunkLen := int(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
		i += 8
		if i+chunkLen > len(raw) {
			return nil, fmt.Errorf("%w: WebP chunk %q truncated", ErrMalformedFile, chunkID)
		}
		if chunkID == "ICCP" {
			out := make([]byte, chunkLen)
			copy(out, raw[i:i+chunkLen])
			return out, nil
		}
		i += chunkLen
		// RIFF chunks are word-aligned — skip the pad byte when
		// chunkLen is odd.
		if chunkLen%2 == 1 {
			i++
		}
	}
	return nil, nil
}

// Sentinel re-export for callers that import this package but not
// the parent (the few exists). Mostly kept for symmetry; new code
// should import the parent package directly.
var ErrICCAbsent = errors.New("metadata: source has no ICC profile")
