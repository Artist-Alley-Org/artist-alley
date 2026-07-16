// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package dispatch is the single home for "which preview job handles
// this file extension?" (#355).
//
// It exists because the answer was previously duplicated: the assets
// package carried its own `*ExtsHandler` copies of the preview
// package's sets, with comments admitting they only existed to dodge
// the assets→preview import cycle (preview imports assets for the
// metadata queries, so assets can never import preview back). Two
// copies of eleven extension sets is a drift bomb — and when `aa seed`
// needed the same mapping it would have been a third.
//
// This package is a leaf: it imports `jobs` (for the JobType constants)
// and nothing else in the tree, so assets, preview, and seed can all
// depend on it without a cycle.
//
// The sets and JobTypeForExt's precedence order are lifted verbatim
// from the upload path (assets.jobTypeForExt) — that map is proven in
// production and this is a relocation, not a redesign.
package dispatch

import (
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/archive"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// Normalize lowercases an extension and strips a leading dot, so
// ".PNG", "PNG", and "png" all agree.
func Normalize(ext string) string {
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

// Canonical extension sets. Every consumer (upload dispatch, preview
// handler Accepts checks, the seeder) reads these — do not re-declare
// them anywhere else.
var (
	// ImageExts gets a raster preview. Includes SVG (rasterized) and
	// HDR/EXR + camera RAW.
	ImageExts = map[string]struct{}{
		"jpg": {}, "jpeg": {}, "png": {}, "gif": {}, "webp": {},
		"bmp": {}, "tiff": {}, "tif": {}, "avif": {}, "heic": {}, "heif": {},
		"svg": {},
		"hdr": {}, "exr": {}, "pic": {},
		"cr2": {}, "nef": {}, "dng": {}, "arw": {}, "rw2": {},
	}

	// VideoExts — anything we'd want a poster frame + hover sprite for.
	VideoExts = map[string]struct{}{
		"mp4": {}, "mov": {}, "mkv": {}, "webm": {}, "avi": {},
		"wmv": {}, "mpg": {}, "mpeg": {}, "3gp": {}, "flv": {},
		"m4v": {}, "ts": {}, "lrv": {}, "insv": {}, "mts": {},
		"m2ts": {}, "vob": {}, "f4v": {}, "mxf": {},
	}

	// ModelExts — formats the preview.3d handler can ingest.
	ModelExts = map[string]struct{}{
		"glb": {}, "gltf": {}, "fbx": {}, "obj": {}, "blend": {}, "mview": {},
		"dae": {}, "ply": {}, "stl": {}, "3ds": {}, "x3d": {}, "wrl": {},
		"usd": {}, "usda": {}, "usdc": {}, "usdz": {}, "abc": {},
		"md2": {}, "md3": {}, "mdl": {}, "ms3d": {},
	}

	// AudioExts — includes the audiobook containers, which route
	// through the same handler for cover extraction, duration probing,
	// and chapter atoms.
	AudioExts = map[string]struct{}{
		"mp3": {}, "wav": {}, "flac": {}, "ogg": {}, "oga": {},
		"m4a": {}, "aac": {}, "opus": {},
		"m4b": {}, "aax": {},
	}

	PDFExts = map[string]struct{}{"pdf": {}}

	FontExts = map[string]struct{}{
		"ttf": {}, "otf": {}, "ttc": {}, "otc": {}, "woff": {}, "woff2": {},
	}

	EbookExts = map[string]struct{}{"epub": {}}

	EPSExts = map[string]struct{}{"eps": {}, "ps": {}}

	PSDExts = map[string]struct{}{"psd": {}, "psb": {}}

	ComicExts = map[string]struct{}{"cbz": {}, "cbr": {}, "cb7": {}}

	TextExts = map[string]struct{}{"txt": {}}

	// ArchiveExts route through the archive preview so the manifest is
	// extracted + cached on metadata.archive. Derived from the archive
	// package's own list rather than hand-copied, so adding a container
	// format there routes it here automatically.
	ArchiveExts = func() map[string]struct{} {
		m := make(map[string]struct{}, len(archive.SupportedExtensions()))
		for _, e := range archive.SupportedExtensions() {
			m[Normalize(e)] = struct{}{}
		}
		return m
	}()
)

// Has reports whether ext (in any case, with or without a leading dot)
// is in set.
func Has(set map[string]struct{}, ext string) bool {
	_, ok := set[Normalize(ext)]
	return ok
}

// JobTypeForExt maps a file extension to the preview job that should
// render it. A nil or unrecognized extension falls back to the raster
// handler — the historical behaviour of the upload path.
//
// Precedence is significant and matches the original dispatcher: the
// first matching set wins.
func JobTypeForExt(ext *string) jobs.JobType {
	if ext == nil {
		return jobs.TypePreviewRaster
	}
	e := Normalize(*ext)
	switch {
	case Has(VideoExts, e):
		return jobs.TypePreviewVideo
	case Has(ModelExts, e):
		return jobs.TypePreview3D
	case Has(AudioExts, e):
		return jobs.TypePreviewAudio
	case Has(PDFExts, e):
		return jobs.TypePreviewPDF
	case Has(FontExts, e):
		return jobs.TypePreviewFont
	case Has(EbookExts, e):
		return jobs.TypePreviewEbook
	case Has(EPSExts, e):
		return jobs.TypePreviewEPS
	case Has(PSDExts, e):
		return jobs.TypePreviewPSD
	case Has(ComicExts, e):
		return jobs.TypePreviewComic
	case Has(TextExts, e):
		return jobs.TypePreviewText
	case Has(ArchiveExts, e):
		return jobs.TypePreviewArchive
	}
	return jobs.TypePreviewRaster
}
