package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// MarshalCompletion shapes the JSON the way OpenAI's API expects.
func TestMarshalCompletion_TextOnly_Roundtrips(t *testing.T) {
	req := ai.CompletionRequest{
		Model:       "gpt-4o",
		Temperature: 0.2,
		MaxTokens:   100,
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "hi"}}},
		},
	}
	body, err := MarshalCompletion(req, "fallback-model")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"` // text-only collapses to string
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", got.Model)
	}
	if got.Temperature != 0.2 || got.MaxTokens != 100 {
		t.Errorf("knobs drift: %+v", got)
	}
	if got.Messages[0].Content != "hi" {
		t.Errorf("text-only message should collapse to string, got %q", got.Messages[0].Content)
	}
}

func TestMarshalCompletion_EmptyModel_UsesDefault(t *testing.T) {
	req := ai.CompletionRequest{
		Messages: []ai.Message{{Role: "user", Content: []ai.Content{{Type: ai.ContentTypeText, Text: "x"}}}},
	}
	body, _ := MarshalCompletion(req, "gpt-4o-mini")
	if !bytesContain(body, []byte(`"model":"gpt-4o-mini"`)) {
		t.Errorf("default model not used: %s", body)
	}
}

func TestMarshalCompletion_VisionMessage_UsesPartsArray(t *testing.T) {
	req := ai.CompletionRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{
				{Type: ai.ContentTypeText, Text: "describe"},
				{Type: ai.ContentTypeImageURL, ImageURL: "http://x/a.png"},
			}},
		},
	}
	body, err := MarshalCompletion(req, "")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Multi-part content should marshal as an array, not a string.
	if !bytesContain(body, []byte(`"type":"image_url"`)) {
		t.Errorf("image_url part missing: %s", body)
	}
	if !bytesContain(body, []byte(`"url":"http://x/a.png"`)) {
		t.Errorf("image url missing: %s", body)
	}
}

func TestMarshalCompletion_ImageB64_EncodesAsDataURL(t *testing.T) {
	req := ai.CompletionRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Content{
				{Type: ai.ContentTypeImageB64, ImageBytes: []byte{0xff, 0xd8, 0xff}, MimeType: "image/jpeg"},
			}},
		},
	}
	body, _ := MarshalCompletion(req, "")
	if !bytesContain(body, []byte(`"url":"data:image/jpeg;base64,`)) {
		t.Errorf("expected data URL prefix; got %s", body)
	}
}

func TestParseCompletion_ExtractsTextAndUsage(t *testing.T) {
	raw := []byte(`{
		"choices": [{"message": {"role": "assistant", "content": "the answer"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19}
	}`)
	resp, err := ParseCompletion(raw, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "the answer" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Errorf("tokens = %+v", resp)
	}
	if resp.Duration != 500*time.Millisecond {
		t.Errorf("duration didn't survive: %v", resp.Duration)
	}
}

func TestParseCompletion_NoChoices_Errors(t *testing.T) {
	if _, err := ParseCompletion([]byte(`{"choices":[]}`), 0); err == nil {
		t.Error("expected error on empty choices")
	}
}

func TestClassifyHTTPError_429_ReturnsRateLimitWithRetryAfter(t *testing.T) {
	err := ClassifyHTTPError(429, "5", "openai", "gpt-4o")
	pe, ok := ai.AsProviderError(err)
	if !ok {
		t.Fatalf("err type = %T, want *ProviderError", err)
	}
	if pe.Class != ai.ErrClassRateLimit {
		t.Errorf("class = %v", pe.Class)
	}
	if pe.RetryAfter != 5*time.Second {
		t.Errorf("retry-after = %v, want 5s", pe.RetryAfter)
	}
}

func TestClassifyHTTPError_5xx_ReturnsTransient(t *testing.T) {
	err := ClassifyHTTPError(503, "", "ollama", "llama3")
	pe, _ := ai.AsProviderError(err)
	if pe.Class != ai.ErrClassTransient {
		t.Errorf("class = %v, want ErrClassTransient", pe.Class)
	}
}

func TestClassifyHTTPError_4xxNon429_ReturnsPermanent(t *testing.T) {
	err := ClassifyHTTPError(400, "", "openai", "gpt-4o")
	pe, _ := ai.AsProviderError(err)
	if pe.Class != ai.ErrClassPermanent {
		t.Errorf("class = %v, want ErrClassPermanent", pe.Class)
	}
}

func TestClassifyHTTPError_2xx_ReturnsNil(t *testing.T) {
	if err := ClassifyHTTPError(200, "", "openai", "gpt-4o"); err != nil {
		t.Errorf("err on 200: %v", err)
	}
}

