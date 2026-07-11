// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package openaicompat is the shared marshaling + HTTP plumbing for
// providers that speak the OpenAI Chat Completions wire format:
// OpenAI direct, Azure OpenAI, Ollama, and vLLM all use the same
// /v1/chat/completions endpoint shape. Per-provider differences
// (base URL, auth header, default model) are configuration.
//
// Per the Phase 1.14.A brief: this file isolates the wire so the
// per-provider packages (providers/openai, providers/ollama,
// providers/vllm) stay thin — they construct a Client with their
// config and let the shared code handle marshaling, HTTP transport,
// error classification, and rate limiting.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// ---------------------------------------------------------------------------
// On-wire request + response shapes (OpenAI chat-completions)
// ---------------------------------------------------------------------------

// chatRequest is the JSON shape every OpenAI-compatible endpoint
// consumes. Fields that some providers ignore (e.g. Ollama's
// response_format) are omitted entirely when zero-valued via the
// omitempty tags.
type chatRequest struct {
	Model       string         `json:"model"`
	Messages    []chatMessage  `json:"messages"`
	Temperature float64        `json:"temperature,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	Tools       []chatToolDecl `json:"tools,omitempty"`
}

// chatMessage carries one role+content turn. Content is `any` so
// the encoder writes either a bare string (most messages) or an
// array of parts (multi-modal vision messages).
type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// chatContentPart is one element of a multi-modal content array.
// OpenAI's shape: {"type": "text"|"image_url", "text"?: "..", "image_url"?: {"url": ".."}}.
type chatContentPart struct {
	Type     string             `json:"type"`
	Text     string             `json:"text,omitempty"`
	ImageURL *chatContentImgURL `json:"image_url,omitempty"`
}

type chatContentImgURL struct {
	URL string `json:"url"`
}

type chatToolDecl struct {
	Type     string             `json:"type"`
	Function chatToolDeclFnSpec `json:"function"`
}

type chatToolDeclFnSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// chatResponse is the shape we read back. Only the fields we
// actually consume are decoded.
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatChoice struct {
	Message      chatMessageOut `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type chatMessageOut struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// MarshalCompletion converts an ai.CompletionRequest to the
// on-wire JSON bytes. Pulled out so per-provider configs can
// substitute their own default model when the caller leaves
// req.Model empty.
func MarshalCompletion(req ai.CompletionRequest, defaultModel string) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{
			Role:    m.Role,
			Content: encodeMessageContent(m.Content),
		})
	}

	var tools []chatToolDecl
	for _, td := range req.Tools {
		tools = append(tools, chatToolDecl{
			Type: "function",
			Function: chatToolDeclFnSpec{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.Parameters,
			},
		})
	}

	wire := chatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		Tools:       tools,
	}
	return json.Marshal(wire)
}

// encodeMessageContent writes the simple `string` form for a single
// text part (the common case), else the multi-modal array form.
func encodeMessageContent(parts []ai.Content) any {
	if len(parts) == 1 && parts[0].Type == ai.ContentTypeText {
		return parts[0].Text
	}
	out := make([]chatContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case ai.ContentTypeText:
			out = append(out, chatContentPart{Type: "text", Text: p.Text})
		case ai.ContentTypeImageURL:
			out = append(out, chatContentPart{
				Type:     "image_url",
				ImageURL: &chatContentImgURL{URL: p.ImageURL},
			})
		case ai.ContentTypeImageB64:
			// OpenAI accepts data: URLs in the image_url field.
			data := "data:" + p.MimeType + ";base64," + base64.StdEncoding.EncodeToString(p.ImageBytes)
			out = append(out, chatContentPart{
				Type:     "image_url",
				ImageURL: &chatContentImgURL{URL: data},
			})
		}
	}
	return out
}

// ParseCompletion decodes the response body into the universal
// ai.CompletionResponse shape. Duration is supplied by the caller
// (the round-trip wall clock).
func ParseCompletion(body []byte, duration time.Duration) (ai.CompletionResponse, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return ai.CompletionResponse{}, fmt.Errorf("parse completion: %w", err)
	}
	if len(wire.Choices) == 0 {
		return ai.CompletionResponse{}, errors.New("completion: no choices in response")
	}
	c := wire.Choices[0]
	return ai.CompletionResponse{
		Text:         c.Message.Content,
		InputTokens:  wire.Usage.PromptTokens,
		OutputTokens: wire.Usage.CompletionTokens,
		FinishReason: c.FinishReason,
		Duration:     duration,
	}, nil
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

