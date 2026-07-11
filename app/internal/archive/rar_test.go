// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package archive

import (
	"io"
	"os"
	"testing"
)

// testdata/sample.rar holds:
//   hello.txt        ("hello from sevenzip\n", 20 bytes — same payload as the 7z fixture)
//   sub/file2.txt    ("nested entry\n", 13 bytes)
//   sub              (directory entry)
const (
	rarFixture       = "testdata/sample.rar"
	rarEntryHello    = "hello.txt"
	rarEntryNested   = "sub/file2.txt"
	rarHelloBody     = "hello from sevenzip\n"
	rarNestedBody    = "nested entry\n"
	rarExpectedFiles = 2
)

func openRarFixture(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(rarFixture)
	if err != nil {
		t.Skipf("testdata fixture missing: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestManifestRAR(t *testing.T) {
	f := openRarFixture(t)
	m, err := ManifestRAR(f)
	if err != nil {
		t.Fatalf("ManifestRAR: %v", err)
	}
	if m.Format != "rar" {
		t.Errorf("Format = %q, want rar", m.Format)
	}
	if len(m.Entries) == 0 {
		t.Fatal("no entries parsed")
	}
	var files int
	var total int64
	got := map[string]Entry{}
	for _, e := range m.Entries {
		got[e.Path] = e
		if !e.IsDir {
			files++
			total += e.Size
		}
	}
	if files != rarExpectedFiles {
		t.Errorf("files = %d, want %d", files, rarExpectedFiles)
	}
	if e, ok := got[rarEntryHello]; !ok {
		t.Errorf("missing %q", rarEntryHello)
	} else if e.Size != int64(len(rarHelloBody)) {
		t.Errorf("hello.txt Size = %d, want %d", e.Size, len(rarHelloBody))
	}
	if e, ok := got[rarEntryNested]; !ok {
		t.Errorf("missing %q", rarEntryNested)
	} else if e.Size != int64(len(rarNestedBody)) {
		t.Errorf("nested Size = %d, want %d", e.Size, len(rarNestedBody))
	}
	if m.TotalSize != total {
		t.Errorf("TotalSize = %d, want %d", m.TotalSize, total)
	}
}

func TestOpenRARStreamEntry(t *testing.T) {
	f := openRarFixture(t)
	rc, hdr, err := OpenRARStreamEntry(f, rarEntryNested)
	if err != nil {
		t.Fatalf("OpenRARStreamEntry: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != rarNestedBody {
		t.Errorf("body = %q, want %q", string(body), rarNestedBody)
	}
	if hdr.UnPackedSize != int64(len(rarNestedBody)) {
		t.Errorf("UnPackedSize = %d", hdr.UnPackedSize)
	}
}

func TestOpenRARStreamEntry_NotFound(t *testing.T) {
	f := openRarFixture(t)
	_, _, err := OpenRARStreamEntry(f, "no-such-file.txt")
	if err == nil {
		t.Fatal("expected error for missing entry")
	}
}
