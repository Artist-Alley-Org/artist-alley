// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #620 — the asset-addressed byte routes shipped
// `Cache-Control: immutable, max-age=31536000` with an ETag of
// `r.URL.Path`, plus a short-circuit that returned 304 before the
// handler ran. The validator was invariant by construction, so the
// server confirmed a stale copy without ever consulting the stored
// bytes: no sequence of requests could return updated content.
//
// The failure was reproduced in a browser before this was written — the
// stored bytes were swapped under a stable asset id, and a request sent
// with `cache: no-store` (which reaches the server, bypassing the
// browser cache) still returned 304 with the old bytes. These tests
// encode that, so the ability to fail is not hypothetical.
//
// THE INVARIANT is the first test: two different byte-sets under the
// SAME asset id must produce DIFFERENT ETags. Everything else here is a
// consequence of it.

package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/http/middleware"
)

const (
	testAssetID = "11111111-2222-3333-4444-555555555555"
	variantURL  = "/api/v1/assets/" + testAssetID + "/variants/col"
	fileURL     = "/api/v1/assets/" + testAssetID + "/file"
)

// storedBytes models what the storage backend reports for a variant.
type storedBytes struct {
	hash    string
	size    int64
	modNano int64
}

func validatorFor(store map[string]storedBytes) middleware.VariantValidator {
	return func(_ context.Context, id uuid.UUID, key string) (string, bool) {
		b, ok := store[id.String()+"|"+key]
		if !ok {
			return "", false
		}
		return middleware.VariantETag(b.hash, key, b.size, b.modNano), true
	}
}

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

func get(t *testing.T, h http.Handler, url, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestDifferentBytesSameAssetIDYieldDifferentETags is THE invariant.
//
// Each case is a real way an asset's bytes change while its id does
// not. The last one is the production case: RecreateAssetPreview
// re-renders derivatives from the SAME original, so file_hash is
// unchanged by definition — a validator built from file_hash alone
// would be identical across it, which is why that obvious fix does not
// work here.
func TestDifferentBytesSameAssetIDYieldDifferentETags(t *testing.T) {
	base := storedBytes{hash: "a" + "0123456789abcdef", size: 1020, modNano: 1_000}

	cases := []struct {
		name  string
		after storedBytes
		why   string
	}{
		{
			name:  "file replaced — new original, new hash",
			after: storedBytes{hash: "b" + "0123456789abcdef", size: 1020, modNano: 1_000},
			why:   "replace-file keeps the asset row and swaps the object",
		},
		{
			name:  "dataset reseed — different image, different size",
			after: storedBytes{hash: "b" + "0123456789abcdef", size: 12248, modNano: 2_000},
			why:   "the 2026-07-25 rebuild swapped files for 916 assets, ids preserved",
		},
		{
			name:  "preview regenerated — SAME hash, re-rendered variant",
			after: storedBytes{hash: base.hash, size: 1044, modNano: 2_000},
			why: "RecreateAssetPreview re-renders from the same original, so " +
				"file_hash cannot change — this is the case a file_hash-derived " +
				"validator would miss, and it is the production one",
		},
		{
			name:  "re-rendered to a byte-identical size",
			after: storedBytes{hash: base.hash, size: base.size, modNano: 2_000},
			why: "only the modification time moved; the validator must still " +
				"change or an identically-sized re-render is invisible",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := middleware.VariantETag(base.hash, "col", base.size, base.modNano)
			after := middleware.VariantETag(c.after.hash, "col", c.after.size, c.after.modNano)
			if before == after {
				t.Errorf("ETag unchanged after the bytes changed — %s\n"+
					"this is #620: the client can never be told to refetch", c.why)
			}
		})
	}

	t.Run("unchanged bytes keep a stable ETag", func(t *testing.T) {
		// Guard the guard. A validator that changed every call would pass
		// every assertion above and destroy caching entirely — which is
		// exactly what the fs backend's random-suffix ETag did.
		a := middleware.VariantETag(base.hash, "col", base.size, base.modNano)
		b := middleware.VariantETag(base.hash, "col", base.size, base.modNano)
		if a != b {
			t.Error("ETag is not stable across calls for unchanged bytes; " +
				"revalidation would never hit and every request re-sends the body")
		}
	})

	t.Run("different variants of one object differ", func(t *testing.T) {
		if middleware.VariantETag(base.hash, "col", base.size, base.modNano) ==
			middleware.VariantETag(base.hash, "hires", base.size, base.modNano) {
			t.Error("col and hires share an ETag; a client could be served one " +
				"for the other")
		}
	})
}

