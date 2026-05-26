//go:build !embed_web

// Package http — dev-mode static-serving stub.
//
// Local `go build` and the test suite use this stub: the Go binary
// does NOT serve the SvelteKit frontend. Developers run `npm run dev`
// (the `web` docker service) on :5173 and hit Vite directly during
// iteration.
//
// CI and prod builds use the `embed_web` build tag, which swaps in
// static_embed.go — that variant embeds the prebuilt `web/build`
// output via //go:embed and serves it from /.
package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// hasEmbeddedFrontend reports whether this build was compiled with
// the SvelteKit static bundle embedded. Used by server.go to log
// the mode at boot.
func hasEmbeddedFrontend() bool { return false }

// mountStaticFrontend is a no-op in dev. Vite serves the frontend on
// its own port; the Go binary only handles /api/v1/* and the health
// probes.
func mountStaticFrontend(_ chi.Router) {}

// silence unused-import warnings when this build tag is selected.
var _ = http.StatusOK
