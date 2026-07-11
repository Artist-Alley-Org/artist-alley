// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package whisperlocal

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

func TestProvider_Name_IsWhisperLocal(t *testing.T) {
	p := NewProvider(Config{}, nil)
	if p.Name() != "whisper_local" {
		t.Errorf("Name() = %q, want whisper_local", p.Name())
	}
}

// Provider implements ai.TranscriptionProvider only — never picked
// for chat/tag/caption/embed concerns even if the operator misroutes.
func TestProvider_DoesNotSatisfyOtherConcerns(t *testing.T) {
	var p ai.Provider = NewProvider(Config{}, nil)
	if _, ok := p.(ai.CompletionProvider); ok {
		t.Error("whisper_local must NOT satisfy CompletionProvider")
	}
	if _, ok := p.(ai.EmbeddingProvider); ok {
		t.Error("whisper_local must NOT satisfy EmbeddingProvider")
	}
	if _, ok := p.(ai.TagProvider); ok {
		t.Error("whisper_local must NOT satisfy TagProvider")
	}
	if _, ok := p.(ai.CaptionProvider); ok {
		t.Error("whisper_local must NOT satisfy CaptionProvider")
	}
}

func TestProvider_Transcribe_VerboseJSON_RoundTrip(t *testing.T) {
	var capturedPath, capturedContentType, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":        "hello world",
			"language":    "en",
			"avg_logprob": -0.2,
			"segments": []map[string]any{
				{"id": 0, "start": 0.0, "end": 1.5, "text": "hello"},
				{"id": 1, "start": 1.5, "end": 3.0, "text": "world", "avg_logprob": -0.1},
			},
		})
	}))
	defer srv.Close()

	p := NewProvider(Config{URL: srv.URL, DefaultModel: "large-v3"}, nil)
	tx, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("fake-wav-bytes"), MimeType: "audio/wav"},
		ai.TranscribeOpts{LanguageHint: "en"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if capturedPath != "/v1/audio/transcriptions" {
		t.Errorf("path = %q, want /v1/audio/transcriptions", capturedPath)
	}
	if !strings.HasPrefix(capturedContentType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart/form-data", capturedContentType)
	}
	for _, want := range []string{"fake-wav-bytes", "large-v3", "verbose_json", `name="language"`} {
		if !strings.Contains(capturedBody, want) {
			t.Errorf("request body missing %q", want)
		}
	}
	if tx.Text != "hello world" {
		t.Errorf("text = %q", tx.Text)
	}
	if tx.DetectedLanguage != "en" {
		t.Errorf("detected lang = %q", tx.DetectedLanguage)
	}
	if len(tx.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(tx.Segments))
	}
	if tx.Segments[0].StartMS != 0 || tx.Segments[0].EndMS != 1500 {
		t.Errorf("segment 0 = (%d, %d), want (0, 1500)", tx.Segments[0].StartMS, tx.Segments[0].EndMS)
	}
	if tx.Segments[1].Text != "world" {
		t.Errorf("segment 1 text = %q", tx.Segments[1].Text)
	}
	if tx.EstimatedCostUSDMicros != 0 {
		t.Errorf("local provider recorded cost = %d, want 0", tx.EstimatedCostUSDMicros)
	}
}

func TestProvider_Transcribe_NoAudio_ReturnsPermanent(t *testing.T) {
	p := NewProvider(Config{}, nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent provider error", err)
	}
}

func TestProvider_Transcribe_URLOnly_ReturnsPermanent(t *testing.T) {
	p := NewProvider(Config{}, nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{URL: "http://example.com/audio.wav"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent provider error (URL form unsupported)", err)
	}
}

func TestProvider_Transcribe_503_ClassifiesAsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("model warming up"))
	}))
	defer srv.Close()
	p := NewProvider(Config{URL: srv.URL}, nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("audio"), MimeType: "audio/wav"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassTransient {
		t.Errorf("got %v, want transient", err)
	}
}

func TestProvider_Transcribe_400_ClassifiesAsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid model"))
	}))
	defer srv.Close()
	p := NewProvider(Config{URL: srv.URL}, nil)
	_, err := p.Transcribe(context.Background(),
		ai.AudioInput{Bytes: []byte("audio"), MimeType: "audio/wav"}, ai.TranscribeOpts{})
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent", err)
	}
}

func TestConfidenceFromAvgLogprob(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
		note string
	}{
		{0.0, 1.0, "logprob 0 = certain"},
		{-1.0, 0.367879, "logprob -1 ≈ 0.37"},
		{-10.0, 0.0000454, "very low confidence"},
	}
	for _, c := range cases {
		got := ConfidenceFromAvgLogprob(c.in)
		if abs(got-c.want) > 1e-4 {
			t.Errorf("ConfidenceFromAvgLogprob(%v) = %v, want ~%v (%s)", c.in, got, c.want, c.note)
		}
	}
}

func TestConfidenceFromAvgLogprob_ClampsOutOfRange(t *testing.T) {
	// Pathological inputs (positive logprob — wouldn't happen for
	// real Whisper but the constraint enforcer doesn't care).
	if got := ConfidenceFromAvgLogprob(5.0); got != 1.0 {
		t.Errorf("positive logprob should clamp to 1.0; got %v", got)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
