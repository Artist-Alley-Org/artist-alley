// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import "testing"

func TestValidateHash(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	bad := []string{
		"",
		"too-short",
		"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF", // uppercase
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde",  // 63 chars
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", // 65 chars
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdez", // non-hex char
	}
	if err := ValidateHash(good); err != nil {
		t.Errorf("good hash rejected: %v", err)
	}
	for _, b := range bad {
		if err := ValidateHash(b); err == nil {
			t.Errorf("bad hash accepted: %q", b)
		}
	}
}

func TestValidateVariantKey(t *testing.T) {
	good := []string{
		"original",
		"preview_2048",
		"preview_2048.webp",
		"thumb_512",
		"hls/index.m3u8",
		"hls/seg00001.ts",
		"hls/bitrate_4000k/seg00042.ts",
		"3d/lod_low.glb",
	}
	bad := []string{
		"",
		"/leading-slash",
		"trailing-slash/",
		"two//slashes",
		"..",
		"path/../escape",
		"has spaces",
		"has\nnewlines",
		"path/with/../trick",
	}
	for _, g := range good {
		if err := ValidateVariantKey(g); err != nil {
			t.Errorf("good key rejected: %q (%v)", g, err)
		}
	}
	for _, b := range bad {
		if err := ValidateVariantKey(b); err == nil {
			t.Errorf("bad key accepted: %q", b)
		}
	}
}

func TestObjectPath_Shape(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := ObjectPath(hash, "original")
	want := "01/23/" + hash + "/original"
	if got != want {
		t.Errorf("ObjectPath=%q want %q", got, want)
	}
}
