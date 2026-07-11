// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"strings"
	"testing"
)

func TestIsTextExt(t *testing.T) {
	for _, ext := range []string{"txt", ".txt", "TXT", ".TXT"} {
		if !isTextExt(ext) {
			t.Errorf("isTextExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"md", "json", "yaml", "doc", "", ".txt.bak"} {
		if isTextExt(ext) {
			t.Errorf("isTextExt(%q) = true, want false", ext)
		}
	}
}

func TestIsEPSExt(t *testing.T) {
	for _, ext := range []string{"eps", ".eps", "EPS", "ps", ".ps"} {
		if !isEPSExt(ext) {
			t.Errorf("isEPSExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"pdf", "epub", "", "txt"} {
		if isEPSExt(ext) {
			t.Errorf("isEPSExt(%q) = true, want false", ext)
		}
	}
}

func TestIsPSDExt(t *testing.T) {
	for _, ext := range []string{"psd", ".psd", "PSD", "psb"} {
		if !isPSDExt(ext) {
			t.Errorf("isPSDExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"png", "jpg", "", "psdx"} {
		if isPSDExt(ext) {
			t.Errorf("isPSDExt(%q) = true, want false", ext)
		}
	}
}

func TestSanitizeLine_StripsControlChars(t *testing.T) {
	// Backspace, bell, NUL — must vanish so the bitmap font doesn't
	// render replacement boxes.
	in := "hello\x00\x07world\b"
	got := sanitizeLine(in, 100)
	if got != "helloworld" {
		t.Errorf("sanitizeLine = %q, want helloworld", got)
	}
}

func TestSanitizeLine_TabsBecomeSpaces(t *testing.T) {
	got := sanitizeLine("a\tb", 100)
	if got != "a    b" {
		t.Errorf("sanitizeLine = %q, want 'a    b' (tab → 4 spaces)", got)
	}
}

func TestSanitizeLine_DropsCR(t *testing.T) {
	got := sanitizeLine("dos\r\nlf", 100)
	// scanner already splits on LF; CR must be stripped so the line
	// isn't rendered with a trailing box.
	if strings.ContainsRune(got, '\r') {
		t.Errorf("sanitizeLine kept CR: %q", got)
	}
}

func TestSanitizeLine_TruncatesWithEllipsis(t *testing.T) {
	got := sanitizeLine("hello world this is too long", 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output %q missing ellipsis suffix", got)
	}
	if len(got) > len("helloworl…") {
		t.Errorf("truncated output exceeded budget: %q (%d bytes)", got, len(got))
	}
}

func TestSanitizeLine_PassesThroughPrintableUnicode(t *testing.T) {
	got := sanitizeLine("café — naïve", 100)
	if got != "café — naïve" {
		t.Errorf("sanitizeLine mangled unicode: got %q", got)
	}
}

// readPreview returns up to PreviewLines, IsTruncated when the file
// has more lines than the cap, and counts every input line in
// LineCount (not just the previewed subset).
func TestReadPreview_TruncationContract(t *testing.T) {
	h := &TextHandler{PreviewLines: 3, CardWidth: 700}
	body := []byte("line 1\nline 2\nline 3\nline 4\nline 5\n")
	lines, meta := h.readPreview(body)
	if len(lines) != 3 {
		t.Errorf("preview lines = %d, want 3", len(lines))
	}
	if meta.LineCount != 5 {
		t.Errorf("LineCount = %d, want 5", meta.LineCount)
	}
	if !meta.IsTruncated {
		t.Error("IsTruncated = false; expected true after exceeding cap")
	}
	if meta.ByteCount != int64(len(body)) {
		t.Errorf("ByteCount = %d, want %d", meta.ByteCount, len(body))
	}
}

func TestReadPreview_ShortFileNotTruncated(t *testing.T) {
	h := &TextHandler{PreviewLines: 10, CardWidth: 700}
	lines, meta := h.readPreview([]byte("only one\n"))
	if len(lines) != 1 {
		t.Errorf("lines = %d, want 1", len(lines))
	}
	if meta.IsTruncated {
		t.Error("IsTruncated = true on a non-truncated file")
	}
}
