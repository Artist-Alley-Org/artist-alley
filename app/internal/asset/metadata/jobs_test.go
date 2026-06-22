package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

type stubLoader struct {
	bytes []byte
	mime  string
	err   error
}

func (s stubLoader) LoadSource(_ context.Context, _ AssetRef) (io.ReadCloser, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	return io.NopCloser(bytes.NewReader(s.bytes)), s.mime, nil
}

type stubLookup struct {
	asset AssetRef
	found bool
	err   error
}

func (s stubLookup) GetAssetRef(_ context.Context, _ uuid.UUID) (AssetRef, bool, error) {
	return s.asset, s.found, s.err
}

type stubExtractor struct {
	supports bool
	result   Result
	err      error
}

func (s stubExtractor) Name() string                       { return "stub" }
func (s stubExtractor) Supports(_ string) bool             { return s.supports }
func (s stubExtractor) Extract(_ context.Context, _ io.Reader, _ string) (Result, error) {
	return s.result, s.err
}

type stubApplier struct {
	summary ApplySummary
	err     error
}

func (s stubApplier) Apply(_ context.Context, _ AssetRef, _ Result) (ApplySummary, error) {
	return s.summary, s.err
}

func makeJob(p ExtractJobPayload) *jobs.Claim {
	b, _ := json.Marshal(p)
	return &jobs.Claim{Type: JobTypeExtract, Payload: b}
}

func TestExtractJob_HappyPath_AppliesAndReturnsResult(t *testing.T) {
	asset := AssetRef{ID: uuid.New(), MimeType: "image/jpeg"}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("png"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{summary: ApplySummary{FieldsSet: []CanonicalField{FieldCameraMake}}},
		nil,
		[]Extractor{stubExtractor{supports: true, result: Result{Format: "image/jpeg"}}},
		nil,
	)
	out, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var r ExtractJobResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(r.FieldsSet) != 1 || r.FieldsSet[0] != FieldCameraMake {
		t.Errorf("FieldsSet = %v, want [camera_make]", r.FieldsSet)
	}
}

func TestExtractJob_AssetGone_Terminal(t *testing.T) {
	h := NewExtractJobHandler(
		stubLoader{},
		stubLookup{found: false},
		stubApplier{},
		nil, nil, nil,
	)
	_, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: uuid.New()}))
	if !jobs.IsTerminal(err) {
		t.Errorf("missing asset should produce TerminalError; got %v", err)
	}
}

func TestExtractJob_UnsupportedFormat_RecordsFailureNotError(t *testing.T) {
	asset := AssetRef{ID: uuid.New()}
	failures := &stubFailures{}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("data"), mime: "video/mp4"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		failures,
		[]Extractor{stubExtractor{supports: false}}, // no extractor claims this MIME
		nil,
	)
	_, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID}))
	if err != nil {
		t.Errorf("unsupported format should succeed with failure_row, not job error: %v", err)
	}
	if len(failures.calls) != 1 || failures.calls[0].ErrorKind != "unsupported_format" {
		t.Errorf("expected one unsupported_format failure_row; got %+v", failures.calls)
	}
}

func TestExtractJob_MalformedFile_RecordsFailureNotError(t *testing.T) {
	asset := AssetRef{ID: uuid.New()}
	failures := &stubFailures{}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("not a jpeg"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		failures,
		[]Extractor{stubExtractor{supports: true, err: ErrMalformedFile}},
		nil,
	)
	_, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID}))
	if err != nil {
		t.Errorf("malformed should succeed with failure_row: %v", err)
	}
	if len(failures.calls) != 1 || failures.calls[0].ErrorKind != "malformed_file" {
		t.Errorf("expected one malformed_file failure_row; got %+v", failures.calls)
	}
}

func TestExtractJob_LibraryPanic_RecordsFailure(t *testing.T) {
	asset := AssetRef{ID: uuid.New()}
	failures := &stubFailures{}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("data"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		failures,
		[]Extractor{stubExtractor{supports: true, err: ErrLibraryPanic}},
		nil,
	)
	if _, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID})); err != nil {
		t.Errorf("library_panic should be recorded not propagated: %v", err)
	}
	if len(failures.calls) != 1 || failures.calls[0].ErrorKind != "library_panic" {
		t.Errorf("expected library_panic failure_row; got %+v", failures.calls)
	}
}

func TestExtractJob_NoMetadata_NoFailureRecorded(t *testing.T) {
	asset := AssetRef{ID: uuid.New()}
	failures := &stubFailures{}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("data"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		failures,
		[]Extractor{stubExtractor{supports: true, err: ErrNoMetadata}},
		nil,
	)
	if _, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID})); err != nil {
		t.Errorf("no-metadata is normal: %v", err)
	}
	if len(failures.calls) != 0 {
		t.Errorf("no_metadata should NOT record a failure_row")
	}
}

func TestExtractJob_UnknownError_TransientNotRecorded(t *testing.T) {
	asset := AssetRef{ID: uuid.New()}
	failures := &stubFailures{}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("data"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		failures,
		[]Extractor{stubExtractor{supports: true, err: errors.New("DB pool exhausted")}},
		nil,
	)
	_, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID}))
	if err == nil {
		t.Errorf("unknown error should propagate as transient (retry expected)")
	}
	if jobs.IsTerminal(err) {
		t.Errorf("unknown error should NOT be terminal; got terminal")
	}
	if len(failures.calls) != 0 {
		t.Errorf("transient error should NOT write a failure_row yet")
	}
}

func TestExtractJob_BadPayload_Terminal(t *testing.T) {
	h := NewExtractJobHandler(nil, nil, nil, nil, nil, nil)
	bad := &jobs.Claim{Type: JobTypeExtract, Payload: json.RawMessage("{not json}")}
	_, err := h.Handle(context.Background(), bad)
	if !jobs.IsTerminal(err) {
		t.Errorf("bad payload should be TerminalError; got %v", err)
	}
}
