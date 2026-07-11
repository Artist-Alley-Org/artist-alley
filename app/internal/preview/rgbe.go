// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"strings"
)

// decodeRadiance parses a Radiance HDR / .pic stream (RGBE-encoded,
// optionally RLE-compressed per scanline) and returns a tonemapped
// sRGB image suitable for the rest of the variant pipeline.
//
// Why Go and not ffmpeg: Debian bookworm's ffmpeg only ships the
// `exr` decoder, not the `hdr` Radiance decoder, so the obvious
// "let ffmpeg handle it" path fails in our runtime container. The
// format is small (~150 LoC of decoding) and well-specified
// (Greg Ward, 1991), so owning it natively avoids both the missing-
// decoder problem and a non-default apt package on every host.
//
// Tonemapping: a fixed-exposure Reinhard curve, then encoded gamma
// 2.2 → sRGB display. Good enough for a thumbnail; the worker is
// not a colour-managed renderer. The curve mirrors what ffmpeg's
// `tonemap=mobius` would have produced for typical HDRI input,
// without the libzimg dependency.
func decodeRadiance(r io.Reader) (image.Image, error) {
	br := bufio.NewReader(r)

	// ── ASCII header ────────────────────────────────────────────────
	// Radiance headers are a magic line ("#?RADIANCE" or "#?RGBE")
	// followed by KEY=VALUE pairs, terminated by a blank line, then
	// a single resolution line (e.g. "-Y 512 +X 1024").
	magic, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("radiance: read magic: %w", err)
	}
	magic = strings.TrimRight(magic, "\r\n")
	if !strings.HasPrefix(magic, "#?") {
		return nil, fmt.Errorf("radiance: bad magic %q", magic)
	}

	format := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("radiance: header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "FORMAT=") {
			format = strings.TrimPrefix(line, "FORMAT=")
		}
	}
	if format != "" && format != "32-bit_rle_rgbe" && format != "32-bit_rle_xyze" {
		return nil, fmt.Errorf("radiance: unsupported FORMAT %q", format)
	}

	// Resolution line. We only handle the common "-Y H +X W"
	// (top-to-bottom, left-to-right) orientation Blender / Substance /
	// HDRI-Haven all emit; other flips are spec-legal but vanishingly
	// rare in artist-pipeline HDRIs.
	res, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("radiance: resolution: %w", err)
	}
	var sy, sx string
	var h, w int
	if _, err := fmt.Sscanf(strings.TrimSpace(res), "%s %d %s %d", &sy, &h, &sx, &w); err != nil {
		return nil, fmt.Errorf("radiance: bad resolution line %q: %w", res, err)
	}
	if sy != "-Y" || sx != "+X" {
		return nil, fmt.Errorf("radiance: unsupported orientation %q %q", sy, sx)
	}
	if w <= 0 || h <= 0 || w > 32768 || h > 32768 {
		return nil, fmt.Errorf("radiance: implausible dimensions %dx%d", w, h)
	}

	// ── Scanlines ──────────────────────────────────────────────────
	// Per-scanline RGBE buffer; reused across the loop.
	scan := make([]byte, w*4)
	// Linear-light RGB plane in [0..∞); built up across scanlines.
	linR := make([]float32, w*h)
	linG := make([]float32, w*h)
	linB := make([]float32, w*h)

	for y := 0; y < h; y++ {
		if err := readScanline(br, scan, w); err != nil {
			return nil, fmt.Errorf("radiance: scanline %d: %w", y, err)
		}
		off := y * w
		for x := 0; x < w; x++ {
			r := scan[x*4+0]
			g := scan[x*4+1]
			b := scan[x*4+2]
			e := scan[x*4+3]
			fr, fg, fb := rgbeToFloat(r, g, b, e)
			linR[off+x] = fr
			linG[off+x] = fg
			linB[off+x] = fb
		}
	}

	// ── Tonemap + sRGB encode ──────────────────────────────────────
	//
	// Reinhard: out = lin / (1 + lin), then gamma-encode to sRGB.
	// Apply a flat exposure bump so mid-grey land near 0.18 in the
	// output; HDRIs are usually authored for a 1.0 = "diffuse white"
	// scale.
	const exposure = 1.0
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			cr := reinhard(linR[i] * exposure)
			cg := reinhard(linG[i] * exposure)
			cb := reinhard(linB[i] * exposure)
			img.SetNRGBA(x, y, color.NRGBA{
				R: linearToSRGB(cr),
				G: linearToSRGB(cg),
				B: linearToSRGB(cb),
				A: 255,
			})
		}
	}
	return img, nil
}

