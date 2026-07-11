// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Friendly wrapper around the generated Kaitai parser (abr.go).
//
// The generated code mirrors the binary layout verbatim — useful for
// correctness but ugly to call from product code. This file exposes
// the two things product code actually needs:
//
//   - Brush:        decoded stamp + metadata, ready to render
//   - ParseBrushes: read a .abr stream into []Brush
//
// PackBits RLE decompression is handled here too — Kaitai gives us
// the raw compressed payload from the `image_data` block, but the
// pixel-level decoding (PSD's per-scanline PackBits with a row-count
// preamble) is product code.
//
// References:
//   - PSD file format spec (Adobe, "Image data" section) — describes
//     PackBits RLE used by both PSD and ABR raster data.
//   - jlai/brush-viewer — the ksy source; their TS wrapper handled
//     the same RLE decoding we re-do here in Go.

package abr

import (
	"bytes"
	"fmt"
	"image"
	"io"

	"github.com/kaitai-io/kaitai_struct_go_runtime/kaitai"
)

// Brush is one decoded brush stamp from a .abr file.
//
// The stamp itself is an alpha mask (grayscale 0..255 where 255 =
// fully solid, 0 = transparent). The renderer composes it with the
// stroke color at draw time, the same way Photoshop / Krita / GIMP
// all do — brush bitmaps don't carry color, only shape + density.
type Brush struct {
	// Brush UUID lifted from the sample's pascal string. Stable
	// across re-imports of the same .abr, so it works as a primary
	// key for our brush-pack storage.
	ID string

	// Stamp dimensions in pixels.
	Width, Height int

	// Alpha mask, row-major, len == Width*Height. Each byte is
	// 0..255 where 255 = solid.
	Alpha []byte
}

// ParseBrushes parses a .abr stream into a slice of decoded brush
// stamps. Returns an error if the header is malformed or if any
// sample fails to decode (we choose strict failure over partial
// results so the caller can decide what to do — re-export the .abr
// from Photoshop, ignore it, etc).
//
// Memory: the entire input is held in memory by the Kaitai stream
// (`ReadBytesFull` etc), plus a decoded Alpha slice per brush. For
// the 54 MB ABR pack in the test corpus that's roughly 2× memory
// during parse. Callers that need to bound RAM should stream
// samples one at a time using the generated `Abr.Sections` iterator
// directly — this helper trades that flexibility for a simple API.
func ParseBrushes(r io.Reader) ([]Brush, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("abr: read input: %w", err)
	}
	stream := kaitai.NewStream(bytes.NewReader(data))
	parsed := NewAbr()
	if err := parsed.Read(stream, nil, parsed); err != nil {
		return nil, fmt.Errorf("abr: parse: %w", err)
	}
	var brushes []Brush
	for _, section := range parsed.Sections {
		if section.Subtag != Abr_Subtag__Samp {
			// We only care about brush samples for the renderer.
			// Descriptor block (Abr_Subtag__Desc) carries the
			// dynamics (spacing, jitter, etc) — wired in a later
			// commit once we have a stamp renderer to feed.
			continue
		}
		sampleBody, ok := section.Body.(*Abr_SamplesSectionBody)
		if !ok {
			return nil, fmt.Errorf("abr: samp body is %T, expected *Abr_SamplesSectionBody", section.Body)
		}
		for i, sample := range sampleBody.Samples {
			brush, err := decodeSample(sample)
			if err != nil {
				return nil, fmt.Errorf("abr: sample %d: %w", i, err)
			}
			brushes = append(brushes, brush)
		}
	}
	return brushes, nil
}

// decodeSample pulls the brush ID + image_data out of one sample
// and runs the PackBits RLE decoder on the raw bitmap bytes. For
// v6.2 samples (the only version we currently support) the relevant
// channel is the first one written — that's the alpha mask we want.
func decodeSample(sample *Abr_Sample) (Brush, error) {
	sd := sample.Data
	if sd == nil {
		return Brush{}, fmt.Errorf("nil sample data")
	}
	brushID := string(sd.BrushId)
	if sd.BodyV62 == nil {
		// v6.1 sample, not currently handled. We could add a
		// branch here that falls back to sd.BodyV61, but the test
		// corpus is all v6.2 and v6.1 brushes are rarer.
		return Brush{}, fmt.Errorf("unsupported sample subversion (v6.1)")
	}
	for _, ch := range sd.BodyV62.Channels {
		if ch == nil || ch.IsWritten == 0 || ch.ImageData == nil {
			continue
		}
		brush, err := decodeImageData(ch.ImageData)
		if err != nil {
			return Brush{}, fmt.Errorf("decode: %w", err)
		}
		brush.ID = brushID
		return brush, nil
	}
	return Brush{}, fmt.Errorf("no written channel")
}

