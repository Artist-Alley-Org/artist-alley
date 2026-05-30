package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os/exec"
	"time"
)

// decodeHDR runs ffmpeg as a one-shot tonemap-and-convert pass to
// turn a high-dynamic-range image (.hdr Radiance, .exr OpenEXR,
// .pic Radiance alt extension) into an 8-bit-per-channel PNG the
// rest of the variant pipeline can consume.
//
// We use ffmpeg's `tonemap` filter in `mobius` mode — a perceptual
// curve that compresses highlights smoothly without flattening
// midtones the way `clip` or `linear` do. The `format=yuv420p`
// step doesn't apply (we want RGB out), so the chain is:
//
//	zscale=transfer=linear  → linearise input gamma
//	tonemap=mobius          → roll off highlights
//	zscale=transfer=bt709   → re-encode to display gamma
//	format=rgb24            → 8-bit-per-channel for image.Decode
//
// The output is piped to stdout as PNG to avoid touching disk.
//
// Cap the wallclock at 60 s; HDR/EXR files are usually small
// (1-50 MB) but a pathological input shouldn't tie up a worker.
func decodeHDR(r io.Reader, ext string) (image.Image, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("hdr: read: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// We can't tell ffmpeg the input format from stdin reliably
	// (some demuxers refuse non-seekable input). The "-f image2pipe"
	// + "-c:v <decoder>" pair is the canonical way.
	codec := map[string]string{
		"hdr": "hdr",
		"exr": "exr",
		"pic": "hdr", // Radiance .pic uses the hdr decoder
	}[ext]
	if codec == "" {
		return nil, fmt.Errorf("hdr: unsupported extension %q", ext)
	}

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "image2pipe",
		"-c:v", codec,
		"-i", "pipe:0",
		"-vf", "zscale=t=linear,tonemap=mobius:param=0.5,zscale=t=bt709,format=rgb24",
		"-frames:v", "1",
		"-c:v", "png",
		"-f", "image2pipe",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Some ffmpeg builds lack libzimg / zscale. Retry with a
		// simpler pure-format conversion that drops the tonemap —
		// the highlights blow out but at least we get *a* thumbnail.
		if fallback, fErr := decodeHDRFallback(ctx, body, codec); fErr == nil {
			return fallback, nil
		}
		tail := stderr.String()
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("ffmpeg hdr: %w: %s", err, tail)
	}
	img, err := png.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("hdr: decode ffmpeg output: %w", err)
	}
	return img, nil
}

func decodeHDRFallback(ctx context.Context, body []byte, codec string) (image.Image, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "image2pipe", "-c:v", codec, "-i", "pipe:0",
		"-vf", "format=rgb24",
		"-frames:v", "1",
		"-c:v", "png",
		"-f", "image2pipe", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg fallback: %w: %s", err, stderr.String())
	}
	return png.Decode(&out)
}
