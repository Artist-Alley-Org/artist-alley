package mcpserver

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

func TestProvider_Name_ReturnsConfiguredName(t *testing.T) {
	p := NewProvider(Config{Name: "comfyui-mcp", URL: "http://example.test"}, nil)
	if p.Name() != "comfyui-mcp" {
		t.Errorf("Name() = %q, want comfyui-mcp", p.Name())
	}
}

func TestProvider_ConfigSnapshot_ReflectsInput(t *testing.T) {
	p := NewProvider(Config{Name: "n", URL: "http://example.test", PrivacyClass: "local"}, nil)
	snap := p.ConfigSnapshot()
	if snap.PrivacyClass != "local" {
		t.Errorf("ConfigSnapshot().PrivacyClass = %q", snap.PrivacyClass)
	}
}

func TestProvider_InvokeTool_HappyPath_PostsJSONRPC(t *testing.T) {
	var capturedPath, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		// Echo a successful result containing one text-content part.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "ok"},
				},
				"isError": false,
			},
		})
	}))
	defer srv.Close()

	p := NewProvider(Config{Name: "test-srv", URL: srv.URL}, nil)
	res, err := p.InvokeTool(context.Background(), "img2img", map[string]any{
		"prompt": "a cat",
		"steps":  20,
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if capturedPath != "/" {
		t.Errorf("path = %q, want /", capturedPath)
	}
	for _, want := range []string{
		`"jsonrpc":"2.0"`,
		`"method":"tools/call"`,
		`"name":"img2img"`,
		`"prompt":"a cat"`,
	} {
		if !strings.Contains(capturedBody, want) {
			t.Errorf("request body missing %q\nfull body: %s", want, capturedBody)
		}
	}
	if !strings.Contains(string(res), `"text":"ok"`) {
		t.Errorf("result missing text content; got %s", res)
	}
}

func TestProvider_InvokeTool_BearerAuthApplied(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{
		Name: "x", URL: srv.URL,
		AuthKind:   "bearer",
		AuthSecret: "tok-12345",
	}, nil)
	if _, err := p.InvokeTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if capturedAuth != "Bearer tok-12345" {
		t.Errorf("Authorization = %q, want Bearer tok-12345", capturedAuth)
	}
}

func TestProvider_InvokeTool_HeaderAuthApplied(t *testing.T) {
	var capturedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.Header.Get("X-Api-Key")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{
		Name: "x", URL: srv.URL,
		AuthKind:       "header",
		AuthSecret:     "secret-1",
		AuthHeaderName: "X-Api-Key",
	}, nil)
	_, _ = p.InvokeTool(context.Background(), "t", nil)
	if capturedKey != "secret-1" {
		t.Errorf("X-Api-Key = %q", capturedKey)
	}
}

func TestProvider_InvokeTool_NoAuth_SkipsHeader(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL, AuthKind: "none"}, nil)
	_, _ = p.InvokeTool(context.Background(), "t", nil)
	if capturedAuth != "" {
		t.Errorf("Authorization should be empty for AuthKind=none; got %q", capturedAuth)
	}
}

func TestProvider_InvokeTool_EmptyToolName_Permanent(t *testing.T) {
	p := NewProvider(Config{Name: "x", URL: "http://example.test"}, nil)
	_, err := p.InvokeTool(context.Background(), "", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent provider error", err)
	}
}

func TestProvider_InvokeTool_503_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("warming up"))
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	_, err := p.InvokeTool(context.Background(), "t", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassTransient {
		t.Errorf("got %v, want transient", err)
	}
}

func TestProvider_InvokeTool_5xx_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	_, err := p.InvokeTool(context.Background(), "t", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassTransient {
		t.Errorf("got %v, want transient on 500", err)
	}
}

func TestProvider_InvokeTool_4xx_Permanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	_, err := p.InvokeTool(context.Background(), "t", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent on 400", err)
	}
}

func TestProvider_InvokeTool_RPCError_InternalErrorTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"error": map[string]any{"code": -32603, "message": "internal"},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	_, err := p.InvokeTool(context.Background(), "t", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassTransient {
		t.Errorf("got %v, want transient for RPC -32603", err)
	}
}

func TestProvider_InvokeTool_RPCError_GenericPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"error": map[string]any{"code": -32602, "message": "invalid params"},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	_, err := p.InvokeTool(context.Background(), "t", nil)
	pe, ok := ai.AsProviderError(err)
	if !ok || pe.Class != ai.ErrClassPermanent {
		t.Errorf("got %v, want permanent for non-internal RPC error", err)
	}
}

func TestProvider_ListTools_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "img2img", "description": "image to image"},
					{"name": "upscale", "description": "upscale image"},
				},
			},
		})
	}))
	defer srv.Close()
	p := NewProvider(Config{Name: "x", URL: srv.URL}, nil)
	tools, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools; got %d", len(tools))
	}
	if tools[0].Name != "img2img" || tools[1].Name != "upscale" {
		t.Errorf("tool names = %+v", tools)
	}
}

// Compile-time: Provider satisfies ai.Provider but NOT typed concerns.
var _ ai.Provider = (*Provider)(nil)

func TestProvider_DoesNotSatisfyConcernInterfaces(t *testing.T) {
	var p ai.Provider = NewProvider(Config{Name: "x"}, nil)
	if _, ok := p.(ai.CompletionProvider); ok {
		t.Error("MCP provider must NOT satisfy CompletionProvider")
	}
	if _, ok := p.(ai.EmbeddingProvider); ok {
		t.Error("MCP provider must NOT satisfy EmbeddingProvider")
	}
	if _, ok := p.(ai.TranscriptionProvider); ok {
		t.Error("MCP provider must NOT satisfy TranscriptionProvider")
	}
	if _, ok := p.(ai.TagProvider); ok {
		t.Error("MCP provider must NOT satisfy TagProvider")
	}
	if _, ok := p.(ai.CaptionProvider); ok {
		t.Error("MCP provider must NOT satisfy CaptionProvider")
	}
}
