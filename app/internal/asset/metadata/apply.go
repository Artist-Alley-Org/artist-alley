// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ConfigReader returns the operator's extraction-enabled field
// definitions. Production reads from the
// ExtractionConfig cache (which loads from
// ListExtractionEnabledFieldDefinitions on miss); tests inject a
// fixed slice.
type ConfigReader interface {
	ListExtractionConfig(ctx context.Context) ([]FieldExtractionConfig, error)
}

// FieldValueReader fetches an asset's current value for ONE field.
// Returns (value, found, err) so the caller distinguishes "field
// is empty" from "field has a value".
type FieldValueReader interface {
	GetAssetFieldValue(ctx context.Context, assetID, fieldID uuid.UUID) (FieldValueSnapshot, bool, error)
}

// FieldValueSnapshot is the typed projection of one asset_field_value
// row. Exactly one column is populated based on the field's type;
// the applier uses it for the equal-value short-circuit + the
// mode dispatch.
type FieldValueSnapshot struct {
	ValueText *string
	ValueNum  *float64
	ValueDate *time.Time
	// SetBy is the row's provenance — the asset_field_value column of
	// the same name.
	//
	// It is here because skip_if_set cannot be decided on PRESENCE
	// alone once upload defaults exist (#793). ADR 0012 always
	// specified the skip in terms of provenance ("skip if set_by =
	// manual"); what shipped read only "is a value there", and nothing
	// noticed because until now every value on an asset had been put
	// there by a person or by an extractor. A default is neither. Left
	// as a presence check, a default written at creation would make
	// extraction skip the field forever — the default outranking the
	// extraction, which is the inverse of ADR 0081 §3, on the 13 of 15
	// live field definitions that use skip_if_set.
	SetBy string
	// Multi-value / ref fields aren't covered by Phase 1.18.A-2
	// extractors (no EXIF tags map to them); shape stays minimal.
}

// SetByDefault is the provenance an upload default writes. Mirrors
// metadata.SetByDefault, duplicated rather than imported for the same
// reason JobTypeExtract is duplicated in the assets package: this
// package sits below the API-facing metadata package and importing it
// would close a cycle.
const SetByDefault = "default"

// isPlaceholder reports whether a present value is one nothing has
// actually chosen — a default sitting there waiting to be improved on.
// Extraction may overwrite one of these even under skip_if_set; it may
// not overwrite anything else.
func (s FieldValueSnapshot) isPlaceholder() bool { return s.SetBy == SetByDefault }

// FieldValueWriter persists ONE extraction-derived value. Production
// implementation wraps the metadata package's UpsertAssetFieldValue
// + AppendAssetFieldValueHistory in one tx.
type FieldValueWriter interface {
	WriteAssetFieldValue(ctx context.Context, p WriteAssetFieldValueParams) error
}

// WriteAssetFieldValueParams is the typed input to
// [FieldValueWriter.WriteAssetFieldValue].
type WriteAssetFieldValueParams struct {
	AssetID uuid.UUID
	FieldID uuid.UUID
	Value   Value

	// SetBy is the NAME OF THE EXTRACTOR that produced this value —
	// "exif" / "iptc" / "xmp" / "pdf" / "raw", from
	// [Result.FieldSources], or [SetByExtraction] when the Result
	// carries no provenance.
	//
	// It lands on asset_field_value.set_by, feeds the audit-history
	// panel, and — since #793 — is read back by skip_if_set, which
	// makes it load-bearing rather than decorative: a value's
	// provenance decides whether a later extraction may replace it.
	SetBy string
}

// FailureWriter records one extraction_failure row for the admin
// review queue.
type FailureWriter interface {
	RecordExtractionFailure(ctx context.Context, p RecordExtractionFailureParams) error
}

// RecordExtractionFailureParams is the typed input. Caller usually
// constructs from a [FailureRecord] + the asset ID.
type RecordExtractionFailureParams struct {
	AssetID   uuid.UUID
	Format    string
	ErrorKind string
	Message   string
	FieldKey  CanonicalField
	RawValue  any
}

// DefaultApplier is the production [Applier]. Composes the three
// reader/writer interfaces above so the boot wire can inject
// real DB-backed impls while tests use in-memory stubs.
type DefaultApplier struct {
	config   ConfigReader
	values   FieldValueReader
	writer   FieldValueWriter
	failures FailureWriter
}

// NewApplier wires the applier dependency graph. Any nil dep
// disables the corresponding behaviour (writes become no-ops,
// failure records become no-ops); tests use nils freely.
func NewApplier(config ConfigReader, values FieldValueReader, writer FieldValueWriter, failures FailureWriter) *DefaultApplier {
	return &DefaultApplier{config: config, values: values, writer: writer, failures: failures}
}

