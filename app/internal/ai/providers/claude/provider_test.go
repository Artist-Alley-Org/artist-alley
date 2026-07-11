// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.Name() != "claude" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestProvider_Complete_SendsAnthropicHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header missing")
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if got["model"] != "claude-3-5-sonnet-20241022" {
			t.Errorf("model = %v", got["model"])
		}
		if _, hasMax := got["max_tokens"]; !hasMax {
			t.Error("max_tokens missing — Claude requires it")
		}
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"claude says hi"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":4}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		DefaultCompletionModel: "claude-3-5-sonnet-20241022",
	}, ai.NewPromptRegistry(), nil)

	resp, err := p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "claude says hi" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 5 || resp.OutputTokens != 4 {
		t.Errorf("tokens drift: %+v", resp)
	}
	if resp.EstimatedCostUSDMicros == 0 {
		t.Error("cost not estimated")
	}
}

func TestProvider_Complete_SystemMessageCollapsedToSystemField(t *testing.T) {
	gotBody := []byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "claude-3-haiku"}, ai.NewPromptRegistry(), nil)
	_, _ = p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "system", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "you are helpful"}}},
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "x"}}},
		},
	})

	var sent struct {
		System   string `json:"system"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(gotBody, &sent)
	if sent.System != "you are helpful" {
		t.Errorf("system = %q (Claude system role should collapse into top-level field)", sent.System)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" {
		t.Errorf("messages drift: %+v", sent.Messages)
	}
}

func TestProvider_Complete_ImageURLContent(t *testing.T) {
	gotBody := []byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "claude-3-5-sonnet"}, ai.NewPromptRegistry(), nil)
	_, _ = p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{
				{Type: ai.ContentTypeText, Text: "look"},
				{Type: ai.ContentTypeImageURL, ImageURL: "http://example.test/img.png"},
			}},
		},
	})
	body := string(gotBody)
	if !strings.Contains(body, `"type":"image"`) {
		t.Errorf("image block missing: %s", body)
	}
	if !strings.Contains(body, `"url":"http://example.test/img.png"`) {
		t.Errorf("url not preserved: %s", body)
	}
}

func TestProvider_Complete_429_ReturnsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "claude-3-haiku"}, ai.NewPromptRegistry(), nil)
	_, err := p.Complete(context.Background(), ai.CompletionRequest{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassRateLimit {
		t.Errorf("err = %v, want ErrClassRateLimit", pe)
	}
}

func TestEstimateChatCost_DifferentiatesSonnetHaikuOpus(t *testing.T) {
	sonnet := estimateChatCost("claude-3-5-sonnet-20241022", 1000, 500)
	haiku := estimateChatCost("claude-3-haiku-20240307", 1000, 500)
	opus := estimateChatCost("claude-3-opus-20240229", 1000, 500)

	// Sonnet: 1000*3 + 500*15 = 10_500 micros (allow ±1 for float64 rounding)
	if sonnet < 10_499 || sonnet > 10_500 {
		t.Errorf("sonnet = %d micros, want ~10_500", sonnet)
	}
	// Haiku is cheaper than Sonnet.
	if haiku >= sonnet {
		t.Errorf("haiku %d >= sonnet %d", haiku, sonnet)
	}
	// Opus is more expensive than Sonnet.
	if opus <= sonnet {
		t.Errorf("opus %d <= sonnet %d", opus, sonnet)
	}
}

func TestNewProvider_DefaultBaseURL(t *testing.T) {
	p := NewProvider(Config{APIKey: "k"}, ai.NewPromptRegistry(), nil)
	if p.cfg.BaseURL != "https://api.anthropic.com" {
		t.Errorf("BaseURL = %q", p.cfg.BaseURL)
	}
}
