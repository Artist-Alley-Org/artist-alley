// Package gemini implements the Google Gemini provider. Gemini's
// generateContent API has its own request/response shape (different
// from both OpenAI and Anthropic); this package has a thin shim per
// the brief (~150 LOC).
//
// Phase 1.14.A scope: CompletionProvider + EmbeddingProvider +
// TagProvider + CaptionProvider. Transcription via Gemini is
// supported but deferred to 1.14.C alongside whisper-local.
//
// Wire reference: https://ai.google.dev/api/generate-content
//   POST https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?key={api_key}
//   Body: {contents: [{role, parts: [{text} | {inline_data}]}], generationConfig: {...}}
//   Response: {candidates: [{content: {parts: [{text}]}, finishReason}], usageMetadata: {...}}
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

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/ai/providers/openaicompat"
)

const Name = "gemini"

const defaultBaseURL = "https://generativelanguage.googleapis.com"

// Config is the operator-tunable Gemini provider config.
type Config struct {
	APIKey  string
	BaseURL string // defaults to generativelanguage.googleapis.com

	DefaultCompletionModel string // e.g. "gemini-1.5-pro-latest"
	DefaultEmbeddingModel  string // e.g. "text-embedding-004"

	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Provider implements ai.CompletionProvider + EmbeddingProvider +
// TagProvider + CaptionProvider.
type Provider struct {
	cfg        Config
	httpClient *http.Client
	limiter    *rate.Limiter
	prompts    *ai.PromptRegistry
	auditor    *ai.CallAuditor
}

func NewProvider(cfg Config, prompts *ai.PromptRegistry, auditor *ai.CallAuditor) *Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	var limiter *rate.Limiter
	if cfg.RateLimitPerSecond > 0 {
		burst := cfg.RateLimitBurst
		if burst <= 0 {
			burst = 1
		}
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimitPerSecond), burst)
	}
	return &Provider{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		limiter:    limiter,
		prompts:    prompts,
		auditor:    auditor,
	}
}

func (p *Provider) Name() string         { return Name }
func (p *Provider) SupportsVision() bool { return true }

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type geminiRequest struct {
	Contents          []geminiContent     `json:"contents"`
	SystemInstruction *geminiContent      `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig    `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []geminiPart  `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
	FileData   *geminiFileData   `json:"fileData,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type geminiFileData struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiGenConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
}

type geminiEmbedRequest struct {
	Content geminiContent `json:"content"`
}

type geminiEmbedResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// ---------------------------------------------------------------------------
// CompletionProvider
// ---------------------------------------------------------------------------

func (p *Provider) Complete(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultCompletionModel
	}
	wire, err := marshalToGemini(req)
	if err != nil {
		return ai.CompletionResponse{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err,
		}
	}

	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return ai.CompletionResponse{}, openaicompat.ClassifyTransportError(err, Name, model)
		}
	}

	// Gemini's API takes the key as a query string (?key=...).
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s",
		p.cfg.BaseURL, model, p.cfg.APIKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
	if err != nil {
		return ai.CompletionResponse{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ai.CompletionResponse{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	if resp.StatusCode >= 400 {
		classed := openaicompat.ClassifyHTTPError(resp.StatusCode, resp.Header.Get("Retry-After"), Name, model)
		var pe *ai.ProviderError
		if errors.As(classed, &pe) && len(body) > 0 {
			snippet := string(body)
			if len(snippet) > 512 {
				snippet = snippet[:512] + "..."
			}
			pe.Wrapped = fmt.Errorf("%w: %s", pe.Wrapped, snippet)
		}
		return ai.CompletionResponse{}, classed
	}

	parsed, err := parseGeminiResponse(body, duration)
	if err != nil {
		return ai.CompletionResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err,
		}
	}
	parsed.EstimatedCostUSDMicros = estimateChatCost(model, parsed.InputTokens, parsed.OutputTokens)

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider:               Name,
			Model:                  model,
			Concern:                ai.ConcernComplete,
			PromptVersion:          req.PromptVersion,
			AssetID:                req.AssetID,
			InputTokens:            parsed.InputTokens,
			OutputTokens:           parsed.OutputTokens,
			Duration:               parsed.Duration,
			EstimatedCostUSDMicros: parsed.EstimatedCostUSDMicros,
			Status:                 ai.CallStatusSuccess,
		})
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// EmbeddingProvider
// ---------------------------------------------------------------------------

func (p *Provider) Embed(ctx context.Context, in ai.EmbedInput) ([]float32, error) {
	model := in.Model
	if model == "" {
		model = p.cfg.DefaultEmbeddingModel
	}
	if in.Text == "" {
		return nil, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name,
			Wrapped: errors.New("embed: text input required")}
	}
	body, err := json.Marshal(geminiEmbedRequest{
		Content: geminiContent{Parts: []geminiPart{{Text: in.Text}}},
	})
	if err != nil {
		return nil, &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: Name, Wrapped: err}
	}

	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return nil, openaicompat.ClassifyTransportError(err, Name, model)
		}
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:embedContent?key=%s",
		p.cfg.BaseURL, model, p.cfg.APIKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, openaicompat.ClassifyTransportError(err, Name, model)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, openaicompat.ClassifyTransportError(err, Name, model)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	if resp.StatusCode >= 400 {
		return nil, openaicompat.ClassifyHTTPError(resp.StatusCode, resp.Header.Get("Retry-After"), Name, model)
	}

	var wire geminiEmbedResponse
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return nil, &ai.ProviderError{Class: ai.ErrClassTransient, Provider: Name, Model: model, Wrapped: err}
	}
	if len(wire.Embedding.Values) == 0 {
		return nil, &ai.ProviderError{Class: ai.ErrClassTransient, Provider: Name, Model: model,
			Wrapped: errors.New("gemini: empty embedding values")}
	}

	if p.auditor != nil {
		p.auditor.RecordCall(ctx, ai.CallRecord{
			Provider: Name, Model: model, Concern: ai.ConcernEmbed,
			Duration: duration, Status: ai.CallStatusSuccess,
		})
	}
	return wire.Embedding.Values, nil
}

