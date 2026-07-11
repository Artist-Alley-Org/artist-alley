// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package iiif implements the IIIF Image API 3.0 Level 0 surface
// on top of the existing variant pipeline. Two endpoints:
//
//   - GET /iiif/3/{asset_id}/info.json — Image Information Document
//     describing the asset's pixel dimensions + the pre-baked sizes
//     the install can serve.
//
//   - GET /iiif/3/{asset_id}/{region}/{size}/{rotation}/{quality}.{format}
//     — Image Request URL. Level 0 = only the pre-baked sizes
//     (full / max / w, / ,h / w,h) at region=full, rotation=0,
//     quality=default. The handler resolves the request to one of
//     our PreviewVariant rows + streams the bytes via the existing
//     storage service.
//
// Why Level 0 + not Level 2: every IIIF viewer (Mirador, Universal
// Viewer, Clover, etc.) is happy with Level 0 + a sizes block; we
// get pan/zoom UX for free without an on-the-fly resampler. Higher
// levels add arbitrary regions / rotations / cropping; those land
// in 1.54.B once the substrate is shipped.
//
// Spec references:
//
//   - https://iiif.io/api/image/3.0/
//   - https://iiif.io/api/image/3.0/compliance/#level0 — the exact
//     subset we implement.
package iiif

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Context is the JSON-LD @context every Image API 3.0 response
// carries. Clients pivot on this to detect the version.
const Context = "http://iiif.io/api/image/3/context.json"

// Profile is the compliance-level string in the info.json's
// `profile` field. "level0" matches the surface this package
// ships.
const Profile = "level0"

// ServiceType is the JSON-LD type tag the info.json carries
// (per Image API 3.0 §5.1).
const ServiceType = "ImageService3"

// Protocol is the Image API URL the info.json carries (per
// Image API 3.0 §5.1). Constant — clients pin on this to
// distinguish IIIF Image services from generic JSON resources.
const Protocol = "http://iiif.io/api/image"

// Size names one pre-baked derivative the install can serve.
// Width + height are the actual decoded pixel dimensions —
// computed from the source aspect ratio + the variant's
// max-dim setting, NOT the cap itself.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Info is the Image Information Document the info.json endpoint
// emits. Field order in the marshal output is alphabetic per
// Go's encoding/json; clients tolerate that.
type Info struct {
	Context  string `json:"@context"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Protocol string `json:"protocol"`
	Profile  string `json:"profile"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Sizes    []Size `json:"sizes"`
}

// VariantSize is the (variant-key, max-edge) pair the info.json
// builder uses to compute Size entries. Mirrors
// sysconfig.PreviewVariant without importing sysconfig (keeps
// the iiif package free of that dep edge so tests stay light).
type VariantSize struct {
	Key    string
	MaxDim int
	// Cover variants (the square "col" thumbnail) are excluded
	// from IIIF sizes because the IIIF size grammar speaks in
	// proportional scales, not square crops.
	Cover bool
}

// BuildInfo composes an Info from the asset's source pixel
// dimensions + the list of pre-baked variants. Returns
// ErrUnsupportedAsset when the source has no pixel info on file
// (operator hasn't run the EXIF extractor yet) — clients see
// 404 from the surrounding handler.
//
// id is the absolute IIIF service URL the info.json advertises
// (e.g. "https://art.example.com/iiif/3/asset-uuid"). Callers
// build this from the site's base URL + the asset id.
func BuildInfo(id string, srcW, srcH int, variants []VariantSize) (Info, error) {
	if srcW <= 0 || srcH <= 0 {
		return Info{}, ErrUnsupportedAsset
	}
	sizes := make([]Size, 0, len(variants))
	for _, v := range variants {
		if v.Cover || v.MaxDim <= 0 {
			continue
		}
		w, h := proportionalFit(srcW, srcH, v.MaxDim)
		sizes = append(sizes, Size{Width: w, Height: h})
	}
	// Always include the native size as the largest entry — IIIF
	// clients expect it as the upper bound for max-size scaling.
	sizes = append(sizes, Size{Width: srcW, Height: srcH})

	// De-dup by (w,h) — when a SkipUpscale variant is smaller
	// than the cap it ends up matching the native dimensions.
	sort.Slice(sizes, func(i, j int) bool {
		if sizes[i].Width != sizes[j].Width {
			return sizes[i].Width < sizes[j].Width
		}
		return sizes[i].Height < sizes[j].Height
	})
	deduped := sizes[:0]
	for i, s := range sizes {
		if i == 0 || sizes[i-1] != s {
			deduped = append(deduped, s)
		}
	}

	return Info{
		Context:  Context,
		ID:       id,
		Type:     ServiceType,
		Protocol: Protocol,
		Profile:  Profile,
		Width:    srcW,
		Height:   srcH,
		Sizes:    deduped,
	}, nil
}

