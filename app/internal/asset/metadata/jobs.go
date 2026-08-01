// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeExtract is the canonical job type identifier for the
// metadata-extraction worker. Workers route to
// [ExtractJobHandler] on this string.
const JobTypeExtract jobs.JobType = "metadata.extract"

// SourceLoader fetches the bytes + mime-type for an asset.
// Wired in the boot stage against storage.Service + the asset
// queries; tests inject in-memory stubs.
type SourceLoader interface {
	LoadSource(ctx context.Context, asset AssetRef) (io.ReadCloser, string, error)
}

// AssetLookup resolves an asset id to an AssetRef. The job
// payload carries only the asset id; the handler reads
// owner/team/sensitivity at job-run time so a tier downgrade
// mid-queue does the right thing.
type AssetLookup interface {
	GetAssetRef(ctx context.Context, id uuid.UUID) (AssetRef, bool, error)
}

// ExtractJobPayload is the JSON shape the worker receives. Tiny
// on purpose — the handler re-resolves everything else.
type ExtractJobPayload struct {
	AssetID uuid.UUID `json:"asset_id"`
}

// ExtractJobHandler implements [jobs.Handler] for the
// metadata.extract job type. One per app process; pure handler
// with no per-job state.
//
// On Handle:
//
//  1. Load the asset's bytes + mime-type via SourceLoader.
//  2. Pick the right extractor via the registered list (today:
//     just exif.Extractor for the image MIME types).
//  3. Recover-wrap the extract call so a third-party library
//     panic becomes ErrLibraryPanic + a failure_row, not a
//     worker crash.
//  4. Pass the result to the Applier.
//  5. Record any extraction-level failure (unsupported_format /
//     malformed_file / library_panic) — validator-rejected
//     fields are already recorded by the Applier.
//
// Errors classify per [jobs.IsTerminal]:
//   - Unsupported source / asset gone / malformed-file →
//     TerminalError (no retry; failure_row written; asset stays
//     usable).
//   - Apply / Read transient errors → retry per job-framework
//     backoff.
type ExtractJobHandler struct {
	loader     SourceLoader
	lookup     AssetLookup
	applier    Applier
	failures   FailureWriter
	extractors []Extractor
	logger     *slog.Logger
	// counter is the per-process extraction event counter from
	// Phase 1.18.A-2 follow-up B (commit 2). Bumped per Handle()
	// outcome; surfaced via /admin/metadata-extraction/health.
	// Nil-safe — tests can construct without a counter.
	counter *Counter

	// attrs writes asset-row attributes like page_count that come
	// out of extraction but aren't field-values. Nil-safe — set
	// post-construction via WithAssetAttributes; tests can leave
	// it nil and Result.PageCount will be silently dropped.
	attrs AssetAttributeWriter

	// previews persists the embedded-preview storage variant when
	// an extractor returns Result.PreviewImageBytes (raw cameras).
	// Nil-safe — wire post-construction via WithPreviewVariants;
	// tests can leave it nil.
	previews PreviewVariantWriter
}

// NewExtractJobHandler wires the dependency graph. extractors are
// tried in registration order; the first one whose Supports() is
// true wins.
func NewExtractJobHandler(
	loader SourceLoader,
	lookup AssetLookup,
	applier Applier,
	failures FailureWriter,
	extractors []Extractor,
	logger *slog.Logger,
) *ExtractJobHandler {
	return &ExtractJobHandler{
		loader:     loader,
		lookup:     lookup,
		applier:    applier,
		failures:   failures,
		extractors: extractors,
		logger:     logger,
	}
}

// WithCounter attaches a [Counter] for observability. Boot wire
// calls this post-construction to keep NewExtractJobHandler's
// signature stable for callers that don't care about metrics
// (tests, single-instance dev runs without the admin UI mounted).
func (h *ExtractJobHandler) WithCounter(c *Counter) *ExtractJobHandler {
	h.counter = c
	return h
}

// WithAssetAttributes attaches an [AssetAttributeWriter] for
// post-Apply asset-row writes (page_count today). Same nil-safe
// post-construction pattern as WithCounter — older callers that
// don't pass one see a no-op for these side-effects.
func (h *ExtractJobHandler) WithAssetAttributes(w AssetAttributeWriter) *ExtractJobHandler {
	h.attrs = w
	return h
}

// WithPreviewVariants attaches a [PreviewVariantWriter] for
// post-Apply embedded-preview persistence (raw cameras today).
// Nil-safe — Phase 1.18.A-2 extractors don't populate
// Result.PreviewImageBytes so older boot wires keep working
// untouched.
func (h *ExtractJobHandler) WithPreviewVariants(w PreviewVariantWriter) *ExtractJobHandler {
	h.previews = w
	return h
}

