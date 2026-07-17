// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Universal request + response types
// ---------------------------------------------------------------------------
//
// Completion is the chat-style surface every cloud + LLM-serving
// provider speaks. OpenAI's `chat/completions` shape is the de-facto
// standard; Ollama + vLLM speak it natively, Claude + Gemini get a
// thin shim each. The CompletionRequest carries everything the
// provider needs to make the call AND everything the audit layer
// needs to record it (asset id, prompt template version, privacy
// class) so the wrapper doesn't have to pass those out-of-band.

// CompletionRequest is the chat-style input the providers consume.
// AssetID + PromptVersion ride along for audit attribution; they're
// not part of the wire payload sent to the provider.
type CompletionRequest struct {
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int
	Tools       []ToolDecl // optional; tool-use providers consume this
	Stream      bool       // v1: always false; streaming surfaces later

	// Privacy + asset + audit fields. The wrapper layer fills these
	// from the caller's context; provider impls don't need to read
	// them (the wrapper handles routing + audit).
	Privacy       PrivacyClass
	AssetID       *uuid.UUID
	PromptVersion string
}

// Message is one turn in the chat history. Content is a slice so a
// single message can carry mixed text + image parts (vision models).
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content []Content
}

// Content is one part of a multi-modal message. Exactly one of the
// optional fields is populated per part; Type names which one.
type Content struct {
	Type       ContentType
	Text       string
	ImageURL   string // populated when the source is a URL
	ImageBytes []byte // populated when the source is inline bytes (b64'd on wire)
	MimeType   string // required for ImageBytes
}

// ContentType names the part shape. Closed enum so providers can
// switch on it exhaustively.
type ContentType string

const (
	ContentTypeText     ContentType = "text"
	ContentTypeImageURL ContentType = "image_url"
	ContentTypeImageB64 ContentType = "image_b64"
	ContentTypeAudioURL ContentType = "audio_url"
)

// ToolDecl is a function-calling tool declaration. Shape mirrors
// OpenAI's; Claude + Gemini providers translate.
type ToolDecl struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema fragment
}

// CompletionResponse is the universal output shape. Token counts +
// estimated cost ride out alongside the text so the caller can audit
// without re-tokenising.
type CompletionResponse struct {
	Text                   string
	InputTokens            int
	OutputTokens           int
	FinishReason           string
	Duration               time.Duration
	EstimatedCostUSDMicros int64
}

// ---------------------------------------------------------------------------
// Embedding + transcription request/response
// ---------------------------------------------------------------------------

// EmbedInput is the universal embedding input. Either Text or
// ImageBytes is populated depending on the model's modality; both can
// be set for true multimodal models (CLIP, 1.14.B). Model is required
// because the same provider often supports multiple embedding models
// at different dimensionalities.
type EmbedInput struct {
	Text       string
	ImageBytes []byte
	MimeType   string
	Model      string
}

// AudioInput names where the audio lives. URL form is preferred when
// the bytes are already in object storage; Bytes form is the fallback
// for pre-storage transcription.
type AudioInput struct {
	URL      string
	Bytes    []byte
	MimeType string
}

// TranscribeOpts carries optional knobs (language hint, model
// override). Adding fields here is non-breaking.
type TranscribeOpts struct {
	Model                 string
	LanguageHint          string // ISO 639-1 if known; "" lets the model auto-detect
	IncludeWordTimestamps bool
}

// Transcript is the universal transcription output shape.
type Transcript struct {
	Text                   string
	DetectedLanguage       string
	Segments               []TranscriptSegment // empty if word/segment timestamps weren't requested
	Duration               time.Duration
	EstimatedCostUSDMicros int64
}

// TranscriptSegment is one timestamped run of text — used by the WebVTT
// subtitle generator in 1.14.C.
type TranscriptSegment struct {
	StartMS int
	EndMS   int
	Text    string
}

// ---------------------------------------------------------------------------
// Task-specialised wrappers
// ---------------------------------------------------------------------------

// AssetRef is the minimal asset identification + content reference a
// Tag/Caption provider needs. Kept opaque to avoid pulling the assets
// package into ai (which would create a cycle); the wrapper layer
// fills these from the asset row.
type AssetRef struct {
	ID          uuid.UUID
	MimeType    string
	PreviewURL  string // typically the medium-size preview
	OriginalURL string // for vision models that need higher fidelity
}

// TagOpts knobs the tagging behaviour: max tags, the prompt template
// version override, optional vocabulary constraints.
type TagOpts struct {
	MaxTags        int
	PromptVersion  string   // empty = use registry default
	VocabularyHint []string // optional bias toward these tags
}

// Tag is one tag from a Tag provider. Confidence is in [0,1] when the
// provider supplies one; otherwise zero (always-true tag).
type Tag struct {
	Term       string
	Confidence float64
}

// CaptionOpts knobs the captioning behaviour.
type CaptionOpts struct {
	MaxLength     int
	PromptVersion string // empty = use registry default
	StyleHint     string // optional ("descriptive" / "concise" / "marketing")
}

