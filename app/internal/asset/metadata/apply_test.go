// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Applier unit tests — stub-driven, no DB. Covers mode dispatch
// (skip_if_set / replace), equal-value no-op, validator rejection
// path, failure-row recording.

package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

type stubConfig struct {
	cfg []FieldExtractionConfig
	err error
}

func (s stubConfig) ListExtractionConfig(_ context.Context) ([]FieldExtractionConfig, error) {
	return s.cfg, s.err
}

type stubValues struct {
	byField map[uuid.UUID]FieldValueSnapshot
	err     error
}

func (s stubValues) GetAssetFieldValue(_ context.Context, _, fieldID uuid.UUID) (FieldValueSnapshot, bool, error) {
	if s.err != nil {
		return FieldValueSnapshot{}, false, s.err
	}
	v, ok := s.byField[fieldID]
	return v, ok, nil
}

type stubWriter struct {
	calls []WriteAssetFieldValueParams
	err   error
}

func (s *stubWriter) WriteAssetFieldValue(_ context.Context, p WriteAssetFieldValueParams) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, p)
	return nil
}

type stubFailures struct {
	calls []RecordExtractionFailureParams
	err   error
}

func (s *stubFailures) RecordExtractionFailure(_ context.Context, p RecordExtractionFailureParams) error {
	s.calls = append(s.calls, p)
	return s.err
}

// ---------------------------------------------------------------------------
// Happy paths
// ---------------------------------------------------------------------------

func TestApply_NewField_WritesAndSummarises(t *testing.T) {
	fid := uuid.New()
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeReplace},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, stubValues{}, writer, nil)

	res := Result{
		Format: "image/jpeg",
		Fields: map[CanonicalField]Value{
			FieldCameraMake: {Kind: ValueKindText, Text: "Canon"},
		},
	}
	asset := AssetRef{ID: uuid.New()}

	summary, err := a.Apply(context.Background(), asset, res)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(summary.FieldsSet) != 1 || summary.FieldsSet[0] != FieldCameraMake {
		t.Errorf("FieldsSet = %v, want [camera_make]", summary.FieldsSet)
	}
	if len(writer.calls) != 1 || writer.calls[0].FieldID != fid {
		t.Errorf("writer not called with the right field; calls=%+v", writer.calls)
	}
	if writer.calls[0].Value.Text != "Canon" {
		t.Errorf("written value mismatch: %v", writer.calls[0].Value)
	}
}

// ---------------------------------------------------------------------------
// Mode dispatch
// ---------------------------------------------------------------------------

func TestApply_SkipIfSet_PopulatedField_Skips(t *testing.T) {
	fid := uuid.New()
	existing := "Nikon"
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeSkipIfSet},
	}}
	values := stubValues{byField: map[uuid.UUID]FieldValueSnapshot{
		fid: {ValueText: &existing},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, values, writer, nil)

	res := Result{Fields: map[CanonicalField]Value{
		FieldCameraMake: {Kind: ValueKindText, Text: "Canon"},
	}}
	summary, _ := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 0 {
		t.Errorf("writer should NOT have been called (skip_if_set + populated)")
	}
	if len(summary.FieldsSkippedMode) != 1 {
		t.Errorf("FieldsSkippedMode = %v, want 1 entry", summary.FieldsSkippedMode)
	}
}

func TestApply_SkipIfSet_EmptyField_Writes(t *testing.T) {
	fid := uuid.New()
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeSkipIfSet},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, stubValues{}, writer, nil) // no existing values

	res := Result{Fields: map[CanonicalField]Value{
		FieldCameraMake: {Kind: ValueKindText, Text: "Canon"},
	}}
	_, _ = a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 1 {
		t.Errorf("writer SHOULD have been called (skip_if_set + empty field)")
	}
}

func TestApply_Replace_PopulatedField_Overwrites(t *testing.T) {
	fid := uuid.New()
	existing := "Nikon"
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeReplace},
	}}
	values := stubValues{byField: map[uuid.UUID]FieldValueSnapshot{
		fid: {ValueText: &existing},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, values, writer, nil)

	res := Result{Fields: map[CanonicalField]Value{
		FieldCameraMake: {Kind: ValueKindText, Text: "Canon"},
	}}
	_, _ = a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 1 || writer.calls[0].Value.Text != "Canon" {
		t.Errorf("replace mode should write Canon over Nikon; calls=%+v", writer.calls)
	}
}

// ---------------------------------------------------------------------------
// Equal-value no-op (brief decision 6 — convergence path)
// ---------------------------------------------------------------------------

