// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"

	"github.com/google/uuid"
)

// variantPath matches /api/v1/assets/{uuid}/variants/{key} and
// /api/v1/assets/{uuid}/file.
//
// THESE URLS ARE ASSET-ADDRESSED, NOT CONTENT-ADDRESSED (#620). The
// UUID is the asset id. An asset keeps its id while its bytes change —
// a preview regeneration, a replace-file, a dataset reseed — so nothing
// about this URL shape licenses an immutable cache.
//
// The previous comment here asserted the opposite ("Both are
// content-addressed under the asset's file_hash — the bytes never
// change for a given URL") and the code took it at its word: it shipped
// `immutable, max-age=31536000` with an ETag of `r.URL.Path`, which is
// invariant by construction, and short-circuited 304 before the handler
// ran. The result was not staleness but PERMANENCE — the server
// confirmed the stale copy without ever consulting the stored bytes, so
// no sequence of requests could return updated content. Verified in a
// browser before the fix: after the stored bytes changed, a request
// sent with `cache: no-store` (which bypasses the browser cache
// entirely and reaches the server) still came back 304.
var variantPath = regexp.MustCompile(`^/api/v1/assets/([0-9a-fA-F-]{36})/(?:variants/([A-Za-z0-9._-][A-Za-z0-9._/-]{0,254})|file)/?$`)

// VariantValidator resolves the identity of the bytes currently stored
// for (assetID, variantKey) — enough to tell a re-render from an
// unchanged file. Returns ok=false when the asset, its file or the
// variant cannot be resolved, in which case the caller emits no
// validator and lets the handler answer.
//
// variantKey is "original" for the /file route.
type VariantValidator func(ctx context.Context, assetID uuid.UUID, variantKey string) (string, bool)

// Cache lifetime for asset-addressed byte routes.
//
// NOT `immutable`, and that is the half of the fix a correct ETag does
// not cover. `immutable` tells the browser not to revalidate AT ALL for
// the lifetime of the entry, so a client holding one would never send
// the conditional request that the new validator exists to answer —
// the ETag would be right and nobody would ask. Correct validator plus
// permission to skip revalidation still equals stale forever.
//
// 60s is a deliberate middle: long enough that a grid of 200 tiles
// scrolling in and out does not reconditional-request every one of
// them, short enough that an operator who regenerates a preview sees it
// within a minute rather than within a year. Revalidation after that is
// cheap — a Stat, not a byte transfer — and a hit returns 304 with no
// body.
const variantCacheControl = "public, max-age=60, must-revalidate"

// VariantCacheControl is the header value the asset-addressed byte
// routes share. Exported so the handlers that set their own headers
// (asset_file, archive_entry) cannot drift from the middleware's
// policy — three sites shipped three different values before this.
func VariantCacheControl() string { return variantCacheControl }

// VariantCache emits a CONTENT-DERIVED ETag and a revalidating
// Cache-Control on successful variant + file responses, and
// `no-store` on 404/5xx so a variant a worker has not generated yet
// cannot poison the cache.
//
// WHY THE VALIDATOR IS THE STORED VARIANT'S IDENTITY, not the asset's
// file_hash. The obvious fix is `file_hash + variant_key`, and it does
// not work for the case that makes this a production defect.
// RecreateAssetPreview re-renders derivatives from the SAME original
// bytes — file_hash is a digest of the original, so it is unchanged by
// definition — and storage.UpsertVariant's ON CONFLICT updates
// size_bytes but not created_at. A validator built from file_hash (or
// from the variant row's created_at) is therefore invariant across
// exactly the operation the operator performs when they want to see new
// output. The stored variant's size + modification time do change when
// it is rewritten, and they need no schema migration to read.
//
// The Stat is the cost the old design existed to avoid. It buys the
// difference between a correct answer and a wrong one, and it is paid
// only where it replaces something more expensive: on a validator hit
// we now skip the byte transfer entirely, which the previous
// short-circuit also did — but correctly.
func VariantCache(validate VariantValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := variantPath.FindStringSubmatch(r.URL.Path)
			if r.Method != http.MethodGet && r.Method != http.MethodHead || m == nil {
				next.ServeHTTP(w, r)
				return
			}
			assetID, err := uuid.Parse(m[1])
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			variantKey := m[2]
			if variantKey == "" {
				// The /file route serves the object's untouched bytes.
				variantKey = "original"
			}

			etag := ""
			if validate != nil {
				if v, ok := validate(r.Context(), assetID, variantKey); ok {
					etag = `"` + v + `"`
				}
			}
			// No validator (asset gone, variant not generated, storage
			// unreachable) means no caching claim at all. Falling back
			// to a weaker-but-present ETag would re-create the original
			// defect in miniature; falling back to none costs a byte
			// transfer and is always correct.
			if etag == "" {
				next.ServeHTTP(w, r)
				return
			}

			if matchesETag(r.Header.Get("If-None-Match"), etag) {
				w.Header().Set("Cache-Control", variantCacheControl)
				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			next.ServeHTTP(&cachingWriter{ResponseWriter: w, etag: etag}, r)
		})
	}
}

// matchesETag implements the If-None-Match comparison for the shapes a
// browser actually sends: a single tag, a comma-separated list after a
// redirect chain, or "*". Weak-comparison per RFC 9110 §8.8.3.2 — the
// W/ prefix is ignored, which is what a conditional GET wants.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := trimWeak(etag)
	start := 0
	for i := 0; i <= len(header); i++ {
		if i == len(header) || header[i] == ',' {
			if trimWeak(trimSpace(header[start:i])) == want {
				return true
			}
			start = i + 1
		}
	}
	return false
}

func trimWeak(s string) string {
	if len(s) >= 2 && s[0] == 'W' && s[1] == '/' {
		return s[2:]
	}
	return s
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// VariantETag builds the validator string from a stored variant's
// identity. Exported so the byte-serving handlers outside the openapi
// surface (asset_file, archive_entry) derive theirs the same way rather
// than each inventing one — three sites invented three different
// path-derived ETags before this.
//
// Hashed rather than concatenated so the value carries no path, no id
// and no size to an untrusted client: an ETag is echoed back by every
// client and logged by every proxy, and there is no reason for it to
// disclose storage layout.
func VariantETag(objectHash, variantKey string, size int64, modUnixNano int64) string {
	sum := sha256.Sum256([]byte(objectHash + "|" + variantKey + "|" +
		strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(modUnixNano, 10)))
	return hex.EncodeToString(sum[:16])
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
			h.Set("Cache-Control", variantCacheControl)
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
