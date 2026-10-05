// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Synthetic SvelteKit adapter-static layout that exercises every
// branch of serveSPA. Mirrors what the real build produces: a flat
// index.html shell, a per-route .html sibling for prerenderable
// routes, and a directory of nested .html files for routes whose
// parent is unprerenderable.
func newTestFS(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<html data-shell=\"yes\"><body><div id=\"svelte\"></div></body></html>"),
		},
		"login.html": &fstest.MapFile{
			Data: []byte("<html data-route=\"login\"></html>"),
		},
		"admin/users.html": &fstest.MapFile{
			Data: []byte("<html data-route=\"admin/users\"></html>"),
		},
		"admin/federation/peers.html": &fstest.MapFile{
			Data: []byte("<html data-route=\"admin/federation/peers\"></html>"),
		},
		"_app/immutable/start.abc.js": &fstest.MapFile{
			Data: []byte("// SvelteKit start chunk"),
		},
	}
}

func doRequest(t *testing.T, fsys fstest.MapFS, urlPath string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, urlPath, nil)
	rec := httptest.NewRecorder()
	serveSPA(fsys, rec, req)
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestServeSPA(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantInBody string
	}{
		{
			name:       "root returns the SPA shell",
			path:       "/",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
		{
			name:       "prerendered route file served directly",
			path:       "/login.html",
			wantStatus: http.StatusOK,
			wantInBody: `data-route="login"`,
		},
		{
			name:       "nested route file served directly",
			path:       "/admin/users.html",
			wantStatus: http.StatusOK,
			wantInBody: `data-route="admin/users"`,
		},
		{
			name:       "deep nested route file served directly",
			path:       "/admin/federation/peers.html",
			wantStatus: http.StatusOK,
			wantInBody: `data-route="admin/federation/peers"`,
		},
		{
			name:       "JS chunk served directly",
			path:       "/_app/immutable/start.abc.js",
			wantStatus: http.StatusOK,
			wantInBody: "SvelteKit start chunk",
		},
		{
			// Regression: SvelteKit's adapter-static emits
			// `admin/users.html` etc., so `admin/` exists as a
			// directory in the embedded FS. Before the dir-check
			// fix, Go's http.FileServer rendered an Apache-style
			// directory listing here, which broke 21 standalone
			// Playwright tests in CI (no <main>, no <title>, SPA
			// never hydrates).
			name:       "directory without index.html falls back to SPA shell — no directory listing",
			path:       "/admin/",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
		{
			// Same regression, no trailing slash. Pre-fix Go would
			// 301-redirect to /admin/.
			name:       "directory path without trailing slash also returns SPA shell",
			path:       "/admin",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
		{
			name:       "nested directory falls back to SPA shell",
			path:       "/admin/federation/",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
		{
			name:       "unknown route falls back to SPA shell",
			path:       "/bogus-route",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
		{
			name:       "unknown deep route falls back to SPA shell",
			path:       "/admin/this-route-does-not-exist",
			wantStatus: http.StatusOK,
			wantInBody: `data-shell="yes"`,
		},
	}

	fsys := newTestFS(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doRequest(t, fsys, tc.path)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body=%q)", resp.StatusCode, tc.wantStatus, body)
			}
			if !strings.Contains(body, tc.wantInBody) {
				t.Fatalf("body missing %q\n  got: %q", tc.wantInBody, body)
			}
			// SPA shell responses must NOT be Apache-style directory
			// listings — those start with `<pre>` after a minimal
			// boilerplate. Sanity assertion across every case so a
			// future refactor that re-introduces the dir-listing
			// fault fails loudly.
			if strings.Contains(body, "<pre>") &&
				strings.Contains(body, ".html</a>") {
				t.Fatalf("response looks like a directory listing:\n%s", body)
			}
		})
	}
}

