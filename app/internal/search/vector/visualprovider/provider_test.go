package visualprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLocalProvider_Health_OK — happy path, sidecar returns 200.
func TestLocalProvider_Health_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "model": "ViT-L-14", "checkpoint": "openai", "dim": 768,
		})
	}))
	defer srv.Close()

	p := New(srv.URL, time.Second)
	h, err := p.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "ok" || h.Model != "ViT-L-14" || h.Dim != 768 {
		t.Fatalf("unexpected health: %+v", h)
	}
}

// TestLocalProvider_Health_Loading — sidecar warming up.
func TestLocalProvider_Health_Loading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "loading"})
	}))
	defer srv.Close()

	p := New(srv.URL, time.Second)
	h, err := p.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "loading" {
		t.Fatalf("expected loading, got %q", h.Status)
	}
}

// TestLocalProvider_Bootstrap_DimMismatch — sidecar reports wrong dim.
func TestLocalProvider_Bootstrap_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "model": "ViT-B-32", "dim": 512,
		})
	}))
	defer srv.Close()

	p := New(srv.URL, time.Second)
	_, err := p.Bootstrap(context.Background())
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("expected ErrDimMismatch, got %v", err)
	}
}

// TestLocalProvider_EmbedImage_HappyPath — sidecar returns a proper
// embedding.
func TestLocalProvider_EmbedImage_HappyPath(t *testing.T) {
	// Build a 768-dim vector server-side; the provider validates
	// that the length matches the claimed dim.
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.01
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed/image" || r.Method != http.MethodPost {
			t.Fatalf("bad request: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("multipart: %v", err)
		}
		f, _, _ := r.FormFile("file")
		defer f.Close()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding": vec, "dim": 768, "model": "ViT-L-14", "checkpoint": "openai",
		})
	}))
	defer srv.Close()

	p := New(srv.URL, time.Second)
	e, err := p.EmbedImage(context.Background(), []byte("fake-jpeg-bytes"))
	if err != nil {
		t.Fatalf("EmbedImage: %v", err)
	}
	if e.Dim != 768 || len(e.Vector) != 768 || e.Model != "ViT-L-14" {
		t.Fatalf("unexpected embed: %+v", e)
	}
	// Info gets checkpoint populated after the first successful
	// embed even without an explicit Bootstrap.
	if p.Info().Checkpoint != "openai" {
		t.Fatalf("info.checkpoint not populated: %+v", p.Info())
	}
}

// TestLocalProvider_EmbedImage_Unreachable — sidecar down.
func TestLocalProvider_EmbedImage_Unreachable(t *testing.T) {
	p := New("http://127.0.0.1:1", 100*time.Millisecond)
	_, err := p.EmbedImage(context.Background(), []byte("x"))
	if !errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("expected ErrSidecarUnreachable, got %v", err)
	}
}

// TestLocalProvider_EmbedImage_DimMismatch — sidecar returned a
// different dim than the schema expects.
func TestLocalProvider_EmbedImage_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float32{0.1, 0.2, 0.3}, "dim": 3, "model": "x",
		})
	}))
	defer srv.Close()
	p := New(srv.URL, time.Second)
	_, err := p.EmbedImage(context.Background(), []byte("x"))
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("expected ErrDimMismatch, got %v", err)
	}
}

// TestLocalProvider_EmbedImage_EmptyBytes — guard against nil-byte
// callers.
func TestLocalProvider_EmbedImage_EmptyBytes(t *testing.T) {
	p := New("http://127.0.0.1", time.Second)
	_, err := p.EmbedImage(context.Background(), nil)
	if err == nil {
		t.Fatal("expected empty-bytes error")
	}
}

// TestLocalProvider_EmbedImage_Non200 — sidecar returned an error
// body (e.g., 400 decode failure).
func TestLocalProvider_EmbedImage_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"decode failed"}`))
	}))
	defer srv.Close()
	p := New(srv.URL, time.Second)
	_, err := p.EmbedImage(context.Background(), []byte("bad"))
	if err == nil {
		t.Fatal("expected error on non-200")
	}
}

// TestLocalProvider_Context_Canceled — respect caller's context.
// Uses a pre-cancelled context so the request never leaves the
// client — no need to spin up a slow httptest server that could
// leak goroutines waiting for its handler to return.
func TestLocalProvider_Context_Canceled(t *testing.T) {
	p := New("http://127.0.0.1:0", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.EmbedImage(ctx, []byte("x"))
	if err == nil {
		t.Fatal("expected context error")
	}
}
