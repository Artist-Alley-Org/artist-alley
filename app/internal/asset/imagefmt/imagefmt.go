// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package imagefmt

import "strings"

// Package imagefmt answers "what kind of image is this file?" for the
// pipelines that need to decide whether they can process an asset.
//
// It is a LEAF — it imports nothing but strings — and that is the point.
// The two consumers that must agree on EXIF-extractability are
// asset/metadata (the backfill) and assets (the upload fanout), and
// those two already import each other's neighbourhood: metadata imports
// assets, so assets cannot import metadata. A shared definition
// therefore has to live below both, or it cannot be shared at all and
// the codebase keeps two copies that drift.
//
// It is deliberately NOT in preview/dispatch, which is the other leaf
// both can reach: that package answers "should we generate a preview
// for this?", and hanging an EXIF question off the preview dispatcher
// is how the next reader ends up believing the two are the same
// question. They are not — see below.

// What "this pipeline can extract from" means, expressed once (#579).
//
// THE BUG THIS REPLACES. The EXIF backfill gated on `assets.has_image`:
//
//	AND ($5::BOOLEAN = TRUE OR has_image = TRUE)
//
// That column is DEFAULT false NOT NULL with no writer anywhere in the
// tree — live 1007/1007 false — so the backfill selected zero assets on
// its normal path and the only way to make it select anything was
// scope.IncludeNonImage, a flag whose job is to WIDEN the population to
// PDFs, not to make the default population non-empty. Nothing failed;
// the run reported success and enqueued nothing.
//
// It was also the wrong QUESTION, independently of the column being
// dead. EXIF lives in the ORIGINAL bytes, so "can we extract from this?"
// is a property of what the asset IS — its format — and not of whether a
// derivative was ever generated. (Contrast #614, where IIIF genuinely
// does depend on stored variants, because IIIF serves those variants and
// nothing else. Same dead column, opposite correct answer.)
//
// WHY THIS LIST AND NOT ONE OF THE THREE THAT ALREADY EXIST. There were
// three overlapping notions of image-ness before this, and each is right
// for its own consumer:
//
//   - dispatch.ImageExts — broad "is this an image at all", used for
//     preview/raster fanout.
//   - visualembed.IsImageExtension — what the CLIP sidecar can embed;
//     its own doc says it deliberately stays independent of the EXIF
//     set, because Pillow may gain HEIC/AVIF before the EXIF extractor
//     does.
//   - assets.isExifExtractableImageExt — the EXIF set, but unexported in
//     a package the backfill cannot import.
//
// The third was already the right answer and simply was not reachable
// from here, so this promotes it rather than adding a fourth. It now
// lives beside the extractor interface it describes, and
// assets.isExifExtractableImageExt delegates to it — one definition, two
// callers, no drift.
//
// Kept in sync with exif.Extractor.Supports (MIME-keyed) by
// TestSupportedExtensionsMatchSupports in the exif package. That test
// lives there because exif imports metadata, so the dependency only
// points one way.

// exifExtractableExtensions is what the EXIF extractor itself claims via
// Supports(). Used by the upload fanout, which enqueues a
// metadata.extract job per uploaded file.
//
// HEIC/HEIF are images and are deliberately ABSENT — decoding them needs
// the libheif CGo add-on, and listing them here would enqueue extract
// jobs that fail on every HEIC in the library. AVIF and SVG are absent
// for the same reason: image, not extractable.
var exifExtractableExtensions = []string{"jpg", "jpeg", "png", "tif", "tiff", "webp"}

// rawExtractableExtensions is what the RAW extractor claims. Camera raw
// files are images, and the metadata.extract job dispatches across a
// REGISTRY of extractors (exif, iptc, xmp, raw, pdf) — so "can the
// pipeline extract from this?" is a question about the registry, not
// about any single extractor.
var rawExtractableExtensions = []string{"cr2", "nef", "dng", "arw", "rw2"}

// ExtractableImageExtensions returns every IMAGE extension the metadata
// pipeline has an extractor for — the EXIF formats plus camera raw.
//
// This is the backfill's default population. It is deliberately the
// union across extractors rather than the EXIF set alone: the extract
// job picks an extractor by MIME from a registry, so gating selection on
// one extractor's formats would silently exclude every camera raw in the
// library from a run that reports success.
//
// PDFs are NOT here. They are extractable (there is a pdf extractor) but
// they are not images, and scope.IncludeNonImage is the flag that widens
// the population to them — keeping that flag meaningful requires the
// default set to exclude them.
//
// Returns a copy: this goes straight into a SQL parameter, and a caller
// mutating the shared backing array would silently change which assets
// every later run selects.
func ExtractableImageExtensions() []string {
	out := make([]string, 0, len(exifExtractableExtensions)+len(rawExtractableExtensions))
	out = append(out, exifExtractableExtensions...)
	out = append(out, rawExtractableExtensions...)
	return out
}

// ExifExtractableExtensions returns only the extensions the EXIF
// extractor itself claims. Narrower than ExtractableImageExtensions —
// use that one for "can the pipeline process this?".
func ExifExtractableExtensions() []string {
	out := make([]string, len(exifExtractableExtensions))
	copy(out, exifExtractableExtensions)
	return out
}

// IsExtractableImageExtension reports whether ANY registered extractor
// can process this image format.
func IsExtractableImageExtension(ext string) bool {
	return IsExifExtractableExtension(ext) || isRawExtension(ext)
}

func isRawExtension(ext string) bool {
	trimmed := normaliseExt(ext)
	for _, e := range rawExtractableExtensions {
		if e == trimmed {
			return true
		}
	}
	return false
}

// IsExifExtractableExtension reports whether the EXIF extractor can
// process this file extension. Accepts "jpg", ".jpg", "JPG" alike —
// callers get extensions from user uploads and from the database, and
// the two do not agree on case or leading dot.
func IsExifExtractableExtension(ext string) bool {
	trimmed := normaliseExt(ext)
	for _, e := range exifExtractableExtensions {
		if e == trimmed {
			return true
		}
	}
	return false
}

// MimeTypeForExtension returns a best-effort image MIME type for a file
// extension, or "" when the extension is not a format we recognise.
//
// Narrow on purpose: this exists so callers that only have an extension
// can hand a real MIME to something that wants one. It is NOT a general
// content-type table — the storage layer records the uploaded
// Content-Type and that stays authoritative wherever it is available.
func normaliseExt(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

func MimeTypeForExtension(ext string) string {
	switch normaliseExt(ext) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "tif", "tiff":
		return "image/tiff"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	// Camera raw — RFC 6838 §4.2.5 mediatypes, matching raw.Supports.
	case "cr2":
		return "image/x-canon-cr2"
	case "nef":
		return "image/x-nikon-nef"
	case "dng":
		return "image/x-adobe-dng"
	case "arw":
		return "image/x-sony-arw"
	case "rw2":
		return "image/x-panasonic-rw2"
	}
	return ""
}