func TestApply_EqualValue_NoWrite_NoAudit(t *testing.T) {
	fid := uuid.New()
	existing := "Canon"
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeReplace},
	}}
	values := stubValues{byField: map[uuid.UUID]FieldValueSnapshot{
		fid: {ValueText: &existing},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, values, writer, nil)

	res := Result{Fields: map[CanonicalField]Value{
		FieldCameraMake: {Kind: ValueKindText, Text: "Canon"}, // same as existing
	}}
	summary, _ := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 0 {
		t.Errorf("equal-value short-circuit failed — writer was called")
	}
	if len(summary.FieldsSkippedNoChange) != 1 {
		t.Errorf("FieldsSkippedNoChange = %v, want 1 entry", summary.FieldsSkippedNoChange)
	}
}

func TestApply_EqualValue_Time_TruncatesToSecond(t *testing.T) {
	fid := uuid.New()
	existing := time.Date(2024, 3, 15, 14, 30, 0, 500_000_000, time.UTC)
	extracted := time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC) // sub-second drift
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCaptureDateTime, Mode: ExtractionModeReplace},
	}}
	values := stubValues{byField: map[uuid.UUID]FieldValueSnapshot{
		fid: {ValueDate: &existing},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, values, writer, nil)

	res := Result{Fields: map[CanonicalField]Value{
		FieldCaptureDateTime: {Kind: ValueKindTime, Time: extracted},
	}}
	_, _ = a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 0 {
		t.Errorf("equal-value-to-second should short-circuit; calls=%+v", writer.calls)
	}
}

// ---------------------------------------------------------------------------
// Validator rejection path
// ---------------------------------------------------------------------------

func TestApply_ValidatorRejects_RecordsFailure_SkipsApply(t *testing.T) {
	fid := uuid.New()
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCaptureDateTime, Mode: ExtractionModeReplace},
	}}
	writer := &stubWriter{}
	failures := &stubFailures{}
	a := NewApplier(cfg, stubValues{}, writer, failures)

	res := Result{
		Format: "image/jpeg",
		Fields: map[CanonicalField]Value{
			// Year 3000 — validator rejects.
			FieldCaptureDateTime: {Kind: ValueKindTime, Time: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
	summary, _ := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)

	if len(writer.calls) != 0 {
		t.Errorf("rejected value should NOT be written")
	}
	if len(failures.calls) != 1 || failures.calls[0].ErrorKind != "validation" {
		t.Errorf("validation failure not recorded: %+v", failures.calls)
	}
	if len(summary.FieldsSkippedValid) != 1 {
		t.Errorf("FieldsSkippedValid = %v, want 1", summary.FieldsSkippedValid)
	}
	if len(summary.FailureRows) != 1 {
		t.Errorf("summary.FailureRows = %d, want 1", len(summary.FailureRows))
	}
}

// ---------------------------------------------------------------------------
// Robustness
// ---------------------------------------------------------------------------

func TestApply_ConfigReadError_Propagates(t *testing.T) {
	a := NewApplier(stubConfig{err: errors.New("db down")}, stubValues{}, nil, nil)
	_, err := a.Apply(context.Background(), AssetRef{}, Result{})
	if err == nil {
		t.Errorf("config read error should propagate")
	}
}

func TestApply_NilConfig_Errors(t *testing.T) {
	a := NewApplier(nil, stubValues{}, nil, nil)
	_, err := a.Apply(context.Background(), AssetRef{}, Result{})
	if err == nil {
		t.Errorf("nil ConfigReader should error")
	}
}

func TestApply_ExtractedFieldNotConfigured_Ignored(t *testing.T) {
	// Operator wired FieldCameraMake; extracted FieldCameraModel.
	// Result should fold cleanly — no writes, no failures.
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: uuid.New(), Source: FieldCameraMake, Mode: ExtractionModeReplace},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, stubValues{}, writer, nil)

	res := Result{Fields: map[CanonicalField]Value{
		FieldCameraModel: {Kind: ValueKindText, Text: "EOS R5"},
	}}
	summary, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 0 || len(summary.FieldsSet) != 0 {
		t.Errorf("unconfigured extracted field should be ignored")
	}
}

func TestApply_ConfiguredFieldNotExtracted_Ignored(t *testing.T) {
	// Operator wired FieldCameraMake; result has nothing.
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: uuid.New(), Source: FieldCameraMake, Mode: ExtractionModeReplace},
	}}
	writer := &stubWriter{}
	a := NewApplier(cfg, stubValues{}, writer, nil)

	summary, _ := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, Result{Fields: map[CanonicalField]Value{}})
	if len(writer.calls) != 0 || len(summary.FieldsSet) != 0 {
		t.Errorf("empty result should produce no writes")
	}
}
