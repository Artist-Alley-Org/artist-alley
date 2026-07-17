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
	// Multi-value / ref fields aren't covered by Phase 1.18.A-2
	// extractors (no EXIF tags map to them); shape stays minimal.
}

// FieldValueWriter persists ONE extraction-derived value. Production
// implementation wraps the metadata package's UpsertAssetFieldValue
// + AppendAssetFieldValueHistory in one tx.
type FieldValueWriter interface {
	WriteAssetFieldValue(ctx context.Context, p WriteAssetFieldValueParams) error
}

// WriteAssetFieldValueParams is the typed input to
// [FieldValueWriter.WriteAssetFieldValue]. SetBy is wired to
// "exif" so the existing audit-history feed surfaces the
// extraction provenance.
type WriteAssetFieldValueParams struct {
	AssetID uuid.UUID
	FieldID uuid.UUID
	Value   Value
	SetBy   string // "exif" / "iptc" / "xmp" / "operator"
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

		// Validate.
		normalized, vErr := ValidateValue(extracted, 0)
		if vErr != nil {
			summary.FieldsSkippedValid = append(summary.FieldsSkippedValid, fc.Source)
			fr := FailureRecord{
				Format:    result.Format,
				ErrorKind: "validation",
				Message:   vErr.Error(),
				Field:     fc.Source,
				RawValue:  rawDisplayValue(extracted),
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
			continue
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

		// skip_if_set: don't overwrite an operator-set value.
		if fc.Mode == ExtractionModeSkipIfSet && present {
			summary.FieldsSkippedMode = append(summary.FieldsSkippedMode, fc.Source)
			continue
		}

		// Equal-value short-circuit (brief decision 6): avoid
		// re-writing the same value. Skips the audit row + the
		// federation outbox event. Critical for backfill
		// idempotency + peer-extraction convergence.
		if present && valuesEqual(current, normalized) {
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
				SetBy:   "exif", // refine to "iptc"/"xmp" in 1.18.A-3
			}); err != nil {
				return summary, fmt.Errorf("metadata.Apply: write %s: %w", fc.Source, err)
			}
		}
		summary.FieldsSet = append(summary.FieldsSet, fc.Source)
	}

	return summary, nil
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
