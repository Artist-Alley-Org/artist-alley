package archive

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

// bundleAssertions walks `bundle` and checks that the expected names
// are present with the expected bodies. Order isn't asserted — zip
// readers index by name.
func bundleAssertions(t *testing.T, bundle []byte, want map[string]string, wantDirs []string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	gotFiles := map[string]string{}
	gotDirs := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			gotDirs[f.Name] = true
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %q: %v", f.Name, err)
		}
		gotFiles[f.Name] = string(body)
	}
	for name, body := range want {
		if g, ok := gotFiles[name]; !ok {
			t.Errorf("bundle missing file %q (have files: %v, dirs: %v)", name, keys(gotFiles), keys(gotDirs))
		} else if g != body {
			t.Errorf("bundle %q body = %q, want %q", name, g, body)
		}
	}
	for _, d := range wantDirs {
		if !gotDirs[d] {
			t.Errorf("bundle missing dir %q", d)
		}
	}
}

func TestWriteBundleZip_FromZip(t *testing.T) {
	src, _ := buildTestZip(t)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	if err := WriteBundleZipReaderAt(bytes.NewReader(src), int64(len(src)), "zip", zw); err != nil {
		t.Fatalf("WriteBundleZipReaderAt: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zw close: %v", err)
	}
	bundleAssertions(t, out.Bytes(), map[string]string{
		"hello.txt":        "hello from zip\n",
		"nested/inner.txt": "deep file body",
	}, []string{"nested/"})
}

func TestWriteBundleZip_FromTar(t *testing.T) {
	src, _ := buildTestTar(t)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	if err := WriteBundleZip(bytes.NewReader(src), "tar", zw); err != nil {
		t.Fatalf("WriteBundleZip tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zw close: %v", err)
	}
	bundleAssertions(t, out.Bytes(), map[string]string{
		"hello.txt":     "tar hello body\n",
		"sub/inner.bin": "tar nested entry",
	}, []string{"sub/"})
}

func TestWriteBundleZip_From7z(t *testing.T) {
	f, _ := openSevenZipFixture(t)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := WriteBundleZipReaderAt(f, st.Size(), "7z", zw); err != nil {
		t.Fatalf("WriteBundleZipReaderAt 7z: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zw close: %v", err)
	}
	bundleAssertions(t, out.Bytes(), map[string]string{
		sevenZipEntryHello:  sevenZipHelloBody,
		sevenZipEntryNested: sevenZipNestedBody,
	}, nil)
}

func TestWriteBundleZip_FromRar(t *testing.T) {
	f := openRarFixture(t)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	if err := WriteBundleZip(f, "rar", zw); err != nil {
		t.Fatalf("WriteBundleZip rar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zw close: %v", err)
	}
	bundleAssertions(t, out.Bytes(), map[string]string{
		rarEntryHello:  rarHelloBody,
		rarEntryNested: rarNestedBody,
	}, nil)
}

// WriteBundleZip refuses to handle ReaderAt-only formats; the typed
// error guards against an accidental dispatch swap that would send
// a ZIP source down the streaming path.
func TestWriteBundleZip_RejectsReaderAtFormats(t *testing.T) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	defer zw.Close()
	for _, format := range []string{"zip", "7z"} {
		if err := WriteBundleZip(bytes.NewReader(nil), format, zw); err == nil {
			t.Errorf("WriteBundleZip(%q) returned nil, want error", format)
		}
	}
}

func TestWriteBundleZipReaderAt_RejectsStreamFormats(t *testing.T) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	defer zw.Close()
	if err := WriteBundleZipReaderAt(bytes.NewReader(nil), 0, "tar", zw); err == nil {
		t.Error("WriteBundleZipReaderAt(tar) returned nil, want error")
	}
}
