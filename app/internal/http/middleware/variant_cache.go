package middleware

import (
	"net/http"
	"regexp"
)

// variantPath matches /api/v1/assets/{uuid}/variants/{key} and
// /api/v1/assets/{uuid}/file. Both are content-addressed under the
// asset's file_hash — the bytes never change for a given URL — so we
// can ship aggressive cache headers.
var variantPath = regexp.MustCompile(`^/api/v1/assets/[0-9a-fA-F-]{36}/(variants/[A-Za-z0-9._-][A-Za-z0-9._/-]{0,254}|file)/?$`)

// VariantCache attaches `Cache-Control: public, max-age=31536000, immutable`
// to responses for asset variant + file endpoints, plus a stable ETag
// derived from the request path so conditional GETs short-circuit.
//
// Pre-emptive: headers go in before the handler runs so they survive
// any 200 response shape (the openapi codegen sets its own Content-
// Type after this middleware exits).
//
// 200 only: 404s shouldn't be cached for a year. We negotiate by
// setting Cache-Control on the *request* and clearing it on errors
// would be ugly; instead the variant 404 handler sets a short
// `Cache-Control: no-store` to override.
func VariantCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && variantPath.MatchString(r.URL.Path) {
			h := w.Header()
			// 1 year, marked immutable so well-behaved clients don't
			// even revalidate. The URL is content-addressed so
			// guaranteed-stable.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			// ETag is the request path itself — stable per (asset,
			// variant), which is what we want. Browsers + CDNs will
			// 304 on a cached match.
			h.Set("ETag", `"`+r.URL.Path+`"`)
			// If the client already has it, short-circuit.
			if match := r.Header.Get("If-None-Match"); match == `"`+r.URL.Path+`"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
