// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.C — Gemini transcription.
//
// Gemini handles audio natively as a `inlineData` content part with a
// transcription prompt. There's no dedicated audio-transcription
// endpoint; we ride the same generateContent surface as
// completion, asking the model to emit a transcript.
//
// # Wire shape
//
// Reuses the existing geminiRequest/Response types from provider.go.
// Audio is base64-encoded in a single inlineData part; prompt asks
// for a plain transcript. We don't request segment timestamps in v1
// — Gemini doesn't reliably emit them in a parseable form across
// model versions, so the Transcript returned here has Text +
// DetectedLanguage but Segments stays empty.
//
// The transcribe handler in app/internal/ai/transcribe synthesises
// segment timecodes from the chunker for cross-provider parity —
// providers that emit segments (whisper_local) use those; providers
// that don't (gemini today) get chunk-boundary segments from the
// handler so the WebVTT output still has timestamps.

package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

// transcribeDefaultModel matches the system_config seed from
// migration 00001. Operators override via Config.DefaultTranscriptionModel
// or TranscribeOpts.Model.
const transcribeDefaultModel = "gemini-2.5-flash"

// transcribePromptTemplate instructs Gemini to emit a clean
// transcript. The "no commentary" + "no markdown" guards keep the
// response parseable (we use the raw text as-is). Language hint, when
// supplied, switches Gemini's output language explicitly instead of
// relying on auto-detection.
const transcribePromptTemplate = `Transcribe the audio. Output ONLY the transcript text.
No commentary, no markdown, no labels.
%s`

// Transcribe satisfies ai.TranscriptionProvider.
func (p *Provider) Transcribe(ctx context.Context, audio ai.AudioInput, opts ai.TranscribeOpts) (ai.Transcript, error) {
	if len(audio.Bytes) == 0 && audio.URL == "" {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name,
			Wrapped: errors.New("transcribe: no audio bytes or URL"),
		}
	}
	if len(audio.Bytes) == 0 {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name,
			Wrapped: errors.New("transcribe: URL form not supported; pass Bytes"),
		}
	}

	model := opts.Model
	if model == "" {
		model = transcribeDefaultModel
	}

	mime := audio.MimeType
	if mime == "" {
		mime = "audio/wav"
	}

	// Prompt — language hint when supplied; otherwise let Gemini
	// auto-detect (it returns language in the text occasionally; we
	// surface what the caller passed, or empty otherwise).
	langDirective := ""
	if opts.LanguageHint != "" {
		langDirective = fmt.Sprintf("Source language: %s.", opts.LanguageHint)
	}
	prompt := fmt.Sprintf(transcribePromptTemplate, langDirective)

	req := geminiRequest{
		Contents: []geminiContent{{
			Role: "user",
			Parts: []geminiPart{
				{Text: prompt},
				{InlineData: &geminiInlineData{
					MimeType: mime,
					Data:     base64.StdEncoding.EncodeToString(audio.Bytes),
				}},
			},
		}},
	}
	wire, err := json.Marshal(req)
	if err != nil {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err,
		}
	}

	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return ai.Transcript{}, openaicompat.ClassifyTransportError(err, Name, model)
		}
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s",
		p.cfg.BaseURL, model, p.cfg.APIKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
	if err != nil {
		return ai.Transcript{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ai.Transcript{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	if resp.StatusCode >= 400 {
		classed := openaicompat.ClassifyHTTPError(resp.StatusCode,
			resp.Header.Get("Retry-After"), Name, model)
		var pe *ai.ProviderError
		if errors.As(classed, &pe) && len(body) > 0 {
			snip := string(body)
			if len(snip) > 512 {
				snip = snip[:512] + "..."
			}
			pe.Wrapped = fmt.Errorf("%w: %s", pe.Wrapped, snip)
		}
		return ai.Transcript{}, classed
	}

	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Model: model,
			Wrapped: errors.New("transcribe: no candidate in response"),
		}
	}

	// Concatenate every text part (Gemini sometimes splits long
	// outputs across multiple parts).
	var text strings.Builder
	for _, part := range parsed.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}
	transcript := strings.TrimSpace(text.String())

	tx := ai.Transcript{
		Text:             transcript,
		DetectedLanguage: opts.LanguageHint, // Gemini doesn't echo a detected-language field
		Segments:         nil,               // chunker layer synthesises chunk-boundary segments
		Duration:         duration,
	}

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:     Name,
			Model:        model,
			Concern:      ai.ConcernTranscribe,
			InputTokens:  parsed.UsageMetadata.PromptTokenCount,
			OutputTokens: parsed.UsageMetadata.CandidatesTokenCount,
			Duration:     duration,
			Status:       ai.CallStatusSuccess,
			InputHash:    ai.CanonicalInputHash(model, fmt.Sprintf("audio:%d", len(audio.Bytes))),
		})
	}

	return tx, nil
}
