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
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
)

//go:embed all:static_assets
var staticAssetsFS embed.FS

func hasEmbeddedFrontend() bool { return true }

// mountStaticFrontend wires the embedded SvelteKit bundle onto the
// router. Routes the chi mux hasn't already claimed (e.g. /api/v1/*,
// /healthz) fall through to this handler.
//
// Behaviour is implemented in serveSPA (static_spa.go) — that
// function is intentionally tag-free so the regression tests in
// static_spa_test.go run regardless of build tag.
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
