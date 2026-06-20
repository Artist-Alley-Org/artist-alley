package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.Name() != "openai" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestProvider_SupportsVision(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if !p.SupportsVision() {
		t.Error("SupportsVision = false, want true")
	}
}

func TestProvider_Complete_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt-4o-mini" {
			t.Errorf("model in body = %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		DefaultCompletionModel: "gpt-4o-mini",
	}, ai.NewPromptRegistry(), nil)

	resp, err := p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello back" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 3 || resp.OutputTokens != 2 {
		t.Errorf("tokens = %+v", resp)
	}
	if resp.EstimatedCostUSDMicros == 0 {
		t.Error("cost estimate not computed")
	}
}

func TestProvider_Complete_429_ReturnsRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gpt-4o"}, ai.NewPromptRegistry(), nil)
	_, err := p.Complete(context.Background(), ai.CompletionRequest{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassRateLimit {
		t.Errorf("err = %v, want ErrClassRateLimit", pe)
	}
}

func TestProvider_Complete_400_ReturnsPermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gpt-4o"}, ai.NewPromptRegistry(), nil)
	_, err := p.Complete(context.Background(), ai.CompletionRequest{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("err = %v, want ErrClassPermanent", pe)
	}
}

func TestProvider_Embed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultEmbeddingModel: "text-embedding-3-large"}, ai.NewPromptRegistry(), nil)
	vec, err := p.Embed(context.Background(), ai.EmbedInput{Text: "hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vec = %v", vec)
	}
}

func TestProvider_Transcribe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// Multipart should carry the model + file field.
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("content-type = %q, want multipart/form-data", ct)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		// Drain so the server sees the full request.
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"text":"transcribed words","language":"en"}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultTranscriptionModel: "whisper-1"}, ai.NewPromptRegistry(), nil)
	tx, err := p.Transcribe(context.Background(), ai.AudioInput{
		Bytes: []byte{0x01, 0x02, 0x03}, MimeType: "audio/mp3",
	}, ai.TranscribeOpts{LanguageHint: "en"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tx.Text != "transcribed words" {
		t.Errorf("text = %q", tx.Text)
	}
	if tx.DetectedLanguage != "en" {
		t.Errorf("language = %q", tx.DetectedLanguage)
	}
}

func TestProvider_Transcribe_NoAudio_Errors(t *testing.T) {
	p := NewProvider(Config{APIKey: "k"}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(), ai.AudioInput{}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("err = %v, want permanent", pe)
	}
}

func TestProvider_Transcribe_URLOnly_NotSupportedInV1(t *testing.T) {
	p := NewProvider(Config{APIKey: "k"}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{URL: "http://x/audio.mp3"}, ai.TranscribeOpts{})
	pe, _ := ai.AsProviderError(err)
	if pe == nil || pe.Class != ai.ErrClassPermanent {
		t.Errorf("err = %v, want permanent", pe)
	}
}

// Tag + Caption delegate to ai.TagViaCompletion / CaptionViaCompletion.
// One smoke test each verifies the delegation works against a stubbed
// HTTP server (proves the provider implements TagProvider + CaptionProvider).

func TestProvider_Tag_DelegatesAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"sunset\nbeach\npalmtree"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gpt-4o-mini"},
		ai.NewPromptRegistry(), nil)
	tags, err := p.Tag(context.Background(),
		ai.AssetRef{ID: uuid.New(), PreviewURL: "http://x/p.jpg"},
		ai.TagOpts{MaxTags: 5})
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if len(tags) != 3 {
		t.Errorf("tags = %+v", tags)
	}
}

func TestProvider_Caption_DelegatesAndTrims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"  A vivid sunset over palms.  "}}],
			"usage":{"prompt_tokens":10,"completion_tokens":8}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gpt-4o-mini"},
		ai.NewPromptRegistry(), nil)
	caption, err := p.Caption(context.Background(),
		ai.AssetRef{ID: uuid.New(), PreviewURL: "http://x/p.jpg"},
		ai.CaptionOpts{})
	if err != nil {
		t.Fatalf("Caption: %v", err)
	}
	if caption != "A vivid sunset over palms." {
		t.Errorf("caption = %q", caption)
	}
}

func TestEstimateChatCost_KnownModelsProduceSensibleNumbers(t *testing.T) {
	// gpt-4o-mini: $0.15 in / $0.60 out per 1M tokens.
	// 1000 in + 500 out = (1000 × 0.15 + 500 × 0.60) / 1M = 0.00045 = 450 micros
	got := estimateChatCost("gpt-4o-mini", 1000, 500)
	if got != 450 {
		t.Errorf("gpt-4o-mini cost = %d micros, want 450", got)
	}

	// gpt-4o: $2.50 in / $10.00 out per 1M tokens.
	// 1000 in + 500 out = 0.0025 + 0.005 = 0.0075 = 7500 micros
	got = estimateChatCost("gpt-4o", 1000, 500)
	if got != 7500 {
		t.Errorf("gpt-4o cost = %d micros, want 7500", got)
	}

	// Unknown model falls back to gpt-4o-mini rates.
	got = estimateChatCost("some-future-model", 1000, 500)
	if got != 450 {
		t.Errorf("unknown model fallback = %d micros, want 450", got)
	}
}

func TestNewProvider_DefaultBaseURLAppliedWhenEmpty(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if !strings.HasPrefix(p.client.BaseURL, "https://api.openai.com") {
		t.Errorf("default BaseURL = %q", p.client.BaseURL)
	}
}

func TestNewProvider_RateLimiterConfigured(t *testing.T) {
	p := NewProvider(Config{
		RateLimitPerSecond: 10,
		RateLimitBurst:     5,
	}, ai.NewPromptRegistry(), nil)
	if p.client.Limiter == nil {
		t.Error("rate limiter not configured")
	}
}

func TestNewProvider_NoRateLimitWhenZero(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.client.Limiter != nil {
		t.Error("rate limiter set when not configured")
	}
}
