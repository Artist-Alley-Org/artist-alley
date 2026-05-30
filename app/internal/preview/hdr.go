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

// decodeHDR turns a high-dynamic-range image into an 8-bit-per-channel
// LDR image the rest of the variant pipeline can consume.
//
// Format dispatch:
//
//   .hdr / .pic  → pure-Go RGBE decoder (decodeRadiance). Debian
//                  bookworm's ffmpeg ships only the EXR decoder, so
//                  shelling out for Radiance fails on stock hosts.
//                  The format is small + well-spec'd, owning it in Go
//                  removes both the missing-decoder problem and the
//                  apt-extras dependency.
//
//   .exr         → ffmpeg with zscale → tonemap=mobius → zscale →
//                  rgb24. OpenEXR is much heavier (multi-channel,
//                  multiple compression schemes, optional tiles) and
//                  ffmpeg's libopenexr binding is the cheapest path.
//                  If libzimg/zscale isn't present we fall back to a
//                  flat-format conversion that loses highlight detail
//                  but still produces *a* thumbnail.
func decodeHDR(r io.Reader, ext string) (image.Image, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("hdr: read: %w", err)
	}

	switch ext {
	case "hdr", "pic":
		return decodeRadianceBytes(body)
	case "exr":
		return decodeEXR(body)
	default:
		return nil, fmt.Errorf("hdr: unsupported extension %q", ext)
	}
}

// decodeEXR shells out to ffmpeg. Capped at 60 s wallclock — EXR
// files can be large (HDRI environment probes routinely hit 200 MB)
// but no single thumbnail decode should tie up a worker forever.
func decodeEXR(body []byte) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "image2pipe",
		"-c:v", "exr",
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
		if fallback, fErr := decodeEXRFallback(ctx, body); fErr == nil {
			return fallback, nil
		}
		tail := stderr.String()
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("ffmpeg exr: %w: %s", err, tail)
	}
	img, err := png.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("exr: decode ffmpeg output: %w", err)
	}
	return img, nil
}

func decodeEXRFallback(ctx context.Context, body []byte) (image.Image, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "image2pipe", "-c:v", "exr", "-i", "pipe:0",
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
