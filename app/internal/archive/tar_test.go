package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// buildTestTar writes a known-shape TAR to an in-memory buffer. Two
// files + one directory header so the IsDir branch is exercised.
func buildTestTar(t *testing.T) ([]byte, time.Time) {
	t.Helper()
	mod := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	write := func(name string, body []byte, isDir bool) {
		t.Helper()
		hdr := &tar.Header{Name: name, ModTime: mod, Mode: 0o644}
		if isDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(body))
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if !isDir {
			if _, err := w.Write(body); err != nil {
				t.Fatalf("tar body %q: %v", name, err)
			}
		}
	}
	write("hello.txt", []byte("tar hello body\n"), false)
	write("sub/", nil, true)
	write("sub/inner.bin", []byte("tar nested entry"), false)
	if err := w.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes(), mod
}

func assertManifestTAR(t *testing.T, m *Manifest, wantFormat string, wantMod time.Time) {
	t.Helper()
	if m.Format != wantFormat {
		t.Errorf("Format = %q, want %q", m.Format, wantFormat)
	}
	if m.EntryCount != 3 {
		t.Fatalf("EntryCount = %d, want 3", m.EntryCount)
	}
	got := map[string]Entry{}
	for _, e := range m.Entries {
		got[e.Path] = e
	}
	if e, ok := got["hello.txt"]; !ok {
		t.Error("missing hello.txt")
	} else {
		if e.Size != int64(len("tar hello body\n")) {
			t.Errorf("hello.txt Size = %d", e.Size)
		}
		if e.IsDir {
			t.Error("hello.txt flagged as dir")
		}
		if !e.Modified.Equal(wantMod) {
			t.Errorf("hello.txt Modified = %v, want %v", e.Modified, wantMod)
		}
	}
	if e, ok := got["sub/"]; !ok {
		t.Error("missing sub/")
	} else if !e.IsDir {
		t.Error("sub/ not flagged as dir")
	}
	if e, ok := got["sub/inner.bin"]; !ok {
		t.Error("missing sub/inner.bin")
	} else if e.IsDir {
		t.Error("sub/inner.bin flagged as dir")
	}
	wantTotal := int64(len("tar hello body\n") + len("tar nested entry"))
	if m.TotalSize != wantTotal {
		t.Errorf("TotalSize = %d, want %d", m.TotalSize, wantTotal)
	}
}

func TestManifestTAR_Raw(t *testing.T) {
	raw, mod := buildTestTar(t)
	m, err := ManifestTAR(bytes.NewReader(raw), "")
	if err != nil {
		t.Fatalf("ManifestTAR raw: %v", err)
	}
	assertManifestTAR(t, m, "tar", mod)
}

func TestManifestTAR_Gz(t *testing.T) {
	raw, mod := buildTestTar(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	m, err := ManifestTAR(bytes.NewReader(buf.Bytes()), "gz")
	if err != nil {
		t.Fatalf("ManifestTAR gz: %v", err)
	}
	assertManifestTAR(t, m, "tar.gz", mod)
}

// Bzip2 wrapping (no encoder in stdlib so we can't round-trip a real
// stream cheaply). Verify the wrapper at least handles malformed
// input without crashing: ManifestTAR is designed to never return
// mid-stream errors — it marks the manifest Truncated and returns
// what it could parse. Garbage in → empty truncated manifest out.
func TestManifestTAR_Bz2_MalformedInput(t *testing.T) {
	m, err := ManifestTAR(bytes.NewReader([]byte("not bzip2")), "bz2")
	if err != nil {
		t.Fatalf("ManifestTAR bz2 garbage: unexpected error %v", err)
	}
	if len(m.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(m.Entries))
	}
	if !m.Truncated {
		t.Error("expected Truncated = true for malformed input")
	}
}

func TestManifestTAR_Xz(t *testing.T) {
	raw, mod := buildTestTar(t)
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	if _, err := xw.Write(raw); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := xw.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	m, err := ManifestTAR(bytes.NewReader(buf.Bytes()), "xz")
	if err != nil {
		t.Fatalf("ManifestTAR xz: %v", err)
	}
	assertManifestTAR(t, m, "tar.xz", mod)
}

func TestManifestTAR_UnknownCompression(t *testing.T) {
	raw, _ := buildTestTar(t)
	_, err := ManifestTAR(bytes.NewReader(raw), "lz4")
	if err == nil {
		t.Fatal("expected error for unknown compression")
	}
}

func TestOpenTARStreamEntry(t *testing.T) {
	raw, _ := buildTestTar(t)
	rc, hdr, err := OpenTARStreamEntry(bytes.NewReader(raw), "", "sub/inner.bin")
	if err != nil {
		t.Fatalf("OpenTARStreamEntry: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "tar nested entry" {
		t.Errorf("body = %q", string(body))
	}
	if hdr.Size != int64(len("tar nested entry")) {
		t.Errorf("hdr.Size = %d", hdr.Size)
	}
}

func TestOpenTARStreamEntry_Gz(t *testing.T) {
	raw, _ := buildTestTar(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(raw)
	_ = gz.Close()
	rc, _, err := OpenTARStreamEntry(bytes.NewReader(buf.Bytes()), "gz", "hello.txt")
	if err != nil {
		t.Fatalf("OpenTARStreamEntry gz: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "tar hello body\n" {
		t.Errorf("body = %q", string(body))
	}
}

func TestOpenTARStreamEntry_NotFound(t *testing.T) {
	raw, _ := buildTestTar(t)
	_, _, err := OpenTARStreamEntry(bytes.NewReader(raw), "", "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing entry")
	}
}
