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
	"sort"
	"strings"

	"github.com/google/uuid"

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

// CanPreview reports whether SOME handler will actually produce a
// preview for ext. It is the guard the enqueue path needs (#366).
//
// JobTypeForExt always returns a concrete type — preview.raster is the
// catch-all — so on its own it can't distinguish "raster handles this"
// from "nothing does": both come back as preview.raster. Enqueue a job
// for the second case and its ONLY possible outcome is a TerminalError
// (raster rejects any ext outside ImageExts), i.e. a guaranteed dead
// job. Enqueueing none is strictly better.
//
// Derived from JobTypeForExt rather than re-listing the sets, so it
// cannot drift from the router: every non-fallback route is a set-
// membership match whose handler accepts the ext, and the sole route
// that can still reject is the raster fallback, which accepts exactly
// ImageExts. A nil/empty ext lands on that fallback and is rejected, so
// it is not previewable either — which matches today's outcome (a job
// that terminal-fails), minus the dead job.
func CanPreview(ext *string) bool {
	if ext == nil {
		return false
	}
	if JobTypeForExt(ext) != jobs.TypePreviewRaster {
		return true
	}
	return Has(ImageExts, Normalize(*ext))
}

// PreviewableExts returns every extension some handler can render, as
// lowercase strings without a leading dot, sorted.
//
// Derived from CanPreview over the declared sets rather than a fresh
// list, so it cannot drift from what the router will actually accept.
// The caller is the bulk rebuild path, whose "no --ext given means all
// of them" needs a concrete allowlist to hand to SQL.
func PreviewableExts() []string {
	seen := map[string]struct{}{}
	for _, set := range []map[string]struct{}{
		ImageExts, VideoExts, ModelExts, AudioExts, PDFExts, FontExts,
		EbookExts, EPSExts, PSDExts, ComicExts, TextExts, ArchiveExts,
	} {
		for e := range set {
			ext := e
			if CanPreview(&ext) {
				seen[ext] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Payload is the JSON body EVERY preview.* job carries, on the wire and
// in every handler. It lives here — next to the router that decides
// which handler reads it — because producer and consumer used to
// disagree in a way the compiler could not see: the three enqueue sites
// (upload, "recreate previews", `aa seed`) each hand-rolled a
// `map[string]string`, while the eleven handlers each declared their
// own struct. Adding a field meant editing fourteen places and hoping.
//
// That is not hypothetical. `map[string]string{"force": "true"}` is a
// JSON string, and a `Force bool` would reject it as a bad payload at
// unmarshal time — a TerminalError for a control that is supposed to
// FIX a preview. One shared type makes that a compile error instead.
//
// Handlers alias this type (`type ModelPayload = dispatch.Payload`)
// rather than redeclaring it, so a new field is available to all of
// them at once and the wire format has exactly one definition.
type Payload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`

	// Force re-renders variants that already exist instead of leaving
	// them alone (#760).
	//
	// Every handler short-circuits on "the output is already in
	// storage", which is what makes the steady-state re-queue nearly
	// free — and what made "Recreate previews" a control that did
	// nothing. An operator whose renderer just got fixed clicked it,
	// got a 202 and a job that completed, and the thumbnail did not
	// change, because the skip check never asked whether the bytes
	// were STALE, only whether they were THERE.
	//
	// Force is the operator saying "the bytes are stale, ignore them".
	// It never deletes: each rung is overwritten in place by an
	// atomic backend Put, so a crash mid-render leaves the old
	// preview intact rather than an asset with none.
	//
	// omitempty keeps the ordinary payload byte-identical to what
	// shipped before, so nothing has to be re-enqueued to upgrade.
	Force bool `json:"force,omitempty"`
}

// NewPayload builds the job body for one asset. Prefer it over a
// struct literal at enqueue sites so a nil extension is normalised the
// same way everywhere.
func NewPayload(assetID uuid.UUID, hash string, ext *string, force bool) Payload {
	e := ""
	if ext != nil {
		e = *ext
	}
	return Payload{AssetID: assetID, FileHash: hash, FileExtension: e, Force: force}
}
