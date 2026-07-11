// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package cliplocal implements the `clip_local` embedding provider —
// the default backend for ai.routing.embed (see migration 00009).
//
// # Scope vs. naming
//
// "clip_local" was named for the eventual goal of running a real CLIP
// model in-process so image embeddings work without a sidecar. We're
// not there yet: today the provider speaks Ollama's OpenAI-compatible
// /v1/embeddings endpoint and routes text-mode requests through
// whatever embedding model the operator's Ollama install has loaded
// (nomic-embed-text by default; 768-dim, matches asset_embedding_d768).
// Image-mode CLIP support lands when a Go-native CLIP runtime ships
// OR we adopt a tested sidecar; either way the upgrade is local to
// this package, not a public-API change.
//
// # Why a separate package vs. just using the `ollama` provider
//
// Two reasons. (1) The routing config defaults to "clip_local"; an
// operator on a fresh install gets a working embed path with no
// config edits. (2) Registering the same Ollama instance under two
// names (ollama for chat, clip_local for embeddings) lets the
// operator tune them independently — e.g. point clip_local at a
// dedicated embedding server while ollama keeps a chat model loaded
// at the default endpoint. Both connect to the same `/v1/embeddings`
// surface; the difference is operator intent, not protocol.
package cliplocal

import (
	"context"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

// Name matches the system_config ai.routing.embed default seeded by
// migration 00009.
const Name = "clip_local"

// defaultBaseURL points at the local Ollama install. Operators on a
// remote box override via Config.BaseURL.
const defaultBaseURL = "http://localhost:11434"

// defaultEmbeddingModel is the seed default — 768-dim, CPU-friendly,
// shipped by Ollama out of the box. Matches asset_embedding_d768.
const defaultEmbeddingModel = "nomic-embed-text"

// Config is the operator-tunable provider config. Mirrors the shape
// of ollama.Config so the admin UI can render both with one form.
type Config struct {
	BaseURL string

	DefaultEmbeddingModel string

	// API key passes through the openaicompat client as the Bearer
	// token. Ollama ignores it; commercial CLIP services (Replicate,
	// HF Inference) honour it. Empty string skips the Authorization
	// header.
	APIKey string

	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Provider implements ai.EmbeddingProvider only. We deliberately do
// NOT satisfy CompletionProvider / TagProvider / CaptionProvider —
// a "clip_local" backend should never be picked for chat-style
// concerns even if the operator routed them there by mistake. The
// router's type-assertion gate ensures it never reaches the wrong
// concern.
type Provider struct {
	cfg     Config
	client  *openaicompat.Client
	auditor *ai.CallAuditor
}

// NewProvider constructs the provider. auditor may be nil for tests.
func NewProvider(cfg Config, auditor *ai.CallAuditor) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	model := cfg.DefaultEmbeddingModel
	if model == "" {
		model = defaultEmbeddingModel
	}

	var limiter *rate.Limiter
	if cfg.RateLimitPerSecond > 0 {
		burst := cfg.RateLimitBurst
		if burst <= 0 {
			burst = 1
		}
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimitPerSecond), burst)
	}

	// openaicompat client is identical shape to ollama's. APIKey can
	// be empty (Ollama path) or populated (commercial service path).
	client := openaicompat.NewClient(Name, base, cfg.APIKey, "", model, limiter)

	return &Provider{cfg: cfg, client: client, auditor: auditor}
}

// Name satisfies ai.Provider.
func (p *Provider) Name() string { return Name }

// Embed satisfies ai.EmbeddingProvider. Returns the vector's
// dimensionality unmodified — the writer validates the shape against
// the dim_registry; mismatches surface as ErrDimensionMismatch there.
func (p *Provider) Embed(ctx context.Context, in ai.EmbedInput) ([]float32, error) {
	model := in.Model
	if model == "" {
		model = p.cfg.DefaultEmbeddingModel
		if model == "" {
			model = defaultEmbeddingModel
		}
	}

	body, err := openaicompat.MarshalEmbedding(in, model)
	if err != nil {
		return nil, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
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
			Provider:  Name,
			Model:     model,
			Concern:   ai.ConcernEmbed,
			Duration:  duration,
			Status:    ai.CallStatusSuccess,
			InputHash: ai.CanonicalInputHash(model, in.Text),
		})
	}
	return vec, nil
}

// Compile-time interface check.
var _ ai.EmbeddingProvider = (*Provider)(nil)