// proportionalFit returns the (width, height) of a contain-fit
// scaling of (srcW, srcH) into a box whose longest edge is
// maxDim. Mirrors the preview pipeline's "longest-side cap" rule;
// returns the source dims unchanged when already smaller (the
// SkipUpscale path).
func proportionalFit(srcW, srcH, maxDim int) (int, int) {
	longest := srcW
	if srcH > longest {
		longest = srcH
	}
	if longest <= maxDim {
		return srcW, srcH
	}
	scale := float64(maxDim) / float64(longest)
	return int(math.Round(float64(srcW) * scale)),
		int(math.Round(float64(srcH) * scale))
}

// ErrUnsupportedAsset is returned by BuildInfo when the asset
// has no source pixel dimensions. Callers map to 404 so peers
// don't probe for "is this an IIIF resource" by hitting random
// uuids.
var ErrUnsupportedAsset = errors.New("iiif: asset has no pixel dimensions on file")

// ---------------------------------------------------------------------------
// Image-request URL grammar (Level 0)
// ---------------------------------------------------------------------------

// ImageRequest is the parsed shape of one IIIF Image URL:
// /{region}/{size}/{rotation}/{quality}.{format}.
type ImageRequest struct {
	// Region — Level 0 only accepts "full".
	Region string

	// Size — one of:
	//   - "max"   → return native size (largest available)
	//   - "full"  → deprecated alias for max, still accepted
	//   - "w,"    → width-constrained, height proportional
	//   - ",h"    → height-constrained, width proportional
	//   - "w,h"   → exact pixel dimensions; Level 0 only when
	//              the pair matches one of the advertised sizes
	Size string

	// Rotation — Level 0 only accepts "0".
	Rotation string

	// Quality — Level 0 only accepts "default".
	Quality string

	// Format — file extension after the final dot. Level 0 only
	// accepts a format the install actually serves (currently
	// "webp"; "jpg" + "png" land when on-the-fly transcoding does).
	Format string
}

// ParseImageRequest decodes the /{region}/{size}/{rotation}/{quality}.{format}
// tail into an ImageRequest. Returns ErrBadRequest on any
// malformed shape; surrounding handler maps to HTTP 400.
func ParseImageRequest(region, size, rotation, qualityDotFormat string) (ImageRequest, error) {
	if region == "" || size == "" || rotation == "" || qualityDotFormat == "" {
		return ImageRequest{}, ErrBadRequest
	}
	dot := strings.LastIndexByte(qualityDotFormat, '.')
	if dot <= 0 || dot == len(qualityDotFormat)-1 {
		return ImageRequest{}, ErrBadRequest
	}
	return ImageRequest{
		Region:   region,
		Size:     size,
		Rotation: rotation,
		Quality:  qualityDotFormat[:dot],
		Format:   strings.ToLower(qualityDotFormat[dot+1:]),
	}, nil
}

// ErrBadRequest is returned by ParseImageRequest for malformed
// URLs + by Resolve for requests outside the Level 0 subset.
var ErrBadRequest = errors.New("iiif: malformed image request")

// ErrSizeNotAvailable is returned by Resolve when the request's
// size doesn't match any advertised pre-baked size. Distinct
// from ErrBadRequest so the handler can return 501 (compliance
// with §4.5 "Not Implemented") instead of 400.
var ErrSizeNotAvailable = errors.New("iiif: size not advertised; Level 0 only serves the sizes in info.json")

// VariantMatch describes one pre-baked variant the resolver can
// return for an image request. Width/height are the actual
// pixel dimensions of THAT variant given the source aspect ratio.
type VariantMatch struct {
	Variant VariantSize
	Width   int
	Height  int
}

