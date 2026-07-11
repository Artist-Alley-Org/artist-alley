// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// IsComicPage decides which entry in a comic archive counts as a
// renderable page. A bug here either picks the wrong cover (e.g.
// __MACOSX/.Thumbs.db sorts to position 0) or skips real pages.
func TestIsComicPage(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Happy paths — typical CBZ contents.
		{"page-001.jpg", true},
		{"01-cover.png", true},
		{"chapter 1/page_002.JPEG", true},
		{"art/scan.webp", true},
		{"intro.bmp", true},
		{"transition.gif", true},
		// OS / metadata noise that historically sorted to position 0.
		{".DS_Store", false},
		{".thumbs/page1.jpg", true}, // hidden folder but the page name isn't itself dotted
		{"._cover.jpg", false},       // macOS resource fork
		{"__MACOSX/cover.jpg", false},
		{"_metadata.json", false},
		{"ComicInfo.xml", false},
		{"Thumbs.db", false},
		// Non-image content types.
		{"notes.txt", false},
		{"page.pdf", false},
		{"", false},
	}
	for _, c := range cases {
		got := isComicPage(c.name)
		if got != c.want {
			t.Errorf("isComicPage(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsComicExt(t *testing.T) {
	for _, ext := range []string{"cbz", ".cbz", "CBZ", "cbr", "cb7"} {
		if !isComicExt(ext) {
			t.Errorf("isComicExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"zip", "rar", "7z", "pdf", "", "txt"} {
		if isComicExt(ext) {
			t.Errorf("isComicExt(%q) = true, want false", ext)
		}
	}
}

// extractCoverZIP pulls the alphabetically-first page out of a CBZ
// (which is just a renamed ZIP). The fan-cover-to-ladder step is
// the integration test; this exercise stays in-memory.
func TestExtractCoverZIP_FirstPageWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.cbz")
	writeFixtureCBZ(t, path, map[string]bool{
		"page-002.png": true, // marker — this should NOT be picked
		"page-001.png": true, // this should be the cover
		"page-003.png": true,
		"__MACOSX/page-000.png": true, // would sort first but must be skipped
		"ComicInfo.xml":         false, // not an image
	})

	h := &ComicHandler{}
	meta, cover, err := h.extractCoverZIP(path)
	if err != nil {
		t.Fatalf("extractCoverZIP: %v", err)
	}
	if meta.ArchiveKind != "zip" {
		t.Errorf("ArchiveKind = %q", meta.ArchiveKind)
	}
	if meta.CoverPath != "page-001.png" {
		t.Errorf("CoverPath = %q, want page-001.png", meta.CoverPath)
	}
	if meta.PageCount != 3 {
		t.Errorf("PageCount = %d, want 3 (3 valid images, __MACOSX excluded)", meta.PageCount)
	}
	if len(cover) == 0 {
		t.Fatal("cover bytes empty")
	}
	if _, err := png.Decode(bytes.NewReader(cover)); err != nil {
		t.Errorf("cover bytes not a valid PNG: %v", err)
	}
}

func TestExtractCoverZIP_EmptyArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.cbz")
	writeFixtureCBZ(t, path, nil)

	h := &ComicHandler{}
	meta, cover, err := h.extractCoverZIP(path)
	if err != nil {
		t.Fatalf("extractCoverZIP: %v", err)
	}
	if cover != nil {
		t.Errorf("expected nil cover for empty archive, got %d bytes", len(cover))
	}
	if meta.PageCount != 0 {
		t.Errorf("PageCount = %d, want 0", meta.PageCount)
	}
}

func TestExtractCoverZIP_OnlyNoise(t *testing.T) {
	// Every entry is excluded by isComicPage — must return cleanly.
	dir := t.TempDir()
	path := filepath.Join(dir, "noise.cbz")
	writeFixtureCBZ(t, path, map[string]bool{
		"ComicInfo.xml":          false,
		"Thumbs.db":              false,
		"__MACOSX/page.jpg":      true, // skipped by macOS-prefix rule
	})
	h := &ComicHandler{}
	_, cover, err := h.extractCoverZIP(path)
	if err != nil {
		t.Fatalf("extractCoverZIP: %v", err)
	}
	if cover != nil {
		t.Errorf("expected nil cover, got %d bytes", len(cover))
	}
}

// writeFixtureCBZ writes a tiny CBZ to disk. The map's value tells
// us whether to fill the entry with valid PNG bytes (true) or a
// metadata stub (false) — only useful for separating which entries
// would decode as images vs which are just garbage.
func writeFixtureCBZ(t *testing.T, path string, entries map[string]bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, isImage := range entries {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip add %s: %v", name, err)
		}
		if isImage {
			img := image.NewRGBA(image.Rect(0, 0, 4, 4))
			for i := range img.Pix {
				img.Pix[i] = 0x80
			}
			img.Set(0, 0, color.RGBA{255, 0, 0, 255})
			if err := png.Encode(fw, img); err != nil {
				t.Fatalf("png encode %s: %v", name, err)
			}
		} else {
			if _, err := fw.Write([]byte("stub")); err != nil {
				t.Fatalf("write stub: %v", err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}
