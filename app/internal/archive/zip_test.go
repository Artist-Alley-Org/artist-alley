package archive

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
	"time"
)

// buildTestZip returns an in-memory ZIP with a known shape so the
// parser tests don't need any disk fixture. Two files + one explicit
// directory entry exercises the IsDir branches; the timestamp lets
// the test assert the parser preserves Modified.
func buildTestZip(t *testing.T) ([]byte, time.Time) {
	t.Helper()
	mod := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, body string, isDir bool) {
		t.Helper()
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
		if isDir {
			hdr.Method = zip.Store
		}
		fw, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip CreateHeader %q: %v", name, err)
		}
		if !isDir {
			if _, err := fw.Write([]byte(body)); err != nil {
				t.Fatalf("zip write %q: %v", name, err)
			}
		}
	}
	add("hello.txt", "hello from zip\n", false)
	add("nested/", "", true)
	add("nested/inner.txt", "deep file body", false)
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes(), mod
}

func TestManifestZIP(t *testing.T) {
	raw, mod := buildTestZip(t)
	m, err := ManifestZIP(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("ManifestZIP: %v", err)
	}
	if m.Format != "zip" {
		t.Errorf("Format = %q, want zip", m.Format)
	}
	if m.EntryCount != 3 || len(m.Entries) != 3 {
		t.Fatalf("EntryCount/len mismatch: %d / %d", m.EntryCount, len(m.Entries))
	}
	got := map[string]Entry{}
	for _, e := range m.Entries {
		got[e.Path] = e
	}
	if e, ok := got["hello.txt"]; !ok {
		t.Error("missing hello.txt")
	} else {
		if e.Size != int64(len("hello from zip\n")) {
			t.Errorf("hello.txt Size = %d", e.Size)
		}
		if e.IsDir {
			t.Error("hello.txt IsDir = true")
		}
		if !e.Modified.Equal(mod) {
			t.Errorf("hello.txt Modified = %v, want %v", e.Modified, mod)
		}
	}
	if e, ok := got["nested/"]; !ok {
		t.Error("missing nested/")
	} else if !e.IsDir {
		t.Error("nested/ not flagged as dir")
	}
	if e, ok := got["nested/inner.txt"]; !ok {
		t.Error("missing nested/inner.txt")
	} else if e.IsDir {
		t.Error("nested/inner.txt IsDir = true")
	}
	// TotalSize sums non-dir uncompressed bytes.
	wantTotal := int64(len("hello from zip\n") + len("deep file body"))
	if m.TotalSize != wantTotal {
		t.Errorf("TotalSize = %d, want %d", m.TotalSize, wantTotal)
	}
	if m.Truncated {
		t.Error("Truncated = true for small archive")
	}
}

func TestOpenZIPEntry(t *testing.T) {
	raw, _ := buildTestZip(t)
	rc, hdr, err := OpenZIPEntry(bytes.NewReader(raw), int64(len(raw)), "nested/inner.txt")
	if err != nil {
		t.Fatalf("OpenZIPEntry: %v", err)
	}
	if rc == nil {
		t.Fatal("nil reader for non-dir entry")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "deep file body" {
		t.Errorf("body = %q", string(body))
	}
	if hdr.UncompressedSize64 != uint64(len("deep file body")) {
		t.Errorf("UncompressedSize64 = %d", hdr.UncompressedSize64)
	}
}

// OpenZIPEntry on a directory returns (nil, hdr, nil) so the caller
// can distinguish "found a folder" from "entry missing".
func TestOpenZIPEntry_Directory(t *testing.T) {
	raw, _ := buildTestZip(t)
	rc, hdr, err := OpenZIPEntry(bytes.NewReader(raw), int64(len(raw)), "nested/")
	if err != nil {
		t.Fatalf("OpenZIPEntry dir: %v", err)
	}
	if rc != nil {
		t.Error("expected nil reader for directory entry")
		rc.Close()
	}
	if hdr == nil {
		t.Error("expected non-nil header for directory entry")
	}
}

func TestOpenZIPEntry_NotFound(t *testing.T) {
	raw, _ := buildTestZip(t)
	_, _, err := OpenZIPEntry(bytes.NewReader(raw), int64(len(raw)), "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing entry")
	}
}
