// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the DNS-TXT verifier — Phase 1.22.B-c.
// Coverage:
//   - matchesRecord accepts well-formed records + rejects every
//     way they can be malformed (wrong version, wrong directory,
//     wrong token, missing keys)
//   - RecordValue / RecordName round-trip with the parser
//   - Verify uses a fakeResolver to exercise the lookup path
//     without real DNS dependency.
//
// No DB; pure unit tests.

package dnstxt_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation/dnstxt"
)

// fakeResolver feeds canned TXT records.
type fakeResolver struct {
	records map[string][]string
	err     error
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.records[name], nil
}

func TestMatchesRecord_Accepts(t *testing.T) {
	cases := []string{
		"v=aa1; directory=artist-alley.org; token=ed7c91",
		"v=aa1;directory=artist-alley.org;token=ed7c91",            // no spaces
		"v=aa1 ; directory=Artist-Alley.org ; token=ed7c91 ",       // mixed case + spacing in directory
		"v=aa1; directory=artist-alley.org; token=ed7c91; extra=ok", // unknown key ignored
	}
	for _, c := range cases {
		if !dnstxt.MatchesRecord(c, "artist-alley.org", "ed7c91") {
			t.Errorf("expected match for %q", c)
		}
	}
}

func TestMatchesRecord_Rejects(t *testing.T) {
	cases := []struct {
		rec  string
		why  string
	}{
		{"v=aa0; directory=artist-alley.org; token=ed7c91", "wrong version"},
		{"directory=artist-alley.org; token=ed7c91", "missing version"},
		{"v=aa1; directory=other.example; token=ed7c91", "wrong directory"},
		{"v=aa1; directory=artist-alley.org; token=wrong", "wrong token"},
		{"v=aa1; directory=artist-alley.org", "missing token"},
		{"", "empty"},
		{"not a record at all", "garbage"},
	}
	for _, c := range cases {
		if dnstxt.MatchesRecord(c.rec, "artist-alley.org", "ed7c91") {
			t.Errorf("expected reject for %q (%s)", c.rec, c.why)
		}
	}
}

func TestMatchesRecord_TokenCaseSensitive(t *testing.T) {
	// Tokens are hex; case mismatch must NOT match. Catches a
	// bug where we'd ToLower the value alongside the key.
	if dnstxt.MatchesRecord("v=aa1; directory=artist-alley.org; token=ED7C91",
		"artist-alley.org", "ed7c91") {
		t.Error("token comparison should be case-sensitive")
	}
}

func TestVerify_FindsMatchingRecordAmongMany(t *testing.T) {
	r := &fakeResolver{
		records: map[string][]string{
			"_artist-alley.studio-a.example": {
				"v=aa1; directory=other-dir.example; token=zzz",
				"v=aa1; directory=artist-alley.org; token=ed7c91", // <-- the one
				"v=aa1; directory=third.example; token=qqq",
			},
		},
	}
	err := dnstxt.Verify(context.Background(), r, dnstxt.VerifyInput{
		InstanceURL:   "https://studio-a.example",
		DirectoryHost: "artist-alley.org",
		Token:         "ed7c91",
	})
	if err != nil {
		t.Errorf("expected match, got %v", err)
	}
}

func TestVerify_NoMatchingRecord(t *testing.T) {
	r := &fakeResolver{
		records: map[string][]string{
			"_artist-alley.studio-a.example": {
				"v=aa1; directory=different.example; token=ed7c91",
			},
		},
	}
	err := dnstxt.Verify(context.Background(), r, dnstxt.VerifyInput{
		InstanceURL:   "https://studio-a.example",
		DirectoryHost: "artist-alley.org",
		Token:         "ed7c91",
	})
	if !errors.Is(err, dnstxt.ErrNoMatchingRecord) {
		t.Errorf("expected ErrNoMatchingRecord, got %v", err)
	}
}

func TestVerify_LookupError(t *testing.T) {
	r := &fakeResolver{err: errors.New("NXDOMAIN")}
	err := dnstxt.Verify(context.Background(), r, dnstxt.VerifyInput{
		InstanceURL:   "https://studio-a.example",
		DirectoryHost: "artist-alley.org",
		Token:         "ed7c91",
	})
	if !errors.Is(err, dnstxt.ErrLookupFailed) {
		t.Errorf("expected ErrLookupFailed, got %v", err)
	}
}

func TestVerify_BadInstanceURL(t *testing.T) {
	cases := []string{
		"",
		"http://no-tls.example",
		"not a url",
		"https://",
	}
	r := &fakeResolver{}
	for _, u := range cases {
		err := dnstxt.Verify(context.Background(), r, dnstxt.VerifyInput{
			InstanceURL:   u,
			DirectoryHost: "artist-alley.org",
			Token:         "ed7c91",
		})
		if !errors.Is(err, dnstxt.ErrBadInstanceURL) {
			t.Errorf("URL %q: expected ErrBadInstanceURL, got %v", u, err)
		}
	}
}

func TestRecordValueParserRoundTrip(t *testing.T) {
	value := dnstxt.RecordValue("artist-alley.org", "ed7c91abcd")
	if !dnstxt.MatchesRecord(value, "artist-alley.org", "ed7c91abcd") {
		t.Errorf("RecordValue output should match its own parser; got %q", value)
	}
}

func TestRecordName(t *testing.T) {
	cases := []struct {
		url, want string
	}{
		{"https://studio-a.example", "_artist-alley.studio-a.example"},
		{"https://art.studio-a.example:8443", "_artist-alley.art.studio-a.example"},
		{"https://STUDIO-A.example", "_artist-alley.studio-a.example"},
	}
	for _, c := range cases {
		got, err := dnstxt.RecordName(c.url)
		if err != nil {
			t.Errorf("URL %q: %v", c.url, err)
			continue
		}
		if got != c.want {
			t.Errorf("URL %q: got %q, want %q", c.url, got, c.want)
		}
	}
}