// Apply implements [Applier]. Walks the extraction Result against
// the operator's configured fields, applying mode + equal-value +
// validator rules per the brief's decision 6 + 8.
//
// One Apply call → at most ONE audit row (decision 12 in the
// brief): the caller writes the audit AFTER Apply returns,
// summarising via [ApplySummary].
func (a *DefaultApplier) Apply(ctx context.Context, asset AssetRef, result Result) (ApplySummary, error) {
	summary := ApplySummary{}
	if a.config == nil {
		return summary, errors.New("metadata.Apply: nil ConfigReader")
	}

	cfg, err := a.config.ListExtractionConfig(ctx)
	if err != nil {
		return summary, fmt.Errorf("metadata.Apply: read config: %w", err)
	}

	for _, fc := range cfg {
		extracted, ok := result.Fields[fc.Source]
		if !ok {
			continue // operator wired this field but the source had no value for it
		}

		// Refuse a field type extraction cannot write. Checked BEFORE
		// validation because the value being well-formed is beside the
		// point when there is no column to put it in — see
		// writableFieldTypes for why this is enforced rather than
		// trusted.
		if fc.FieldType != "" && !writableFieldTypes[fc.FieldType] {
			a.reject(ctx, &summary, asset, result.Format, fc.Source, extracted,
				fmt.Sprintf("field type %q cannot be written by extraction: "+
					"the applier carries value_text / value_num / value_date only, "+
					"and a %s value needs a column none of those is",
					fc.FieldType, fc.FieldType))
			continue
		}

		// Validate.
		normalized, vErr := ValidateValue(extracted, 0)
		if vErr != nil {
			a.reject(ctx, &summary, asset, result.Format, fc.Source, extracted, vErr.Error())
			continue
		}

		// Resolve a controlled-vocabulary value from the LABEL a file
		// carries to the SLUG the column stores. On no match nothing
		// is written — a label we have no term for is not a value, and
		// storing it raw would produce a row that renders plausibly
		// and addresses nothing. See vocabulary.go.
		if vocabularyFieldTypes[fc.FieldType] {
			if normalized.Kind != ValueKindText {
				a.reject(ctx, &summary, asset, result.Format, fc.Source, extracted,
					fmt.Sprintf("field type %q takes a vocabulary slug, "+
						"but the extracted value is not text", fc.FieldType))
				continue
			}
			slug, ok := resolveVocabularySlug(fc.Options, normalized.Text)
			if !ok {
				a.reject(ctx, &summary, asset, result.Format, fc.Source, extracted,
					fmt.Sprintf("no term in this field's vocabulary matches %q "+
						"(matching is on label or slug, case-insensitive); "+
						"add the term or correct the file", normalized.Text))
				continue
			}
			normalized.Text = slug
		}

		// Read current value (for equal-value + skip_if_set checks).
		var current FieldValueSnapshot
		var present bool
		if a.values != nil {
			c, p, rErr := a.values.GetAssetFieldValue(ctx, asset.ID, fc.FieldID)
			if rErr != nil {
				return summary, fmt.Errorf("metadata.Apply: read current %s: %w", fc.Source, rErr)
			}
			current, present = c, p
		}

		// skip_if_set: don't overwrite a value someone CHOSE.
		//
		// The check is on provenance, not presence. An upload default
		// is present and chosen by nobody, so extraction is free to
		// improve on it — that is what makes ADR 0081 §3's
		// `extracted > team default > field default` true rather than
		// backwards. Everything else present stays: a human's edit, an
		// import, a computed dimension.
		if fc.Mode == ExtractionModeSkipIfSet && present && !current.isPlaceholder() {
			summary.FieldsSkippedMode = append(summary.FieldsSkippedMode, fc.Source)
			continue
		}

		// Equal-value short-circuit (brief decision 6): avoid
		// re-writing the same value. Skips the audit row + the
		// federation outbox event. Critical for backfill
		// idempotency + peer-extraction convergence.
		//
		// A placeholder is exempt even when the values match, because
		// the VALUE is not the only thing being corrected — the
		// provenance is. A row reading "a default put this here" when
		// the file itself says the same thing is a row that will keep
		// inviting the next extraction to re-examine it. Letting the
		// write through once relabels it, and every pass after that
		// takes this short-circuit normally, so idempotency is
		// preserved after a single converging write.
		if present && !current.isPlaceholder() && valuesEqual(current, normalized) {
			summary.FieldsSkippedNoChange = append(summary.FieldsSkippedNoChange, fc.Source)
			continue
		}

		// Apply / append / prepend handled the same way for v1
		// (the brief lists append/prepend as future modes; current
		// extractors only emit scalar EXIF values where the only
		// meaningful difference vs replace is for multi-value
		// fields — which 1.18.A-2 doesn't ship).
		if a.writer != nil {
			if err := a.writer.WriteAssetFieldValue(ctx, WriteAssetFieldValueParams{
				AssetID: asset.ID,
				FieldID: fc.FieldID,
				Value:   normalized,
				// The provenance of the value being written, from the
				// extractor that produced it (#799). This was the
				// constant "exif" — five extractors fanning into one
				// applier, every one of them recorded as the first.
				// An IPTC credit line and an XMP rights statement both
				// claimed to be EXIF, which is a lie the audit history
				// keeps and no reader can detect.
				SetBy: result.sourceFor(fc.Source),
			}); err != nil {
				return summary, fmt.Errorf("metadata.Apply: write %s: %w", fc.Source, err)
			}
		}
		summary.FieldsSet = append(summary.FieldsSet, fc.Source)
	}

	return summary, nil
}

