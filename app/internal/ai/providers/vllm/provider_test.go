// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.Name() != "vllm" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestProvider_Complete_HappyPath_NoAuthByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("auth sent on standalone vLLM: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"vllm response"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		BaseURL:                srv.URL,
		DefaultCompletionModel: "mistral-7b-instruct",
	}, ai.NewPromptRegistry(), nil)
	resp, err := p.Complete(context.Background(), ai.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "vllm response" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.EstimatedCostUSDMicros != 0 {
		t.Errorf("vLLM call recorded cost; want 0 for local infra")
	}
}

func TestProvider_Complete_AuthSentWhenAPIKeyConfigured(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                 "vllm-secret",
		BaseURL:                srv.URL,
		DefaultCompletionModel: "mistral-7b-instruct",
	}, ai.NewPromptRegistry(), nil)
	if _, err := p.Complete(context.Background(), ai.CompletionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer vllm-secret" {
		t.Errorf("auth = %q, want Bearer vllm-secret", gotAuth)
	}
}

func TestProvider_Embed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.9, 0.8, 0.7]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{BaseURL: srv.URL, DefaultEmbeddingModel: "BAAI/bge-base"}, ai.NewPromptRegistry(), nil)
	vec, err := p.Embed(context.Background(), ai.EmbedInput{Text: "x"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("len = %d", len(vec))
	}
}

func TestNewProvider_DefaultBaseURL(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.client.BaseURL != "http://localhost:8000" {
		t.Errorf("BaseURL = %q", p.client.BaseURL)
	}
}
