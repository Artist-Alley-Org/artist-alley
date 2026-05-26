//go:build embed_web

// Package http — production static-serving with the SvelteKit bundle
// baked into the binary.
//
// Selected with: `go build -tags embed_web ./cmd/aa`.
//
// The CI / prod Docker image builds the SvelteKit project first
// (`npm run build` produces `web/build/`), copies the output into
// this package's `static_assets/` directory, then compiles with the
// `embed_web` tag. The directive below pulls every file in that
// directory into the binary at compile time.
//
// Local `go build` (no tag) falls through to static_dev.go so the
// embed doesn't fail when `static_assets/` is empty.
package http

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

//go:embed all:static_assets
var staticAssetsFS embed.FS

func hasEmbeddedFrontend() bool { return true }

// mountStaticFrontend wires the embedded SvelteKit bundle onto the
// router. Routes the chi mux hasn't already claimed (e.g. /api/v1/*,
// /healthz) fall through to this handler.
//
// Behaviour:
//   - Exact-match static asset -> serve from the embed.
//   - Otherwise -> serve index.html and let the SPA router resolve
//     the path client-side (SvelteKit + adapter-static SPA fallback).
func mountStaticFrontend(r chi.Router) {
	sub, err := fs.Sub(staticAssetsFS, "static_assets")
	if err != nil {
		// Build-time impossibility — but if it ever happens, mount
		// nothing rather than crash the boot.
		return
	}
	r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveSPA(sub, w, req)
	}))
}

func serveSPA(root fs.FS, w http.ResponseWriter, r *http.Request) {
	// chi's NotFound runs after route matching, so anything reaching
	// here is meant for the frontend. Resolve the requested path
	// against the embedded FS first.
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}

	if f, err := root.Open(p); err == nil {
		_ = f.Close()
		http.FileServer(http.FS(root)).ServeHTTP(w, r)
		return
	}

	// Unknown path — hand the SPA shell back, the client router
	// (SvelteKit) handles it.
	index, err := root.Open("index.html")
	if err != nil {
		http.Error(w, "frontend bundle missing", http.StatusInternalServerError)
		return
	}
	defer index.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if rs, ok := index.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "index.html", time.Time{}, rs)
		return
	}
	// Fallback if the FS impl doesn't return a Seeker.
	_, _ = io.Copy(w, index)
}
