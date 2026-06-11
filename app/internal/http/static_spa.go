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
func serveSPA(root fs.FS, w http.ResponseWriter, r *http.Request) {
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
