package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

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
	loader      SourceLoader
	lookup      AssetLookup
	applier     Applier
	failures    FailureWriter
	extractors  []Extractor
	logger      *slog.Logger
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

	// Pick the right extractor. Failure here = unsupported format
	// — record + return TERMINAL so the worker doesn't retry.
	var ext Extractor
	for _, e := range h.extractors {
		if e.Supports(mimeType) {
			ext = e
			break
		}
	}
	if ext == nil {
		h.recordFailure(ctx, asset.ID, mimeType, "unsupported_format",
			fmt.Sprintf("no registered extractor for %q", mimeType), "")
		return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
	}

	result, extErr := ext.Extract(ctx, rc, mimeType)
	if extErr != nil {
		switch {
		case errors.Is(extErr, ErrNoMetadata):
			// Normal outcome; no failure row.
			return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
		case errors.Is(extErr, ErrUnsupportedFormat):
			h.recordFailure(ctx, asset.ID, mimeType, "unsupported_format", extErr.Error(), "")
			return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
		case errors.Is(extErr, ErrMalformedFile):
			h.recordFailure(ctx, asset.ID, mimeType, "malformed_file", extErr.Error(), "")
			return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
		case errors.Is(extErr, ErrLibraryPanic):
			h.recordFailure(ctx, asset.ID, mimeType, "library_panic", extErr.Error(), "")
			return jsonMarshalIgnoreErr(ExtractJobResult{Format: mimeType}), nil
		default:
			// Unknown error class — treat as transient + let the
			// job framework retry per its backoff policy. Don't
			// record a failure row yet (avoid spamming the queue
			// for a transient error that the next retry might
			// resolve).
			return nil, fmt.Errorf("metadata.extract: %w", extErr)
		}
	}

	// Apply the extracted values + record per-field failures.
	summary, err := h.applier.Apply(ctx, asset, result)
	if err != nil {
		return nil, fmt.Errorf("metadata.extract: apply: %w", err)
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
