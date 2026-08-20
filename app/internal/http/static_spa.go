// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package http — SPA static-asset handler.
//
// This file is intentionally NOT gated by a build tag. The dev build
// (static_dev.go) doesn't call serveSPA because it expects an
// external Vite dev server to handle the SPA; the prod build
// (static_embed.go) wires serveSPA into chi's NotFound handler.
// Keeping the function tag-free makes it always-compiled and
// reachable by `go test ./...`, so the regression tests in
// static_spa_test.go run regardless of which build tag is set.

package http

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// serveSPA serves a static asset from the embedded FS, falling back
// to index.html (the SvelteKit SPA shell) for anything that doesn't
// resolve to a regular file. Three branches:
//
//  1. Path resolves to a regular file → serve it directly.
//  2. Path resolves to a directory → fall through to (3). Directories
//     without an index.html (e.g. `/admin/` — SvelteKit's adapter-
//     static emits `admin.html` at the root, not `admin/index.html`)
//     would otherwise hit Go's `http.FileServer` which serves an
//     Apache-style directory listing. Browsers landing on the
//     listing see no <main>, no <title>, no JS bundle, so the SPA
//     never hydrates. That regressed 21 standalone Playwright tests
//     in CI before this branch was added; static_spa_test.go pins
//     it.
//  3. Otherwise → return index.html with content-type text/html and
//     no-cache headers. The client SvelteKit router resolves the
//     path on the browser side.
//
// Branch (3) is why the API guard below exists: this function is chi's
// NotFound handler in the embed_web build, so EVERY unrouted path
// reaches it — including unrouted `/api/...` paths.
func serveSPA(root fs.FS, w http.ResponseWriter, r *http.Request) {
	// ---------------------------------------------------------------
	// An unrouted API path is 404 JSON, never the SPA shell.
	// ---------------------------------------------------------------
	//
	// #1161 found this the hard way. That sprint retired
	// `DELETE /api/v1/collections/{id}/resources/{asset_id}`, and its
	// acceptance test asserted the route was GONE — 404 or 405, because
	// a handler answering anything else means the endpoint is merely
	// unused rather than retired. It passed locally and failed in CI
	// with **200**, and the difference was not the code: the local app
	// image is built WITHOUT `embed_web`, so mountStaticFrontend is a
	// no-op and chi's own NotFound answers 404; the CI image is built
	// WITH it, so chi's NotFound is this function and the deleted
	// endpoint answered `index.html`.
	//
	// So this was never about one endpoint. In every production build,
	// ANY misspelled, removed or not-yet-shipped `/api/...` path
	// answered `200 text/html` with the SPA shell in the body. What
	// that costs:
	//
	//   - a client cannot tell "this endpoint is gone" from "this
	//     endpoint worked", because both are 2xx. A fetch that JSON-
	//     parses the shell fails with a syntax error pointing at the
	//     response body rather than at the URL that was wrong.
	//   - a retired endpoint looks alive to anything probing it, which
	//     is exactly the distinction #1161's test exists to make.
	//   - monitoring on 4xx/5xx rates sees nothing at all when a route
	//     disappears from a deploy.
	//
	// The guard is deliberately narrow: `/api/` and nothing else. IIIF
	// paths are mounted at the root and their unmatched tail still
	// falls through to the shell, which is a separate question with its
	// own history (see the dual-mount block in server.go). Widening
	// this to "anything that looks machine-facing" would be guessing.
	//
	// Cleaned before comparing so `/api/../x` (a legitimate SPA path)
	// is not caught and `/api/v1/../v1/x` (an API path) is not missed.
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "/api" || strings.HasPrefix(clean, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		// The same envelope every other API refusal uses, so a client
		// that already parses `{"error": …}` reads this one too.
		_, _ = io.WriteString(w, `{"error":"not found"}`)
		return
	}

	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}

	if f, err := root.Open(p); err == nil {
		stat, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil && !stat.IsDir() {
			http.FileServer(http.FS(root)).ServeHTTP(w, r)
			return
		}
	}

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
