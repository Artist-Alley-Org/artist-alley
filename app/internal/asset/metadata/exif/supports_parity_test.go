// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #579 — the EXIF extractor states what it can process TWICE, in two
// different vocabularies:
//
//   - Extractor.Supports(mimeType) — consulted at extraction time, when
//     the bytes and their content type are in hand.
//   - imagefmt.ExifExtractableExtensions() — consulted at SELECTION
//     time, by the backfill and the upload fanout, which only have a
//     file_extension column.
//
// Both are necessary; neither can be derived from the other at the point
// of use. What must not happen is the two disagreeing, because the
// failure is silent in the expensive direction: an extension listed here
// but rejected by Supports() enqueues an extract job for every matching
// asset in the library, and each one fails at the extractor. The
// backfill reports "ran", the run row counts them, and no pixel
// dimensions appear.
//
// This test lives in the exif package rather than beside the list
// because exif imports imagefmt and not the reverse — the dependency
// only points one way, and the test follows it.

package exif_test

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/imagefmt"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/exif"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/raw"
)

func TestSupportedExtensionsMatchSupports(t *testing.T) {
	var ex exif.Extractor
	for _, e := range imagefmt.ExifExtractableExtensions() {
		mime := imagefmt.MimeTypeForExtension(e)
		if mime == "" {
			t.Errorf("extension %q is advertised as EXIF-extractable but has no "+
				"MIME mapping — selection would enqueue it and extraction could "+
				"not identify it", e)
			continue
		}
		if !ex.Supports(mime) {
			t.Errorf("extension %q maps to %q which Extractor.Supports rejects: "+
				"the backfill would enqueue every %s asset in the library and "+
				"every job would fail at the extractor", e, mime, e)
		}
	}
}

// TestExtractableImageSetMatchesTheRegistry covers the list the BACKFILL
// selects on, which is the union across extractors rather than any one
// of them. Every extension it advertises must be claimed by SOME
// registered extractor, or the backfill enqueues work that cannot
// succeed.
//
// This is the assertion that would have caught my first attempt at
// #579: gating the backfill on the EXIF set alone silently excluded
// every camera raw in the library from a run that reported success.
func TestExtractableImageSetMatchesTheRegistry(t *testing.T) {
	var (
		ex exif.Extractor
		rw raw.Extractor
	)
	for _, e := range imagefmt.ExtractableImageExtensions() {
		mime := imagefmt.MimeTypeForExtension(e)
		if mime == "" {
			t.Errorf("extension %q is selectable by the backfill but has no MIME "+
				"mapping", e)
			continue
		}
		if !ex.Supports(mime) && !rw.Supports(mime) {
			t.Errorf("extension %q (%s) is selectable by the backfill but no "+
				"registered extractor claims it — those jobs would fail", e, mime)
		}
	}
	// The union must actually be a superset, or it is just the EXIF set
	// wearing a wider name.
	if len(imagefmt.ExtractableImageExtensions()) <= len(imagefmt.ExifExtractableExtensions()) {
		t.Error("ExtractableImageExtensions is no wider than the EXIF set; camera " +
			"raw is missing and the backfill will skip every raw asset")
	}
	if !imagefmt.IsExtractableImageExtension("cr2") {
		t.Error("cr2 is not selectable — the raw extractor exists and claims it")
	}
}

// TestUnsupportedFormatsAreNotAdvertised is the other direction, and it
// guards a specific temptation: HEIC and AVIF are images, they look like
// they belong in an "image extensions" list, and adding them is a
// one-word edit. Decoding them needs the libheif CGo add-on that is not
// built yet, so listing them would select assets the extractor must
// refuse.
func TestUnsupportedFormatsAreNotAdvertised(t *testing.T) {
	for _, e := range []string{"heic", "heif", "avif", "svg", "gif", "bmp", "pdf"} {
		if imagefmt.IsExtractableImageExtension(e) {
			t.Errorf("%q is advertised as EXIF-extractable; Extractor.Supports "+
				"does not accept it, so selecting on it enqueues jobs that fail", e)
		}
	}
	// Guard the guard: the list is not simply empty.
	if !imagefmt.IsExifExtractableExtension("jpg") {
		t.Fatal("jpg is not EXIF-extractable — the list is broken, and the " +
			"negative assertions above prove nothing")
	}
}

// TestExtensionMatchingIsForgivingAboutShape — extensions reach these
// helpers from two places that format them differently: the upload path
// carries a user-supplied ".JPG", the database column carries "jpg".
func TestExtensionMatchingIsForgivingAboutShape(t *testing.T) {
	for _, form := range []string{"jpg", ".jpg", "JPG", ".JPG", " jpg ", "JpG"} {
		if !imagefmt.IsExifExtractableExtension(form) {
			t.Errorf("%q not recognised as jpg", form)
		}
	}
	if imagefmt.IsExifExtractableExtension("") {
		t.Error("empty extension must not be extractable")
	}
}