func TestServeSPA_NoIndexHTML_ReturnsInternalError(t *testing.T) {
	// If the embed somehow lacks index.html, the SPA fallback can't
	// recover — surface a 500 instead of a blank 200.
	fsys := fstest.MapFS{
		"login.html": &fstest.MapFile{Data: []byte("login")},
	}
	resp, body := doRequest(t, fsys, "/some-route")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d want %d (body=%q)", resp.StatusCode, http.StatusInternalServerError, body)
	}
	if !strings.Contains(body, "frontend bundle missing") {
		t.Fatalf("expected 'frontend bundle missing', got %q", body)
	}
}

func TestServeSPA_CacheControlOnShell(t *testing.T) {
	// The SPA shell must NOT be cached aggressively (or the client
	// router never picks up a new bundle hash). Asset files get
	// FileServer's default headers and can be cached.
	fsys := newTestFS(t)
	resp, _ := doRequest(t, fsys, "/some-unknown-route")
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control: got %q want %q", got, "no-cache")
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type: got %q want text/html prefix", got)
	}
}

// TestServeSPA_UnroutedAPIPathIs404JSON is #1161's regression.
//
// # The bug this pins
//
// serveSPA is chi's NotFound handler in the embed_web (production)
// build, so every unrouted path reaches it — including `/api/...`. It
// answered them with `200 text/html` and the SvelteKit shell in the
// body, which means a REMOVED endpoint was indistinguishable from a
// working one to any client, and a misspelled URL "succeeded".
//
// It was invisible for as long as it existed because the two builds
// disagreed: the dev image's mountStaticFrontend is a no-op, so chi's
// own NotFound answered a clean 404 there. #1161 retired
// `DELETE /api/v1/collections/{id}/resources/{asset_id}`, asserted the
// route was gone, and got 404 locally and 200 in CI — the same code,
// two build tags.
//
// The table is written as (path, wantAPI) rather than as two lists so
// the NEAR MISSES sit next to the hits: `/apidocs` and `/api-keys` are
// SPA routes that start with the same four letters, and a
// `strings.HasPrefix(path, "/api")` guard — the obvious first spelling
// — swallows both.
func TestServeSPA_UnroutedAPIPathIs404JSON(t *testing.T) {
	fsys := newTestFS(t)
	for _, tc := range []struct {
		path    string
		wantAPI bool
		why     string
	}{
		{"/api/v1/collections/x/resources/y", true, "the retired endpoint that found this"},
		{"/api/v1/nonesuch", true, "any unrouted API path"},
		{"/api", true, "the bare prefix is not an SPA route either"},
		{"/api/", true, "trailing slash"},
		{"/api/v1/../v1/nonesuch", true, "cleaned before comparing, so this is still API"},
		{"/apidocs", false, "an SPA route that merely starts with the same letters"},
		{"/api-keys", false, "ditto, with a hyphen"},
		{"/", false, "the shell itself"},
		{"/admin/users", false, "an ordinary client route"},
		{"/api/../admin/users", false, "cleans to an SPA path and must be served as one"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := doRequest(t, fsys, tc.path)
			if !tc.wantAPI {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s (%s): status %d, want 200 — the SPA shell", tc.path, tc.why, resp.StatusCode)
				}
				if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
					t.Errorf("%s (%s): content-type %q, want text/html",
						tc.path, tc.why, resp.Header.Get("Content-Type"))
				}
				return
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s (%s): status %d, want 404 — an unrouted API path must not be answered by the SPA",
					tc.path, tc.why, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("%s: content-type %q, want application/json", tc.path, ct)
			}
			// The body is the API's own error envelope, not the shell.
			// Asserting the ABSENCE of the shell as well as the presence
			// of the envelope: a handler that wrote both would pass a
			// presence-only check.
			if !strings.Contains(body, `"error"`) {
				t.Errorf("%s: body %q carries no error envelope", tc.path, body)
			}
			if strings.Contains(body, "<html") {
				t.Errorf("%s: the SPA shell is still in the body: %q", tc.path, body)
			}
		})
	}
}
