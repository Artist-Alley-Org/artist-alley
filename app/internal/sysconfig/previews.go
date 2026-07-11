// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
	"strings"
)

// KeyPreviews — system_config key for the preview-pipeline settings
// (variant sizes + qualities). Admins tune these through the UI
// without restarts; the worker re-reads on every job.
const KeyPreviews = "previews"

// PreviewFit is how a source image is mapped into the target box.
type PreviewFit string

const (
	// PreviewFitCover crops to fill the target box exactly (square
	// thumbnails — for the grid).
	PreviewFitCover PreviewFit = "cover"
	// PreviewFitContain scales to fit while preserving aspect ratio
	// (longest-side cap — for previews + the modal main image).
	PreviewFitContain PreviewFit = "contain"
)

// PreviewFormat is the encoder for the variant. WebP is default
// everywhere; JPEG is a fallback for older browsers in a future
// phase if anyone asks. Add formats here as the encoder list grows.
type PreviewFormat string

const (
	PreviewFormatWebP PreviewFormat = "webp"
	PreviewFormatJPEG PreviewFormat = "jpeg"
	PreviewFormatPNG  PreviewFormat = "png"
)

// PreviewVariant is one configured size. The worker generates every
// listed variant for each processable asset, in the order given.
//
// `Key` is the URL-visible string under /assets/{id}/variants/{key}.
// Never rename a key in place — add a new variant + retire the old
// one — because clients cache URLs aggressively.
type PreviewVariant struct {
	Key     string        `json:"key"`     // "col", "preview", "screen", "hires"
	Fit     PreviewFit    `json:"fit"`
	MaxDim  int           `json:"max_dim"` // longest-side cap (or square edge for cover)
	Format  PreviewFormat `json:"format"`
	Quality int           `json:"quality"` // 1–100 (only meaningful for lossy formats)
	// SkipUpscale=true: if the original is already smaller than
	// MaxDim, store the (re-encoded) original at native size instead
	// of upscaling. Always recommended.
	SkipUpscale bool `json:"skip_upscale,omitempty"`
}

// PreviewConfig is the full preview-pipeline settings payload.
type PreviewConfig struct {
	Variants []PreviewVariant `json:"variants"`
}

// DefaultPreviewConfig is the variant set we ship with. WebP for the
// whole ladder (1.18.E): pure-Go encoder via HugoSmits86/nativewebp
// — no cgo, no libwebp-dev. Compared to the JPEG defaults we shipped
// in 1.18.A:
//
//   - ~25-35% smaller bytes at perceptually equivalent quality
//   - lossless mode handles transparent sources (SVG, alpha PNG,
//     waveform thumbnails) so we don't need to switch to PNG just
//     to preserve alpha
//   - browser support is universal (Safari ≥ 14, every other
//     evergreen since 2010s)
//
// Lossy / lossless choice happens at encode time in
// preview.encodeImage() — when the source carries actual
// transparency the encoder switches to lossless WebP to avoid the
// alpha-channel quantisation lossy mode shows on hard edges.
//
// Tuned so the grid feels instant (small col), the post modal looks
// gorgeous (nearly lossless hires), and the in-between sizes serve
// responsive breakpoints without bloating storage.
func DefaultPreviewConfig() PreviewConfig {
	return PreviewConfig{
		Variants: []PreviewVariant{
			// 320² square — collection-card / grid cover.
			{Key: "col", Fit: PreviewFitCover, MaxDim: 320, Format: PreviewFormatWebP, Quality: 82, SkipUpscale: true},
			// 1024 longest — mobile + intermediate breakpoints.
			{Key: "preview", Fit: PreviewFitContain, MaxDim: 1024, Format: PreviewFormatWebP, Quality: 86, SkipUpscale: true},
			// 1920 longest — desktop default.
			{Key: "screen", Fit: PreviewFitContain, MaxDim: 1920, Format: PreviewFormatWebP, Quality: 90, SkipUpscale: true},
			// 4096 longest @ q95 — "nearly lossless". The post modal's
			// main image; also what zoom-in views serve.
			{Key: "hires", Fit: PreviewFitContain, MaxDim: 4096, Format: PreviewFormatWebP, Quality: 95, SkipUpscale: true},
		},
	}
}

// GetPreviews returns the configured PreviewConfig, falling back to
// DefaultPreviewConfig when the row is unset (fresh install).
func (s *Store) GetPreviews(ctx context.Context) (PreviewConfig, error) {
	var out PreviewConfig
	err := s.getKey(ctx, KeyPreviews, &out)
	if err != nil {
		return PreviewConfig{}, err
	}
	if len(out.Variants) == 0 {
		return DefaultPreviewConfig(), nil
	}
	return out, nil
}

// SetPreviews validates and writes the preview config.
func (s *Store) SetPreviews(ctx context.Context, v PreviewConfig) error {
	if len(v.Variants) == 0 {
		return fmt.Errorf("sysconfig: previews must list at least one variant")
	}
	seen := map[string]int{}
	for i, p := range v.Variants {
		if p.Key == "" {
			return fmt.Errorf("sysconfig: variants[%d]: key is required", i)
		}
		if strings.ContainsAny(p.Key, " /\\") {
			return fmt.Errorf("sysconfig: variants[%d]: key %q must not contain spaces or path separators", i, p.Key)
		}
		if prev, dup := seen[p.Key]; dup {
			return fmt.Errorf("sysconfig: variants[%d] and [%d] share key %q", prev, i, p.Key)
		}
		seen[p.Key] = i
		switch p.Fit {
		case PreviewFitCover, PreviewFitContain:
		default:
			return fmt.Errorf("sysconfig: variants[%d]: fit %q not one of cover|contain", i, p.Fit)
		}
		switch p.Format {
		case PreviewFormatWebP, PreviewFormatJPEG, PreviewFormatPNG:
		default:
			return fmt.Errorf("sysconfig: variants[%d]: format %q not one of webp|jpeg|png", i, p.Format)
		}
		if p.MaxDim < 16 || p.MaxDim > 16384 {
			return fmt.Errorf("sysconfig: variants[%d]: max_dim %d out of range (16..16384)", i, p.MaxDim)
		}
		if p.Quality < 1 || p.Quality > 100 {
			return fmt.Errorf("sysconfig: variants[%d]: quality %d out of range (1..100)", i, p.Quality)
		}
	}
	return s.setKey(ctx, KeyPreviews, v)
}
