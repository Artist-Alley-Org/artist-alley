// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package iiif_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// stubLookup serves a fixed asset row. The test stamps in the
// pixel dimensions + file hash; missing IDs fall through to
// ErrAssetNotFound.
type stubLookup struct {
	byID map[uuid.UUID]iiif.IIIFAsset
}

func (s stubLookup) GetIIIFAsset(_ context.Context, id uuid.UUID, _ visibility.Caller) (iiif.IIIFAsset, error) {
	if a, ok := s.byID[id]; ok {
		return a, nil
	}
	return iiif.IIIFAsset{}, iiif.ErrAssetNotFound
}

type stubVariants struct{ list []iiif.VariantSize }

func (s stubVariants) ListIIIFVariants(_ context.Context) ([]iiif.VariantSize, error) {
	return s.list, nil
}

// stubStreamer returns a recorded body for the matching (hash,
// variant) pair. The "no row" case returns an error so the
// handler emits 404.
type stubStreamer struct {
	hash, key string
	body      string
	mimeType  string
}

func (s stubStreamer) OpenVariant(_ context.Context, hash, key string) (io.ReadCloser, int64, string, error) {
	if hash != s.hash || key != s.key {
		return nil, 0, "", io.ErrUnexpectedEOF
	}
	return io.NopCloser(strings.NewReader(s.body)), int64(len(s.body)), s.mimeType, nil
}

func newRouterFor(t *testing.T, h *iiif.Handler) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func defaultIIIFHandler(t *testing.T, asset iiif.IIIFAsset) *iiif.Handler {
	t.Helper()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	return &iiif.Handler{
		Lookup:   stubLookup{byID: map[uuid.UUID]iiif.IIIFAsset{id: asset}},
		Variants: stubVariants{list: defaultVariants()},
		Streamer: stubStreamer{hash: asset.FileHash, key: "hires", body: "WEBPBYTES", mimeType: "image/webp"},
	}
}

func TestInfoJSON_ServesExpectedShape(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{
		FileHash: "abc", HasImage: true, PixelWidth: 6000, PixelHeight: 4000,
	})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"http://test.example.com/iiif/3/11111111-1111-1111-1111-111111111111/info.json", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/ld+json") {
		t.Errorf("Content-Type = %q, want application/ld+json", ct)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("missing CORS header")
	}
	var got iiif.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if got.Width != 6000 || got.Height != 4000 {
		t.Errorf("dims = %dx%d, want 6000x4000", got.Width, got.Height)
	}
	if got.Profile != "level0" || got.Type != "ImageService3" {
		t.Errorf("profile/type wrong: %+v", got)
	}
	if !strings.HasPrefix(got.ID, "http://test.example.com/iiif/3/11111111") {
		t.Errorf("ID = %q, expected absolute URL based on request host", got.ID)
	}
}

func TestInfoJSON_404OnMissingAsset(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{FileHash: "abc", HasImage: true, PixelWidth: 100, PixelHeight: 100})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/22222222-2222-2222-2222-222222222222/info.json", nil))
	if rr.Code != 404 {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestInfoJSON_BadID_400(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/iiif/3/not-a-uuid/info.json", nil))
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestInfoJSON_AssetWithNoPixelInfo_404(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{FileHash: "abc", HasImage: true})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/11111111-1111-1111-1111-111111111111/info.json", nil))
	if rr.Code != 404 {
		t.Errorf("status = %d, want 404 (no pixel info on file)", rr.Code)
	}
}

func TestServeImage_HappyPath(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{
		FileHash: "abc", HasImage: true, PixelWidth: 6000, PixelHeight: 4000,
	})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/11111111-1111-1111-1111-111111111111/full/max/0/default.webp", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "WEBPBYTES" {
		t.Errorf("body = %q, want WEBPBYTES", got)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/webp" {
		t.Errorf("Content-Type = %q, want image/webp", ct)
	}
}

func TestServeImage_SizeNotAdvertised_501(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{
		FileHash: "abc", HasImage: true, PixelWidth: 6000, PixelHeight: 4000,
	})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/11111111-1111-1111-1111-111111111111/full/500,333/0/default.webp", nil))
	if rr.Code != 501 {
		t.Errorf("status = %d, want 501 (Not Implemented, IIIF §4.5)", rr.Code)
	}
}

func TestServeImage_BadRotation_400(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{
		FileHash: "abc", HasImage: true, PixelWidth: 1000, PixelHeight: 1000,
	})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/11111111-1111-1111-1111-111111111111/full/max/45/default.webp", nil))
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400 (rotation outside Level 0)", rr.Code)
	}
}

func TestServeImage_ForwardedProtoHostHonored(t *testing.T) {
	h := defaultIIIFHandler(t, iiif.IIIFAsset{
		FileHash: "abc", HasImage: true, PixelWidth: 1000, PixelHeight: 1000,
	})
	router := newRouterFor(t, h)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET",
		"http://test.example.com/iiif/3/11111111-1111-1111-1111-111111111111/info.json", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "art.production.example.com")
	router.ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var got iiif.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(got.ID, "https://art.production.example.com/iiif/3/") {
		t.Errorf("ID = %q, expected reverse-proxy-aware URL", got.ID)
	}
}