// TestStaleValidatorNoLongerShortCircuits is the regression proper: the
// exact request the old middleware answered 304 must now return bytes.
func TestStaleValidatorNoLongerShortCircuits(t *testing.T) {
	store := map[string]storedBytes{
		testAssetID + "|col": {hash: "newhash0123456789", size: 12248, modNano: 2_000},
	}
	h := middleware.VariantCache(validatorFor(store))(okHandler("NEW-BYTES"))

	t.Run("the old path-derived validator no longer matches", func(t *testing.T) {
		// This is verbatim what a browser cached before the fix.
		rr := get(t, h, variantURL, `"`+variantURL+`"`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — a client holding the pre-fix ETag "+
				"is still being told its stale copy is current (#620)", rr.Code)
		}
		if rr.Body.String() != "NEW-BYTES" {
			t.Errorf("body = %q, want the new bytes", rr.Body.String())
		}
	})

	t.Run("a validator matching the CURRENT bytes still 304s", func(t *testing.T) {
		current := `"` + middleware.VariantETag("newhash0123456789", "col", 12248, 2_000) + `"`
		rr := get(t, h, variantURL, current)
		if rr.Code != http.StatusNotModified {
			t.Errorf("status = %d, want 304 — caching must still work, or the "+
				"fix has just disabled it", rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Error("304 carried a body")
		}
	})
}

func TestCacheControlIsNotImmutable(t *testing.T) {
	store := map[string]storedBytes{
		testAssetID + "|col":      {hash: "h0123456789abcdef", size: 10, modNano: 1},
		testAssetID + "|original": {hash: "h0123456789abcdef", size: 10, modNano: 1},
	}
	h := middleware.VariantCache(validatorFor(store))(okHandler("BYTES"))

	for _, url := range []string{variantURL, fileURL} {
		rr := get(t, h, url, "")
		cc := rr.Header().Get("Cache-Control")
		if cc == "" {
			t.Fatalf("%s: no Cache-Control", url)
		}
		// `immutable` licenses the client to skip revalidation for the
		// whole max-age, which makes a correct ETag inert — the client
		// never asks. Both halves are required.
		if containsToken(cc, "immutable") {
			t.Errorf("%s: Cache-Control = %q still contains `immutable`; the "+
				"validator would be correct and never consulted", url, cc)
		}
		if rr.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag emitted", url)
		}
	}
}

// TestErrorsStillEmitNoStore — an ungenerated variant must not be
// cached, or a worker that has not run yet poisons the client.
func TestErrorsStillEmitNoStore(t *testing.T) {
	store := map[string]storedBytes{
		testAssetID + "|col": {hash: "h0123456789abcdef", size: 10, modNano: 1},
	}
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		h := middleware.VariantCache(validatorFor(store))(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
		rr := get(t, h, variantURL, "")
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store, max-age=0" {
			t.Errorf("status %d: Cache-Control = %q, want no-store", code, cc)
		}
	}
}

// TestUnresolvableVariantMakesNoCachingClaim — when the validator
// cannot be computed the middleware must fall through rather than
// inventing a weaker one, which is how the original defect was born.
func TestUnresolvableVariantMakesNoCachingClaim(t *testing.T) {
	h := middleware.VariantCache(validatorFor(map[string]storedBytes{}))(okHandler("BYTES"))
	rr := get(t, h, variantURL, `"anything"`)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — an unresolvable validator must never 304", rr.Code)
	}
	if rr.Header().Get("ETag") != "" {
		t.Error("emitted an ETag without being able to validate the bytes")
	}
}

// TestNonAssetPathsUntouched — the middleware runs globally.
func TestNonAssetPathsUntouched(t *testing.T) {
	h := middleware.VariantCache(validatorFor(map[string]storedBytes{}))(okHandler("X"))
	for _, url := range []string{
		"/api/v1/assets",
		"/api/v1/posts",
		// Genuinely content-addressed, and deliberately NOT handled here
		// — see the PR: derived variants under a stable object hash can
		// still be re-rendered, so this route is not immutable either.
		"/api/v1/storage/objects/abc/variants/col",
	} {
		rr := get(t, h, url, "")
		if rr.Header().Get("Cache-Control") != "" {
			t.Errorf("%s: middleware set Cache-Control on an unrelated path", url)
		}
	}
}

func containsToken(header, token string) bool {
	for i := 0; i+len(token) <= len(header); i++ {
		if header[i:i+len(token)] == token {
			return true
		}
	}
	return false
}
