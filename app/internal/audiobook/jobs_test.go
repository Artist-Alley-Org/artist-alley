package audiobook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// Both audiobook handlers are intentional stubs awaiting Phase B-2.
// The contract is: they must register a real Type(), parse their
// payload, and return a TerminalError (NOT a transient error) so
// the dispatcher doesn't endlessly retry an unimplemented worker.
// These tests pin that contract.

func nullLogger() *slog.Logger { return slog.New(slog.NewTextHandler(discardWriter{}, nil)) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestMergeHandler_Type(t *testing.T) {
	h := NewMergeHandler(nil, nil, nil, nullLogger())
	if h.Type() != jobs.TypeAudiobookMerge {
		t.Errorf("Type = %q, want %q", h.Type(), jobs.TypeAudiobookMerge)
	}
}

func TestMergeHandler_StubReturnsTerminal(t *testing.T) {
	h := NewMergeHandler(nil, nil, nil, nullLogger())
	payload, _ := json.Marshal(MergePayload{
		PostID:         uuid.New(),
		MemberAssetIDs: []uuid.UUID{uuid.New(), uuid.New()},
		ChapterTitles:  []string{"Ch 1", "Ch 2"},
		OutputTitle:    "Test Audiobook",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err == nil {
		t.Fatal("expected terminal stub error")
	}
	var terr *jobs.TerminalError
	if !errors.As(err, &terr) {
		t.Fatalf("expected *jobs.TerminalError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "Phase B-2 stub") {
		t.Errorf("error %q missing stub marker — has the stub been removed without removing the test?", err.Error())
	}
}

// A garbage payload should ALSO return a TerminalError — invalid
// JSON isn't going to become valid on retry, so the dispatcher
// shouldn't keep firing the job.
func TestMergeHandler_BadPayloadIsTerminal(t *testing.T) {
	h := NewMergeHandler(nil, nil, nil, nullLogger())
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: []byte(`{not json`)})
	var terr *jobs.TerminalError
	if !errors.As(err, &terr) {
		t.Fatalf("expected *jobs.TerminalError for bad JSON, got %T: %v", err, err)
	}
}

func TestDecryptHandler_Type(t *testing.T) {
	h := NewDecryptHandler(nil, nil, nil, nullLogger())
	if h.Type() != jobs.TypeAudiobookDecrypt {
		t.Errorf("Type = %q, want %q", h.Type(), jobs.TypeAudiobookDecrypt)
	}
}

func TestDecryptHandler_StubReturnsTerminal(t *testing.T) {
	h := NewDecryptHandler(nil, nil, nil, nullLogger())
	payload, _ := json.Marshal(DecryptPayload{
		AssetID:         uuid.New(),
		ActivationBytes: "deadbeef",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	var terr *jobs.TerminalError
	if !errors.As(err, &terr) {
		t.Fatalf("expected *jobs.TerminalError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "Phase B-2 stub") {
		t.Errorf("error %q missing stub marker", err.Error())
	}
}

func TestDecryptHandler_BadPayloadIsTerminal(t *testing.T) {
	h := NewDecryptHandler(nil, nil, nil, nullLogger())
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: []byte(`null but not json`)})
	var terr *jobs.TerminalError
	if !errors.As(err, &terr) {
		t.Fatalf("expected *jobs.TerminalError, got %T: %v", err, err)
	}
}

// Round-trip the payload structs through JSON so a field rename
// here surfaces as a test failure instead of a silent enqueue/decode
// mismatch in production.
func TestMergePayload_JSONRoundtrip(t *testing.T) {
	in := MergePayload{
		PostID:           uuid.MustParse("12345678-1234-1234-1234-123456789012"),
		MemberAssetIDs:   []uuid.UUID{uuid.New()},
		ChapterTitles:    []string{"Ch 1"},
		OutputTitle:      "Test",
		AuthorOverride:   "Author",
		NarratorOverride: "Narrator",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out MergePayload
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PostID != in.PostID || out.OutputTitle != in.OutputTitle ||
		out.AuthorOverride != in.AuthorOverride || out.NarratorOverride != in.NarratorOverride {
		t.Errorf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestDecryptPayload_JSONRoundtrip(t *testing.T) {
	in := DecryptPayload{
		AssetID:         uuid.MustParse("12345678-1234-1234-1234-123456789012"),
		KeyAssetID:      uuid.MustParse("87654321-4321-4321-4321-210987654321"),
		ActivationBytes: "deadbeef",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out DecryptPayload
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}
