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

// VariantCache emits `Cache-Control: public, max-age=31536000, immutable`
// + a stable ETag on successful variant + file responses (content is
// hash-addressed → bytes never change for a given URL). 404s and 5xxs
// get `Cache-Control: no-store, max-age=0` so a worker that hasn't
// generated a variant yet doesn't poison the browser cache for a
// year.
//
// We wrap the ResponseWriter so the header decision happens after
// the handler picks a status code.
func VariantCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !variantPath.MatchString(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// If the client already has it, short-circuit before the
		// handler runs to skip the work entirely.
		etag := `"` + r.URL.Path + `"`
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		ww := &cachingWriter{ResponseWriter: w, etag: etag}
		next.ServeHTTP(ww, r)
	})
}

// cachingWriter writes the appropriate Cache-Control header at
// WriteHeader time, based on the chosen status.
type cachingWriter struct {
	http.ResponseWriter
	etag        string
	wroteHeader bool
}

func (c *cachingWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.wroteHeader = true
		h := c.ResponseWriter.Header()
		if code == http.StatusOK || code == http.StatusPartialContent {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			h.Set("ETag", c.etag)
		} else {
			// Don't poison the cache on miss / error.
			h.Set("Cache-Control", "no-store, max-age=0")
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *cachingWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}
