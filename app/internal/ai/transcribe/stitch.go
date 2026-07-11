// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.C — per-chunk transcript stitching.
//
// Each Chunk gets transcribed independently; the per-chunk
// transcripts come back with timecodes relative to the chunk
// start. Stitch re-bases them to absolute time + drops the
// duplicate segments that the overlap regions produce.
//
// Stitch policy: for each pair of adjacent chunks N and N+1 with an
// overlap region of width `overlap` ms starting at chunk N+1's
// StartMS, segments from chunk N whose midpoint falls in the second
// half of the overlap are dropped, and segments from chunk N+1
// whose midpoint falls in the first half are dropped. Net effect:
// segments near the boundary appear exactly once, taken from
// whichever chunk's centre is closer.
//
// When a provider doesn't emit per-segment timestamps (gemini today),
// SynthesiseSegments below fills the gap by producing one segment
// per chunk spanning the chunk's full timecode range with the chunk's
// transcript as the text. Lower fidelity than Whisper's native
// segments but the WebVTT output stays usable.

package transcribe

import (
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// ChunkTranscript pairs a chunk's absolute timecode bounds with the
// provider's transcription output for that chunk.
type ChunkTranscript struct {
	Chunk      TimeChunk
	Transcript ai.Transcript
}

// Stitch re-bases per-chunk segment timecodes to absolute time +
// trims overlap-region duplicates. Returns one merged transcript.
// The Duration field is the sum of per-chunk Durations (wall-clock
// for the provider work); EstimatedCostUSDMicros is also summed.
// DetectedLanguage is the first non-empty value (chunks rarely
// disagree; first wins for determinism).
func Stitch(parts []ChunkTranscript, overlapMS int) ai.Transcript {
	if len(parts) == 0 {
		return ai.Transcript{}
	}
	if len(parts) == 1 {
		// Single chunk: nothing to stitch; just re-base in case the
		// provider returned chunk-relative timecodes anyway. Use the
		// chunk's StartMS as the rebase offset (0 for the always-
		// first chunk → no-op).
		out := parts[0].Transcript
		out.Segments = rebase(out.Segments, parts[0].Chunk.StartMS)
		out.Text = strings.TrimSpace(out.Text)
		return out
	}

	out := ai.Transcript{
		DetectedLanguage: firstNonEmptyLang(parts),
	}

	var allSegs []ai.TranscriptSegment
	for i, p := range parts {
		segs := rebase(p.Transcript.Segments, p.Chunk.StartMS)

		// Trim the trailing-half overlap on chunks before the last,
		// and the leading-half overlap on chunks after the first.
		if i < len(parts)-1 {
			// Drop segments whose midpoint is in the second half of
			// the overlap region [chunk.EndMS - overlap/2, chunk.EndMS).
			cutAt := p.Chunk.EndMS - overlapMS/2
			segs = filterSegments(segs, func(s ai.TranscriptSegment) bool {
				return midpoint(s) < cutAt
			})
		}
		if i > 0 {
			// Drop segments whose midpoint is in the first half of
			// the overlap region [chunk.StartMS, chunk.StartMS + overlap/2).
			cutAt := p.Chunk.StartMS + overlapMS/2
			segs = filterSegments(segs, func(s ai.TranscriptSegment) bool {
				return midpoint(s) >= cutAt
			})
		}
		allSegs = append(allSegs, segs...)

		out.Duration += p.Transcript.Duration
		out.EstimatedCostUSDMicros += p.Transcript.EstimatedCostUSDMicros
	}

	// Defensive sort by start — providers may emit out-of-order
	// segments when the overlap region produces tied timestamps.
	sort.Slice(allSegs, func(i, j int) bool {
		if allSegs[i].StartMS != allSegs[j].StartMS {
			return allSegs[i].StartMS < allSegs[j].StartMS
		}
		return allSegs[i].EndMS < allSegs[j].EndMS
	})

	out.Segments = allSegs

	// Concatenate texts. Each chunk's text overlaps the next; stitch
	// the segment texts as the canonical source so duplicate words
	// at boundaries don't appear twice.
	var sb strings.Builder
	for i, s := range allSegs {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.TrimSpace(s.Text))
	}
	out.Text = strings.TrimSpace(sb.String())
	return out
}

// SynthesiseSegments produces one chunk-wide segment when the provider
// returned an empty Segments slice (e.g. Gemini). The text is the
// transcript text as-is; the timecode is the chunk's full range. This
// is lower-fidelity than per-utterance segments but keeps the WebVTT
// output usable for providers that don't emit segment timestamps.
//
// Callers run this BEFORE Stitch so the stitcher sees a uniform
// shape across providers.
func SynthesiseSegments(parts []ChunkTranscript) []ChunkTranscript {
	out := make([]ChunkTranscript, len(parts))
	for i, p := range parts {
		if len(p.Transcript.Segments) > 0 {
			out[i] = p
			continue
		}
		text := strings.TrimSpace(p.Transcript.Text)
		if text == "" {
			out[i] = p
			continue
		}
		// One segment spanning the chunk; timestamps are chunk-relative
		// so rebase() in Stitch lifts them to absolute time.
		seg := ai.TranscriptSegment{
			StartMS: 0,
			EndMS:   p.Chunk.EndMS - p.Chunk.StartMS,
			Text:    text,
		}
		t := p.Transcript
		t.Segments = []ai.TranscriptSegment{seg}
		out[i] = ChunkTranscript{Chunk: p.Chunk, Transcript: t}
	}
	return out
}

func rebase(segs []ai.TranscriptSegment, offsetMS int) []ai.TranscriptSegment {
	if offsetMS == 0 {
		return segs
	}
	out := make([]ai.TranscriptSegment, len(segs))
	for i, s := range segs {
		out[i] = ai.TranscriptSegment{
			StartMS: s.StartMS + offsetMS,
			EndMS:   s.EndMS + offsetMS,
			Text:    s.Text,
		}
	}
	return out
}

func filterSegments(segs []ai.TranscriptSegment, keep func(ai.TranscriptSegment) bool) []ai.TranscriptSegment {
	out := make([]ai.TranscriptSegment, 0, len(segs))
	for _, s := range segs {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func midpoint(s ai.TranscriptSegment) int {
	return (s.StartMS + s.EndMS) / 2
}

func firstNonEmptyLang(parts []ChunkTranscript) string {
	for _, p := range parts {
		if p.Transcript.DetectedLanguage != "" {
			return p.Transcript.DetectedLanguage
		}
	}
	return ""
}
