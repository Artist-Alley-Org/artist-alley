// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HEIF-family decode for preview.raster — AVIF / HEIC / HEIF (#362).
//
// These routed to preview.raster but the handler's accept-set didn't
// list them, so every one became a TerminalError and the asset never
// got a preview. `.heic` is the default iPhone photo format, so "upload
// photos from your phone" produced failed jobs and no thumbnails.
//
// Why ImageMagick and not ffmpeg: the runtime already ships ffmpeg AND
// imagemagick (both explicit in infra/docker/app/Dockerfile), so this
// costs no new dependency either way — but only one of them can read
// the format. Measured on the runtime image:
//
//	ffmpeg 5.1.9 (bookworm): 0 heif demuxers. Feeding it a real .heic
//	  fails with "moov atom not found" — HEIF is ISOBMFF but structured
//	  with meta/iinf boxes, not moov, and the demuxer only landed in
//	  ffmpeg 7.1. It CAN read .avif (via libdav1d/libaom + the ISOBMFF
//	  path), so avif alone could have gone through ffmpeg — splitting
//	  the family across two tools for no reason.
//	imagemagick 6 + libheif 1.15.1: `convert -list format` reports
//	  HEIC rw+ and AVIF rw+. Verified by round-tripping a real .heic.
//
// So the whole family goes through one path. If ffmpeg is ever bumped
// past 7.1 this could fold into decodeHDR's shell-out, but there'd be
// no gain — libheif is the reference implementation.

package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"os/exec"
	"time"
)

// heifDecodeTimeout caps a single still-image decode. Generous next to
// the work (these are photos, not 200 MB HDRI probes) but bounded so a
// pathological file can't pin a worker.
const heifDecodeTimeout = 60 * time.Second

// decodeHEIF decodes an AVIF / HEIC / HEIF still via ImageMagick,
// which carries the libheif delegate. `ext` selects the input format
// explicitly rather than letting ImageMagick sniff — the stream comes
// in on stdin with no filename to infer from.
func decodeHEIF(r io.Reader, ext string) (image.Image, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("heif: read: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), heifDecodeTimeout)
	defer cancel()

	// `convert heic:- png:-` — explicit input format, PNG on stdout so
	// image.Decode can take it from here like every other path.
	cmd := exec.CommandContext(ctx, "convert", ext+":-", "png:-")
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("heif: convert %s: %w: %s", ext, err, tail)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("heif: convert %s produced no output", ext)
	}

	img, _, err := image.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("heif: decode converted png: %w", err)
	}
	return img, nil
}
