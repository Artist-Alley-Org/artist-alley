// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package nfo parses Kodi / Jellyfin-style album.nfo XML files —
// the canonical metadata sidecars that ship next to ripped CD
// audiobooks (e.g. each Dark Tower book folder is N MP3s + one
// album.nfo).
//
// The schema is documented at https://kodi.wiki/view/NFO_files —
// fields here cover what audiobook ingestion needs (title /
// artist / runtime / per-track list + duration / MusicBrainz IDs).
// Movie/TV-show variants live in cousin nfo formats and aren't
// handled here; the unmarshal is forgiving so unknown fields just
// fall on the floor.
//
// Encoding quirks: the sample files in the wild are UTF-8 with a
// BOM (the user's Dark Tower set shipped that way). encoding/xml
// doesn't error on a leading BOM if we pass an XMLDecoder with the
// CharsetReader hook, but the stdlib does choke on it for raw
// Unmarshal. ParseAlbum strips a leading BOM up-front so callers
// can pass the bytes as-is.

package nfo

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Album mirrors the <album> XML root of a Kodi album.nfo. All
// fields are optional — real-world .nfo files vary in completeness.
type Album struct {
	Title       string  // <title>
	Artist      string  // <artist>
	AlbumArtist string  // <albumartist>
	Genre       string  // <genre>
	Year        string  // <year> (free-form; some files use full date)
	Outline     string  // <outline> (short description)
	Review      string  // <review>  (long description)
	Runtime     float64 // <runtime> in MINUTES (industry convention)
	DateAdded   string  // <dateadded> "YYYY-MM-DD HH:MM:SS" if present
	MBAlbumID   string  // <musicbrainzalbumid>
	MBArtistID  string  // <musicbrainzalbumartistid>
	MBReleaseID string  // <musicbrainzreleasegroupid>
	Tracks      []Track
}

// Track mirrors each <track> child. Duration is parsed from the
// "MM:SS" / "H:MM:SS" / bare-seconds forms commonly seen in Kodi
// files; the parsed value lives in DurationS so the consumer
// doesn't have to re-split the string.
type Track struct {
	Position  int
	Title     string
	DurationS float64
	// DurationRaw preserves the original string for display when
	// it's a non-standard format the parser couldn't normalise.
	DurationRaw string
}

// raw is the wire-shape encoding/xml decodes into. Kept private so
// the public Album type can normalise field types (e.g. Runtime
// → float64).
type raw struct {
	XMLName     xml.Name   `xml:"album"`
	Title       string     `xml:"title"`
	Artist      string     `xml:"artist"`
	AlbumArtist string     `xml:"albumartist"`
	Genre       string     `xml:"genre"`
	Year        string     `xml:"year"`
	Outline     string     `xml:"outline"`
	Review      string     `xml:"review"`
	Runtime     string     `xml:"runtime"`
	DateAdded   string     `xml:"dateadded"`
	MBAlbumID   string     `xml:"musicbrainzalbumid"`
	MBArtistID  string     `xml:"musicbrainzalbumartistid"`
	MBReleaseID string     `xml:"musicbrainzreleasegroupid"`
	Tracks      []rawTrack `xml:"track"`
}

type rawTrack struct {
	Position string `xml:"position"`
	Title    string `xml:"title"`
	Duration string `xml:"duration"`
}

// ParseAlbum decodes one album.nfo blob. Leading UTF-8 BOMs are
// stripped — stdlib encoding/xml chokes on them otherwise.
func ParseAlbum(data []byte) (*Album, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	var r raw
	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("nfo: parse album: %w", err)
	}
	out := &Album{
		Title:       strings.TrimSpace(r.Title),
		Artist:      strings.TrimSpace(r.Artist),
		AlbumArtist: strings.TrimSpace(r.AlbumArtist),
		Genre:       strings.TrimSpace(r.Genre),
		Year:        strings.TrimSpace(r.Year),
		Outline:     strings.TrimSpace(r.Outline),
		Review:      strings.TrimSpace(r.Review),
		DateAdded:   strings.TrimSpace(r.DateAdded),
		MBAlbumID:   strings.TrimSpace(r.MBAlbumID),
		MBArtistID:  strings.TrimSpace(r.MBArtistID),
		MBReleaseID: strings.TrimSpace(r.MBReleaseID),
	}
	// Runtime — Kodi convention is minutes; we keep that.
	if r.Runtime != "" {
		if n, err := strconv.ParseFloat(strings.TrimSpace(r.Runtime), 64); err == nil {
			out.Runtime = n
		}
	}
	for _, t := range r.Tracks {
		pos, _ := strconv.Atoi(strings.TrimSpace(t.Position))
		secs, _ := parseDuration(t.Duration)
		out.Tracks = append(out.Tracks, Track{
			Position:    pos,
			Title:       strings.TrimSpace(t.Title),
			DurationS:   secs,
			DurationRaw: strings.TrimSpace(t.Duration),
		})
	}
	return out, nil
}

// parseDuration handles the three common Kodi forms:
//
//	"M:SS"      → minutes:seconds
//	"MM:SS"     → minutes:seconds
//	"H:MM:SS"   → hours:minutes:seconds
//	"<number>"  → bare seconds OR bare minutes (Kodi tends to be
//	              inconsistent; we treat bare numbers as seconds
//	              since that's what ffprobe emits and the more
//	              common modern convention)
func parseDuration(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		// Bare number — seconds.
		v, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		return v, nil
	case 2:
		m, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, err
		}
		sec, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return 0, err
		}
		return float64(m)*60 + sec, nil
	case 3:
		h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, err
		}
		m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, err
		}
		sec, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		if err != nil {
			return 0, err
		}
		return float64(h)*3600 + float64(m)*60 + sec, nil
	}
	return 0, fmt.Errorf("nfo: unrecognised duration %q", s)
}
