package archive

import (
	"strings"
	"testing"
)

// TestParseFormat covers every extension surface the preview job +
// dispatcher branch on. A regression here is what would silently
// route a `.tar.xz` upload through the wrong parser.
func TestParseFormat(t *testing.T) {
	cases := []struct {
		ext  string
		want string
	}{
		// Single-extension archives.
		{"zip", "zip"}, {".zip", "zip"}, {"ZIP", "zip"},
		{"jar", "zip"}, {"war", "zip"}, {"ear", "zip"}, {"apk", "zip"}, {"ipa", "zip"},
		{"7z", "7z"}, {".7z", "7z"},
		{"rar", "rar"}, {".RAR", "rar"},
		{"tar", "tar"}, {".tar", "tar"},
		{"tgz", "tar.gz"}, {".tgz", "tar.gz"},
		{"tbz2", "tar.bz2"},
		{"txz", "tar.xz"},
		// Compound extensions — match only when the dotted suffix is
		// present on the full filename. ParseFormat strips a single
		// leading dot for the switch but uses the original input for
		// the compound-suffix scan.
		{".tar.gz", "tar.gz"}, {"foo.tar.gz", "tar.gz"},
		{".tar.bz2", "tar.bz2"}, {"backup.tar.bz2", "tar.bz2"},
		{".tar.xz", "tar.xz"}, {"x.tar.xz", "tar.xz"},
		// Bare compound names without a leading dot don't match the
		// suffix scan — callers must preserve the dot.
		{"tar.gz", ""}, {"tar.xz", ""}, {"tar.bz2", ""},
		// Single-file compressors aren't archives — must not match.
		{"gz", ""}, {".gz", ""}, {"bz2", ""}, {"xz", ""},
		// Unsupported.
		{"", ""}, {".doc", ""}, {"unknown", ""},
	}
	for _, c := range cases {
		got := ParseFormat(c.ext)
		if got != c.want {
			t.Errorf("ParseFormat(%q) = %q, want %q", c.ext, got, c.want)
		}
	}
}

// TestSupportedExtensions guards the contract that the assets
// dispatcher's archiveExtsHandler + the frontend's ARCHIVE_EXTS
// mirror. Anything new in archive/ must round-trip through
// ParseFormat with a non-empty result.
func TestSupportedExtensions(t *testing.T) {
	exts := SupportedExtensions()
	if len(exts) == 0 {
		t.Fatal("SupportedExtensions returned empty list")
	}
	seen := map[string]bool{}
	for _, e := range exts {
		if seen[e] {
			t.Errorf("duplicate extension %q", e)
		}
		seen[e] = true
		if strings.HasPrefix(e, ".") {
			t.Errorf("extension %q should not include leading dot", e)
		}
		// ParseFormat resolves both the bare single-segment form
		// ("tgz", "zip") via its switch and dotted compound names
		// ("foo.tar.gz") via the suffix scan. A bare compound like
		// "tar.gz" passes through the switch unchanged so we probe
		// with a leading dot as the canonical filename form.
		if ParseFormat(e) == "" && ParseFormat("."+e) == "" {
			t.Errorf("ParseFormat(%q) and ParseFormat(.%s) both empty — supported list out of sync with dispatcher", e, e)
		}
	}
}

// TestIsUnsupported verifies the sentinel-error helper so callers
// can decide between "skip preview" (unsupported) vs "mark failed"
// (real parse error).
func TestIsUnsupported(t *testing.T) {
	if IsUnsupported(nil) {
		t.Error("IsUnsupported(nil) = true, want false")
	}
	if !IsUnsupported(&errUnsupported{ext: "x"}) {
		t.Error("IsUnsupported(&errUnsupported{}) = false, want true")
	}
}
