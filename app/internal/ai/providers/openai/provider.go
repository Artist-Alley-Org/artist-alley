// Package openai implements the OpenAI cloud provider (and, by
// configuration, Azure OpenAI — same wire format, different
// baseURL + auth header). Builds on the openaicompat shared base
// for the chat-completions surface; transcribe rides its own
// multipart endpoint defined inline here.
//
// Phase 1.14.A scope: this provider implements all 5 concerns
// (CompletionProvider, EmbeddingProvider, TranscriptionProvider,
// TagProvider, CaptionProvider). Tag + Caption delegate to the
// shared ai.TagViaCompletion / ai.CaptionViaCompletion helpers so
// every provider's tagging behaviour stays consistent under the
// same prompt registry.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

// Name is the operator-facing provider identifier. Matches the
// system_config routing keys (`ai.routing.*: "openai"`).
const Name = "openai"

// Config is the operator-tunable provider configuration. Stored in
// system_config under `ai.providers.openai` as JSONB; the admin UI
// (later slice) writes it.
//
// Azure OpenAI is supported by configuring BaseURL to the Azure
// resource endpoint + an api-key header; this implementation uses
// the standard Bearer auth via the shared openaicompat client.
type Config struct {
	APIKey  string
	BaseURL string // defaults to https://api.openai.com when empty
	Org     string // optional OpenAI-Organization header

	DefaultCompletionModel  string // e.g. "gpt-4o-mini"
	DefaultEmbeddingModel   string // e.g. "text-embedding-3-large"
	DefaultTranscriptionModel string // e.g. "whisper-1"

	// RateLimit is requests per second. Zero = no rate limiting.
	RateLimitPerSecond float64
	RateLimitBurst     int
}

// defaultBaseURL is the public OpenAI API. Operators that want
// Azure / a forward proxy override via Config.BaseURL.
const defaultBaseURL = "https://api.openai.com"

// Provider implements ai.{Completion,Embedding,Transcription,Tag,Caption}Provider.
type Provider struct {
	cfg       Config
	client    *openaicompat.Client
	prompts   *ai.PromptRegistry
	auditor   *ai.CallAuditor
}

// NewProvider constructs an OpenAI provider ready to register with
// the router. The prompt registry is required for Tag + Caption;
// auditor may be nil for tests.
func NewProvider(cfg Config, prompts *ai.PromptRegistry, auditor *ai.CallAuditor) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	var limiter *rate.Limiter
	if cfg.RateLimitPerSecond > 0 {
		burst := cfg.RateLimitBurst
		if burst <= 0 {
			burst = 1
		}
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimitPerSecond), burst)
	}
	client := openaicompat.NewClient(Name, base, cfg.APIKey, cfg.Org, cfg.DefaultCompletionModel, limiter)
	return &Provider{
		cfg:     cfg,
		client:  client,
		prompts: prompts,
		auditor: auditor,
	}
}

// Name returns the provider identifier for routing.
func (p *Provider) Name() string { return Name }

// SupportsVision is true for OpenAI's GPT-4o family. Operators on
// older models (gpt-3.5) can still use this provider for text-only
// completions; the router doesn't gate on this — it's an admin-UI
// surface flag.
func (p *Provider) SupportsVision() bool { return true }

// ---------------------------------------------------------------------------
// CompletionProvider
// ---------------------------------------------------------------------------

func (p *Provider) Complete(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResponse, error) {
	body, err := openaicompat.MarshalCompletion(req, p.cfg.DefaultCompletionModel)
	if err != nil {
		return ai.CompletionResponse{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err,
		}
	}
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultCompletionModel
	}

	start := time.Now()
	respBody, err := p.client.PostJSON(ctx, "/v1/chat/completions", body, model)
	duration := time.Since(start)
	if err != nil {
		// Audit the failure (status reflects the error class).
		p.auditFailure(ctx, model, ai.ConcernComplete, req, duration, err)
		return ai.CompletionResponse{}, err
	}

	resp, err := openaicompat.ParseCompletion(respBody, duration)
	if err != nil {
		p.auditFailure(ctx, model, ai.ConcernComplete, req, duration, err)
		return ai.CompletionResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}

	resp.EstimatedCostUSDMicros = estimateChatCost(model, resp.InputTokens, resp.OutputTokens)

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:               Name,
			Model:                  model,
			Concern:                ai.ConcernComplete,
			PromptVersion:          req.PromptVersion,
			AssetID:                req.AssetID,
			InputHash:              ai.CanonicalInputHash(model, req.Messages),
			InputTokens:            resp.InputTokens,
			OutputTokens:           resp.OutputTokens,
			Duration:               resp.Duration,
			EstimatedCostUSDMicros: resp.EstimatedCostUSDMicros,
			Status:                 ai.CallStatusSuccess,
		})
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// EmbeddingProvider
// ---------------------------------------------------------------------------

func (p *Provider) Embed(ctx context.Context, in ai.EmbedInput) ([]float32, error) {
	body, err := openaicompat.MarshalEmbedding(in, p.cfg.DefaultEmbeddingModel)
	if err != nil {
		return nil, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}
	model := in.Model
	if model == "" {
		model = p.cfg.DefaultEmbeddingModel
	}

	start := time.Now()
	respBody, err := p.client.PostJSON(ctx, "/v1/embeddings", body, model)
	duration := time.Since(start)
	if err != nil {
		return nil, err
	}
	vec, err := openaicompat.ParseEmbedding(respBody)
	if err != nil {
		return nil, &ai.ProviderError{Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err}
	}

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider: Name, Model: model, Concern: ai.ConcernEmbed,
			Duration: duration, Status: ai.CallStatusSuccess,
			InputHash: ai.CanonicalInputHash(model, in.Text),
		})
	}
	return vec, nil
}