// ---------------------------------------------------------------------------
// Tag + Caption — delegate
// ---------------------------------------------------------------------------

func (p *Provider) Tag(ctx context.Context, asset ai.AssetRef, opts ai.TagOpts) ([]ai.Tag, error) {
	return ai.TagViaCompletion(ctx, p, p.prompts, asset, opts)
}

func (p *Provider) Caption(ctx context.Context, asset ai.AssetRef, opts ai.CaptionOpts) (string, error) {
	return ai.CaptionViaCompletion(ctx, p, p.prompts, asset, opts)
}

// ---------------------------------------------------------------------------
// Wire marshaling
// ---------------------------------------------------------------------------

func marshalToGemini(req ai.CompletionRequest) ([]byte, error) {
	// Gemini separates systemInstruction from contents. Collapse any
	// system-role messages into a single systemInstruction; rest go
	// into contents with role "user" or "model".
	var systemParts []string
	var contents []geminiContent

	for _, m := range req.Messages {
		role := m.Role
		if role == "assistant" {
			role = "model" // Gemini uses "model" for the assistant's turn
		}

		parts := make([]geminiPart, 0, len(m.Content))
		for _, c := range m.Content {
			switch c.Type {
			case ai.ContentTypeText:
				if m.Role == "system" {
					systemParts = append(systemParts, c.Text)
					continue
				}
				parts = append(parts, geminiPart{Text: c.Text})
			case ai.ContentTypeImageURL:
				// Gemini's File API would let us pass a URL via FileData,
				// but it requires the URI be a gs:// Cloud Storage path
				// for it to fetch. For arbitrary HTTP URLs, the safe
				// bet for v1 is to leave a text placeholder; the
				// operator wires a CDN that serves Cloud-Storage-
				// backed URLs if they need this path.
				parts = append(parts, geminiPart{
					FileData: &geminiFileData{
						MimeType: c.MimeType,
						FileURI:  c.ImageURL,
					},
				})
			case ai.ContentTypeImageB64:
				parts = append(parts, geminiPart{
					InlineData: &geminiInlineData{
						MimeType: c.MimeType,
						Data:     base64.StdEncoding.EncodeToString(c.ImageBytes),
					},
				})
			}
		}
		if m.Role == "system" {
			// All parts collapsed into systemParts; no content row to add.
			continue
		}
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}

	body := geminiRequest{
		Contents: contents,
	}
	if len(systemParts) > 0 {
		body.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}},
		}
	}
	if req.Temperature > 0 || req.MaxTokens > 0 {
		body.GenerationConfig = &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		}
	}
	return json.Marshal(body)
}

func parseGeminiResponse(body []byte, duration time.Duration) (ai.CompletionResponse, error) {
	var wire geminiResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return ai.CompletionResponse{}, fmt.Errorf("parse gemini response: %w", err)
	}
	if len(wire.Candidates) == 0 {
		return ai.CompletionResponse{}, errors.New("gemini: no candidates in response")
	}
	var sb strings.Builder
	for _, part := range wire.Candidates[0].Content.Parts {
		sb.WriteString(part.Text)
	}
	return ai.CompletionResponse{
		Text:         sb.String(),
		InputTokens:  wire.UsageMetadata.PromptTokenCount,
		OutputTokens: wire.UsageMetadata.CandidatesTokenCount,
		FinishReason: wire.Candidates[0].FinishReason,
		Duration:     duration,
	}, nil
}

// estimateChatCost — Gemini pricing as of 2026-06.
// 1.5-pro:   $1.25 input / $5 output per 1M (under 128k tokens)
// 1.5-flash: $0.075 input / $0.30 output per 1M
// 2.0-flash: $0.10 input / $0.40 output per 1M (default if unknown)
func estimateChatCost(model string, inputTokens, outputTokens int) int64 {
	inputUSDper1M := 0.10
	outputUSDper1M := 0.40
	switch {
	case strings.Contains(model, "1.5-pro"):
		inputUSDper1M = 1.25
		outputUSDper1M = 5.0
	case strings.Contains(model, "1.5-flash"):
		inputUSDper1M = 0.075
		outputUSDper1M = 0.30
	}
	inputUSD := float64(inputTokens) * inputUSDper1M / 1_000_000.0
	outputUSD := float64(outputTokens) * outputUSDper1M / 1_000_000.0
	return int64((inputUSD + outputUSD) * 1_000_000)
}

var _ ai.CompletionProvider = (*Provider)(nil)
var _ ai.EmbeddingProvider = (*Provider)(nil)
var _ ai.TagProvider = (*Provider)(nil)
var _ ai.CaptionProvider = (*Provider)(nil)
