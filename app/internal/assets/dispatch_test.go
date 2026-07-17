// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// assetTypeFor decides which row in asset_types a new upload lands
// under — drives the kind-routing in the AssetViewer + the side-panel
// tool inventory. The Phase-9 sprite alt-files commit (9fd523d) is
// what makes this dispatcher load-bearing: it added Font / Comic /
// Ebook / Audiobook / Texture / Sprite / Code recognition on top of
// the bare Image / Video / Audio / 3D / Archive primitives.
//
// Type refs are seeded by migrations 00027 + 00031 + 00033 + 00034
// and the constants here mirror those:
const (
	typeImage     = 1
	typeDocument  = 2
	typeVideo     = 3
	typeAudio     = 4
	type3D        = 5
	typeArchive   = 6
	typeFont      = 7
	typeComic     = 8
	typeEbook     = 10
	typeAudiobook = 11
	typeTexture   = 12
	typeSprite    = 13
	typeCode      = 14
)

func TestAssetTypeFor_Fonts(t *testing.T) {
	for _, ext := range []string{"ttf", ".ttf", "OTF", "ttc", "otc", "woff", "woff2"} {
		if got := assetTypeFor(ext); got != typeFont {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Font)", ext, got, typeFont)
		}
	}
}

func TestAssetTypeFor_Comics(t *testing.T) {
	// Comic extensions MUST resolve to Comic, not Archive — the
	// kind-routing in the AssetViewer picks the comic reader on this
	// signal alone, and a wrong return here would render a comic as
	// a generic zip browser.
	for _, ext := range []string{"cbr", "cbz", "cb7"} {
		if got := assetTypeFor(ext); got != typeComic {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Comic)", ext, got, typeComic)
		}
	}
}

func TestAssetTypeFor_Ebooks(t *testing.T) {
	for _, ext := range []string{"epub", "mobi", "azw", "azw3", "fb2", "lit", "prc", "pdb"} {
		if got := assetTypeFor(ext); got != typeEbook {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Ebook)", ext, got, typeEbook)
		}
	}
}

func TestAssetTypeFor_Audiobooks(t *testing.T) {
	// m4b + aax MUST resolve to Audiobook (not Audio) so the
	// audiobook reader mounts instead of the generic audio player.
	for _, ext := range []string{"m4b", "aax", "M4B"} {
		if got := assetTypeFor(ext); got != typeAudiobook {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Audiobook)", ext, got, typeAudiobook)
		}
	}
}

func TestAssetTypeFor_Textures(t *testing.T) {
	for _, ext := range []string{"dds", "ktx", "ktx2", "basis", "sbsar", "sbs"} {
		if got := assetTypeFor(ext); got != typeTexture {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Texture)", ext, got, typeTexture)
		}
	}
}

func TestAssetTypeFor_Sprites(t *testing.T) {
	for _, ext := range []string{"aseprite", "ase", "pyxel"} {
		if got := assetTypeFor(ext); got != typeSprite {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Sprite)", ext, got, typeSprite)
		}
	}
}

func TestAssetTypeFor_Code(t *testing.T) {
	// One per language family — full surface would be tedious.
	for _, ext := range []string{
		"py", "js", "ts", "go", "rs", "cpp", "h",
		"sh", "lua", "gd", "tres", "mel", "hlsl", "glsl",
	} {
		if got := assetTypeFor(ext); got != typeCode {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Code)", ext, got, typeCode)
		}
	}
}

// Editor-source files (psd/ai/sketch/etc.) MUST land as Image
// alongside finished raster outputs — the AssetViewer renders them
// with the ImageView fallback, NOT a custom Photoshop viewer.
func TestAssetTypeFor_EditorSourcesAreImage(t *testing.T) {
	for _, ext := range []string{"psd", "psb", "ai", "sketch", "fig", "xd", "eps", "cdr", "afdesign", "afphoto", "afpub", "clip", "ora", "kra"} {
		if got := assetTypeFor(ext); got != typeImage {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Image)", ext, got, typeImage)
		}
	}
}

func TestAssetTypeFor_RegularImages(t *testing.T) {
	for _, ext := range []string{"jpg", "jpeg", "png", "gif", "webp"} {
		if got := assetTypeFor(ext); got != typeImage {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Image)", ext, got, typeImage)
		}
	}
}

// videoExts grew in 9fd523d to cover camera + broadcast formats
// (.mts / .m2ts AVCHD, .lrv GoPro proxy, .insv Insta360, etc.).
// A regression that drops these silently sends them down the
// raster path instead of preview.video.
//
// Note: "ts" appears in BOTH videoExts (MPEG transport stream) AND
// the Code switch (TypeScript). The Code switch fires first, so
// `.ts` resolves to Code. That's a deliberate tradeoff — TypeScript
// is the common case in this app's expected uploads — but it means
// MPEG-TS uploads need an explicit asset-type override. Captured
// here as expected behaviour rather than a video-side assertion.
func TestAssetTypeFor_CameraAndBroadcastVideo(t *testing.T) {
	for _, ext := range []string{"mp4", "mov", "mkv", "webm", "avi",
		"m4v", "lrv", "insv", "mts", "m2ts", "vob", "f4v", "mxf"} {
		if got := assetTypeFor(ext); got != typeVideo {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Video)", ext, got, typeVideo)
		}
	}
}

