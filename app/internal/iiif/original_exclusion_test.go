// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1573: IIIF serves derivatives only. Original bytes need an explicit
// grant (spec v1 §7.5.7) and must not be reachable through any IIIF
// size, even when the operator's variant catalogue carries an entry
// under the original's storage key, which nothing in the preview
// configuration forbids.

package iiif_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

func TestOriginalVariantKey_IsTheStorageOriginal(t *testing.T) {
	if iiif.OriginalVariantKey != storage.VariantOriginal {
		t.Fatalf("iiif.OriginalVariantKey = %q, storage.VariantOriginal = %q", iiif.OriginalVariantKey, storage.VariantOriginal)
	}
}

// catalogueWithOriginal is the shipped ladder plus a contain variant
// keyed as the original and LARGER than every real one, so any size
// rule that can pick it, would.
func catalogueWithOriginal() []iiif.VariantSize {
	return append(defaultVariants(), iiif.VariantSize{Key: iiif.OriginalVariantKey, MaxDim: 100000})
}

func TestResolve_NeverPicksTheOriginal(t *testing.T) {
	const w, h = 6000, 4000
	sizes := []string{
		"max", "full", "^max",
		"6000,", ",4000", "6000,4000", // the native size info.json advertises
		"4096,", "1920,", "1024,", "320,",
	}
	for _, region := range []string{"full", "square"} {
		for _, size := range sizes {
			req := iiif.ImageRequest{Region: region, Size: size, Rotation: "0", Quality: "default", Format: "webp"}
			m, err := iiif.Resolve(req, w, h, catalogueWithOriginal())
			if err == nil && m.Variant.Key == iiif.OriginalVariantKey {
				t.Errorf("region=%s size=%s resolved to the original", region, size)
			}
		}
	}
	// max is the largest DERIVATIVE.
	m, err := iiif.Resolve(iiif.ImageRequest{Region: "full", Size: "max", Rotation: "0", Quality: "default", Format: "webp"},
		w, h, catalogueWithOriginal())
	if err != nil || m.Variant.Key != "hires" {
		t.Fatalf("max = %+v, %v; want hires", m, err)
	}
	// A catalogue holding only the original serves nothing.
	if _, err := iiif.Resolve(iiif.ImageRequest{Region: "full", Size: "max", Rotation: "0", Quality: "default", Format: "webp"},
		w, h, []iiif.VariantSize{{Key: iiif.OriginalVariantKey, MaxDim: 100000}}); err == nil {
		t.Fatal("an original-only catalogue resolved a size")
	}
}

func TestBuildInfo_DoesNotAdvertiseTheOriginalVariant(t *testing.T) {
	with, err := iiif.BuildInfo("id", 6000, 4000, catalogueWithOriginal())
	if err != nil {
		t.Fatal(err)
	}
	without, err := iiif.BuildInfo("id", 6000, 4000, defaultVariants())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(with.Sizes)
	b, _ := json.Marshal(without.Sizes)
	if string(a) != string(b) {
		t.Fatalf("sizes with an original-keyed variant = %s, without = %s", a, b)
	}
}

// recordingStreamer records every key the handler asks for and serves
// a marker body for any of them.
type recordingStreamer struct{ keys *[]string }

func (s recordingStreamer) OpenVariant(_ context.Context, _, key string) (io.ReadCloser, int64, string, error) {
	*s.keys = append(*s.keys, key)
	body := "DERIVATIVE:" + key
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), "image/webp", nil
}

// Through the production handler: no IIIF URL makes it open the
// original's storage key, and an asset whose only stored bytes are the
// original has no IIIF image at all.
func TestServeImage_NeverStreamsTheOriginal(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	run := func(t *testing.T, stored []string, path string) (int, string, []string) {
		t.Helper()
		var keys []string
		h := &iiif.Handler{
			Lookup: stubLookup{byID: map[uuid.UUID]iiif.IIIFAsset{id: {
				FileHash: "abc", VariantKeys: stored, PixelWidth: 6000, PixelHeight: 4000,
			}}},
			Variants: stubVariants{list: catalogueWithOriginal()},
			Streamer: recordingStreamer{keys: &keys},
			Content:  allowAllContent{},
		}
		rr := httptest.NewRecorder()
		newRouterFor(t, h).ServeHTTP(rr, httptest.NewRequest("GET", "/iiif/3/"+id.String()+path, nil))
		return rr.Code, rr.Body.String(), keys
	}

	all := []string{"original", "col", "preview", "screen", "hires"}
	for _, path := range []string{
		"/full/max/0/default.webp",
		"/full/full/0/default.webp",
		"/full/6000,/0/default.webp",
		"/full/6000,4000/0/default.webp",
	} {
		code, body, keys := run(t, all, path)
		for _, k := range keys {
			if k == iiif.OriginalVariantKey {
				t.Errorf("%s: handler opened the original (status %d, body %q)", path, code, body)
			}
		}
	}

	// Only the original stored: not servable.
	if code, body, keys := run(t, []string{"original"}, "/full/max/0/default.webp"); code != 404 || len(keys) != 0 {
		t.Errorf("original-only asset: status %d body %q opened %v; want 404 and nothing opened", code, body, keys)
	}
}
