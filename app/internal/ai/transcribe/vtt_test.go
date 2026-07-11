// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package transcribe

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestToWebVTT_HeaderAndCues(t *testing.T) {
	tx := ai.Transcript{Segments: []ai.TranscriptSegment{
		{StartMS: 0, EndMS: 1500, Text: "hello"},
		{StartMS: 1500, EndMS: 3000, Text: "world"},
	}}
	got := string(ToWebVTT(tx))
	if !strings.HasPrefix(got, "WEBVTT\n\n") {
		t.Errorf("missing WEBVTT header; got %q", got[:20])
	}
	wantParts := []string{
		"00:00:00.000 --> 00:00:01.500",
		"hello",
		"00:00:01.500 --> 00:00:03.000",
		"world",
	}
	for _, w := range wantParts {
		if !strings.Contains(got, w) {
			t.Errorf("VTT missing %q\nfull output:\n%s", w, got)
		}
	}
}

func TestToWebVTT_SkipsEmptyText(t *testing.T) {
	tx := ai.Transcript{Segments: []ai.TranscriptSegment{
		{StartMS: 0, EndMS: 1000, Text: ""},
		{StartMS: 1000, EndMS: 2000, Text: "  "}, // whitespace
		{StartMS: 2000, EndMS: 3000, Text: "real"},
	}}
	got := string(ToWebVTT(tx))
	// Header + 1 cue → 1 "--> " marker.
	if c := strings.Count(got, " --> "); c != 1 {
		t.Errorf("expected 1 cue (empty/whitespace skipped); got %d cues\n%s", c, got)
	}
}

func TestToWebVTT_EmptyTranscript_HeaderOnly(t *testing.T) {
	got := string(ToWebVTT(ai.Transcript{}))
	if got != "WEBVTT\n\n" {
		t.Errorf("empty transcript should produce header-only output; got %q", got)
	}
}

func TestToWebVTT_LongTimestamps(t *testing.T) {
	// 1h 23m 45.678s
	tx := ai.Transcript{Segments: []ai.TranscriptSegment{
		{StartMS: 5025678, EndMS: 5026678, Text: "deep"},
	}}
	got := string(ToWebVTT(tx))
	if !strings.Contains(got, "01:23:45.678") {
		t.Errorf("long timestamp not formatted correctly; got %q", got)
	}
}

func TestToWebVTT_ClampsBadOrdering(t *testing.T) {
	tx := ai.Transcript{Segments: []ai.TranscriptSegment{
		{StartMS: 5000, EndMS: 1000, Text: "backwards"}, // end < start
		{StartMS: -1000, EndMS: 500, Text: "negative-start"},
	}}
	got := string(ToWebVTT(tx))
	// Should produce two cues without crashing; backwards clamps to start==end.
	if c := strings.Count(got, " --> "); c != 2 {
		t.Errorf("expected 2 cues even with bad ordering; got %d\n%s", c, got)
	}
}
