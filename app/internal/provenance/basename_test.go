// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package provenance

import (
	"errors"
	"strings"
	"testing"
)

func TestFromUploadHeader(t *testing.T) {
	long := strings.Repeat("a", MaxBasenameBytes)
	cases := []struct {
		name, raw, want string
		ok              bool
	}{
		{"plain ascii", "photo.jpg", "photo.jpg", true},
		{"percent-encoded UTF-8", "%E5%86%99%E7%9C%9F%20%C3%A9t%C3%A9.png", "写真 été.png", true},
		{"raw UTF-8 from a non-browser client", "写真.png", "写真.png", true},
		{"unix path stripped", "/home/bob/art/sketch.psd", "sketch.psd", true},
		{"windows path stripped", `C:\Users\bob\sketch.psd`, "sketch.psd", true},
		{"encoded path stripped", "a%2Fb%5Cc.tif", "c.tif", true},
		{"mixed separators", `a/b\c/d.png`, "d.png", true},
		{"spaces kept", "my file (1).jpg", "my file (1).jpg", true},
		{"encoded percent", "100%25.jpg", "100%.jpg", true},
		{"exactly 255 bytes", long, long, true},
		{"256 bytes", long + "b", "", false},
		{"255 bytes of name after a path", "dir/" + long, long, true},
		{"empty", "", "", false},
		{"dot", ".", "", false},
		{"dotdot", "..", "", false},
		{"path ending in dotdot", "a/..", "", false},
		{"trailing slash", "dir/", "", false},
		{"bad percent escape", "100%.jpg", "", false},
		{"encoded NUL", "a%00.jpg", "", false},
		{"encoded newline", "a%0Ab.jpg", "", false},
		{"control in a directory part", "di%01r/ok.jpg", "", false},
		{"DEL", "a%7F.jpg", "", false},
		{"C1 control", "a%C2%85.jpg", "", false},
		{"bidi override", "photo%E2%80%AEgpj.exe", "", false},
		{"bidi isolate", "a%E2%81%A6.jpg", "", false},
		{"invalid UTF-8", "a%FF.jpg", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FromUploadHeader(c.raw)
			if c.ok {
				if err != nil || got != c.want {
					t.Fatalf("FromUploadHeader(%q) = %q, %v; want %q", c.raw, got, err, c.want)
				}
				if !IsBareBasename(got) {
					t.Fatalf("accepted %q is not a bare base name", got)
				}
				return
			}
			if !errors.Is(err, ErrInvalidBasename) || got != "" {
				t.Fatalf("FromUploadHeader(%q) = %q, %v; want ErrInvalidBasename", c.raw, got, err)
			}
			if strings.Contains(err.Error(), c.raw) && c.raw != "" {
				t.Fatalf("error %q echoes the supplied value", err)
			}
		})
	}
}

func TestIsBareBasename_NeverAcceptsAPath(t *testing.T) {
	for _, s := range []string{"a/b.jpg", `a\b.jpg`, "/x", "", ".", "..", "a\x00b", "a\u202eb", strings.Repeat("x", 256)} {
		if IsBareBasename(s) {
			t.Errorf("IsBareBasename(%q) = true", s)
		}
	}
	for _, s := range []string{"a.jpg", "写真.png", "x", strings.Repeat("x", 255), "100%.jpg"} {
		if !IsBareBasename(s) {
			t.Errorf("IsBareBasename(%q) = false", s)
		}
	}
}

func TestArchivalOriginalFilename_OnlyFromTheUploadRequest(t *testing.T) {
	name := "sketch.psd"
	path := "dir/sketch.psd"
	cases := []struct {
		basename *string
		source   Source
		want     string
	}{
		{&name, SourceUploadRequest, "sketch.psd"},
		{&name, SourceClientReported, ""},
		{nil, SourceUnknown, ""},
		{nil, SourceUploadRequest, ""},
		{&path, SourceUploadRequest, ""}, // never a path, whatever the source says
	}
	for _, c := range cases {
		if got := ArchivalOriginalFilename(c.basename, c.source); got != c.want {
			t.Errorf("ArchivalOriginalFilename(%v, %s) = %q, want %q", c.basename, c.source, got, c.want)
		}
	}
}
