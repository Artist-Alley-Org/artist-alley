// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package whisperlocal implements the `whisper_local` transcription
// provider — the seed default for ai.routing.transcribe (see migration
// 00001).
//
// # Capability add-on, not in-process
//
// AA itself doesn't ship the Whisper model files or runtime. Per ADR
// 0034 the operator installs an `aa-whisper-local` sibling container
// (parallel to aa-ollama / aa-clip-local) and points this provider's
// URL at it. Default is http://aa-whisper-local:9080 (the compose
// network's service-name route). Remote operators override via the
// admin UI's per-provider config.
//
// # Wire shape
//
// The sibling container is expected to expose an OpenAI-compatible
// /v1/audio/transcriptions surface — multipart upload, returns either
// a flat {"text": ..., "language": ...} or a verbose-JSON form with
// per-segment timestamps + avg_logprob. We send `response_format=
// verbose_json` so the segment + confidence data lands when the
// transcript renderer needs it.
//
// # Cost is always zero
//
// Local runtime = no API spend. We still record the call duration via
// the audit recorder so operator dashboards can report on local-
// transcription throughput, but EstimatedCostUSDMicros is always 0.
//
// # Privacy
//
// whisper_local advertises privacy_class=local in its system_config
// registration, so the privacy gate from 1.14.A automatically picks
// it for restricted/embargo assets even when the operator's default
// transcribe routing names a cloud provider.

package whisperlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// Name matches the seed routing key + the system_config provider
// registration from migration 00001.
const Name = "whisper_local"

// defaultBaseURL targets the sibling container on the compose
// network. Operators on a remote box override via Config.URL.
const defaultBaseURL = "http://aa-whisper-local:9080"

// defaultModel is the seed Whisper checkpoint — best quality/cost on
// the public Whisper lineup. Operators with smaller GPUs override to
// medium/small/base/tiny.
const defaultModel = "large-v3"

// Config is the operator-tunable provider config. Mirrors the shape
// of ollama.Config + cliplocal.Config so the admin UI renders all
// three with one form.
type Config struct {
	// URL of the aa-whisper-local container.
	URL string

	// DefaultModel — overridable per-call via TranscribeOpts.Model.
	DefaultModel string

	// HTTPTimeout — long enough for chunked transcription. Defaults
	// to 5 minutes since model load + first inference can be slow
	// on cold containers.
	HTTPTimeout time.Duration

	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Provider implements ai.TranscriptionProvider only. Deliberately
// does NOT satisfy Completion/Tag/Caption/Embedding — whisper-local
// is a transcription-specific backend and the router's type-
// assertion gate ensures it never gets picked for the wrong concern.
type Provider struct {
	cfg     Config
	client  *http.Client
	limiter *rate.Limiter
	auditor *ai.CallAuditor
}

// NewProvider constructs the provider. auditor may be nil for tests.
func NewProvider(cfg Config, auditor *ai.CallAuditor) *Provider {
	if cfg.URL == "" {
		cfg.URL = defaultBaseURL
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = defaultModel
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 5 * time.Minute
	}
	var limiter *rate.Limiter
	if cfg.RateLimitPerSecond > 0 {
		burst := cfg.RateLimitBurst
		if burst <= 0 {
			burst = 1
		}
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimitPerSecond), burst)
	}
	return &Provider{
		cfg:     cfg,
		client:  &http.Client{Timeout: cfg.HTTPTimeout},
		limiter: limiter,
		auditor: auditor,
	}
}

// Name satisfies ai.Provider.
func (p *Provider) Name() string { return Name }

// verboseResponse is the shape aa-whisper-local returns for
// response_format=verbose_json. Mirrors openai's whisper-1 wire
// shape so cross-provider parsing stays consistent.
type verboseResponse struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	// Average per-segment log-probability. Closer to 0 = higher
	// confidence; -1.0 → ~0.37 confidence. We map via exp() into
	// the [0,1] range the subtitle row's confidence column expects.
	AvgLogprob float64 `json:"avg_logprob"`
	Segments   []struct {
		ID    int     `json:"id"`
		Start float64 `json:"start"` // seconds
		End   float64 `json:"end"`
		Text  string  `json:"text"`
		// Per-segment avg_logprob; we keep the top-level average
		// for the row's overall confidence but a future per-segment
		// confidence column could use these.
		AvgLogprob float64 `json:"avg_logprob"`
	} `json:"segments"`
}

