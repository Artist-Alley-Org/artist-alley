// Package claude implements the Anthropic Claude provider. The
// Anthropic API shape differs enough from OpenAI's chat-completions
// that openaicompat doesn't apply; this package has its own thin
// marshaler (~150 LOC per the brief).
//
// Phase 1.14.A scope: CompletionProvider, TagProvider,
// CaptionProvider. Claude doesn't expose a public embedding API
// (operator can route embeddings to OpenAI / Gemini / Ollama
// instead — that's what the per-task routing is for); no
// transcription support either.
//
// Wire reference: https://docs.anthropic.com/en/api/messages
//   POST https://api.anthropic.com/v1/messages
//   Headers:
//     x-api-key: <key>
//     anthropic-version: 2023-06-01
//     content-type: application/json
//   Body: {model, max_tokens, system?, messages: [{role, content}]}
//   Response: {content: [{type, text}], usage: {input_tokens, output_tokens}}
package claude

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

// Name is the operator-facing identifier.
const Name = "claude"

const (
	defaultBaseURL  = "https://api.anthropic.com"
	anthropicVersion = "2023-06-01"
)

// Config is the operator-tunable Claude provider config.
type Config struct {
	APIKey  string
	BaseURL string // defaults to https://api.anthropic.com

	DefaultCompletionModel string // e.g. "claude-3-5-sonnet-20241022"

	RateLimitPerSecond float64
	RateLimitBurst     int
}

// Provider implements ai.CompletionProvider + Tag + Caption.
type Provider struct {
	cfg        Config
	httpClient *http.Client
	limiter    *rate.Limiter
	prompts    *ai.PromptRegistry
	auditor    *ai.CallAuditor
}

// NewProvider constructs a Claude provider.
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
func (p *Provider) SupportsVision() bool { return true } // Claude 3+ supports vision

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type claudeRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []claudeMessage  `json:"messages"`
}

type claudeMessage struct {
	Role    string                `json:"role"` // "user" | "assistant"
	Content []claudeContentBlock  `json:"content"`
}

type claudeContentBlock struct {
	Type   string             `json:"type"` // "text" | "image"
	Text   string             `json:"text,omitempty"`
	Source *claudeImageSource `json:"source,omitempty"`
}

type claudeImageSource struct {
	Type      string `json:"type"`       // "base64" | "url"
	MediaType string `json:"media_type"`
	Data      string `json:"data,omitempty"` // base64-encoded; for type="base64"
	URL       string `json:"url,omitempty"`  // for type="url"
}

type claudeResponse struct {
	Content    []claudeRespBlock `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      claudeUsage       `json:"usage"`
}

type claudeRespBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------------------------------------------------------------------------
// CompletionProvider
// ---------------------------------------------------------------------------

func (p *Provider) Complete(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultCompletionModel
	}
	wire, err := marshalToClaude(req, model)
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.BaseURL+"/v1/messages", bytes.NewReader(wire))
	if err != nil {
		return ai.CompletionResponse{}, openaicompat.ClassifyTransportError(err, Name, model)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

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

	parsed, err := parseClaudeResponse(body, duration)
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
// Tag + Caption — delegate to shared helpers
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

func marshalToClaude(req ai.CompletionRequest, model string) ([]byte, error) {
	// Claude requires max_tokens; OpenAI doesn't. Default to 4096
	// when caller omits.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Claude separates the system prompt from messages. Collapse any
	// system-role messages into a single System string; rest become
	// messages.
	var systemParts []string
	var msgs []claudeMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			for _, c := range m.Content {
				if c.Type == ai.ContentTypeText {
					systemParts = append(systemParts, c.Text)
				}
			}
			continue
		}
		// Map ai.Content → claudeContentBlock per part.
		blocks := make([]claudeContentBlock, 0, len(m.Content))
		for _, c := range m.Content {
			switch c.Type {
			case ai.ContentTypeText:
				blocks = append(blocks, claudeContentBlock{Type: "text", Text: c.Text})
			case ai.ContentTypeImageURL:
				blocks = append(blocks, claudeContentBlock{
					Type:   "image",
					Source: &claudeImageSource{Type: "url", URL: c.ImageURL},
				})
			case ai.ContentTypeImageB64:
				blocks = append(blocks, claudeContentBlock{
					Type: "image",
					Source: &claudeImageSource{
						Type:      "base64",
						MediaType: c.MimeType,
						Data:      base64.StdEncoding.EncodeToString(c.ImageBytes),
					},
				})
			}
		}
		msgs = append(msgs, claudeMessage{Role: m.Role, Content: blocks})
	}

	body := claudeRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    strings.Join(systemParts, "\n\n"),
		Messages:  msgs,
	}
	return json.Marshal(body)
}

func parseClaudeResponse(body []byte, duration time.Duration) (ai.CompletionResponse, error) {
	var wire claudeResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return ai.CompletionResponse{}, fmt.Errorf("parse claude response: %w", err)
	}
	// Concatenate every text block into the universal Text field.
	var sb strings.Builder
	for _, b := range wire.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 && len(wire.Content) > 0 {
		return ai.CompletionResponse{}, errors.New("claude: response contains no text blocks")
	}
	return ai.CompletionResponse{
		Text:         sb.String(),
		InputTokens:  wire.Usage.InputTokens,
		OutputTokens: wire.Usage.OutputTokens,
		FinishReason: wire.StopReason,
		Duration:     duration,
	}, nil
}

// estimateChatCost — Claude pricing as of 2026-06.
// Sonnet (4o-tier quality): $3 input / $15 output per 1M tokens.
// Haiku (mini-tier):       $0.80 input / $4 output per 1M tokens.
// Opus (largest):          $15 input / $75 output per 1M tokens.
func estimateChatCost(model string, inputTokens, outputTokens int) int64 {
	inputUSDper1M := 3.0
	outputUSDper1M := 15.0
	switch {
	case strings.Contains(model, "haiku"):
		inputUSDper1M = 0.80
		outputUSDper1M = 4.0
	case strings.Contains(model, "opus"):
		inputUSDper1M = 15.0
		outputUSDper1M = 75.0
	}
	inputUSD := float64(inputTokens) * inputUSDper1M / 1_000_000.0
	outputUSD := float64(outputTokens) * outputUSDper1M / 1_000_000.0
	return int64((inputUSD + outputUSD) * 1_000_000)
}

var _ ai.CompletionProvider = (*Provider)(nil)
var _ ai.TagProvider = (*Provider)(nil)
var _ ai.CaptionProvider = (*Provider)(nil)
