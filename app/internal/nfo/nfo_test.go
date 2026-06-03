package nfo

import (
	"testing"
)

func TestParseAlbum_Full(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<album>
  <title>The Gunslinger</title>
  <artist>Stephen King</artist>
  <albumartist>Stephen King</albumartist>
  <genre>Audiobook</genre>
  <year>1982</year>
  <runtime>432</runtime>
  <outline>Book 1 of the Dark Tower series.</outline>
  <review>Long review here.</review>
  <dateadded>2025-01-15 14:30:00</dateadded>
  <musicbrainzalbumid>00000000-0000-0000-0000-000000000001</musicbrainzalbumid>
  <track>
    <position>1</position>
    <title>Chapter 1</title>
    <duration>14:30</duration>
  </track>
  <track>
    <position>2</position>
    <title>Chapter 2</title>
    <duration>1:02:45</duration>
  </track>
  <track>
    <position>3</position>
    <title>Chapter 3</title>
    <duration>900</duration>
  </track>
</album>`
	a, err := ParseAlbum([]byte(src))
	if err != nil {
		t.Fatalf("ParseAlbum: %v", err)
	}
	if a.Title != "The Gunslinger" {
		t.Errorf("Title = %q", a.Title)
	}
	if a.Artist != "Stephen King" {
		t.Errorf("Artist = %q", a.Artist)
	}
	if a.Runtime != 432 {
		t.Errorf("Runtime = %v", a.Runtime)
	}
	if a.MBAlbumID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("MBAlbumID = %q", a.MBAlbumID)
	}
	if len(a.Tracks) != 3 {
		t.Fatalf("Tracks = %d, want 3", len(a.Tracks))
	}
	if a.Tracks[0].DurationS != 14*60+30 {
		t.Errorf("Tracks[0].DurationS = %v, want 870", a.Tracks[0].DurationS)
	}
	if a.Tracks[1].DurationS != 1*3600+2*60+45 {
		t.Errorf("Tracks[1].DurationS = %v, want 3765", a.Tracks[1].DurationS)
	}
	if a.Tracks[2].DurationS != 900 {
		t.Errorf("Tracks[2].DurationS = %v, want 900", a.Tracks[2].DurationS)
	}
}

func TestParseAlbum_StripsUTF8BOM(t *testing.T) {
	// stdlib encoding/xml panics-style fails on a leading BOM; the
	// parser must strip it for compat with the (real-world) Dark
	// Tower set the user ingested.
	src := append([]byte{0xef, 0xbb, 0xbf}, []byte(`<album><title>X</title></album>`)...)
	a, err := ParseAlbum(src)
	if err != nil {
		t.Fatalf("ParseAlbum: %v", err)
	}
	if a.Title != "X" {
		t.Errorf("BOM not handled — Title = %q", a.Title)
	}
}

func TestParseAlbum_TolerantOfMissingFields(t *testing.T) {
	a, err := ParseAlbum([]byte(`<album><title>Just A Title</title></album>`))
	if err != nil {
		t.Fatalf("ParseAlbum: %v", err)
	}
	if a.Title != "Just A Title" {
		t.Errorf("Title = %q", a.Title)
	}
	if a.Runtime != 0 {
		t.Errorf("Runtime should default to 0, got %v", a.Runtime)
	}
	if len(a.Tracks) != 0 {
		t.Errorf("Tracks should default to empty, got %d", len(a.Tracks))
	}
}

func TestParseAlbum_Malformed(t *testing.T) {
	if _, err := ParseAlbum([]byte(`<unclosed`)); err == nil {
		t.Error("expected error for malformed XML")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"45", 45},                           // bare seconds
		{"1:30", 90},                         // M:SS
		{"14:30", 870},                       // MM:SS
		{"1:02:45", 1*3600 + 2*60 + 45},      // H:MM:SS
		{"  2 : 03 : 04  ", 2*3600 + 3*60 + 4}, // tolerant of inner whitespace
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDuration_Bad(t *testing.T) {
	for _, in := range []string{"abc", "1:2:3:4", "x:y:z"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = nil, want error", in)
		}
	}
}

func TestParseAlbum_PreservesDurationRaw(t *testing.T) {
	// Non-standard duration string falls into DurationRaw so the UI
	// can still display whatever Kodi emitted.
	a, err := ParseAlbum([]byte(`<album><track><position>1</position><title>T</title><duration>weird-format</duration></track></album>`))
	if err != nil {
		t.Fatalf("ParseAlbum: %v", err)
	}
	if len(a.Tracks) != 1 {
		t.Fatalf("Tracks = %d, want 1", len(a.Tracks))
	}
	if a.Tracks[0].DurationRaw != "weird-format" {
		t.Errorf("DurationRaw = %q", a.Tracks[0].DurationRaw)
	}
	if a.Tracks[0].DurationS != 0 {
		t.Errorf("DurationS = %v, want 0 (unparseable)", a.Tracks[0].DurationS)
	}
}
