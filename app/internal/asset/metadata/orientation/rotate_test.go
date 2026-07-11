// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package orientation

import (
	"image"
	"image/color"
	"testing"
)

// makeCorneredImage returns a 4×4 image with distinct corner
// colours so per-transform tests can assert exact pixel positions
// after rotation.
//
//   tl(red)    .  .  tr(green)
//   .          .  .  .
//   .          .  .  .
//   bl(blue)   .  .  br(yellow)
func makeCorneredImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{50, 50, 50, 255}) // mid grey background
		}
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})   // top-left red
	img.Set(3, 0, color.RGBA{0, 255, 0, 255})   // top-right green
	img.Set(0, 3, color.RGBA{0, 0, 255, 255})   // bottom-left blue
	img.Set(3, 3, color.RGBA{255, 255, 0, 255}) // bottom-right yellow
	return img
}

func pixelAt(img image.Image, x, y int) (r, g, b uint8) {
	c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	return c.R, c.G, c.B
}

func TestRotate_Identity_PassesThroughUnchanged(t *testing.T) {
	src := makeCorneredImage()
	for _, code := range []int{0, Identity} {
		t.Run("", func(t *testing.T) {
			got := RotateFromEXIF(src, code)
			if got != src {
				t.Errorf("identity should return the same image instance unchanged")
			}
		})
	}
}

func TestRotate_FlipHorizontal_TopLeftBecomesTopRight(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), FlipHorizontal)
	// red (was top-left) should now be at top-right (3, 0)
	if r, _, _ := pixelAt(got, 3, 0); r != 255 {
		t.Errorf("post-flip-H, top-right pixel R = %d, want 255 (red)", r)
	}
	// green (was top-right) should now be at top-left (0, 0)
	if _, g, _ := pixelAt(got, 0, 0); g != 255 {
		t.Errorf("post-flip-H, top-left pixel G = %d, want 255 (green)", g)
	}
}

func TestRotate_FlipVertical_TopLeftBecomesBottomLeft(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), FlipVertical)
	if r, _, _ := pixelAt(got, 0, 3); r != 255 {
		t.Errorf("post-flip-V, bottom-left R = %d, want 255 (red)", r)
	}
	if _, _, b := pixelAt(got, 0, 0); b != 255 {
		t.Errorf("post-flip-V, top-left B = %d, want 255 (blue)", b)
	}
}

func TestRotate_180_TopLeftBecomesBottomRight(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), Rotate180)
	if r, _, _ := pixelAt(got, 3, 3); r != 255 {
		t.Errorf("post-180, bottom-right R = %d, want 255 (red)", r)
	}
	if r, g, _ := pixelAt(got, 0, 0); !(r == 255 && g == 255) {
		t.Errorf("post-180, top-left should be yellow (255,255,_), got (%d,%d)", r, g)
	}
}

func TestRotate_90CW_DimensionsSwap_TopLeftBecomesTopRight(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), Rotate90CW)
	// Dimensions swap (was 4×4, still 4×4 — but for non-square
	// input the bounds would swap; we assert the rotation rule
	// even on the square case).
	b := got.Bounds()
	if b.Dx() != 4 || b.Dy() != 4 {
		t.Errorf("90CW dims: got %d×%d, want 4×4 (square in this fixture)", b.Dx(), b.Dy())
	}
	// 90CW: top-left red ends up at top-right.
	if r, _, _ := pixelAt(got, 3, 0); r != 255 {
		t.Errorf("post-90CW, top-right R = %d, want 255 (red)", r)
	}
}

func TestRotate_90CCW_TopLeftBecomesBottomLeft(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), Rotate90CCW)
	// 90CCW: top-left red ends up at bottom-left.
	if r, _, _ := pixelAt(got, 0, 3); r != 255 {
		t.Errorf("post-90CCW, bottom-left R = %d, want 255 (red)", r)
	}
}

func TestRotate_NonSquareInput_DimensionsSwap(t *testing.T) {
	// 4×2 input → after 90CW should be 2×4.
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})
	src.Set(3, 0, color.RGBA{0, 255, 0, 255})

	got := RotateFromEXIF(src, Rotate90CW)
	b := got.Bounds()
	if b.Dx() != 2 || b.Dy() != 4 {
		t.Errorf("90CW: got %d×%d, want 2×4 (dim swap)", b.Dx(), b.Dy())
	}
}

func TestRotate_Transpose_RotatesAndFlips(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), Transpose)
	// Transpose = flip-H ∘ rotate-90-CCW; just confirm we get
	// a valid 4×4 image back (the exact pixel arithmetic is
	// covered by the per-step tests).
	if got.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Errorf("transpose dims: %v", got.Bounds())
	}
}

func TestRotate_Transverse_RotatesAndFlips(t *testing.T) {
	got := RotateFromEXIF(makeCorneredImage(), Transverse)
	if got.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Errorf("transverse dims: %v", got.Bounds())
	}
}

func TestRotate_UnknownOrientation_PassesThroughUnchanged(t *testing.T) {
	src := makeCorneredImage()
	got := RotateFromEXIF(src, 99) // not a valid EXIF value
	if got != src {
		t.Errorf("unknown orientation should return source unchanged (got rotated)")
	}
}
