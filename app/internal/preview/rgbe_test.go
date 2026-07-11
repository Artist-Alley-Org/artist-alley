// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"fmt"
	"image/color"
	"testing"
)

// TestDecodeRadiance_SmallNewFormat builds a minimal Radiance file
// by hand and decodes it. We deliberately bypass the new-format RLE
// header path (width < 8) so the fixture stays tiny — the RLE path
// has its own test below.
func TestDecodeRadiance_NoRLE(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("#?RADIANCE\n")
	buf.WriteString("FORMAT=32-bit_rle_rgbe\n")
	buf.WriteString("\n")
	buf.WriteString("-Y 2 +X 4\n")
	// Four pixels per row; old-format scanline (no 2,2,w_hi,w_lo
	// preamble since width < 8). All pixels: 0.5,0.5,0.5 linear.
	//   0.5 = mantissa * 2^(e - 128 - 8)
	//   pick e = 128 (so 2^-8 = 1/256), mantissa = 128 → 128/256 = 0.5
	px := []byte{128, 128, 128, 128}
	for i := 0; i < 2*4; i++ {
		buf.Write(px)
	}
	img, err := decodeRadiance(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != 4 {
		t.Fatalf("width = %d, want 4", got)
	}
	if got := img.Bounds().Dy(); got != 2 {
		t.Fatalf("height = %d, want 2", got)
	}
	// Reinhard(0.5) = 0.5/1.5 = 0.333, then sRGB-encode → ~159.
	c := color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA)
	if c.R < 140 || c.R > 175 {
		t.Fatalf("R = %d, expected ~159 (reinhard+sRGB of 0.5 linear)", c.R)
	}
	if c.R != c.G || c.G != c.B {
		t.Fatalf("expected neutral grey, got %v", c)
	}
	if c.A != 255 {
		t.Fatalf("A = %d, want 255", c.A)
	}
}

// TestDecodeRadiance_RLE exercises the new-format per-channel RLE
// path: width=16 (≥8) so readScanline writes a 2,2,w_hi,w_lo header
// and RLE-encodes each of R,G,B,E separately across the scanline.
func TestDecodeRadiance_RLE(t *testing.T) {
	const w, h = 16, 1
	var buf bytes.Buffer
	buf.WriteString("#?RADIANCE\n")
	buf.WriteString("\n")
	fmt.Fprintf(&buf, "-Y %d +X %d\n", h, w)
	// new-format header
	buf.WriteByte(2)
	buf.WriteByte(2)
	buf.WriteByte(byte(w >> 8))
	buf.WriteByte(byte(w & 0xff))
	// Each channel: a single run of 16 bytes = (128+16, value).
	for _, v := range []byte{128, 64, 32, 128} { // R G B E
		buf.WriteByte(128 + 16)
		buf.WriteByte(v)
	}
	img, err := decodeRadiance(&buf)
	if err != nil {
		t.Fatalf("decode RLE: %v", err)
	}
	c := color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA)
	c2 := color.NRGBAModel.Convert(img.At(w-1, 0)).(color.NRGBA)
	if c != c2 {
		t.Fatalf("RLE pixels should be identical: %v vs %v", c, c2)
	}
	if c.R == 0 && c.G == 0 && c.B == 0 {
		t.Fatalf("RLE pixel decoded to black: %v", c)
	}
}

func TestDecodeRadiance_BadMagic(t *testing.T) {
	buf := bytes.NewBufferString("NOT A RADIANCE FILE\n\n-Y 1 +X 1\n")
	if _, err := decodeRadiance(buf); err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestDecodeRadiance_UnsupportedOrientation(t *testing.T) {
	buf := bytes.NewBufferString("#?RADIANCE\n\n+Y 2 +X 2\nABCDEFGHIJKLMNOP")
	if _, err := decodeRadiance(buf); err == nil {
		t.Fatal("expected error for +Y orientation")
	}
}

func TestRGBEZeroExponent(t *testing.T) {
	r, g, b := rgbeToFloat(255, 255, 255, 0)
	if r != 0 || g != 0 || b != 0 {
		t.Fatalf("e=0 should yield black; got %v %v %v", r, g, b)
	}
}