// ---------------------------------------------------------------------------
// The five typed concern interfaces
// ---------------------------------------------------------------------------
//
// A provider implements only the concerns it supports. Type assertion
// at the router layer (`if cp, ok := p.(CompletionProvider); ok {...}`)
// lets the router pick a candidate set per concern without runtime
// `Supports(c)` checks.

// Provider is the marker interface every concrete provider satisfies.
// Name() is the operator-facing identifier matching system_config
// (`ai.routing`, `ai.fallback_chains`, etc.).
type Provider interface {
	Name() string
}

// CompletionProvider speaks the universal chat surface.
type CompletionProvider interface {
	Provider
	SupportsVision() bool
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// EmbeddingProvider produces fixed-dimension vectors from text or
// images. Embedding dim is per-model; the caller asserts the shape.
type EmbeddingProvider interface {
	Provider
	Embed(ctx context.Context, in EmbedInput) ([]float32, error)
}

// TranscriptionProvider converts audio to text.
type TranscriptionProvider interface {
	Provider
	Transcribe(ctx context.Context, audio AudioInput, opts TranscribeOpts) (Transcript, error)
}

// TagProvider is a task-specialised wrapper. The default impl in 1.14.A
// wraps a CompletionProvider with a prompt template; 1.14.B will add a
// CLIP-similarity-based impl that doesn't need a chat backend.
type TagProvider interface {
	Provider
	Tag(ctx context.Context, asset AssetRef, opts TagOpts) ([]Tag, error)
}

// CaptionProvider is the caption analogue of TagProvider.
type CaptionProvider interface {
	Provider
	Caption(ctx context.Context, asset AssetRef, opts CaptionOpts) (string, error)
}

// ---------------------------------------------------------------------------
// Error classification — drives retry behaviour at the worker layer
// ---------------------------------------------------------------------------
//
// Every provider error wraps in a *ProviderError so the router can
// route on class without parsing strings. The job worker
// (jobs/handlers/ai_tag.go etc.) classifies on Class:
//
//   ErrClassTransient + ErrClassRateLimit → worker retries with backoff
//   ErrClassPermanent → worker marks the job TerminalError (no retry)
//   ErrClassBudget    → same; operator must raise the cap before retry
//   ErrClassPrivacy   → same; operator must change the policy
//
// The router's Call() helper handles the fallback-chain walk: rate-
// limit + transient triggers fall-through; permanent + budget + privacy
// short-circuit (no fallback could succeed).

type ErrorClass int

const (
	// ErrClassTransient covers 5xx, connection failures, EOFs.
	ErrClassTransient ErrorClass = iota

	// ErrClassRateLimit is a 429 (or provider-specific equivalent).
	// RetryAfter is populated when the provider hints at one.
	ErrClassRateLimit

	// ErrClassPermanent covers 4xx (non-429) — bad request, bad
	// model name, content filter, etc. Retrying won't help.
	ErrClassPermanent

	// ErrClassBudget — the cost.Tracker rejected the call before the
	// HTTP request fired. Provider name is set; RetryAfter is zero.
	ErrClassBudget

	// ErrClassPrivacy — the router's privacy gate filtered every
	// candidate provider out. Surfaced so the operator dashboard can
	// show "N calls blocked by privacy policy".
	ErrClassPrivacy
)

// ProviderError wraps any provider-originating error with the
// classification the router + worker need.
type ProviderError struct {
	Class      ErrorClass
	Provider   string
	Model      string
	RetryAfter time.Duration
	Wrapped    error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	prefix := "ai: " + e.Provider
	if e.Model != "" {
		prefix += "/" + e.Model
	}
	if e.Wrapped != nil {
		return prefix + ": " + e.Wrapped.Error()
	}
	return prefix + ": " + classString(e.Class)
}

func (e *ProviderError) Unwrap() error { return e.Wrapped }

// IsRetryable reports whether the router/worker should try a fallback
// (or schedule a retry). Permanent/budget/privacy are terminal; the
// caller surfaces them rather than walking the chain.
func (e *ProviderError) IsRetryable() bool {
	if e == nil {
		return false
	}
	switch e.Class {
	case ErrClassTransient, ErrClassRateLimit:
		return true
	}
	return false
}

func classString(c ErrorClass) string {
	switch c {
	case ErrClassTransient:
		return "transient"
	case ErrClassRateLimit:
		return "rate_limited"
	case ErrClassPermanent:
		return "permanent"
	case ErrClassBudget:
		return "budget_blocked"
	case ErrClassPrivacy:
		return "privacy_blocked"
	}
	return "unknown"
}

// AsProviderError unwraps an error into a *ProviderError if one is in
// the chain. Sugar over errors.As for the common case.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// ErrNoProviderAvailable is returned by the router when no provider
// can serve the request — either none registered for the concern,
// budget exhausted across all candidates, or privacy gate emptied
// the candidate set. The caller should surface this with the
// operator-actionable signal (privacy_blocked vs budget_blocked) by
// inspecting the underlying ProviderError if one is wrapped.
var ErrNoProviderAvailable = errors.New("ai: no provider available")
