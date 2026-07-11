// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package vllm implements the vLLM provider. vLLM exposes the same
// OpenAI-compatible /v1/chat/completions endpoint that openai +
// ollama use; this package is a thin operator-config wrapper over
// openaicompat with a vLLM-typical default port (8000).
//
// The Lumina reference (per the brief's user-project grounding)
// validated vLLM as the operator on-prem deployment choice for
// running larger models than Ollama handles. Same wire format,
// different default port, optional API key for protected
// deployments.
//
// Phase 1.14.A: implements CompletionProvider + EmbeddingProvider
// + TagProvider + CaptionProvider. No transcription.
package vllm

import (
	"context"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

// Name is the operator-facing identifier matching system_config
// routing keys (`ai.routing.complete: "vllm"`).
const Name = "vllm"

// defaultBaseURL is the vLLM standard listen address.
const defaultBaseURL = "http://localhost:8000"

// Config is the operator-tunable vLLM provider config. APIKey is
// optional — some operators run vLLM behind a reverse proxy that
// requires a token; standalone deployments leave it empty.
type Config struct {
	APIKey  string // optional
	BaseURL string // defaults to http://localhost:8000

	DefaultCompletionModel string
	DefaultEmbeddingModel  string

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

// NewProvider constructs a vLLM provider.
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
	client := openaicompat.NewClient(Name, base, cfg.APIKey, "", cfg.DefaultCompletionModel, limiter)
	return &Provider{
		cfg:     cfg,
		client:  client,
		prompts: prompts,
		auditor: auditor,
	}
}

func (p *Provider) Name() string         { return Name }
func (p *Provider) SupportsVision() bool { return true }

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
	// Local infrastructure — cost stays 0 (operator's own compute).
	resp.EstimatedCostUSDMicros = 0

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:      Name,
			Model:         model,
			Concern:       ai.ConcernComplete,
			PromptVersion: req.PromptVersion,
			AssetID:       req.AssetID,
			InputTokens:   resp.InputTokens,
			OutputTokens:  resp.OutputTokens,
			Duration:      resp.Duration,
			Status:        ai.CallStatusSuccess,
		})
	}
	return resp, nil
}

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

var _ ai.CompletionProvider = (*Provider)(nil)
var _ ai.EmbeddingProvider = (*Provider)(nil)
var _ ai.TagProvider = (*Provider)(nil)
var _ ai.CaptionProvider = (*Provider)(nil)
