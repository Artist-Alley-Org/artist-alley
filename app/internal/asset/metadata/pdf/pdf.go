// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package pdf implements the metadata.Extractor for PDF documents.
//
// Pure-Go: uses github.com/pdfcpu/pdfcpu to walk the xref + read the
// /Info dictionary. No subprocess, no CGo. Replaces the pdfinfo
// poppler subprocess that the existing preview.pdf handler shells
// out to for the JSONB asset.metadata.pdf blob; that path stays for
// now (it's wired separately) but new asset metadata lives in the
// canonical field-value system via the extractor pipeline.
//
// Why route PDF metadata through the extractor at all?
//
//   - PDF /Info fields (Title, Author, Subject, Keywords, Creator,
//     Producer) are semantic metadata that operators want to mix
//     with the same field-definitions that EXIF/IPTC/XMP populate
//     for images. Routing them through the field-value system means
//     the same extraction_config rows govern PDF and image fields.
//   - The page count is asset-intrinsic (like pixel dimensions);
//     it stamps assets.page_count directly via the applier's
//     AssetAttributeWriter, not the field-value system. The PDF
//     viewer reads it for the "Page X of Y" indicator without a
//     JSONB shape lookup.
//
// What's NOT extracted: bookmarks / outlines, attachments, form
// fields, signatures. pdfcpu exposes them on PDFInfo but they don't
// map to per-asset canonical metadata. If a future phase needs them
// (e.g., "list attachments on the asset page") we add them to the
// per-extractor blob; they're not field-values.
package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	metadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// MaxSourceBytes guards against unbounded uploads. The PDF preview
// pipeline already caps at 256 MB; we match here so the two paths
// agree on what's "too big to bother extracting from."
const MaxSourceBytes = 256 * 1024 * 1024

// Extractor implements metadata.Extractor for application/pdf.
type Extractor struct {
	// conf carries pdfcpu's runtime options. We keep relaxed
	// validation on so a malformed-but-readable PDF doesn't fail
	// extraction outright — most "broken" PDFs in the wild are
	// fine for info-dict reads, just fail strict structure checks.
	conf *pdfcpumodel.Configuration
}

// New constructs a ready-to-use Extractor with pdfcpu's relaxed
// validation profile. Stateless + concurrency-safe.
//
// pdfcpu's default NewDefaultConfiguration() tries to write a
// user-config directory at ~/.config/pdfcpu on first call, which
// blows up the container boot when running as an unprivileged
// user (no $HOME write access). We set the sentinel "disable"
// BEFORE calling NewDefaultConfiguration so the lib skips the
// disk write entirely and the boot stays side-effect-free.
func New() *Extractor {
	pdfcpumodel.ConfigPath = "disable"
	conf := pdfcpumodel.NewDefaultConfiguration()
	conf.ValidationMode = pdfcpumodel.ValidationRelaxed
	// pdfcpu logs to stdout by default; silence it so a noisy
	// extraction doesn't pollute the worker log.
	conf.Cmd = pdfcpumodel.LISTINFO
	return &Extractor{conf: conf}
}

// Name implements metadata.Extractor.
func (Extractor) Name() string { return "pdf" }

// Supports implements metadata.Extractor.
func (Extractor) Supports(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "application/pdf", "application/x-pdf":
		return true
	}
	return false
}