func TestClassifyTransportError_WrapsAsTransient(t *testing.T) {
	err := ClassifyTransportError(errors.New("connection refused"), "openai", "gpt-4o")
	pe, ok := ai.AsProviderError(err)
	if !ok {
		t.Fatal("not a ProviderError")
	}
	if pe.Class != ai.ErrClassTransient {
		t.Errorf("class = %v", pe.Class)
	}
}

func TestPostJSON_HappyPath_2xx_ReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"echoed":` + string(body) + `}`))
	}))
	defer srv.Close()

	c := NewClient("openai", srv.URL, "key123", "", "gpt-4o", nil)
	respBody, err := c.PostJSON(context.Background(), "/v1/chat/completions", []byte(`{"hi":1}`), "gpt-4o")
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if !strings.Contains(string(respBody), `"echoed":{"hi":1}`) {
		t.Errorf("body = %s", respBody)
	}
}

func TestPostJSON_SendsAuthHeaderWhenAPIKeySet(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient("openai", srv.URL, "sk-test", "", "", nil)
	_, _ = c.PostJSON(context.Background(), "/v1/chat/completions", []byte(`{}`), "")
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization header = %q, want \"Bearer sk-test\"", gotAuth)
	}
}

func TestPostJSON_SkipsAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	// Ollama + vLLM call without auth — empty APIKey means no header.
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient("ollama", srv.URL, "", "", "", nil)
	_, _ = c.PostJSON(context.Background(), "/api/chat", []byte(`{}`), "")
	if gotAuth != "" {
		t.Errorf("Authorization header sent for Ollama: %q", gotAuth)
	}
}

func TestPostJSON_429_ReturnsRateLimitErrorWithBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	c := NewClient("openai", srv.URL, "k", "", "", nil)
	_, err := c.PostJSON(context.Background(), "/v1/chat/completions", []byte(`{}`), "gpt-4o")
	pe, ok := ai.AsProviderError(err)
	if !ok {
		t.Fatalf("not a ProviderError: %v", err)
	}
	if pe.Class != ai.ErrClassRateLimit {
		t.Errorf("class = %v", pe.Class)
	}
	if pe.RetryAfter != 10*time.Second {
		t.Errorf("retry-after = %v", pe.RetryAfter)
	}
	if !strings.Contains(err.Error(), "slow down") {
		t.Errorf("err body snippet missing: %v", err)
	}
}

func TestPostJSON_400_ReturnsPermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := NewClient("openai", srv.URL, "k", "", "", nil)
	_, err := c.PostJSON(context.Background(), "/v1/chat/completions", []byte(`{}`), "gpt-4o")
	pe, _ := ai.AsProviderError(err)
	if pe == nil || pe.Class != ai.ErrClassPermanent {
		t.Errorf("class = %v, want ErrClassPermanent", pe)
	}
}

func TestPostJSON_502_ReturnsTransientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()

	c := NewClient("openai", srv.URL, "k", "", "", nil)
	_, err := c.PostJSON(context.Background(), "/v1/chat/completions", []byte(`{}`), "gpt-4o")
	pe, _ := ai.AsProviderError(err)
	if pe == nil || pe.Class != ai.ErrClassTransient {
		t.Errorf("class = %v, want ErrClassTransient", pe)
	}
}

func TestMarshalEmbedding_TextInputRoundtrips(t *testing.T) {
	in := ai.EmbedInput{Model: "text-embedding-3-large", Text: "hello"}
	body, err := MarshalEmbedding(in, "fallback")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	_ = json.Unmarshal(body, &got)
	if got.Model != "text-embedding-3-large" || got.Input != "hello" {
		t.Errorf("got = %+v", got)
	}
}

func TestMarshalEmbedding_NoText_Errors(t *testing.T) {
	if _, err := MarshalEmbedding(ai.EmbedInput{Model: "x"}, ""); err == nil {
		t.Error("expected error on empty text")
	}
}

func TestParseEmbedding_ReturnsFirstVector(t *testing.T) {
	raw := []byte(`{"data":[{"embedding":[0.1, 0.2, 0.3]}]}`)
	vec, err := ParseEmbedding(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vec = %v", vec)
	}
}

func TestParseEmbedding_EmptyData_Errors(t *testing.T) {
	if _, err := ParseEmbedding([]byte(`{"data":[]}`)); err == nil {
		t.Error("expected error on empty data")
	}
}

func TestParseRetryAfter_AcceptsSeconds(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Errorf("got %v", got)
	}
}

func TestParseRetryAfter_AcceptsHTTPDate(t *testing.T) {
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	// Allow 5s slack for the test clock.
	if got < 35*time.Second || got > 50*time.Second {
		t.Errorf("got %v, want ~45s", got)
	}
}

func TestParseRetryAfter_GibberishReturnsZero(t *testing.T) {
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func bytesContain(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
