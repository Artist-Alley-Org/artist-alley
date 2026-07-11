// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package cliplocal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name_IsClipLocal(t *testing.T) {
	p := NewProvider(Config{}, nil)
	if p.Name() != "clip_local" {
		t.Errorf("Name() = %q, want clip_local", p.Name())
	}
}

func TestProvider_Embed_HappyPath_HitsEmbeddingsEndpoint(t *testing.T) {
	var capturedPath, capturedAuth string
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		BaseURL:               srv.URL,
		DefaultEmbeddingModel: "nomic-embed-text",
	}, nil)

	vec, err := p.Embed(context.Background(), ai.EmbedInput{
		Text:  "kittens in a basket",
		Model: "nomic-embed-text",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vec = %v", vec)
	}
	if capturedPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", capturedPath)
	}
	if capturedAuth != "" {
		t.Errorf("Authorization sent without API key: %q", capturedAuth)
	}
	if !strings.Contains(capturedBody, "kittens") {
		t.Errorf("request body missing input text: %s", capturedBody)
	}
}

func TestProvider_Embed_WithAPIKey_SendsBearer(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		BaseURL:               srv.URL,
		DefaultEmbeddingModel: "nomic-embed-text",
		APIKey:                "sk-fake-1234",
	}, nil)

	_, err := p.Embed(context.Background(), ai.EmbedInput{Text: "test", Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if capturedAuth != "Bearer sk-fake-1234" {
		t.Errorf("Authorization = %q, want Bearer sk-fake-1234", capturedAuth)
	}
}

func TestProvider_Embed_DefaultModel_UsedWhenInputEmpty(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{BaseURL: srv.URL}, nil) // no DefaultEmbeddingModel
	_, err := p.Embed(context.Background(), ai.EmbedInput{Text: "x"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// Falls all the way through to the package-level default.
	if !strings.Contains(capturedBody, `"model":"nomic-embed-text"`) {
		t.Errorf("body didn't include default model: %s", capturedBody)
	}
}

// Compile-time check that we ONLY satisfy EmbeddingProvider — not
// CompletionProvider/TagProvider/CaptionProvider. Catches a future
// accidental method addition that would let the router pick clip_local
// for the wrong concern.
var (
	_ ai.EmbeddingProvider = (*Provider)(nil)
)

// (No compile-time NEGATIVE assertion in Go; the test below catches
// it at runtime via type assertion.)

func TestProvider_DoesNotSatisfyCompletionProvider(t *testing.T) {
	var p ai.Provider = NewProvider(Config{}, nil)
	if _, ok := p.(ai.CompletionProvider); ok {
		t.Error("clip_local must NOT satisfy CompletionProvider (router would route chat to it)")
	}
	if _, ok := p.(ai.TagProvider); ok {
		t.Error("clip_local must NOT satisfy TagProvider")
	}
	if _, ok := p.(ai.CaptionProvider); ok {
		t.Error("clip_local must NOT satisfy CaptionProvider")
	}
	if _, ok := p.(ai.TranscriptionProvider); ok {
		t.Error("clip_local must NOT satisfy TranscriptionProvider")
	}
}