// recordResult bumps the counter when one is attached. No-op
// when nil so the test surface stays cheap.
func (h *ExtractJobHandler) recordResult(format string, result ExtractionResult) {
	if h.counter != nil {
		h.counter.Record(format, result, time.Time{})
	}
}

// Type implements [jobs.Handler].
func (h *ExtractJobHandler) Type() jobs.JobType { return JobTypeExtract }

// ExtractJobResult is persisted as the job's result row + read
// by the admin UI's "last extracted on this asset" panel.
type ExtractJobResult struct {
	Format                  string           `json:"format"`
	FieldsSet               []CanonicalField `json:"fields_set,omitempty"`
	FieldsSkippedNoChange   []CanonicalField `json:"fields_skipped_no_change,omitempty"`
	FieldsSkippedMode       []CanonicalField `json:"fields_skipped_mode,omitempty"`
	FieldsSkippedValidation []CanonicalField `json:"fields_skipped_validation,omitempty"`
	FailureCount            int              `json:"failure_count,omitempty"`
}

// Handle implements [jobs.Handler]. See package doc + the type
// doc above for the error-classification contract.
func (h *ExtractJobHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p ExtractJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("metadata.extract: parse payload: %w", err)}
	}

	asset, found, err := h.lookup.GetAssetRef(ctx, p.AssetID)
	if err != nil {
		return nil, fmt.Errorf("metadata.extract: lookup asset: %w", err)
	}
	if !found {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("metadata.extract: asset %s gone before job ran", p.AssetID)}
	}

	rc, mimeType, err := h.loader.LoadSource(ctx, asset)
	if err != nil {
		return nil, fmt.Errorf("metadata.extract: load source: %w", err)
	}
	defer rc.Close()

	// Collect EVERY extractor that supports the type, not the first.
	// See merge.go's header for why the first-wins dispatch this
	// replaced meant no JPEG was ever read for IPTC or XMP.
	var supporting []Extractor
	for _, e := range h.extractors {
		if e.Supports(mimeType) {
			supporting = append(supporting, e)
		}
	}
	if len(supporting) == 0 {
		h.recordFailure(ctx, asset.ID, mimeType, "unsupported_format",
			fmt.Sprintf("no registered extractor for %q", mimeType), "")
		h.recordResult(mimeType, ResultUnsupportedFormat)
		return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
	}

	// Buffer the source once. Every extractor io.ReadAll's its reader
	// anyway, so this is the same peak memory as the single-extractor
	// path used and one read of storage instead of N.
	src, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("metadata.extract: read source: %w", err)
	}

	parts, hardFailures, extErr := h.runExtractors(ctx, asset, mimeType, src, supporting)
	if extErr != nil {
		// Unknown error class from some extractor — treat as transient
		// + let the job framework retry per its backoff policy. Don't
		// record a failure row yet (avoid spamming the queue for a
		// transient error that the next retry might resolve).
		return nil, fmt.Errorf("metadata.extract: %w", extErr)
	}

	// NO extractor came back with a Result. If one failed hard, that
	// classification is the job's outcome; otherwise every extractor
	// said ErrNoMetadata, which is the ordinary "valid file, nothing
	// in it" case — not a failure, and no row.
	//
	// The condition is "no extractor SUCCEEDED", not "the merged
	// Result is empty": an extractor that returns cleanly with no
	// fields is a successful read of a file that has none, and it went
	// through Apply before this fan-out existed. Skipping Apply for it
	// would quietly change the counter classification of every such
	// asset from success to no_metadata.
	if len(parts) == 0 {
		if len(hardFailures) > 0 {
			h.recordResult(mimeType, hardFailures[0])
		} else {
			h.recordResult(mimeType, ResultNoMetadata)
		}
		return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
	}

	result := MergeResults(parts)
	if result.Format == "" {
		result.Format = mimeType
	}

	// Apply the extracted values + record per-field failures.
	summary, err := h.applier.Apply(ctx, asset, result)
	if err != nil {
		return nil, fmt.Errorf("metadata.extract: apply: %w", err)
	}

	// Post-Apply: persist the non-field Result side-channels (page
	// count + embedded preview). Both are best-effort: a failure
	// here logs but does NOT fail the job — the field-value writes
	// already landed and re-extraction will converge.
	if result.PageCount > 0 && h.attrs != nil {
		if err := h.attrs.SetAssetPageCount(ctx, asset.ID, result.PageCount); err != nil && h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelWarn,
				"metadata.extract.page_count_write_error",
				slog.String("asset_id", asset.ID.String()),
				slog.Int("page_count", result.PageCount),
				slog.String("err", err.Error()),
			)
		}
	}
	if len(result.PreviewImageBytes) > 0 && h.previews != nil && asset.FileHash != "" {
		if err := h.previews.WriteEmbeddedPreview(ctx, asset.FileHash, result.PreviewImageBytes); err != nil && h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelWarn,
				"metadata.extract.embedded_preview_write_error",
				slog.String("asset_id", asset.ID.String()),
				slog.Int("bytes", len(result.PreviewImageBytes)),
				slog.String("err", err.Error()),
			)
		}
	}

	// Per-Apply observability: if every field that was extracted
	// got rejected by the validator we record validation_failed;
	// otherwise success (which includes the "everything was a
	// no-op via equal-value" path — the asset DID have the
	// values, just nothing changed this run).
	if len(summary.FieldsSet) == 0 && len(summary.FieldsSkippedValid) > 0 && len(summary.FieldsSkippedNoChange) == 0 && len(summary.FieldsSkippedMode) == 0 {
		h.recordResult(mimeType, ResultValidationFailed)
	} else {
		h.recordResult(mimeType, ResultSuccess)
	}

	out := ExtractJobResult{
		Format:                  mimeType,
		FieldsSet:               summary.FieldsSet,
		FieldsSkippedNoChange:   summary.FieldsSkippedNoChange,
		FieldsSkippedMode:       summary.FieldsSkippedMode,
		FieldsSkippedValidation: summary.FieldsSkippedValid,
		FailureCount:            len(summary.FailureRows),
	}
	return jsonMarshalIgnoreErr(out), nil
}