// Resolve picks the pre-baked variant that satisfies r, given
// the source dimensions + the install's variant catalogue.
// Implements Level 0 grammar:
//
//   - region   must be "full" or "square" (square only when a
//     cover-fit variant exists, e.g. our "col" 320²).
//   - size     must be max | full | w, | ,h | w,h matching an
//     advertised size (cover variants match w=h cases).
//   - rotation must be "0".
//   - quality  must be "default".
//   - format   is currently "webp" only; "jpg"+"png" become valid
//     once an on-the-fly transcoder lands.
func Resolve(r ImageRequest, srcW, srcH int, variants []VariantSize) (VariantMatch, error) {
	if r.Rotation != "0" {
		return VariantMatch{}, ErrBadRequest
	}
	if r.Quality != "default" {
		return VariantMatch{}, ErrBadRequest
	}
	if r.Format != "webp" {
		return VariantMatch{}, ErrBadRequest
	}

	// region pre-filter — Level 0 only honours "full" + "square".
	switch r.Region {
	case "full":
		// nominal; size handler picks the contain-fit variant.
	case "square":
		// Square mode requires a cover variant.
		for _, v := range variants {
			if v.Cover && v.MaxDim > 0 && sizeMatchesSquare(r.Size, v.MaxDim) {
				return VariantMatch{Variant: v, Width: v.MaxDim, Height: v.MaxDim}, nil
			}
		}
		return VariantMatch{}, ErrSizeNotAvailable
	default:
		return VariantMatch{}, ErrBadRequest
	}

	// region=full from here on. Special-case max/full first:
	// IIIF clients use "max" to request "the largest the install
	// can serve". We map that to the largest contain-fit variant
	// (typically "hires"), regardless of whether the source is
	// larger than its cap. Returning ErrSizeNotAvailable for
	// max would surprise every viewer.
	if r.Size == "max" || r.Size == "full" {
		largest := largestContain(variants)
		if largest.MaxDim == 0 {
			return VariantMatch{}, ErrSizeNotAvailable
		}
		w, h := proportionalFit(srcW, srcH, largest.MaxDim)
		return VariantMatch{Variant: largest, Width: w, Height: h}, nil
	}
	wantW, wantH, err := parseSize(r.Size, srcW, srcH)
	if err != nil {
		return VariantMatch{}, err
	}

	for _, v := range variants {
		if v.Cover || v.MaxDim <= 0 {
			continue
		}
		vw, vh := proportionalFit(srcW, srcH, v.MaxDim)
		if vw == wantW && vh == wantH {
			return VariantMatch{Variant: v, Width: vw, Height: vh}, nil
		}
	}
	// No advertised size matches. Original is NOT served via the
	// IIIF surface (clients that want it use the asset's /file
	// endpoint).
	return VariantMatch{}, ErrSizeNotAvailable
}

// largestContain returns the contain-fit variant with the
// biggest MaxDim. Zero value when none exist.
func largestContain(variants []VariantSize) VariantSize {
	var best VariantSize
	for _, v := range variants {
		if v.Cover || v.MaxDim <= 0 {
			continue
		}
		if v.MaxDim > best.MaxDim {
			best = v
		}
	}
	return best
}

// parseSize decodes the IIIF size grammar within Level 0:
// max | full | w, | ,h | w,h.
func parseSize(s string, srcW, srcH int) (int, int, error) {
	if s == "max" || s == "full" {
		return srcW, srcH, nil
	}
	// w, → width only
	if strings.HasSuffix(s, ",") {
		w, err := parsePosInt(strings.TrimSuffix(s, ","))
		if err != nil {
			return 0, 0, ErrBadRequest
		}
		h := int(math.Round(float64(srcH) * float64(w) / float64(srcW)))
		return w, h, nil
	}
	// ,h → height only
	if strings.HasPrefix(s, ",") {
		h, err := parsePosInt(strings.TrimPrefix(s, ","))
		if err != nil {
			return 0, 0, ErrBadRequest
		}
		w := int(math.Round(float64(srcW) * float64(h) / float64(srcH)))
		return w, h, nil
	}
	// w,h → exact
	comma := strings.IndexByte(s, ',')
	if comma <= 0 || comma == len(s)-1 {
		return 0, 0, ErrBadRequest
	}
	w, err := parsePosInt(s[:comma])
	if err != nil {
		return 0, 0, ErrBadRequest
	}
	h, err := parsePosInt(s[comma+1:])
	if err != nil {
		return 0, 0, ErrBadRequest
	}
	return w, h, nil
}

// sizeMatchesSquare reports whether the size token resolves to
// the cover variant's edge (e.g. "320,320" or "max" against the
// 320² col variant).
func sizeMatchesSquare(s string, edge int) bool {
	if s == "max" || s == "full" {
		return true
	}
	if s == fmt.Sprintf("%d,%d", edge, edge) {
		return true
	}
	if s == fmt.Sprintf("%d,", edge) || s == fmt.Sprintf(",%d", edge) {
		return true
	}
	return false
}

func parsePosInt(s string) (int, error) {
	if s == "" {
		return 0, ErrBadRequest
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrBadRequest
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0, ErrBadRequest
		}
	}
	if n == 0 {
		return 0, ErrBadRequest
	}
	return n, nil
}
