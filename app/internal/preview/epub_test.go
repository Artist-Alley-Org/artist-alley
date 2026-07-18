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

// TestExtractEPUB_MinimalCoverByMeta builds an in-memory EPUB with a
// cover image bound via the EPUB-2-style <meta name="cover" content="ID">
// pattern and confirms we extract title + creator + ISBN + cover bytes.
func TestExtractEPUB_MinimalCoverByMeta(t *testing.T) {
	epubPath := writeFixtureEPUB(t, true, "cover")
	h := &EPUBHandler{}
	meta, cover, err := h.extractEPUB(epubPath)
	if err != nil {
		t.Fatalf("extractEPUB: %v", err)
	}
	if meta.Title != "Test Book" {
		t.Errorf("Title = %q, want %q", meta.Title, "Test Book")
	}
	if meta.Creator != "Jane Doe" {
		t.Errorf("Creator = %q, want %q", meta.Creator, "Jane Doe")
	}
	if meta.ISBN != "9781234567897" {
		t.Errorf("ISBN = %q, want %q", meta.ISBN, "9781234567897")
	}
	if len(meta.Subject) != 2 || meta.Subject[0] != "Fiction" {
		t.Errorf("Subject = %v, want [Fiction Mystery]", meta.Subject)
	}
	if len(cover) == 0 {
		t.Fatalf("cover bytes empty")
	}
	// Verify the cover bytes are a real PNG by decoding.
	if _, err := png.Decode(bytes.NewReader(cover)); err != nil {
		t.Errorf("cover not a valid PNG: %v", err)
	}
}

// TestExtractEPUB_CoverByProperties exercises the EPUB-3 manifest
// `properties="cover-image"` discovery path.
func TestExtractEPUB_CoverByProperties(t *testing.T) {
	epubPath := writeFixtureEPUB(t, false, "properties")
	h := &EPUBHandler{}
	_, cover, err := h.extractEPUB(epubPath)
	if err != nil {
		t.Fatalf("extractEPUB: %v", err)
	}
	if len(cover) == 0 {
		t.Fatalf("cover bytes empty via properties path")
	}
}

// TestExtractEPUB_CoverByFilenameFallback uses neither meta nor
// properties — the cover is just an archive entry named "cover.png".
func TestExtractEPUB_CoverByFilenameFallback(t *testing.T) {
	epubPath := writeFixtureEPUB(t, false, "fallback")
	h := &EPUBHandler{}
	_, cover, err := h.extractEPUB(epubPath)
	if err != nil {
		t.Fatalf("extractEPUB: %v", err)
	}
	if len(cover) == 0 {
		t.Fatalf("cover bytes empty via filename fallback")
	}
}

// TestExtractEPUB_NoCover confirms metadata still parses when the
// archive has no cover image at all.
func TestExtractEPUB_NoCover(t *testing.T) {
	epubPath := writeFixtureEPUB(t, false, "none")
	h := &EPUBHandler{}
	meta, cover, err := h.extractEPUB(epubPath)
	if err != nil {
		t.Fatalf("extractEPUB: %v", err)
	}
	if cover != nil {
		t.Errorf("expected nil cover, got %d bytes", len(cover))
	}
	if meta.Title != "Test Book" {
		t.Errorf("Title = %q, want %q", meta.Title, "Test Book")
	}
}

// writeFixtureEPUB writes a small valid EPUB to a temp dir and
// returns its path. `coverStyle` controls how (or whether) the cover
// is bound: "cover" (meta name=cover), "properties" (EPUB-3 manifest
// properties=cover-image), "fallback" (just a cover.png file), "none"
// (no cover at all).
func writeFixtureEPUB(t *testing.T, useISBNPrefix bool, coverStyle string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fixture.epub")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip add %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}

	// 8×8 magenta PNG as the cover.
	var pngBuf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.NRGBA{255, 0, 200, 255})
		}
	}
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("encode cover: %v", err)
	}
	addBytes := func(name string, b []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip add %s: %v", name, err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}

	add("mimetype", "application/epub+zip")
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0">
  <rootfiles><rootfile media-type="application/oebps-package+xml" full-path="OEBPS/content.opf"/></rootfiles>
</container>`)

	identifier := "9781234567897"
	if useISBNPrefix {
		identifier = "isbn:" + identifier
	}

	// Build manifest items + (optionally) the cover-binding meta tag.
	manifest := `<item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>`
	coverMeta := ""
	switch coverStyle {
	case "cover":
		manifest += `<item id="cov" href="cover.png" media-type="image/png"/>`
		coverMeta = `<meta name="cover" content="cov"/>`
		addBytes("OEBPS/cover.png", pngBuf.Bytes())
	case "properties":
		manifest += `<item id="cov" href="cover.png" media-type="image/png" properties="cover-image"/>`
		addBytes("OEBPS/cover.png", pngBuf.Bytes())
	case "fallback":
		// No manifest cover entry — just dump the cover at a
		// known filename so the readCover fallback path catches it.
		addBytes("OEBPS/cover.png", pngBuf.Bytes())
	case "none":
		// nothing
	}

	add("OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" unique-identifier="id" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Test Book</dc:title>
    <dc:creator>Jane Doe</dc:creator>
    <dc:language>en</dc:language>
    <dc:publisher>Test Press</dc:publisher>
    <dc:date>2026-01-01</dc:date>
    <dc:identifier id="id">`+identifier+`</dc:identifier>
    <dc:subject>Fiction</dc:subject>
    <dc:subject>Mystery</dc:subject>
    `+coverMeta+`
  </metadata>
  <manifest>`+manifest+`</manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`)
	add("OEBPS/ch1.xhtml", `<html><body><h1>Chapter 1</h1><p>Once upon a time…</p></body></html>`)

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return p
}
