package archive

import (
	"io"
	"os"
	"testing"
)

// testdata/sample.7z is a tiny p7zip-built archive — see
// testdata/README.md for the generation recipe. Layout:
//   payload/hello.txt        ("hello from sevenzip\n", 20 bytes)
//   payload/sub/file2.txt    ("nested entry\n",         13 bytes)
const (
	sevenZipFixture        = "testdata/sample.7z"
	sevenZipEntryHello     = "payload/hello.txt"
	sevenZipEntryNested    = "payload/sub/file2.txt"
	sevenZipHelloBody      = "hello from sevenzip\n"
	sevenZipNestedBody     = "nested entry\n"
	sevenZipExpectedEntry  = 3 // 1 root dir + 2 files (sevenzip lists payload/ and sub/ as dirs)
	sevenZipExpectedFiles  = 2
	sevenZipExpectedDirs   = 1
)

func openSevenZipFixture(t *testing.T) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(sevenZipFixture)
	if err != nil {
		t.Skipf("testdata fixture missing: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatalf("stat: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f, st.Size()
}

func TestManifestSevenZip(t *testing.T) {
	f, size := openSevenZipFixture(t)
	m, err := ManifestSevenZip(f, size)
	if err != nil {
		t.Fatalf("ManifestSevenZip: %v", err)
	}
	if m.Format != "7z" {
		t.Errorf("Format = %q, want 7z", m.Format)
	}
	if len(m.Entries) == 0 {
		t.Fatal("no entries parsed")
	}
	var files, dirs int
	var totalBody int64
	got := map[string]Entry{}
	for _, e := range m.Entries {
		got[e.Path] = e
		if e.IsDir {
			dirs++
		} else {
			files++
			totalBody += e.Size
		}
	}
	if files != sevenZipExpectedFiles {
		t.Errorf("files = %d, want %d", files, sevenZipExpectedFiles)
	}
	if e, ok := got[sevenZipEntryHello]; !ok {
		t.Errorf("missing %q (got: %v)", sevenZipEntryHello, keys(got))
	} else if e.Size != int64(len(sevenZipHelloBody)) {
		t.Errorf("hello.txt Size = %d, want %d", e.Size, len(sevenZipHelloBody))
	}
	if e, ok := got[sevenZipEntryNested]; !ok {
		t.Errorf("missing %q", sevenZipEntryNested)
	} else if e.Size != int64(len(sevenZipNestedBody)) {
		t.Errorf("nested Size = %d, want %d", e.Size, len(sevenZipNestedBody))
	}
	if m.TotalSize != totalBody {
		t.Errorf("TotalSize = %d, want %d", m.TotalSize, totalBody)
	}
}

func TestOpenSevenZipEntry(t *testing.T) {
	f, size := openSevenZipFixture(t)
	rc, hdr, err := OpenSevenZipEntry(f, size, sevenZipEntryHello)
	if err != nil {
		t.Fatalf("OpenSevenZipEntry: %v", err)
	}
	if rc == nil {
		t.Fatal("nil reader for file entry")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != sevenZipHelloBody {
		t.Errorf("body = %q, want %q", string(body), sevenZipHelloBody)
	}
	if hdr.UncompressedSize != uint64(len(sevenZipHelloBody)) {
		t.Errorf("UncompressedSize = %d", hdr.UncompressedSize)
	}
}

func TestOpenSevenZipEntry_NotFound(t *testing.T) {
	f, size := openSevenZipFixture(t)
	_, _, err := OpenSevenZipEntry(f, size, "no-such-file.txt")
	if err == nil {
		t.Fatal("expected error for missing entry")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
