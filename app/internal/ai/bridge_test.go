package ai

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestTagSource_KnownValuesValid(t *testing.T) {
	for _, s := range []TagSource{TagSourceManual, TagSourceAI, TagSourceImport} {
		if !s.Valid() {
			t.Errorf("TagSource(%q).Valid() = false, want true", s)
		}
	}
}

func TestTagSource_UnknownInvalid(t *testing.T) {
	for _, s := range []TagSource{"", "auto", "MANUAL", "ai-generated"} {
		if s.Valid() {
			t.Errorf("TagSource(%q).Valid() = true, want false", s)
		}
	}
}

func TestSentinelErrors_DistinctIdentities(t *testing.T) {
	// errors.Is must match the exact sentinel for each kind so job
	// handlers + dashboards can classify reliably. A future commit
	// that accidentally re-defines one of these would flip the
	// match — lock down here.
	if !errors.Is(ErrAssetNotFound, ErrAssetNotFound) {
		t.Error("ErrAssetNotFound !is itself")
	}
	if errors.Is(ErrAssetNotFound, ErrAssetKindNotSupported) {
		t.Error("ErrAssetNotFound matches ErrAssetKindNotSupported (sentinel collision)")
	}
	if errors.Is(ErrAssetNotFound, ErrNotImplementedYet) {
		t.Error("ErrAssetNotFound matches ErrNotImplementedYet (sentinel collision)")
	}
}

func TestStubCaptionWriter_ReturnsErrNotImplementedYet(t *testing.T) {
	w := NewStubCaptionWriter()
	err := w.SetAICaptionForAsset(context.Background(), uuid.New(), "ignored", AIProvenance{})
	if !errors.Is(err, ErrNotImplementedYet) {
		t.Errorf("stub returned %v, want ErrNotImplementedYet", err)
	}
}

func TestStubEmbeddingWriter_ReturnsErrNotImplementedYet(t *testing.T) {
	w := NewStubEmbeddingWriter()
	err := w.UpsertAssetEmbedding(context.Background(), EmbeddingInput{AssetID: uuid.New()})
	if !errors.Is(err, ErrNotImplementedYet) {
		t.Errorf("stub returned %v, want ErrNotImplementedYet", err)
	}
}

func TestStubTranscriptWriter_ReturnsErrNotImplementedYet(t *testing.T) {
	w := NewStubTranscriptWriter()
	err := w.SetAITranscriptForAsset(context.Background(), TranscriptInput{AssetID: uuid.New()})
	if !errors.Is(err, ErrNotImplementedYet) {
		t.Errorf("stub returned %v, want ErrNotImplementedYet", err)
	}
}

// Bridge struct aggregator round-trips its fields.
func TestBridge_AggregatorHoldsAllFiveFields(t *testing.T) {
	b := Bridge{
		Lookup:           stubLookup{},
		TagWriter:        stubTagW{},
		CaptionWriter:    NewStubCaptionWriter(),
		EmbeddingWriter:  NewStubEmbeddingWriter(),
		TranscriptWriter: NewStubTranscriptWriter(),
	}
	if b.Lookup == nil || b.TagWriter == nil || b.CaptionWriter == nil ||
		b.EmbeddingWriter == nil || b.TranscriptWriter == nil {
		t.Errorf("bridge fields not preserved: %+v", b)
	}
}

// AIProvenance carries a nil JobID for direct admin actions.
func TestAIProvenance_NilJobID_DocumentedShape(t *testing.T) {
	p := AIProvenance{Provider: "openai", Model: "gpt-4o", JobID: nil}
	if p.JobID != nil {
		t.Error("JobID should default-construct as nil for admin-triggered writes")
	}
}

// ---------------------------------------------------------------------------
// Local minimal stubs used by the aggregator test above
// ---------------------------------------------------------------------------

type stubLookup struct{}

func (stubLookup) GetAssetForAI(_ context.Context, _ uuid.UUID) (AssetForAI, error) {
	return AssetForAI{}, nil
}

type stubTagW struct{}

func (stubTagW) SetAITagsForAsset(_ context.Context, _ uuid.UUID, _ []TagOutput, _ AIProvenance) error {
	return nil
}
