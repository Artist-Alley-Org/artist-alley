// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package mcpserver wraps one registered MCP server as an
// ai.Provider — so the existing router + audit + cost + privacy gate
// machinery from Phase 1.14.A applies uniformly without a parallel
// orchestration layer.
//
// # Surface
//
// The provider is a marker (Name() string) only; MCP tools don't
// fit any of the typed concern interfaces (Complete/Embed/Transcribe/
// Tag/Caption) because tool schemas are runtime-discovered. The
// generic dispatcher (app/internal/ai/mcp_dispatch) calls
// p.InvokeTool(ctx, tool, args) directly via type assertion — same
// shape as the dispatcher's other consumers (TagProvider, etc.).
//
// # One provider per registration
//
// The boot wire constructs one Provider per row in
// mcp_server_registration AND registers each under its operator-
// chosen name. Operator-named instances mean two ComfyUI bridges
// (e.g., "comfyui-mcp" for prod + "comfyui-mcp-staging" for staging)
// register under distinct router slots without code duplication.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// Config is the per-server runtime config the boot wire materialises
// from a mcpregistry.Server row. Kept narrow so this package doesn't
// import mcpregistry directly (avoids a cycle when the dispatcher
// imports both).
type Config struct {
	// Name — operator-chosen identifier; surfaces as ai.Provider.Name()
	// + ai_provider_call.provider.
	Name string

	// URL — base URL of the MCP server. HTTP transport only in v1;
	// stdio reserved for a follow-up extension when an operator
	// needs it.
	URL string

	// AuthKind / AuthSecret / AuthHeaderName — outbound auth config.
	// 'none' skips the Authorization header entirely; 'bearer' sets
	// Authorization: Bearer <secret>; 'header' sets a custom header.
	// 'mtls' is parsed by the schema but not yet wired in v1.
	AuthKind       string
	AuthSecret     string
	AuthHeaderName string

	// PrivacyClass — 'local' or 'cloud'. The dispatcher's privacy
	// gate reads this; included here so the audit hash includes
	// the classification (operators auditing for compliance can
	// see "this restricted call was routed to a local server").
	PrivacyClass string

	// RateLimitPerSecond / Burst — token-bucket gate. The dispatcher
	// awaits the limiter before issuing the HTTP call.
	RateLimitPerSecond float64
	RateLimitBurst     int

	// HTTPTimeout — per-call wall-clock cap. Tools that run image
	// generation can take minutes; default of 5min covers most
	// well-tuned MCP servers.
	HTTPTimeout time.Duration
}

// Provider satisfies ai.Provider only. The dispatcher calls
// InvokeTool directly via type assertion since the generic MCP
// surface doesn't map to the typed concern interfaces.
type Provider struct {
	cfg     Config
	client  *http.Client
	limiter *rate.Limiter
	auditor *ai.CallAuditor
}

// NewProvider constructs a provider. auditor may be nil (tests).
func NewProvider(cfg Config, auditor *ai.CallAuditor) *Provider {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
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

// Name satisfies ai.Provider — the operator-chosen registration
// identifier.
func (p *Provider) Name() string { return p.cfg.Name }

// Config exposes the underlying config (read-only via the value
// return) so the dispatcher's guard chain can read PrivacyClass
// without an extra registry round-trip.
func (p *Provider) ConfigSnapshot() Config { return p.cfg }

// ---------------------------------------------------------------------------
// MCP wire types
// ---------------------------------------------------------------------------
//
// MCP uses JSON-RPC 2.0 over HTTP. Two methods we exercise in v1:
//
//   - tools/list — discovers the server's tool surface. Returns
//     {tools: [{name, description, inputSchema}, ...]}. The dispatcher
//     calls this on health-check to refresh the per-server tool
//     registry; not used by InvokeTool's hot path.
//
//   - tools/call — invokes one tool. Request:
//       {method: "tools/call", params: {name, arguments}}
//     Response:
//       {result: {content: [{type, text|data}, ...], isError?: bool}}
//
// We don't model the full content-part union here (TextContent,
// ImageContent, EmbeddedResource, ...); the dispatcher returns the
// raw response JSON to the caller and lets it interpret based on
// the tool's known shape.

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ListedTool is the public shape ListTools returns — what the
// health-check tool-list refresh stores in the per-server cache.
type ListedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type toolsListResult struct {
	Tools []ListedTool `json:"tools"`
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// ListTools queries the server's tool surface via tools/list. Used
// by the health-check goroutine to refresh the per-server tool list
// cache so the operator's admin UI shows "what's available right
// now"; the dispatcher itself uses the operator-curated whitelist
// (mcp_server_tool_grant rows), not this list.
func (p *Provider) ListTools(ctx context.Context) ([]ListedTool, error) {
	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return nil, &ai.ProviderError{
				Class: ai.ErrClassTransient, Provider: p.cfg.Name, Wrapped: err,
			}
		}
	}
	resp, err := p.rpc(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: p.cfg.Name,
			Wrapped: fmt.Errorf("tools/list: %s (code %d)", resp.Error.Message, resp.Error.Code),
		}
	}
	var parsed toolsListResult
	if err := json.Unmarshal(resp.Result, &parsed); err != nil {
		return nil, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: p.cfg.Name, Wrapped: err,
		}
	}
	return parsed.Tools, nil
}

