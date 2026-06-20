package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.Name() != "ollama" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestProvider_Complete_HappyPath_NoAuthHeaderSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ollama doesn't take auth — verify we don't send one.
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Ollama call sent Authorization header: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"local llama says hi"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":4}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		BaseURL:                srv.URL,
		DefaultCompletionModel: "llama3.1:8b",
	}, ai.NewPromptRegistry(), nil)

	resp, err := p.Complete(context.Background(), ai.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "local llama says hi" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.EstimatedCostUSDMicros != 0 {
		t.Errorf("Ollama call recorded cost = %d, want 0 (local + free)", resp.EstimatedCostUSDMicros)
	}
}

func TestProvider_Embed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5,0.6,0.7]}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		BaseURL:               srv.URL,
		DefaultEmbeddingModel: "nomic-embed-text",
	}, ai.NewPromptRegistry(), nil)

	vec, err := p.Embed(context.Background(), ai.EmbedInput{Text: "hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("vec len = %d", len(vec))
	}
}

func TestProvider_503_ReturnsTransient(t *testing.T) {
	// Ollama down → transient → router walks fallback chain.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	p := NewProvider(Config{BaseURL: srv.URL, DefaultCompletionModel: "llama3.1:8b"}, ai.NewPromptRegistry(), nil)
	_, err := p.Complete(context.Background(), ai.CompletionRequest{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassTransient {
		t.Errorf("err = %v, want transient", pe)
	}
}

func TestNewProvider_DefaultBaseURL(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.client.BaseURL != "http://localhost:11434" {
		t.Errorf("BaseURL = %q", p.client.BaseURL)
	}
}