// ---------------------------------------------------------------------------
// TranscriptionProvider
// ---------------------------------------------------------------------------
//
// OpenAI's transcription surface is /v1/audio/transcriptions, which
// takes multipart/form-data (file + model + optional language).
// Inline here rather than openaicompat because no other 1.14.A
// provider uses this exact shape.

func (p *Provider) Transcribe(ctx context.Context, audio ai.AudioInput, opts ai.TranscribeOpts) (ai.Transcript, error) {
	if len(audio.Bytes) == 0 && audio.URL == "" {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name,
			Wrapped: errors.New("transcribe: no audio bytes or URL"),
		}
	}
	model := opts.Model
	if model == "" {
		model = p.cfg.DefaultTranscriptionModel
		if model == "" {
			model = "whisper-1"
		}
	}

	// For v1 we only support inline bytes — URL form would require
	// the server to fetch externally, which it doesn't.
	if len(audio.Bytes) == 0 {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name,
			Wrapped: errors.New("transcribe: URL form not supported in v1; pass Bytes"),
		}
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	filePart, err := mw.CreateFormFile("file", "audio")
	if err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}
	if _, err := filePart.Write(audio.Bytes); err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}
	_ = mw.WriteField("model", model)
	if opts.LanguageHint != "" {
		_ = mw.WriteField("language", opts.LanguageHint)
	}
	if err := mw.Close(); err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}

	base := p.cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/transcriptions", &buf)
	if err != nil {
		return ai.Transcript{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	start := time.Now()
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ai.Transcript{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode >= 400 {
		return ai.Transcript{}, openaicompat.ClassifyHTTPError(httpResp.StatusCode, httpResp.Header.Get("Retry-After"), Name, model)
	}
	duration := time.Since(start)

	var wire struct {
		Text     string `json:"text"`
		Language string `json:"language"`
	}
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err}
	}

	tx := ai.Transcript{
		Text:             wire.Text,
		DetectedLanguage: wire.Language,
		Duration:         duration,
	}

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider: Name, Model: model, Concern: ai.ConcernTranscribe,
			Duration: duration, Status: ai.CallStatusSuccess,
		})
	}
	return tx, nil
}

// ---------------------------------------------------------------------------
// Tag + Caption
// ---------------------------------------------------------------------------

func (p *Provider) Tag(ctx context.Context, asset ai.AssetRef, opts ai.TagOpts) ([]ai.Tag, error) {
	return ai.TagViaCompletion(ctx, p, p.prompts, asset, opts)
}

func (p *Provider) Caption(ctx context.Context, asset ai.AssetRef, opts ai.CaptionOpts) (string, error) {
	return ai.CaptionViaCompletion(ctx, p, p.prompts, asset, opts)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// auditFailure records a non-success call. Best-effort; nil-safe
// when the auditor isn't wired.
func (p *Provider) auditFailure(ctx context.Context, model string, concern ai.Concern, req ai.CompletionRequest, duration time.Duration, err error) {
	if p.auditor == nil {
		return
	}
	status := ai.CallStatusPermanentError
	if pe, ok := ai.AsProviderError(err); ok {
		switch pe.Class {
		case ai.ErrClassRateLimit:
			status = ai.CallStatusRateLimited
		case ai.ErrClassTransient:
			status = ai.CallStatusTransientError
		case ai.ErrClassBudget:
			status = ai.CallStatusBudgetBlocked
		case ai.ErrClassPrivacy:
			status = ai.CallStatusPrivacyBlocked
		}
	}
	p.auditor.RecordCall(ctx, ai.CallRecord{
		Provider:      Name,
		Model:         model,
		Concern:       concern,
		PromptVersion: req.PromptVersion,
		AssetID:       req.AssetID,
		Duration:      duration,
		Status:        status,
		ErrorMessage:  err.Error(),
	})
}

// estimateChatCost is a per-model micros estimate. Values from the
// OpenAI public pricing page as of 2026-06 (operator can adjust
// per-provider rates later via system_config; for 1.14.A these are
// hard-coded so the budget tracker has something to rollup).
//
// Pricing format: input + output per 1M tokens, then × tokens /
// 1_000_000 yields total cost in dollars. We return micros.
func estimateChatCost(model string, inputTokens, outputTokens int) int64 {
	// Default rates (gpt-4o-mini family): $0.15 in / $0.60 out per 1M.
	inputUSDper1M := 0.15
	outputUSDper1M := 0.60
	switch {
	case model == "gpt-4o" || model == "gpt-4o-2024-08-06":
		inputUSDper1M = 2.50
		outputUSDper1M = 10.00
	case model == "gpt-4-turbo" || model == "gpt-4":
		inputUSDper1M = 10.00
		outputUSDper1M = 30.00
	case model == "gpt-3.5-turbo":
		inputUSDper1M = 0.50
		outputUSDper1M = 1.50
	}
	inputUSD := float64(inputTokens) * inputUSDper1M / 1_000_000.0
	outputUSD := float64(outputTokens) * outputUSDper1M / 1_000_000.0
	totalMicros := int64((inputUSD + outputUSD) * 1_000_000)
	return totalMicros
}

// Compile-time interface checks: panic at build, not runtime, if a
// future signature change breaks a contract.
var _ ai.CompletionProvider = (*Provider)(nil)
var _ ai.EmbeddingProvider = (*Provider)(nil)
var _ ai.TranscriptionProvider = (*Provider)(nil)
var _ ai.TagProvider = (*Provider)(nil)
var _ ai.CaptionProvider = (*Provider)(nil)