// InvokeTool calls tools/call with the operator-chosen tool + args.
// Returns the raw result JSON; caller interprets based on the tool's
// known shape. The dispatcher's audit row records the call regardless
// of result; this method's job is just the wire round-trip + error
// classification.
//
// Returns a 1-byte "{}" when the server's response carries no result
// payload (rare; some MCP servers reply with empty results for
// fire-and-forget tools).
func (p *Provider) InvokeTool(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	if tool == "" {
		return nil, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: p.cfg.Name,
			Wrapped: errors.New("invoke: tool name required"),
		}
	}
	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return nil, &ai.ProviderError{
				Class: ai.ErrClassTransient, Provider: p.cfg.Name, Model: tool, Wrapped: err,
			}
		}
	}
	resp, err := p.rpc(ctx, "tools/call", toolsCallParams{
		Name:      tool,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		// JSON-RPC error codes -32600..-32000 are protocol errors;
		// everything else is server-defined. We treat -32603
		// (internal error) as transient (server bug, may recover);
		// everything else as permanent. Operator can refine via
		// future config if a specific server uses custom codes.
		class := ai.ErrClassPermanent
		if resp.Error.Code == -32603 {
			class = ai.ErrClassTransient
		}
		return nil, &ai.ProviderError{
			Class: class, Provider: p.cfg.Name, Model: tool,
			Wrapped: fmt.Errorf("tools/call %q: %s (code %d)", tool, resp.Error.Message, resp.Error.Code),
		}
	}
	if len(resp.Result) == 0 {
		return json.RawMessage("{}"), nil
	}
	return resp.Result, nil
}

// rpc is the JSON-RPC POST round-trip shared by ListTools +
// InvokeTool. Classifies HTTP errors into ProviderError per the
// standard 1.14.A class set.
func (p *Provider) rpc(ctx context.Context, method string, params any) (jsonRPCResponse, error) {
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: p.cfg.Name, Wrapped: err,
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: p.cfg.Name, Wrapped: err,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	p.applyAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		// Network errors are transient; the dispatcher's worker
		// retries.
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: p.cfg.Name, Wrapped: err,
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode >= 500 {
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: p.cfg.Name,
			Wrapped: fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 200)),
		}
	}
	if resp.StatusCode >= 400 {
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassPermanent, Provider: p.cfg.Name,
			Wrapped: fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 200)),
		}
	}

	var parsed jsonRPCResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return jsonRPCResponse{}, &ai.ProviderError{
			Class: ai.ErrClassTransient, Provider: p.cfg.Name, Wrapped: err,
		}
	}
	return parsed, nil
}

// applyAuth attaches the operator-configured auth header to req.
// 'none' is a no-op (operator either trusts the network or relies
// on the server's own auth gate). 'mtls' is not yet wired — the
// schema permits the value but no v1 code path handles client
// certificates.
func (p *Provider) applyAuth(req *http.Request) {
	switch p.cfg.AuthKind {
	case "bearer":
		if p.cfg.AuthSecret != "" {
			req.Header.Set("Authorization", "Bearer "+p.cfg.AuthSecret)
		}
	case "header":
		if p.cfg.AuthHeaderName != "" && p.cfg.AuthSecret != "" {
			req.Header.Set(p.cfg.AuthHeaderName, p.cfg.AuthSecret)
		}
	}
}

// snippet truncates a response body for inclusion in an error
// message — full body could include credentials echoed back by a
// misconfigured server.
func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// Compile-time interface check — Provider satisfies ai.Provider but
// NOT any typed concern interface (the dispatcher type-asserts
// elsewhere).
var _ ai.Provider = (*Provider)(nil)
