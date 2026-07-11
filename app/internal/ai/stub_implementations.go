// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"

	"github.com/google/uuid"
)

// Stub bridge implementations registered at boot until the
// providing phase ships its concrete implementation. Each stub
// returns ErrNotImplementedYet so the calling job handler can
// classify (TerminalError; operator must wait for the providing
// phase) without panic.
//
// # Replacement plan
//
//   - NewStubCaptionWriter   → assets.Handler.SetAICaptionForAsset
//                              once the caption column lands
//                              (follow-up to 1.14.A-bridge)
//   - NewStubEmbeddingWriter → app/internal/ai/embeddings/.NewWriter
//                              when Phase 1.14.B ships pgvector +
//                              the asset_embedding table
//   - NewStubTranscriptWriter → app/internal/subtitles/.NewAIWriter
//                              when Phase 1.14.C ships the Whisper
//                              integration + source_format='whisper'
//                              extension
//
// Each replacement is one line in the boot wire.

// ---------------------------------------------------------------------------
// CaptionWriter stub
// ---------------------------------------------------------------------------

type stubCaptionImpl struct{}

// NewStubCaptionWriter returns the no-op writer registered until
// the concrete caption persistence ships. Every call returns
// ErrNotImplementedYet so the calling job handler classifies the
// error as terminal.
func NewStubCaptionWriter() CaptionWriter { return stubCaptionImpl{} }

func (stubCaptionImpl) SetAICaptionForAsset(_ context.Context, _ uuid.UUID, _ string, _ AIProvenance) error {
	return ErrNotImplementedYet
}

// ---------------------------------------------------------------------------
// EmbeddingWriter stub
// ---------------------------------------------------------------------------

type stubEmbeddingImpl struct{}

// NewStubEmbeddingWriter returns the no-op writer registered until
// Phase 1.14.B ships pgvector storage + the concrete impl.
func NewStubEmbeddingWriter() EmbeddingWriter { return stubEmbeddingImpl{} }

func (stubEmbeddingImpl) UpsertAssetEmbedding(_ context.Context, _ EmbeddingInput) error {
	return ErrNotImplementedYet
}

// ---------------------------------------------------------------------------
// TranscriptWriter stub
// ---------------------------------------------------------------------------

type stubTranscriptImpl struct{}

// NewStubTranscriptWriter returns the no-op writer registered until
// Phase 1.14.C wires Whisper transcription + the subtitles adapter.
func NewStubTranscriptWriter() TranscriptWriter { return stubTranscriptImpl{} }

func (stubTranscriptImpl) SetAITranscriptForAsset(_ context.Context, _ TranscriptInput) error {
	return ErrNotImplementedYet
}
