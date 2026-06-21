package gemini

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

func TestTranscribe_HappyPath_ReturnsText(t *testing.T) {
	var capturedURL, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Parts: []geminiPart{{Text: "this is the transcript"}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: geminiUsage{PromptTokenCount: 12, CandidatesTokenCount: 4},
		})
	}))
	defer srv.Close()

	p := NewProvider(Config{
		APIKey:                 "fake-key",
		BaseURL:                srv.URL,
		DefaultCompletionModel: "gemini-1.5-pro",
	}, ai.NewPromptRegistry(), nil)
	tx, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("wavbytes"), MimeType: "audio/wav"},
		ai.TranscribeOpts{LanguageHint: "en"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tx.Text != "this is the transcript" {
		t.Errorf("text = %q", tx.Text)
	}
	if tx.DetectedLanguage != "en" {
		t.Errorf("detected lang = %q, want en (LanguageHint echoed)", tx.DetectedLanguage)
	}
	if len(tx.Segments) != 0 {
		t.Errorf("gemini transcribe should not emit segments in v1; got %d", len(tx.Segments))
	}
	if !strings.Contains(capturedURL, "gemini-2.5-flash") {
		t.Errorf("URL didn't use default transcribe model: %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "key=fake-key") {
		t.Errorf("URL missing API key: %s", capturedURL)
	}
	// Audio bytes were base64'd into the inline_data.data field.
	if !strings.Contains(capturedBody, "d2F2Ynl0ZXM=") { // base64("wavbytes")
		t.Errorf("request body missing base64-encoded audio: %s", capturedBody[:200])
	}
}

func TestTranscribe_NoAudio_Permanent(t *testing.T) {
	p := NewProvider(Config{APIKey: "fake"}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(), ai.AudioInput{}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent provider error", err)
	}
}

func TestTranscribe_URLOnly_Permanent(t *testing.T) {
	p := NewProvider(Config{APIKey: "fake"}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{URL: "http://example.com/audio.mp3"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent provider error", err)
	}
}

func TestTranscribe_NoCandidates_Permanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(geminiResponse{Candidates: nil})
	}))
	defer srv.Close()
	p := NewProvider(Config{APIKey: "fake", BaseURL: srv.URL}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("audio"), MimeType: "audio/wav"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent (empty candidates)", err)
	}
}

func TestTranscribe_429_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()
	p := NewProvider(Config{APIKey: "fake", BaseURL: srv.URL}, ai.NewPromptRegistry(), nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("audio"), MimeType: "audio/wav"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok {
		t.Fatalf("got %v, want provider error", err)
	}
	// openaicompat.ClassifyHTTPError maps 429 to ErrClassRateLimit
	// (which the router treats as transient for retry purposes).
	if pe.Class != ai.ErrClassRateLimit && pe.Class != ai.ErrClassTransient {
		t.Errorf("got class %v, want rate-limit/transient", pe.Class)
	}
}