// Documented ambiguity: `.ts` resolves to Code, not Video. Pinning
// this so a later "fix" doesn't silently flip the routing.
func TestAssetTypeFor_TS_ResolvesToCode(t *testing.T) {
	if got := assetTypeFor("ts"); got != typeCode {
		t.Errorf("assetTypeFor(\"ts\") = %d, want %d (Code — TypeScript wins over MPEG-TS)", got, typeCode)
	}
}

func TestAssetTypeFor_3D(t *testing.T) {
	for _, ext := range []string{"glb", "gltf", "fbx", "obj", "blend", "stl"} {
		if got := assetTypeFor(ext); got != type3D {
			t.Errorf("assetTypeFor(%q) = %d, want %d (3D)", ext, got, type3D)
		}
	}
}

func TestAssetTypeFor_ArchiveOnlyWhenNotComic(t *testing.T) {
	// zip itself stays Archive — only the cb? branded versions are
	// hijacked for Comic detection.
	for _, ext := range []string{"zip", "rar", "7z", "tar", "tgz"} {
		if got := assetTypeFor(ext); got != typeArchive {
			t.Errorf("assetTypeFor(%q) = %d, want %d (Archive)", ext, got, typeArchive)
		}
	}
}

func TestAssetTypeFor_Unknown(t *testing.T) {
	for _, ext := range []string{"", "weirdformat", "unknown"} {
		if got := assetTypeFor(ext); got != 0 {
			t.Errorf("assetTypeFor(%q) = %d, want 0 (unset)", ext, got)
		}
	}
}

// jobTypeForExt picks the preview-pipeline worker for an upload. A
// regression here silently routes work to the wrong handler (e.g.,
// a video to the raster-thumbnail path), which fails late and
// confusingly — the asset's processing_status stays "processing"
// while the wrong worker burns retries.
func TestJobTypeForExt(t *testing.T) {
	cases := []struct {
		ext  string
		want jobs.JobType
	}{
		{"mp4", jobs.TypePreviewVideo},
		{"glb", jobs.TypePreview3D},
		{"mp3", jobs.TypePreviewAudio},
		{"pdf", jobs.TypePreviewPDF},
		{"ttf", jobs.TypePreviewFont},
		{"epub", jobs.TypePreviewEbook},
		{"eps", jobs.TypePreviewEPS},
		{"psd", jobs.TypePreviewPSD},
		{"cbz", jobs.TypePreviewComic},
		{"txt", jobs.TypePreviewText},
		{"zip", jobs.TypePreviewArchive},
		{"7z", jobs.TypePreviewArchive},
		{"rar", jobs.TypePreviewArchive},
		{"png", jobs.TypePreviewRaster},     // image fallback
		{"unknown", jobs.TypePreviewRaster}, // unknown fallback
	}
	for _, c := range cases {
		ext := c.ext
		got := jobTypeForExt(&ext)
		if got != c.want {
			t.Errorf("jobTypeForExt(%q) = %q, want %q", c.ext, got, c.want)
		}
	}
}

func TestJobTypeForExt_NilDefault(t *testing.T) {
	if got := jobTypeForExt(nil); got != jobs.TypePreviewRaster {
		t.Errorf("jobTypeForExt(nil) = %q, want %q", got, jobs.TypePreviewRaster)
	}
}

// isImageExt is what the upload path uses to decide whether to
// compute a thumbhash inline (image) vs defer to a job (everything
// else). Cheap pure dispatcher worth a quick pin.
func TestIsImageExt(t *testing.T) {
	for _, ext := range []string{"jpg", ".png", "WEBP", "gif"} {
		e := ext
		if !isImageExt(&e) {
			t.Errorf("isImageExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"mp4", "pdf", "zip", ""} {
		e := ext
		if isImageExt(&e) {
			t.Errorf("isImageExt(%q) = true, want false", ext)
		}
	}
	if isImageExt(nil) {
		t.Error("isImageExt(nil) = true, want false")
	}
}

// needsProcessing decides whether an upload requires a preview job
// at all. Anything that returns true must round-trip through
// jobTypeForExt to a real job type (we don't want needsProcessing
// to say "yes" then leave the worker pool with nothing to do).
func TestNeedsProcessing_RoundtripsToRealJobType(t *testing.T) {
	exts := []string{
		"jpg", "mp4", "glb", "mp3", "pdf", "ttf",
		"epub", "eps", "psd", "cbz", "txt", "zip",
	}
	for _, ext := range exts {
		e := ext
		if !needsProcessing(&e) {
			t.Errorf("needsProcessing(%q) = false; upload would skip the job pool", ext)
			continue
		}
		// And the job type it routes to must be something a real
		// handler is registered for (any non-raster preview type is
		// fine; raster is the fallback).
		if jobTypeForExt(&e) == "" {
			t.Errorf("needsProcessing(%q) = true but jobTypeForExt returns empty", ext)
		}
	}
}
