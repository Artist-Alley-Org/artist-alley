// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1595: the ingest base name an asset keeps (provenance) and the
// archival-1 originalFilename a receiver accepts (checkBasename) are one
// contract. Every name the upload path accepts, mapped through
// provenance.ArchivalOriginalFilename, must be a valid originalFilename
// on a real file entry; nothing but an upload_request name is exported;
// and a path is never exported, only its base name.
//
// There is no production archival emitter yet (#1574 owns runtime
// emission), so this pins the mapping it will use rather than a wire run.

package federation

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/provenance"
)

func TestArchivalOriginalFilename_ContractWithIngestProvenance(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	size := int64(3)
	file := func(name string) ArchivalFile {
		return ArchivalFile{
			Role:             "original",
			ContentURI:       "cas://" + hash,
			Checksums:        []ArchivalChecksum{{Algorithm: "sha256", Value: hash}},
			Size:             &size,
			MediaType:        "image/png",
			OriginalFilename: name,
		}
	}
	if err := file("").validate("files[0]"); err != nil {
		t.Fatalf("fixture entry without a name is invalid: %v", err)
	}

	headers := []string{
		"photo.jpg",
		"%E5%86%99%E7%9C%9F%20%C3%A9t%C3%A9.png",
		"/home/bob/art/sketch.psd",
		`C:\Users\bob\sketch.psd`,
		"a%2Fb%5Cc.tif",
		"100%25.jpg",
		strings.Repeat("n", provenance.MaxBasenameBytes),
		"dir/" + strings.Repeat("é", 127) + "x",
	}
	for _, h := range headers {
		name, err := provenance.FromUploadHeader(h)
		if err != nil {
			t.Fatalf("FromUploadHeader(%q): %v", h, err)
		}
		exported := provenance.ArchivalOriginalFilename(&name, provenance.SourceUploadRequest)
		if exported != name {
			t.Fatalf("upload_request name %q exported as %q", name, exported)
		}
		if strings.ContainsAny(exported, `/\`) {
			t.Fatalf("exported a path: %q", exported)
		}
		if err := checkBasename("originalFilename", exported); err != nil {
			t.Fatalf("checkBasename refuses accepted ingest name %q: %v", exported, err)
		}
		if err := file(exported).validate("files[0]"); err != nil {
			t.Fatalf("file entry with ingest name %q is invalid: %v", exported, err)
		}
		// The same name from any other source is not exported at all.
		for _, src := range []provenance.Source{provenance.SourceClientReported, provenance.SourceUnknown} {
			if got := provenance.ArchivalOriginalFilename(&name, src); got != "" {
				t.Fatalf("%s name exported as originalFilename %q", src, got)
			}
		}
	}

	// Values the receiver refuses are values ingest refuses too.
	for _, bad := range []string{"..", ".", "a\x01b", strings.Repeat("z", 256)} {
		if checkBasename("originalFilename", bad) == nil {
			t.Fatalf("fixture %q unexpectedly valid for the receiver", bad)
		}
		if _, err := provenance.Basename(bad); err == nil {
			t.Fatalf("ingest accepts %q, which archival-1 refuses", bad)
		}
	}
}
