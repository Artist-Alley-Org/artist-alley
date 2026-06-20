package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestProvider_Name(t *testing.T) {
	p := NewProvider(Config{}, ai.NewPromptRegistry(), nil)
	if p.Name() != "gemini" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestProvider_Complete_PassesAPIKeyAsQueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gemini takes the API key as ?key=...
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("key query = %q", r.URL.Query().Get("key"))
		}
		if !strings.Contains(r.URL.Path, "generateContent") {
			t.Errorf("path = %q, want :generateContent suffix", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"candidates":[{"content":{"parts":[{"text":"gemini says hi"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4}
		}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		DefaultCompletionModel: "gemini-1.5-flash",
	}, ai.NewPromptRegistry(), nil)

	resp, err := p.Complete(context.Background(), ai.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "gemini says hi" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 3 || resp.OutputTokens != 4 {
		t.Errorf("tokens drift: %+v", resp)
	}
}

func TestProvider_Complete_SystemMessageGoesToSystemInstruction(t *testing.T) {
	gotBody := []byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gemini-1.5-flash"}, ai.NewPromptRegistry(), nil)
	_, _ = p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "system", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "you are helpful"}}},
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "x"}}},
		},
	})
	body := string(gotBody)
	if !strings.Contains(body, `"systemInstruction":`) {
		t.Errorf("systemInstruction missing: %s", body)
	}
	if !strings.Contains(body, "you are helpful") {
		t.Errorf("system body missing: %s", body)
	}
}

func TestProvider_Complete_AssistantRoleMappedToModel(t *testing.T) {
	gotBody := []byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gemini-1.5-flash"}, ai.NewPromptRegistry(), nil)
	_, _ = p.Complete(context.Background(), ai.CompletionRequest{
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "x"}}},
			{Role: "assistant", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "y"}}},
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "z"}}},
		},
	})
	body := string(gotBody)
	if !strings.Contains(body, `"role":"model"`) {
		t.Errorf("assistant role not mapped to 'model': %s", body)
	}
	if strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("assistant role leaked into wire: %s", body)
	}
}

func TestProvider_Embed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "embedContent") {
			t.Errorf("path = %q, want :embedContent", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1, 0.2, 0.3]}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                "k",
		BaseURL:               srv.URL,
		DefaultEmbeddingModel: "text-embedding-004",
	}, ai.NewPromptRegistry(), nil)
	vec, err := p.Embed(context.Background(), ai.EmbedInput{Text: "hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("vec = %v", vec)
	}
}

func TestProvider_Complete_4xx_ReturnsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{APIKey: "k", BaseURL: srv.URL, DefaultCompletionModel: "gemini-1.5-flash"}, ai.NewPromptRegistry(), nil)
	_, err := p.Complete(context.Background(), ai.CompletionRequest{})
	pe, _ := ai.AsProviderError(err)
	if pe == nil || pe.Class != ai.ErrClassPermanent {
		t.Errorf("err = %v, want permanent", pe)
	}
}

func TestEstimateChatCost_DifferentiatesProAndFlash(t *testing.T) {
	pro := estimateChatCost("gemini-1.5-pro-latest", 1000, 500)
	flash := estimateChatCost("gemini-1.5-flash-latest", 1000, 500)
	if pro <= flash {
		t.Errorf("pro %d should cost more than flash %d", pro, flash)
	}
}