// Extract implements metadata.Extractor. Reads the entire source
// into memory (pdfcpu needs an io.ReadSeeker; for our typical
// document-PDF sizes under the cap this is a few MB at most), then
// asks pdfcpu for the /Info dict + page count.
//
// Error mapping:
//
//   - context cancel → propagated
//   - source bigger than MaxSourceBytes → ErrMalformedFile (operator
//     sees it as a failure_row they can opt to backfill manually
//     by raising the cap)
//   - pdfcpu rejects the file as not-a-PDF / encrypted → ErrMalformedFile
//   - pdfcpu can't find /Info AND no page tree → ErrNoMetadata
//
// The handler wraps panics, but pdfcpu is pretty defensive — recover
// is a backstop, not the primary plan.
func (e *Extractor) Extract(ctx context.Context, r io.Reader, mimeType string) (out metadata.Result, retErr error) {
	if !e.Supports(mimeType) {
		return metadata.Result{}, metadata.ErrUnsupportedFormat
	}

	defer func() {
		if rec := recover(); rec != nil {
			retErr = fmt.Errorf("%w: %v", metadata.ErrLibraryPanic, rec)
			out = metadata.Result{Format: mimeType}
		}
	}()

	// Buffer the source. pdfcpu needs ReadSeeker; we cap to keep
	// memory predictable.
	raw, err := io.ReadAll(io.LimitReader(r, MaxSourceBytes+1))
	if err != nil {
		return metadata.Result{}, fmt.Errorf("pdf: read source: %w", err)
	}
	if int64(len(raw)) > MaxSourceBytes {
		return metadata.Result{Format: mimeType}, fmt.Errorf("pdf: source %d bytes > cap %d: %w",
			len(raw), MaxSourceBytes, metadata.ErrMalformedFile)
	}

	info, err := pdfcpuapi.PDFInfo(bytes.NewReader(raw), "", nil, false, e.conf)
	if err != nil {
		// pdfcpu's surface for "this isn't a PDF" / "header bad" is
		// a plain error string. We can't classify finely without
		// fragile string matching, so any failure here is
		// malformed_file from the operator's perspective.
		return metadata.Result{Format: mimeType}, fmt.Errorf("pdf: pdfcpu info: %w: %w", err, metadata.ErrMalformedFile)
	}
	if info == nil {
		return metadata.Result{Format: mimeType}, metadata.ErrNoMetadata
	}

	// Context cancellation: pdfcpu doesn't take a context; we
	// check between the (synchronous) info call and the field-map
	// build. Practically the info call is fast on any
	// under-the-cap document.
	if err := ctx.Err(); err != nil {
		return metadata.Result{Format: mimeType}, err
	}

	result := metadata.Result{
		Format:    mimeType,
		Fields:    map[metadata.CanonicalField]metadata.Value{},
		PageCount: info.PageCount,
	}

	// /Info dictionary entries → canonical fields. Skip empties so
	// the applier's "field had no value" branch fires the same way
	// it does for an image with no EXIF copyright tag.
	if v := strings.TrimSpace(info.Title); v != "" {
		result.Fields[metadata.FieldPDFTitle] = metadata.Value{Kind: metadata.ValueKindText, Text: v}
	}
	if v := strings.TrimSpace(info.Author); v != "" {
		result.Fields[metadata.FieldPDFAuthor] = metadata.Value{Kind: metadata.ValueKindText, Text: v}
	}
	if v := strings.TrimSpace(info.Subject); v != "" {
		result.Fields[metadata.FieldPDFSubject] = metadata.Value{Kind: metadata.ValueKindText, Text: v}
	}
	if v := strings.TrimSpace(info.Creator); v != "" {
		result.Fields[metadata.FieldPDFCreator] = metadata.Value{Kind: metadata.ValueKindText, Text: v}
	}
	if v := strings.TrimSpace(info.Producer); v != "" {
		result.Fields[metadata.FieldPDFProducer] = metadata.Value{Kind: metadata.ValueKindText, Text: v}
	}
	if len(info.Keywords) > 0 {
		// pdfcpu returns the parsed keyword list. Join with
		// ", " so it matches the IPTC keywords representation
		// (canonical text values are single strings in the
		// applier — multi-value fields are a follow-up).
		joined := strings.Join(filterEmpty(info.Keywords), ", ")
		if joined != "" {
			result.Fields[metadata.FieldPDFKeywords] = metadata.Value{Kind: metadata.ValueKindText, Text: joined}
		}
	}

	// "No metadata" only when /Info has zero usable strings AND
	// page count is zero (which itself shouldn't happen for a
	// loaded PDF, but defend against a zero-page no-info file).
	if len(result.Fields) == 0 && result.PageCount == 0 {
		return metadata.Result{Format: mimeType}, metadata.ErrNoMetadata
	}

	return result, nil
}

// filterEmpty drops blanks + trims surrounding whitespace from each
// element. pdfcpu hands back the raw keyword tokens; some PDFs have
// trailing spaces in their /Keywords entry and we don't want to
// emit ", , foo" after the join.
func filterEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Compile-time conformance check.
var _ metadata.Extractor = (*Extractor)(nil)

// ErrUnsupportedFormat is re-exported here for callers that want to
// branch on the sentinel without importing the parent package.
var ErrUnsupportedFormat = errors.New("pdf: unsupported format")
