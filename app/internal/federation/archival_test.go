// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit tests for the archival-1 validators (#1513) that the
// conformance vectors under testdata/conformance/archival-1 do not
// reach: bounded reads, member placement and per-type requirements.

package federation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	e.n += int64(len(p))
	return len(p), nil
}

func listedFile(content string) federation.ArchivalFile {
	sum := sha256.Sum256([]byte(content))
	h := hex.EncodeToString(sum[:])
	size := int64(len(content))
	return federation.ArchivalFile{
		Role: "original", ContentURI: "cas://" + h, Size: &size, MediaType: "text/plain",
		Checksums: []federation.ArchivalChecksum{{Algorithm: "sha256", Value: h}},
	}
}

// An endless stream is a mismatch after size+1 bytes, never read to
// its end.
func TestVerifyFileFixity_BoundedRead(t *testing.T) {
	f := listedFile("aaaa")
	src := &endless{}
	err := federation.VerifyFileFixity(f, src)
	if !errors.Is(err, federation.ErrFixityMismatch) {
		t.Fatalf("err = %v, want ErrFixityMismatch", err)
	}
	if src.n > 64*1024 {
		t.Errorf("read %d bytes from an endless stream; the read must stop after size+1", src.n)
	}
}

func TestVerifyFileFixity_ExactAndShort(t *testing.T) {
	f := listedFile("payload")
	if err := federation.VerifyFileFixity(f, strings.NewReader("payload")); err != nil {
		t.Fatalf("exact bytes: %v", err)
	}
	if err := federation.VerifyFileFixity(f, strings.NewReader("paylo")); !errors.Is(err, federation.ErrFixityMismatch) {
		t.Errorf("short bytes: %v, want ErrFixityMismatch", err)
	}
	noSHA := f
	noSHA.Checksums = []federation.ArchivalChecksum{{Algorithm: "md5", Value: strings.Repeat("0", 32)}}
	if err := federation.VerifyFileFixity(noSHA, strings.NewReader("payload")); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("md5-only fixity: %v, want ErrArchivalInvalid (sha256 is required)", err)
	}
}

func envelopeWith(t *testing.T, typ federation.ActivityType, member, raw string) *federation.Envelope {
	t.Helper()
	env := &federation.Envelope{Type: typ, Object: "https://studio-a.example/assets/x", Extra: map[string]json.RawMessage{}}
	if member != "" {
		env.Extra[member] = json.RawMessage(raw)
	}
	return env
}

func TestValidateArchivalMembers_Placement(t *testing.T) {
	// A member on an activity that carries none.
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityLike, federation.MemberArchivalAsset, `{}`), true); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("aa:asset on Like: %v, want ErrArchivalInvalid", err)
	}
	// A tombstone on Create.
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityCreate, federation.MemberArchivalTombstone, `{}`), true); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("aa:tombstone on Create: %v, want ErrArchivalInvalid", err)
	}
	// Under the profile, aa:AssetVersion and aa:Provenance require their member.
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityAAAssetVersion, "", ""), true); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("bare aa:AssetVersion under archival-1: %v, want ErrArchivalInvalid", err)
	}
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityAAProvenance, "", ""), true); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("bare aa:Provenance under archival-1: %v, want ErrArchivalInvalid", err)
	}
	// Without the profile: an envelope with no archival members is
	// untouched (today's traffic), any member is refused.
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityLike, "", ""), false); err != nil {
		t.Errorf("plain Like without the profile: %v", err)
	}
	if err := federation.ValidateArchivalMembers(envelopeWith(t, federation.ActivityCreate, federation.MemberArchivalAsset, `{}`), false); !errors.Is(err, federation.ErrProfileNotNegotiated) {
		t.Errorf("aa:asset without the profile: %v, want ErrProfileNotNegotiated", err)
	}
	// Trailing data after the member object is rejected.
	if _, err := federation.ParseArchivalAsset(json.RawMessage(`{} {}`)); !errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("trailing data: %v", err)
	}
}

// countingReader records how many bytes were read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// #1513 correction A: VerifyFileFixity validates the complete listed
// file BEFORE reading any byte. Malformed metadata is ErrArchivalInvalid
// and the stream is not touched; it is never a pass, never a fixity
// result.
func TestVerifyFileFixity_RejectsMalformedMetadataWithoutReading(t *testing.T) {
	const content = "payload"
	other := sha256.Sum256([]byte("different bytes"))
	matching := sha256.Sum256([]byte(content))

	cases := map[string]func(f *federation.ArchivalFile){
		// The listed sha256 matches the received bytes but not the
		// CAS URI: the bytes are not the object the URI names.
		"sha256 claim differs from the CAS URI": func(f *federation.ArchivalFile) {
			f.ContentURI = "cas://" + hex.EncodeToString(other[:])
			f.Checksums = []federation.ArchivalChecksum{{Algorithm: "sha256", Value: hex.EncodeToString(matching[:])}}
		},
		"negative size": func(f *federation.ArchivalFile) {
			n := int64(-1)
			f.Size = &n
		},
		"size above 2^53-1": func(f *federation.ArchivalFile) {
			n := int64(federation.ArchivalMaxSafeInteger) + 1
			f.Size = &n
		},
		"size at max int64 (size+1 would overflow)": func(f *federation.ArchivalFile) {
			n := int64(math.MaxInt64)
			f.Size = &n
		},
		"duplicate algorithm": func(f *federation.ArchivalFile) {
			f.Checksums = append(f.Checksums, f.Checksums[0])
		},
		"uppercase hex": func(f *federation.ArchivalFile) {
			f.Checksums[0].Value = strings.ToUpper(f.Checksums[0].Value)
		},
		"unknown role": func(f *federation.ArchivalFile) {
			f.Role = "thumbnail"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := listedFile(content)
			mutate(&f)
			src := &countingReader{r: strings.NewReader(content)}
			err := federation.VerifyFileFixity(f, src)
			if !errors.Is(err, federation.ErrArchivalInvalid) {
				t.Fatalf("err = %v, want ErrArchivalInvalid", err)
			}
			if src.n != 0 {
				t.Errorf("read %d bytes before rejecting malformed metadata; want 0", src.n)
			}
		})
	}
}