// ClassifyHTTPError maps an HTTP response status to the ai
// ErrorClass enum. Pulled out so per-provider packages all
// classify the same way without copying the switch.
func ClassifyHTTPError(status int, retryAfter string, provider, model string) error {
	switch {
	case status == http.StatusTooManyRequests:
		return &ai.ProviderError{
			Class:      ai.ErrClassRateLimit,
			Provider:   provider,
			Model:      model,
			RetryAfter: parseRetryAfter(retryAfter),
			Wrapped:    fmt.Errorf("http %d", status),
		}
	case status >= 500:
		return &ai.ProviderError{
			Class:    ai.ErrClassTransient,
			Provider: provider,
			Model:    model,
			Wrapped:  fmt.Errorf("http %d", status),
		}
	case status >= 400:
		return &ai.ProviderError{
			Class:    ai.ErrClassPermanent,
			Provider: provider,
			Model:    model,
			Wrapped:  fmt.Errorf("http %d", status),
		}
	}
	return nil // 2xx + 3xx — the caller decides
}

// parseRetryAfter accepts either an integer-seconds value or an
// HTTP-date per RFC 7231. Returns zero when both fail; the worker
// then uses its own backoff.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ClassifyTransportError wraps a non-HTTP error (DNS failure,
// timeout, connection refused, EOF) as transient.
func ClassifyTransportError(err error, provider, model string) error {
	return &ai.ProviderError{
		Class:    ai.ErrClassTransient,
		Provider: provider,
		Model:    model,
		Wrapped:  err,
	}
}

// ---------------------------------------------------------------------------
// HTTP client + rate limiter
// ---------------------------------------------------------------------------

// Client is the shared OpenAI-compatible HTTP transport. Per-
// provider packages construct one with their config + reuse its
// Do method.
type Client struct {
	BaseURL      string
	APIKey       string // empty for Ollama / vLLM
	Org          string // OpenAI optional
	HTTPClient   *http.Client
	Limiter      *rate.Limiter
	ProviderName string
	DefaultModel string
}

// NewClient builds a ready-to-use Client. Limiter may be nil
// (no rate limiting); HTTPClient defaults to a sensible reusable
// http.Client when nil.
func NewClient(name, baseURL, apiKey, org, defaultModel string, limiter *rate.Limiter) *Client {
	return &Client{
		ProviderName: name,
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		Org:          org,
		DefaultModel: defaultModel,
		Limiter:      limiter,
		HTTPClient:   &http.Client{Timeout: 60 * time.Second},
	}
}

// PostJSON sends a POST with a JSON body to BaseURL+path. Returns
// the response body bytes + nil on 2xx; a classified
// *ai.ProviderError on any non-2xx or transport failure.
//
// Honors the rate limiter (Limiter.Wait) if set.
func (c *Client) PostJSON(ctx context.Context, path string, body []byte, model string) ([]byte, error) {
	if c.Limiter != nil {
		if err := c.Limiter.Wait(ctx); err != nil {
			// Limiter errors when ctx is done; surface as transient
			// so the worker retries (the underlying cancel will
			// re-fire and terminate cleanly).
			return nil, ClassifyTransportError(err, c.ProviderName, model)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, ClassifyTransportError(err, c.ProviderName, model)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Org != "" {
		req.Header.Set("OpenAI-Organization", c.Org)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, ClassifyTransportError(err, c.ProviderName, model)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Surface the provider's body in the error wrap so the
		// admin dashboard can show the model's actual rejection
		// reason ("model not found", "context length exceeded",
		// etc.) without the operator having to grep logs.
		classed := ClassifyHTTPError(resp.StatusCode, resp.Header.Get("Retry-After"), c.ProviderName, model)
		var pe *ai.ProviderError
		if errors.As(classed, &pe) && len(respBody) > 0 {
			snippet := string(respBody)
			if len(snippet) > 512 {
				snippet = snippet[:512] + "..."
			}
			pe.Wrapped = fmt.Errorf("%w: %s", pe.Wrapped, snippet)
		}
		return nil, classed
	}
	return respBody, nil
}

// ---------------------------------------------------------------------------
// Embedding wire (OpenAI-compatible)
// ---------------------------------------------------------------------------

type embedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

type embedResponse struct {
	Data []embedData `json:"data"`
}

type embedData struct {
	Embedding []float32 `json:"embedding"`
}

// MarshalEmbedding produces the JSON body for /v1/embeddings.
// Text-only for now; image embedding via the OpenAI-compat surface
// isn't standardized (CLIP-local handles image embeddings in 1.14.B).
func MarshalEmbedding(in ai.EmbedInput, defaultModel string) ([]byte, error) {
	model := in.Model
	if model == "" {
		model = defaultModel
	}
	if in.Text == "" {
		return nil, errors.New("embed: text input required for openai-compat endpoint")
	}
	return json.Marshal(embedRequest{Model: model, Input: in.Text})
}

// ParseEmbedding decodes the embedding response. Returns the first
// vector (callers send one item per request).
func ParseEmbedding(body []byte) ([]float32, error) {
	var wire embedResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("parse embedding: %w", err)
	}
	if len(wire.Data) == 0 {
		return nil, errors.New("embedding: no data in response")
	}
	return wire.Data[0].Embedding, nil
}
