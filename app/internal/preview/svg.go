// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// svgRenderSize is the square edge used for SVG rasterization. 2048
// matches what most modern browsers ship for retina screenshots — big
// enough that the hires variant (2000²) downscales without artefacts,
// small enough to keep peak memory under ~16 MB per render.
const svgRenderSize = 2048

// decodeSVG rasterises an SVG stream to an image.RGBA at
// svgRenderSize². Aspect ratio is preserved by fitting the SVG's
// declared viewBox / width inside the square (transparent margin
// around the smaller axis), so a portrait SVG doesn't get stretched.
//
// Two-tier decoder:
//   1. oksvg + rasterx (pure Go) — covers SVG 1.1 paths, basic
//      shapes, gradients, transforms, and most styling. Fast, no
//      subprocess, fits the Go-native preference.
//   2. rsvg-convert (librsvg subprocess) — fallback when oksvg can't
//      parse the input OR produces an essentially empty image. Handles
//      filters, complex text, full CSS, embedded raster images, and
//      everything else oksvg gives up on. Only invoked when needed
//      so the fast path stays fast.
//
// If both fail, returns the original oksvg error so the upload is
// rejected with a meaningful message.
func decodeSVG(r io.Reader) (image.Image, error) {
	// Buffer once so we can hand the same bytes to both decoders
	// without re-downloading. SVG bodies are bounded by the caller's
	// MaxSourceBytes cap so peak memory stays predictable.
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read svg: %w", err)
	}

	if img, err := decodeSVGOksvg(body); err == nil && !mostlyTransparent(img) {
		return img, nil
	} else if rsvgImg, rsvgErr := decodeSVGRsvg(body); rsvgErr == nil {
		return rsvgImg, nil
	} else if err != nil {
		return nil, fmt.Errorf("parse svg: %w (rsvg fallback: %v)", err, rsvgErr)
	} else {
		return nil, fmt.Errorf("svg renders blank with both backends (rsvg: %v)", rsvgErr)
	}
}

func decodeSVGOksvg(body []byte) (image.Image, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(body), oksvg.WarnErrorMode)
	if err != nil {
		return nil, err
	}

	w, h := svgRenderSize, svgRenderSize
	srcW, srcH := icon.ViewBox.W, icon.ViewBox.H
	if srcW <= 0 || srcH <= 0 {
		srcW, srcH = float64(svgRenderSize), float64(svgRenderSize)
	}

	scale := float64(w) / srcW
	if s := float64(h) / srcH; s < scale {
		scale = s
	}
	icon.SetTarget(0, 0, srcW*scale, srcH*scale)

	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := rasterx.NewScannerGV(w, h, rgba, rgba.Bounds())
	dasher := rasterx.NewDasher(w, h, scanner)
	icon.Draw(dasher, 1.0)

	return rgba, nil
}

// decodeSVGRsvg shells out to librsvg's rsvg-convert. Always
// available in the container (librsvg2-bin in the runtime image).
// 15s wallclock cap so a pathological SVG can't tie up a worker.
func decodeSVGRsvg(body []byte) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rsvg-convert",
		"--format", "png",
		"--width", strconv.Itoa(svgRenderSize),
		"--keep-aspect-ratio",
	)
	cmd.Stdin = bytes.NewReader(body)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rsvg-convert: %w: %s", err, errBuf.String())
	}
	img, err := png.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("rsvg output: %w", err)
	}
	return img, nil
}

// mostlyTransparent reports whether the rendered image looks like a
// failed decode — virtually every pixel is fully transparent. oksvg
// silently produces these for SVGs it can't actually parse (filters,
// foreign content, etc), so we use this as the trigger for the rsvg
// fallback.
func mostlyTransparent(img image.Image) bool {
	rgba, ok := img.(*image.RGBA)
	if !ok {
		return false
	}
	if len(rgba.Pix) < 4 {
		return true
	}
	// Sample every 64th pixel; if fewer than 0.5% of samples have any
	// alpha, treat as blank.
	hits := 0
	samples := 0
	for i := 3; i < len(rgba.Pix); i += 64 * 4 {
		samples++
		if rgba.Pix[i] > 0 {
			hits++
		}
	}
	if samples == 0 {
		return true
	}
	return hits*200 < samples // <0.5%
}
