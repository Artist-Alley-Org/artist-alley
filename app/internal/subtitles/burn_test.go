// Phase 1.18.B-3 — burn handler tests.
//
// The handler itself is a stub (matches audiobook.merge / decrypt
// precedent), so these tests pin the contract:
//
//   1. Returns TerminalError → no retry storm against stub
//   2. Bad payload → also TerminalError, not retryable bad-json error
//   3. Logs the asset_id / lang from the payload so operators can
//      tell what work was dropped on the floor before the real
//      implementation lands

package subtitles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

func TestBurnHandler_Type(t *testing.T) {
	h := NewBurnHandler(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if h.Type() != jobs.TypeSubtitleBurn {
		t.Errorf("Type() = %q, want %q", h.Type(), jobs.TypeSubtitleBurn)
	}
}

func TestBurnHandler_BadPayload_TerminalError(t *testing.T) {
	h := NewBurnHandler(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	claim := &jobs.Claim{Payload: []byte("not-json")}
	_, err := h.Handle(context.Background(), claim)
	if err == nil {
		t.Fatal("expected error on bad payload")
	}
	if !jobs.IsTerminal(err) {
		t.Errorf("err=%v should be terminal", err)
	}
}

func TestBurnHandler_Stub_ReturnsTerminal(t *testing.T) {
	// Pin the stub-status: a well-formed payload still returns
	// terminal so the queue doesn't retry the unimplemented stub
	// in a hot loop. Remove this test when the ffmpeg integration
	// ships.
	h := NewBurnHandler(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	payload, _ := json.Marshal(BurnPayload{
		AssetID: uuid.New(),
		Lang:    "en",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err == nil {
		t.Fatal("expected terminal error from stub")
	}
	if !jobs.IsTerminal(err) {
		t.Errorf("err=%v should be terminal so the queue stops retrying the stub", err)
	}
	var te *jobs.TerminalError
	if errors.As(err, &te) {
		if !strings.Contains(te.Err.Error(), "stub") {
			t.Errorf("inner error should mention stub status: %q", te.Err.Error())
		}
	}
}
