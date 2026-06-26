package iiif_test

import (
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/iiif"
)

// defaultVariants mirrors the 4-tier preview ladder shipped by
// sysconfig.DefaultPreviewConfig — col (320² cover), preview
// (1024 contain), screen (1920 contain), hires (4096 contain).
func defaultVariants() []iiif.VariantSize {
	return []iiif.VariantSize{
		{Key: "col", MaxDim: 320, Cover: true},
		{Key: "preview", MaxDim: 1024},
		{Key: "screen", MaxDim: 1920},
		{Key: "hires", MaxDim: 4096},
	}
}

func TestBuildInfo_LargeImage_AllSizes(t *testing.T) {
	info, err := iiif.BuildInfo("https://example.com/iiif/3/abc", 6000, 4000, defaultVariants())
	if err != nil {
		t.Fatalf("BuildInfo: %v", err)
	}
	if info.Width != 6000 || info.Height != 4000 {
		t.Errorf("dimensions = %dx%d, want 6000x4000", info.Width, info.Height)
	}
	if info.Context != iiif.Context || info.Profile != iiif.Profile {
		t.Errorf("@context/profile wrong: %+v", info)
	}
	// preview/screen/hires + native = 4 entries (col excluded).
	if len(info.Sizes) != 4 {
		t.Fatalf("sizes len = %d, want 4 (preview/screen/hires/native); got %+v", len(info.Sizes), info.Sizes)
	}
	// Computed widths from contain-fit at MaxDim against the
	// 6000x4000 source: 1024→683, 1920→1280, 4096→2730, native 6000x4000.
	want := []iiif.Size{
		{Width: 1024, Height: 683},
		{Width: 1920, Height: 1280},
		{Width: 4096, Height: 2731},
		{Width: 6000, Height: 4000},
	}
	for i, s := range info.Sizes {
		if s.Width != want[i].Width || s.Height != want[i].Height {
			t.Errorf("size[%d] = %+v, want %+v (full=%+v)", i, s, want[i], info.Sizes)
		}
	}
}

func TestBuildInfo_SmallImage_SkipsUpscaledDupes(t *testing.T) {
	// 800x600 source — preview/screen/hires all skip upscale →
	// each computes to the native 800x600. BuildInfo de-dupes so
	// the sizes list ends up with one entry.
	info, err := iiif.BuildInfo("https://x/iiif/3/x", 800, 600, defaultVariants())
	if err != nil {
		t.Fatalf("BuildInfo: %v", err)
	}
	if len(info.Sizes) != 1 || info.Sizes[0] != (iiif.Size{Width: 800, Height: 600}) {
		t.Errorf("sizes should de-dup to one native entry; got %+v", info.Sizes)
	}
}

func TestBuildInfo_NoDimensions_Error(t *testing.T) {
	if _, err := iiif.BuildInfo("https://x/y", 0, 0, defaultVariants()); !errors.Is(err, iiif.ErrUnsupportedAsset) {
		t.Errorf("expected ErrUnsupportedAsset, got %v", err)
	}
}

func TestParseImageRequest(t *testing.T) {
	cases := []struct {
		name              string
		region, size, rot string
		qDotFmt           string
		wantErr           bool
		wantQ, wantF      string
	}{
		{"happy", "full", "max", "0", "default.webp", false, "default", "webp"},
		{"square crop", "square", "320,320", "0", "default.webp", false, "default", "webp"},
		{"empty region", "", "max", "0", "default.webp", true, "", ""},
		{"no dot", "full", "max", "0", "defaultwebp", true, "", ""},
		{"trailing dot", "full", "max", "0", "default.", true, "", ""},
		{"upper-case format normalised", "full", "max", "0", "default.WEBP", false, "default", "webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := iiif.ParseImageRequest(tc.region, tc.size, tc.rot, tc.qDotFmt)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && (r.Quality != tc.wantQ || r.Format != tc.wantF) {
				t.Errorf("got q=%q f=%q, want q=%q f=%q", r.Quality, r.Format, tc.wantQ, tc.wantF)
			}
		})
	}
}

func TestResolve_FullMaxPicksLargestContainVariant(t *testing.T) {
	r := iiif.ImageRequest{Region: "full", Size: "max", Rotation: "0", Quality: "default", Format: "webp"}
	m, err := iiif.Resolve(r, 6000, 4000, defaultVariants())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Variant.Key != "hires" || m.Width != 4096 || m.Height != 2731 {
		t.Errorf("got %+v; want hires 4096x2731", m)
	}
}

func TestResolve_WidthOnly_MatchesContainVariant(t *testing.T) {
	r := iiif.ImageRequest{Region: "full", Size: "1024,", Rotation: "0", Quality: "default", Format: "webp"}
	m, err := iiif.Resolve(r, 6000, 4000, defaultVariants())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Variant.Key != "preview" || m.Width != 1024 {
		t.Errorf("got %+v; want preview 1024,", m)
	}
}

func TestResolve_ExactSize_NotAdvertised_501(t *testing.T) {
	// 500,333 isn't in the contain ladder for 6000x4000 → not
	// available at Level 0.
	r := iiif.ImageRequest{Region: "full", Size: "500,333", Rotation: "0", Quality: "default", Format: "webp"}
	if _, err := iiif.Resolve(r, 6000, 4000, defaultVariants()); !errors.Is(err, iiif.ErrSizeNotAvailable) {
		t.Errorf("expected ErrSizeNotAvailable; got %v", err)
	}
}

func TestResolve_Rotation_NonZero_Rejected(t *testing.T) {
	r := iiif.ImageRequest{Region: "full", Size: "max", Rotation: "90", Quality: "default", Format: "webp"}
	if _, err := iiif.Resolve(r, 1000, 1000, defaultVariants()); !errors.Is(err, iiif.ErrBadRequest) {
		t.Errorf("expected ErrBadRequest; got %v", err)
	}
}

func TestResolve_FormatJPG_RejectedAtLevel0(t *testing.T) {
	// Level 0 ships only the formats we actually serve from
	// variants; jpg lands once an on-the-fly transcoder does.
	r := iiif.ImageRequest{Region: "full", Size: "max", Rotation: "0", Quality: "default", Format: "jpg"}
	if _, err := iiif.Resolve(r, 1000, 1000, defaultVariants()); !errors.Is(err, iiif.ErrBadRequest) {
		t.Errorf("expected ErrBadRequest; got %v", err)
	}
}

func TestResolve_SquareCropMatchesCoverVariant(t *testing.T) {
	r := iiif.ImageRequest{Region: "square", Size: "320,320", Rotation: "0", Quality: "default", Format: "webp"}
	m, err := iiif.Resolve(r, 6000, 4000, defaultVariants())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Variant.Key != "col" || m.Width != 320 || m.Height != 320 {
		t.Errorf("got %+v; want col 320x320", m)
	}
}

func TestResolve_SquareCropOnNonCover_ErrSizeNotAvailable(t *testing.T) {
	// Drop the cover variant — square should now refuse.
	noCover := []iiif.VariantSize{
		{Key: "preview", MaxDim: 1024},
	}
	r := iiif.ImageRequest{Region: "square", Size: "max", Rotation: "0", Quality: "default", Format: "webp"}
	if _, err := iiif.Resolve(r, 1000, 1000, noCover); !errors.Is(err, iiif.ErrSizeNotAvailable) {
		t.Errorf("expected ErrSizeNotAvailable; got %v", err)
	}
}