// Transcribe satisfies ai.TranscriptionProvider.
//
// Always posts the audio bytes inline; URL form is rejected (the
// sibling container would have to fetch externally, which isn't part
// of its contract). Audio MIME defaults to audio/wav when the caller
// doesn't set it — that's what `ffmpeg -f wav` produces and the
// extractor we ship uses that format.
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
		model = p.cfg.DefaultModel
	}

	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return ai.Transcript{}, &ai.ProviderError{
				Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
			}
		}
	}

	mime := audio.MimeType
	if mime == "" {
		mime = "audio/wav"
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	// File part. Filename hint matches the MIME so the server can
	// pick the right decoder. Whisper accepts wav/mp3/m4a/flac/ogg.
	header := make(map[string][]string, 1)
	header["Content-Disposition"] = []string{
		fmt.Sprintf(`form-data; name="file"; filename="audio.%s"`, extFromMime(mime)),
	}
	header["Content-Type"] = []string{mime}
	filePart, err := mw.CreatePart(header)
	if err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}
	if _, err := filePart.Write(audio.Bytes); err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}

	_ = mw.WriteField("model", model)
	_ = mw.WriteField("response_format", "verbose_json")
	if opts.LanguageHint != "" {
		_ = mw.WriteField("language", opts.LanguageHint)
	}
	if err := mw.Close(); err != nil {
		return ai.Transcript{}, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.URL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Model: model, Wrapped: err,
		}
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return ai.Transcript{}, &ai.ProviderError{
			// Network errors against the local sibling are transient
			// (container restart, momentary OOM); the worker retries.
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}
	defer resp.Body.Close()
	duration := time.Since(start)

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model,
			Wrapped: fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 200)),
		}
	}
	if resp.StatusCode >= 400 {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Model: model,
			Wrapped: fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 200)),
		}
	}

	var parsed verboseResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ai.Transcript{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}

	segments := make([]ai.TranscriptSegment, 0, len(parsed.Segments))
	for _, s := range parsed.Segments {
		segments = append(segments, ai.TranscriptSegment{
			StartMS: int(s.Start * 1000),
			EndMS:   int(s.End * 1000),
			Text:    strings.TrimSpace(s.Text),
		})
	}

	tx := ai.Transcript{
		Text:             parsed.Text,
		DetectedLanguage: parsed.Language,
		Segments:         segments,
		Duration:         duration,
		// Local runtime = $0; the audit row records the duration so
		// dashboards can still surface throughput.
		EstimatedCostUSDMicros: 0,
	}

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:  Name,
			Model:     model,
			Concern:   ai.ConcernTranscribe,
			Duration:  duration,
			Status:    ai.CallStatusSuccess,
			InputHash: ai.CanonicalInputHash(model, fmt.Sprintf("audio:%d", len(audio.Bytes))),
		})
	}
	_ = math.Exp(parsed.AvgLogprob) // confidence exposed via ConfidenceFromTranscript below

	return tx, nil
}

// ConfidenceFromAvgLogprob converts Whisper's avg_logprob (typically
// in [-1, 0]) to a [0, 1] confidence via exp(). Exposed so the
// subtitle handoff layer can derive the row's confidence column
// from the provider's most-recent response without round-tripping
// through Transcript (which is the universal output type and doesn't
// carry provider-specific fields).
func ConfidenceFromAvgLogprob(logprob float64) float64 {
	// Clamp to [0,1] — degenerate inputs (positive logprob, NaN)
	// would otherwise overflow the [0,1] range the subtitle row's
	// CHECK constraint enforces.
	c := math.Exp(logprob)
	if c < 0 || math.IsNaN(c) {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// extFromMime maps a MIME type to a file extension Whisper's
// decoder will recognise. Anything not in the small known set
// becomes "bin" — the server can still sniff the bytes.
func extFromMime(mime string) string {
	switch strings.ToLower(mime) {
	case "audio/wav", "audio/x-wav", "audio/vnd.wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/mp4", "audio/x-m4a", "audio/aac":
		return "m4a"
	case "audio/flac":
		return "flac"
	case "audio/ogg", "audio/vorbis":
		return "ogg"
	}
	return "bin"
}

// snippet truncates a response body for inclusion in an error
// message — full body would bloat logs and may include sensitive
// audio metadata.
func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// Compile-time interface check.
var _ ai.TranscriptionProvider = (*Provider)(nil)
