// Package ollama implements the Ollama local-LLM provider. Ollama
// exposes an OpenAI-compatible /v1/chat/completions endpoint, so
// this is essentially a thin wrapper over openaicompat with no API
// key and a different default baseURL (operator's local Ollama
// install, typically http://localhost:11434).
//
// Phase 1.14.A: implements CompletionProvider + EmbeddingProvider
// + TagProvider + CaptionProvider. No transcription (Ollama doesn't
// run Whisper natively); 1.14.C adds whisper-local for that.
//
// Operator deploys Ollama as a separate container/process per ADR
// 0034 — we just connect via the configured BaseURL. When Ollama
// is offline the provider's calls return ErrClassTransient and the
// router walks the fallback chain.
package ollama

import (
	"context"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

// Name is the operator-facing identifier matching system_config
// routing keys (`ai.routing.tag: "ollama"`, default for tag concern).
const Name = "ollama"

// defaultBaseURL is Ollama's standard listen address. Operators on
// a remote box override via Config.BaseURL.
const defaultBaseURL = "http://localhost:11434"

// Config is the operator-tunable Ollama provider config.
type Config struct {
	BaseURL string // defaults to http://localhost:11434 when empty

	DefaultCompletionModel string // e.g. "llama3.1:8b"
	DefaultEmbeddingModel  string // e.g. "nomic-embed-text"

	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Provider implements ai.CompletionProvider, EmbeddingProvider,
// TagProvider, CaptionProvider.
type Provider struct {
	cfg     Config
	client  *openaicompat.Client
	prompts *ai.PromptRegistry
	auditor *ai.CallAuditor
}

// NewProvider constructs an Ollama provider. prompts is required for
// Tag + Caption; auditor may be nil for tests.
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
	// No API key for Ollama — empty string skips the Authorization
	// header in openaicompat.Client.
	client := openaicompat.NewClient(Name, base, "", "", cfg.DefaultCompletionModel, limiter)
	return &Provider{
		cfg:     cfg,
		client:  client,
		prompts: prompts,
		auditor: auditor,
	}
}

// Name returns the operator-facing identifier.
func (p *Provider) Name() string { return Name }

// SupportsVision depends on which model the operator runs. Llama 3.2
// vision + Llava family support images; smaller text-only models
// don't. Returning true here so the router doesn't pre-filter for
// vision use cases; the operator picks a vision-capable model.
func (p *Provider) SupportsVision() bool { return true }

// Complete sends a chat-completion request to Ollama's OpenAI-
// compatible endpoint.
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
		return ai.CompletionResponse{}, err
	}

	resp, err := openaicompat.ParseCompletion(respBody, duration)
	if err != nil {
		return ai.CompletionResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}

	// Ollama is local + free — record duration but cost stays 0.
	resp.EstimatedCostUSDMicros = 0

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:      Name,
			Model:         model,
			Concern:       ai.ConcernComplete,
			PromptVersion: req.PromptVersion,
			AssetID:       req.AssetID,
			InputHash:     ai.CanonicalInputHash(model, req.Messages),
			InputTokens:   resp.InputTokens,
			OutputTokens:  resp.OutputTokens,
			Duration:      resp.Duration,
			Status:        ai.CallStatusSuccess,
		})
	}
	return resp, nil
}

// Embed sends a text embedding request. Ollama's embedding endpoint
// is also OpenAI-compatible (/v1/embeddings).
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

func (p *Provider) Tag(ctx context.Context, asset ai.AssetRef, opts ai.TagOpts) ([]ai.Tag, error) {
	return ai.TagViaCompletion(ctx, p, p.prompts, asset, opts)
}

func (p *Provider) Caption(ctx context.Context, asset ai.AssetRef, opts ai.CaptionOpts) (string, error) {
	return ai.CaptionViaCompletion(ctx, p, p.prompts, asset, opts)
}

// Compile-time interface checks.
var _ ai.CompletionProvider = (*Provider)(nil)
var _ ai.EmbeddingProvider = (*Provider)(nil)
var _ ai.TagProvider = (*Provider)(nil)
var _ ai.CaptionProvider = (*Provider)(nil)