// runExtractors runs every supporting extractor over the same source
// bytes and returns their Results in registration order.
//
// Error handling is per extractor, which is the substantive change
// from the single-extractor dispatch. Three of the four sentinel
// classes are now ROUTINE rather than fatal: a JPEG with EXIF and no
// IPTC makes the IPTC extractor return ErrNoMetadata, and a JPEG whose
// XMP packet is truncated makes the XMP extractor return
// ErrMalformedFile — neither is a reason to discard what the other
// extractors read. So each is recorded against its own extractor and
// the walk continues; only the aggregate outcome (see the caller)
// decides the job's classification.
//
// An UNKNOWN error class still aborts. That class means "we don't know
// what went wrong", the job framework's answer to which is to retry
// the whole job, and retrying half of one is not a thing the framework
// can express.
//
// Returns the per-extractor Results, the hard-failure classifications
// in the order they occurred, and a non-nil error only for the abort
// case.
func (h *ExtractJobHandler) runExtractors(
	ctx context.Context,
	asset AssetRef,
	mimeType string,
	src []byte,
	supporting []Extractor,
) ([]SourcedResult, []ExtractionResult, error) {
	var (
		parts        []SourcedResult
		hardFailures []ExtractionResult
	)
	for _, e := range supporting {
		res, err := e.Extract(ctx, bytes.NewReader(src), mimeType)
		if err == nil {
			parts = append(parts, SourcedResult{Source: e.Name(), Result: res})
			continue
		}
		switch {
		case errors.Is(err, ErrNoMetadata):
			// This extractor's namespace is simply absent from the
			// file. The overwhelmingly common case once more than one
			// extractor runs — no failure row, no log.
		case errors.Is(err, ErrUnsupportedFormat):
			// Supports() said yes and Extract() said no. A real
			// disagreement inside one extractor, worth a row, but
			// scoped to that extractor.
			h.recordFailure(ctx, asset.ID, mimeType, "unsupported_format",
				fmt.Sprintf("%s: %s", e.Name(), err.Error()), "")
			hardFailures = append(hardFailures, ResultUnsupportedFormat)
		case errors.Is(err, ErrMalformedFile):
			h.recordFailure(ctx, asset.ID, mimeType, "malformed_file",
				fmt.Sprintf("%s: %s", e.Name(), err.Error()), "")
			hardFailures = append(hardFailures, ResultMalformedFile)
		case errors.Is(err, ErrLibraryPanic):
			h.recordFailure(ctx, asset.ID, mimeType, "library_panic",
				fmt.Sprintf("%s: %s", e.Name(), err.Error()), "")
			hardFailures = append(hardFailures, ResultLibraryError)
		default:
			return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return parts, hardFailures, nil
}

// recordFailure is a soft-error helper — if the failure-recording
// write itself fails we log + move on; the job's own outcome
// shouldn't change because the admin-queue write didn't land.
func (h *ExtractJobHandler) recordFailure(ctx context.Context, assetID uuid.UUID, format, kind, msg string, field CanonicalField) {
	if h.failures == nil {
		return
	}
	if err := h.failures.RecordExtractionFailure(ctx, RecordExtractionFailureParams{
		AssetID:   assetID,
		Format:    format,
		ErrorKind: kind,
		Message:   msg,
		FieldKey:  field,
	}); err != nil && h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelWarn,
			"metadata.extract.failure_record_error",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()),
		)
	}
}

func jsonMarshalIgnoreErr(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Compile-time assertion.
var _ jobs.Handler = (*ExtractJobHandler)(nil)
