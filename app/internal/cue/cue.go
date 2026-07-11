// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package cue parses CUE sheets — the text-based chapter / track
// marker format Audible bundles next to its .aax / .m4b exports.
//
// Format (simplified — full spec at
// https://www.gnu.org/software/ccd2cue/manual/html_node/CUE-sheet-format.html):
//
//   FILE "audiobook.m4b" MP3
//   TRACK 1 AUDIO
//     TITLE "Opening Credits"
//     INDEX 01 0:00:00
//   TRACK 2 AUDIO
//     TITLE "Chapter 1"
//     INDEX 01 14:54:00
//
// INDEX timecodes are MM:SS:FF where FF is a 75-per-second frame
// counter. That's CD-DA convention; audiobook exports often
// stretch the M field past 59 to encode hours (e.g. 109:49:51 =
// 109 minutes 49 seconds 51 frames). We treat M as a free integer
// of minutes — never re-decompose into hours since that loses
// information when the file genuinely uses an hour-overflow.
//
// Used as a chapter fallback when the m4b container itself doesn't
// ship chpl/chap atoms: preview.audio reads the companion .cue
// and folds the tracks into AudioMetadata.Chapters so the
// audiobook reader's chapter list still populates.

package cue

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Track is one parsed CUE track entry. Start is the resolved
// offset in seconds from the beginning of the file (INDEX 01).
type Track struct {
	Number  int
	Title   string
	StartS  float64
}

// Sheet is the parsed .cue file. File is the referenced source
// from the leading FILE directive ("audiobook.m4b" in the example
// above); Tracks lists every TRACK block in order.
type Sheet struct {
	File   string
	Tracks []Track
}

// Parse decodes a CUE sheet from raw bytes. The format is
// line-oriented + whitespace-tolerant; encoding/csv doesn't fit
// because the quoted-string handling differs (no embedded quote
// escaping, but the quotes are required for titles with spaces).
func Parse(data []byte) (*Sheet, error) {
	sh := &Sheet{}
	var current *Track
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	// Audiobook .cue files can have very long lines if titles get
	// expansive — bump the scanner buffer past the 64KB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		tok, rest := nextToken(line)
		switch strings.ToUpper(tok) {
		case "FILE":
			// FILE "name" TYPE   →   pluck the quoted "name".
			sh.File = stripQuotes(firstField(rest))
		case "TRACK":
			// TRACK NN TYPE
			numStr, _ := nextToken(rest)
			n, _ := strconv.Atoi(numStr)
			sh.Tracks = append(sh.Tracks, Track{Number: n})
			current = &sh.Tracks[len(sh.Tracks)-1]
		case "TITLE":
			if current != nil {
				current.Title = stripQuotes(strings.TrimSpace(rest))
			}
		case "INDEX":
			// INDEX 01 MM:SS:FF
			idStr, tc := nextToken(rest)
			if idStr != "01" && idStr != "1" {
				continue // we only track the playback-start index
			}
			s, err := parseTimecode(strings.TrimSpace(tc))
			if err == nil && current != nil {
				current.StartS = s
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cue: scan: %w", err)
	}
	return sh, nil
}

// parseTimecode reads MM:SS:FF where FF is 1/75 of a second
// (CD-DA convention). M may overflow 59 to encode hours (Audible's
// .cue files routinely emit "109:49:51" = ~1h50m).
func parseTimecode(s string) (float64, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("cue: bad timecode %q", s)
	}
	m, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	sec, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	frames, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, err
	}
	return float64(m)*60 + float64(sec) + float64(frames)/75.0, nil
}

// nextToken pops the first whitespace-delimited token and returns
// (token, rest). Handles tab / multi-space gaps.
func nextToken(s string) (string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	tok := s[start:i]
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return tok, s[i:]
}

func firstField(s string) string {
	tok, _ := nextToken(s)
	return tok
}

func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