// decodeImageData runs the per-row PackBits decoder over the raw
// bitmap bytes. The bitmap format is the same one PSD uses for
// layer image data:
//
//   - First (h * 2) bytes: big-endian uint16 per scanline giving
//     the compressed byte length of that row. We use these to find
//     row boundaries — without them PackBits can't be unambiguously
//     re-segmented because terminating runs aren't marked.
//   - Then the per-row PackBits-encoded payload, concatenated.
//
// PackBits opcodes:
//   - n in [0, 127]:  next (n+1) bytes are literal
//   - n in [-127, -1] (signed): repeat the next byte (-n+1) times
//   - n == -128 (0x80): no-op
//
// We only support depth=8 (one byte per pixel = alpha 0..255).
// Higher depths exist in PSD-land but ABR brushes are always 8-bit
// per channel — they're masks, not photographs.
func decodeImageData(img *Abr_ImageData) (Brush, error) {
	if img.Depth != 8 {
		return Brush{}, fmt.Errorf("unsupported bit depth %d (only 8 supported)", img.Depth)
	}
	w := int(img.Right - img.Left)
	h := int(img.Bottom - img.Top)
	if w <= 0 || h <= 0 {
		return Brush{}, fmt.Errorf("invalid bounds %dx%d", w, h)
	}
	raw := img.Bitmap
	var alpha []byte
	var err error
	switch img.Compression {
	case 0: // raw
		if len(raw) < w*h {
			return Brush{}, fmt.Errorf("raw bitmap too short: %d bytes, expected %d", len(raw), w*h)
		}
		alpha = raw[:w*h]
	case 1: // PackBits RLE
		alpha, err = decompressPackBits(raw, w, h)
		if err != nil {
			return Brush{}, err
		}
	default:
		return Brush{}, fmt.Errorf("unsupported compression %d", img.Compression)
	}
	return Brush{Width: w, Height: h, Alpha: alpha}, nil
}

// decompressPackBits decodes h rows of PackBits-encoded data into a
// flat w*h alpha buffer. Each row is preceded by a uint16 BE giving
// that row's compressed byte length; we use those to seek per row
// rather than relying on PackBits's self-terminating semantics.
func decompressPackBits(data []byte, w, h int) ([]byte, error) {
	if len(data) < h*2 {
		return nil, fmt.Errorf("rle: missing row-length table (need %d bytes, have %d)", h*2, len(data))
	}
	rowLens := make([]int, h)
	for i := 0; i < h; i++ {
		rowLens[i] = int(data[i*2])<<8 | int(data[i*2+1])
	}
	body := data[h*2:]
	out := make([]byte, 0, w*h)
	rowBuf := make([]byte, 0, w)
	pos := 0
	for row := 0; row < h; row++ {
		rowEnd := pos + rowLens[row]
		if rowEnd > len(body) {
			return nil, fmt.Errorf("rle: row %d overruns payload (need %d bytes, have %d)", row, rowEnd, len(body))
		}
		rowBuf = rowBuf[:0]
		for pos < rowEnd {
			op := int8(body[pos])
			pos++
			switch {
			case op >= 0:
				// Literal run: next (op+1) bytes are copied.
				n := int(op) + 1
				if pos+n > rowEnd {
					return nil, fmt.Errorf("rle: literal run overflows row %d", row)
				}
				rowBuf = append(rowBuf, body[pos:pos+n]...)
				pos += n
			case op == -128:
				// No-op per the PSD spec. Some encoders never emit
				// this; we tolerate it for compat.
			default:
				// Repeat run: next byte repeated (-op + 1) times.
				if pos >= rowEnd {
					return nil, fmt.Errorf("rle: repeat-run missing payload byte (row %d)", row)
				}
				n := -int(op) + 1
				b := body[pos]
				pos++
				for i := 0; i < n; i++ {
					rowBuf = append(rowBuf, b)
				}
			}
		}
		if len(rowBuf) != w {
			return nil, fmt.Errorf("rle: row %d decoded to %d bytes, expected %d", row, len(rowBuf), w)
		}
		out = append(out, rowBuf...)
	}
	return out, nil
}

// AsImage returns the brush as an image.Gray so callers can hand it
// straight to image/png. The Alpha slice is shared (no copy).
func (b Brush) AsImage() *image.Gray {
	return &image.Gray{
		Pix:    b.Alpha,
		Stride: b.Width,
		Rect:   image.Rect(0, 0, b.Width, b.Height),
	}
}