// reject records ONE field as not-written: it goes in the summary's
// skipped-by-validation list and gets an extraction_failure row so the
// operator can see, at /admin/extraction-failures, both the value that
// was in the file and why it did not become a value on the asset.
//
// The three callers are the validator, the unwritable-type refusal and
// the unresolved-vocabulary-term drop. They share a shape — a field
// the operator deliberately wired, carrying a value the file
// deliberately supplied, that we are declining to store — and the
// operator needs the same information about all three. error_kind is
// "validation" for each because the extraction_failure CHECK
// constraint admits five kinds and that is the one that means "we read
// this and would not write it"; the message carries the distinction.
func (a *DefaultApplier) reject(
	ctx context.Context,
	summary *ApplySummary,
	asset AssetRef,
	format string,
	field CanonicalField,
	raw Value,
	message string,
) {
	summary.FieldsSkippedValid = append(summary.FieldsSkippedValid, field)
	fr := FailureRecord{
		Format:    format,
		ErrorKind: "validation",
		Message:   message,
		Field:     field,
		RawValue:  rawDisplayValue(raw),
	}
	summary.FailureRows = append(summary.FailureRows, fr)
	if a.failures != nil {
		_ = a.failures.RecordExtractionFailure(ctx, RecordExtractionFailureParams{
			AssetID:   asset.ID,
			Format:    fr.Format,
			ErrorKind: fr.ErrorKind,
			Message:   fr.Message,
			FieldKey:  fr.Field,
			RawValue:  fr.RawValue,
		})
	}
}

// valuesEqual reports whether the extracted Value matches the
// current FieldValueSnapshot for the equal-value short-circuit.
// Type-aware: text compares strings, num compares floats with
// strict equality (EXIF rationals round-trip identically; no
// fuzz needed), time compares Truncate-to-second to absorb sub-
// second precision drift in TIMESTAMPTZ storage.
func valuesEqual(current FieldValueSnapshot, extracted Value) bool {
	switch extracted.Kind {
	case ValueKindText:
		return current.ValueText != nil && *current.ValueText == extracted.Text
	case ValueKindNum:
		return current.ValueNum != nil && *current.ValueNum == extracted.Num
	case ValueKindTime:
		return current.ValueDate != nil && current.ValueDate.Truncate(time.Second).Equal(extracted.Time.Truncate(time.Second))
	case ValueKindGPS:
		// GPS stores in the field-value system as two numeric
		// fields (lat + lon) on separate field-definitions; the
		// single-value snapshot can't represent this on its own.
		// For 1.18.A-2: GPS treated as never-equal so it always
		// writes; the equal-value optimisation is a follow-up when
		// GPS gets a typed value-column.
		return false
	}
	return false
}

// rawDisplayValue returns a JSON-marshallable view of the
// extracted value for the extraction_failure.raw_value column.
// Operators see this in the admin review queue so they can decide
// whether to fix the source file or adjust the validator.
func rawDisplayValue(v Value) any {
	switch v.Kind {
	case ValueKindText:
		return v.Text
	case ValueKindNum:
		return v.Num
	case ValueKindTime:
		return v.Time.Format(time.RFC3339Nano)
	case ValueKindGPS:
		return map[string]float64{
			"latitude":  v.GPS.Latitude,
			"longitude": v.GPS.Longitude,
		}
	}
	return nil
}

// Compile-time assertion.
var _ Applier = (*DefaultApplier)(nil)
