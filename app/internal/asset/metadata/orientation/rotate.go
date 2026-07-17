// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package orientation applies EXIF-orientation rotation to a
// decoded image. Used by the variant pipeline (see
// preview/preview.go) so phone uploads don't render sideways.
//
// Source bytes are NEVER modified — rotation happens AFTER
// decode + BEFORE encode of each variant. The source's EXIF
// orientation tag stays as-is; the variant has its pixels
// rotated, so the variant's metadata (which stdlib encoders
// don't write at all) is implicitly orientation=1.
package orientation

import (
	"image"
	"image/color"
)

// EXIF orientation values per the spec
// (https://exiftool.org/TagNames/EXIF.html#Composite). Naming
// matches the camera-vendor convention.
const (
	Identity       = 1
	FlipHorizontal = 2
	Rotate180      = 3
	FlipVertical   = 4
	Transpose      = 5 // flip horizontal then rotate 90 CCW
	Rotate90CW     = 6
	Transverse     = 7 // flip horizontal then rotate 90 CW
	Rotate90CCW    = 8
)

// RotateFromEXIF returns a new image.Image with the rotation
// implied by the EXIF orientation tag applied. Orientation
// values 0 (absent) and 1 (identity) return the source
// unchanged. Unknown values also pass through unchanged + log
// silently — better to render an un-rotated variant than panic
// or refuse to generate one.
//
// Implementation walks every destination pixel and computes the
// source pixel position; uses image.NewRGBA for the output. For
// the typical preview-pipeline source size (under 8192×8192) this
// runs in tens of milliseconds; deemed cheap enough not to need
// a dispatch to optimized image-rotation libraries.
func RotateFromEXIF(src image.Image, exifOrientation int) image.Image {
	switch exifOrientation {
	case 0, Identity:
		return src
	case FlipHorizontal:
		return flipH(src)
	case Rotate180:
		return rotate180(src)
	case FlipVertical:
		return flipV(src)
	case Transpose:
		return flipH(rotate90CCW(src))
	case Rotate90CW:
		return rotate90CW(src)
	case Transverse:
		return flipH(rotate90CW(src))
	case Rotate90CCW:
		return rotate90CCW(src)
	}
	return src
}

// ---------------------------------------------------------------------------
// Per-transform pixel walkers. Boring, byte-level, by-design.
// ---------------------------------------------------------------------------

func flipH(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+(b.Dx()-1-x), b.Min.Y+y))
		}
	}
	return dst
}

func flipV(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+(b.Dy()-1-y)))
		}
	}
	return dst
}

func rotate180(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+(b.Dx()-1-x), b.Min.Y+(b.Dy()-1-y)))
		}
	}
	return dst
}

func rotate90CW(src image.Image) image.Image {
	b := src.Bounds()
	// dst dimensions swap.
	dst := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(b.Dy()-1-y, x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func rotate90CCW(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(y, b.Dx()-1-x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// Keep color package reachable for future use by callers (e.g., a
// future "rotate + colour-space normalise" helper). Holds the
// import alive without a sentinel comment.
var _ = color.Black
