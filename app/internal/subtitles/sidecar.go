// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 — sidecar file detection.
//
// Plex / Jellyfin / VLC convention: a subtitle file shipped next
// to its video shares the basename and adds an optional language
// segment + a subtitle-format extension.
//
//   clip.mp4               + clip.srt              → lang=und
//   clip.mp4               + clip.en.srt           → lang=en
//   clip.mp4               + clip.en-US.srt        → lang=en-US
//   clip.mp4               + clip.en.forced.srt    → lang=en, label="Forced"
//   clip.mp4               + clip.ja.srt           → lang=ja
//   clip.mp4               + other.srt             → not a sidecar (basename mismatch)
//   clip.mp4               + clip.txt              → not a sidecar (extension not in catalog)
//
// The parser is intentionally permissive on the language segment
// — anything that passes [ValidateLang] is accepted; anything
// else falls through to "und". We don't try to be clever about
// "english" → "en" mapping; that's user input territory.
//
// Returns hints, not commitments. The caller decides whether to
// actually create tracks (e.g., the upload pipeline checks
// RequiresAudioVideo first; subtitles for an image-asset sidecar
// get silently dropped, not 422'd — the operator never asked).

package subtitles

import (
	"path/filepath"
	"strings"
)

// SidecarHint is one detected (basename + lang + format) tuple
// surfaced by ParseSidecars. The caller (upload pipeline) decides
// whether to enqueue a conversion job for it.
type SidecarHint struct {
	// Filename of the sidecar as uploaded (e.g., "clip.en.srt").
	// Carried for audit + log lines; not used for keying.
	Filename string

	// Inferred RFC 5646 tag — already passed [ValidateLang]. "und"
	// when the filename had no recognisable lang segment.
	Lang string

	// Human-readable hint extracted from filename modifiers
	// ("forced", "sdh", "cc"). Empty when none surfaced. The
	// upsert path stores this as label.
	Label string

	// Source format. One of the values [SourceFormats] permits.
	// Always matches the file extension (lowercased) modulo "vtt"
	// being canonical.
	SourceFormat string
}

// SourceFormats is the closed catalogue of subtitle extensions
// the parser + conversion worker recognise. Mirror of the
// CHECK (source_format IN (...)) on the schema; if you add one
// here, add a migration that loosens the CHECK and a converter
// in convert.go for it.
var SourceFormats = map[string]bool{
	"vtt": true, // canonical; no conversion needed
	"srt": true,
	"ssa": true,
	"ass": true,
	"sub": true,
	"idx": true, // bitmap; OCR'd (confidence < 1.0)
}

// ParseSidecars looks at a batch of uploaded filenames + the
// primary asset's filename, and surfaces hints for each subtitle
// file that matches the primary by basename. Returns hints in
// input order; deduplication on (lang, format) is the caller's
// job (multiple sidecars with the same lang means the operator
// uploaded a confused set — keep them all, surface the conflict
// to the UI).
//
// The primary's basename is everything before the LAST dot of
// the primary filename ("clip.mp4" → "clip"; "Some.Show.S01E02.mkv"
// → "Some.Show.S01E02"). This is the segment sidecars match
// against.
//
// Filenames that match the primary but have a non-subtitle
// extension (e.g., clip.nfo, clip.jpg as a poster) are ignored
// silently.
func ParseSidecars(primary string, candidates []string) []SidecarHint {
	primaryBase := stripExt(primary)
	if primaryBase == "" {
		return nil
	}
	hints := make([]SidecarHint, 0, len(candidates))
	for _, c := range candidates {
		if c == primary {
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(c), "."))
		if !SourceFormats[ext] {
			continue
		}
		// Strip the extension to look at the optional lang +
		// modifier segments.
		base := stripExt(c)
		if base == "" {
			continue
		}
		// The base must equal the primary's basename OR be the
		// primary's basename followed by ".<seg>" (one or more).
		if !strings.HasPrefix(base, primaryBase) {
			continue
		}
		rem := base[len(primaryBase):]
		var lang, label string
		switch {
		case rem == "":
			// "clip.srt" — no lang segment.
			lang = "und"
		case strings.HasPrefix(rem, "."):
			// One or more dot-separated segments. The convention:
			// first segment is the lang tag; subsequent segments
			// are modifiers ("forced", "sdh", "cc").
			segs := strings.Split(rem[1:], ".")
			if len(segs) == 0 {
				lang = "und"
			} else {
				if ValidateLang(segs[0]) == nil {
					lang = segs[0]
					segs = segs[1:]
				} else {
					lang = "und"
				}
				if len(segs) > 0 {
					label = strings.Join(segs, " ")
				}
			}
		default:
			// "clipXYZ.srt" — primary basename is a strict prefix
			// of the candidate's basename but the gap isn't a "."
			// separator. Not a sidecar.
			continue
		}
		hints = append(hints, SidecarHint{
			Filename:     c,
			Lang:         lang,
			Label:        label,
			SourceFormat: ext,
		})
	}
	return hints
}

// stripExt returns the input minus its LAST extension. Returns
// the input unchanged when there's no extension.
//
// Distinct from filepath.Base + manual stripping because the input
// may carry directory components; we treat the BASENAME of the
// path, not the whole path.
func stripExt(name string) string {
	b := filepath.Base(name)
	if b == "" || b == "." || b == ".." {
		return ""
	}
	if i := strings.LastIndex(b, "."); i > 0 {
		return b[:i]
	}
	return b
}