// readScanline reads one scanline of `w` RGBE pixels into `dst`
// (which must be w*4 bytes). Handles both the new (RLE) and old
// (un-RLE'd or single-run) encodings; the encoding is per-scanline
// so a file can mix them.
func readScanline(br *bufio.Reader, dst []byte, w int) error {
	if w < 8 || w > 0x7fff {
		// New-format RLE only covers 8..32767 wide scanlines; fall
		// back to the byte-stream decoder for very narrow images.
		return readScanlineOld(br, dst, w)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return err
	}
	// New-format marker: bytes 0,1 = 2,2; bytes 2,3 = big-endian width.
	if hdr[0] != 2 || hdr[1] != 2 || hdr[2]&0x80 != 0 {
		// Not the new format: push it back into the stream by treating
		// the 4 bytes as the first pixel of an old-format scanline.
		copy(dst, hdr)
		return readScanlineOldTail(br, dst[4:], w-1)
	}
	if int(hdr[2])<<8|int(hdr[3]) != w {
		return fmt.Errorf("scanline width %d != expected %d", int(hdr[2])<<8|int(hdr[3]), w)
	}
	// Each of R, G, B, E channels is RLE-encoded separately across
	// the scanline. A run header byte > 128 means "repeat the next
	// byte (count - 128) times"; ≤ 128 means "copy the next count
	// raw bytes". Greg Ward's quirk: count of 0 is illegal.
	for c := 0; c < 4; c++ {
		x := 0
		for x < w {
			n, err := br.ReadByte()
			if err != nil {
				return err
			}
			if n > 128 {
				runLen := int(n) - 128
				if runLen == 0 || x+runLen > w {
					return errors.New("bad run length")
				}
				val, err := br.ReadByte()
				if err != nil {
					return err
				}
				for k := 0; k < runLen; k++ {
					dst[(x+k)*4+c] = val
				}
				x += runLen
			} else {
				runLen := int(n)
				if runLen == 0 || x+runLen > w {
					return errors.New("bad raw length")
				}
				for k := 0; k < runLen; k++ {
					b, err := br.ReadByte()
					if err != nil {
						return err
					}
					dst[(x+k)*4+c] = b
				}
				x += runLen
			}
		}
	}
	return nil
}

// readScanlineOld decodes an old-format scanline: pixels are stored
// linearly, with an optional RLE run signaled by an RGBE quad of
// (1,1,1,count) that repeats the previous pixel `count << shift`
// times. Modern HDR writers (Photoshop, Substance, HDRI-Haven)
// always emit new-format; old-format support is for completeness.
func readScanlineOld(br *bufio.Reader, dst []byte, w int) error {
	return readScanlineOldTail(br, dst, w)
}

func readScanlineOldTail(br *bufio.Reader, dst []byte, n int) error {
	shift := 0
	x := 0
	for x < n {
		q := make([]byte, 4)
		if _, err := io.ReadFull(br, q); err != nil {
			return err
		}
		if q[0] == 1 && q[1] == 1 && q[2] == 1 {
			runLen := int(q[3]) << shift
			if runLen == 0 || x+runLen > n {
				return errors.New("bad old-format run")
			}
			if x == 0 {
				return errors.New("run before first pixel")
			}
			prev := dst[(x-1)*4 : x*4]
			for k := 0; k < runLen; k++ {
				copy(dst[(x+k)*4:(x+k+1)*4], prev)
			}
			x += runLen
			shift += 8
		} else {
			copy(dst[x*4:(x+1)*4], q)
			x++
			shift = 0
		}
	}
	return nil
}

// rgbeToFloat converts a single RGBE quad to floating-point linear
// RGB. The exponent byte stores a base-2 power biased by +128; the
// mantissa bytes are 0..255 representing 0..(255/256) of that scale.
func rgbeToFloat(r, g, b, e byte) (float32, float32, float32) {
	if e == 0 {
		return 0, 0, 0
	}
	f := float32(math.Ldexp(1.0, int(e)-(128+8)))
	return float32(r) * f, float32(g) * f, float32(b) * f
}

// reinhard maps [0..∞) to [0..1) via x/(1+x). Cheap, monotonic,
// and visually pleasant for HDRI environment-probe thumbnails;
// not photoreal but appropriate for a 200×200 col tile.
func reinhard(x float32) float32 {
	if x < 0 {
		x = 0
	}
	return x / (1 + x)
}

// linearToSRGB applies the standard sRGB encoding curve.
func linearToSRGB(x float32) byte {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 255
	}
	var v float64
	if x < 0.0031308 {
		v = 12.92 * float64(x)
	} else {
		v = 1.055*math.Pow(float64(x), 1.0/2.4) - 0.055
	}
	return byte(v*255 + 0.5)
}

// decodeRadianceBytes is a convenience wrapper used by the worker
// when it has already buffered the whole file.
func decodeRadianceBytes(b []byte) (image.Image, error) {
	return decodeRadiance(bytes.NewReader(b))
}
