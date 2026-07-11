// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package iptc

import (
	"context"
	"errors"
	"io"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// Extractor implements metadata.Extractor for the IPTC IIM
// metadata embedded in JPEG (APP13 / Photoshop 3.0 / 8BIM
// resource 0x0404). TIFF support shares the IPTC walker
// downstream; the carrier-extraction half is JPEG-only today
// because operators rarely upload IPTC-in-TIFF and that path
// needs a separate TIFF segment walker.
type Extractor struct{}

// New returns the singleton extractor. The registry walks
// extractors in registration order; place IPTC after EXIF so
// EXIF's standard fields stay authoritative when both carriers
// declare the same semantic (the per-field extraction-config
// picker is the operator's tie-breaker).
func New() *Extractor { return &Extractor{} }

// Name implements metadata.Extractor. Recorded in
// extraction_failure rows + on asset_field_value.set_by.
func (Extractor) Name() string { return "iptc" }

// Supports implements metadata.Extractor. JPEG only for the
// MVP; PNG/WebP rarely carry IPTC and we'd need separate
// carrier walkers for each.
func (Extractor) Supports(mime string) bool {
	switch mime {
	case "image/jpeg", "image/jpg":
		return true
	}
	return false
}

// Extract implements metadata.Extractor. Pulls the IPTC blob
// from the JPEG carrier + decodes the application-record
// datasets we care about. Returns metadata.ErrNoMetadata when
// the file has no IPTC payload — the standard "we ran, found
// nothing" outcome the job handler counts as success.
func (e Extractor) Extract(_ context.Context, r io.Reader, _ string) (metadata.Result, error) {
	bytes, err := io.ReadAll(r)
	if err != nil {
		return metadata.Result{}, metadata.ErrMalformedFile
	}
	// Panic guard — a future bug in the binary walker shouldn't
	// take down the worker. Same posture as the EXIF extractor.
	var blob []byte
	if err := safe(func() error {
		b, ferr := FindJPEGIPTCBlob(bytes)
		if ferr != nil {
			return ferr
		}
		blob = b
		return nil
	}); err != nil {
		if errors.Is(err, ErrNoIPTC) {
			return metadata.Result{Format: "image/jpeg"}, metadata.ErrNoMetadata
		}
		return metadata.Result{Format: "image/jpeg"}, metadata.ErrLibraryPanic
	}

	res, err := ParseIPTCBlob(blob)
	if err != nil {
		if errors.Is(err, ErrNoIPTC) {
			return metadata.Result{Format: "image/jpeg"}, metadata.ErrNoMetadata
		}
		return metadata.Result{Format: "image/jpeg"}, metadata.ErrMalformedFile
	}
	return metadata.Result{
		Format: "image/jpeg",
		Fields: res.Fields,
	}, nil
}

// safe runs fn, converting any panic into an error. Mirrors the
// pattern the EXIF extractor uses around dsoprea calls.
func safe(fn func() error) (out error) {
	defer func() {
		if r := recover(); r != nil {
			out = errors.New("iptc: panic recovered")
		}
	}()
	return fn()
}

// Compile-time interface check.
var _ metadata.Extractor = (*Extractor)(nil)
