package cue

import (
	"strings"
	"testing"
)

func TestParse_AudibleStyleSheet(t *testing.T) {
	// Real-world shape — Audible bundles this with .aax exports. The
	// 109:49:51 timecode exercises the M-overflow case (hours encoded
	// as minutes past 59), which is the trickiest corner.
	src := `FILE "audiobook.m4b" MP3
TRACK 1 AUDIO
  TITLE "Opening Credits"
  INDEX 01 0:00:00
TRACK 2 AUDIO
  TITLE "Chapter 1"
  INDEX 01 14:54:00
TRACK 3 AUDIO
  TITLE "Chapter 2"
  INDEX 01 109:49:51
`
	sh, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sh.File != "audiobook.m4b" {
		t.Errorf("File = %q", sh.File)
	}
	if len(sh.Tracks) != 3 {
		t.Fatalf("Tracks = %d, want 3", len(sh.Tracks))
	}
	if sh.Tracks[0].Title != "Opening Credits" {
		t.Errorf("Track[0].Title = %q", sh.Tracks[0].Title)
	}
	if sh.Tracks[1].StartS != 14*60+54 {
		t.Errorf("Track[1].StartS = %v, want 894", sh.Tracks[1].StartS)
	}
	// 109:49:51 = 109*60 + 49 + 51/75 = 6589.68
	want := float64(109*60+49) + 51.0/75.0
	if sh.Tracks[2].StartS != want {
		t.Errorf("Track[2].StartS = %v, want %v", sh.Tracks[2].StartS, want)
	}
}

func TestParse_StripsUTF8BOM(t *testing.T) {
	src := append([]byte{0xef, 0xbb, 0xbf}, []byte(`FILE "x.m4b" MP3
TRACK 1 AUDIO
  TITLE "A"
  INDEX 01 0:00:00
`)...)
	sh, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sh.File != "x.m4b" {
		t.Errorf("BOM not stripped — File = %q", sh.File)
	}
}

// INDEX 00 marks pre-gap; only INDEX 01 is the playback start. The
// parser must ignore the pre-gap so multi-track CDs map cleanly.
// Timecodes are MM:SS:FF (frames at 75/sec), so "2:00:00" is 2 minutes.
func TestParse_IgnoresIndex00(t *testing.T) {
	src := `FILE "a.m4b" MP3
TRACK 1 AUDIO
  TITLE "T"
  INDEX 00 0:00:00
  INDEX 01 2:00:00
`
	sh, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sh.Tracks[0].StartS != 120 {
		t.Errorf("StartS = %v, want 120 (INDEX 01)", sh.Tracks[0].StartS)
	}
}

func TestParse_TolerantOfBlankLinesAndMixedIndent(t *testing.T) {
	src := "\nFILE \"a.m4b\" MP3\n\n\tTRACK 1 AUDIO\n      TITLE \"T\"\n  INDEX 01 0:00:00\n\n"
	sh, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(sh.Tracks) != 1 || sh.Tracks[0].Title != "T" {
		t.Errorf("unexpected tracks: %+v", sh.Tracks)
	}
}

func TestParseTimecode(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0:00:00", 0},
		{"0:00:75", 1.0},                       // 75 frames = 1 second
		{"1:30:00", 90},                        // 1m30s
		{"109:49:51", 109*60 + 49 + 51.0/75.0}, // hour-overflow
	}
	for _, c := range cases {
		got, err := parseTimecode(c.in)
		if err != nil {
			t.Errorf("parseTimecode(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseTimecode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseTimecode_Bad(t *testing.T) {
	for _, in := range []string{"", "00:00", "1:2:3:4", "a:b:c", "1:b:3"} {
		if _, err := parseTimecode(in); err == nil {
			t.Errorf("parseTimecode(%q) = nil err, want error", in)
		}
	}
}

func TestStripQuotes(t *testing.T) {
	if got := stripQuotes(`  "hello world"  `); got != "hello world" {
		t.Errorf("stripQuotes = %q", got)
	}
	if got := stripQuotes(`no-quotes`); got != "no-quotes" {
		t.Errorf("stripQuotes passthrough = %q", got)
	}
	if got := stripQuotes(`"unbalanced`); got != `"unbalanced` {
		t.Errorf("stripQuotes unbalanced = %q", got)
	}
}

// Truly malformed input returns an empty sheet without panicking —
// we surface as "no tracks recognised" rather than failing hard so
// a borderline-broken .cue companion doesn't tank the audiobook
// import; the player just falls back to ffprobe-only chapters.
func TestParse_Garbage(t *testing.T) {
	for _, in := range []string{
		"",
		"not a cue sheet",
		strings.Repeat("garbage\n", 100),
	} {
		sh, err := Parse([]byte(in))
		if err != nil {
			t.Errorf("Parse(%q): err = %v, want nil", in, err)
			continue
		}
		if len(sh.Tracks) != 0 {
			t.Errorf("Parse(%q): tracks = %d, want 0", in, len(sh.Tracks))
		}
	}
}
