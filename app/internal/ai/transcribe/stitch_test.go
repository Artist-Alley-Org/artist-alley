package transcribe

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

func TestStitch_SingleChunk_PassThrough(t *testing.T) {
	parts := []ChunkTranscript{
		{
			Chunk: TimeChunk{StartMS: 0, EndMS: 10_000},
			Transcript: ai.Transcript{
				Text:             "hello world",
				DetectedLanguage: "en",
				Segments: []ai.TranscriptSegment{
					{StartMS: 0, EndMS: 1500, Text: "hello"},
					{StartMS: 1500, EndMS: 3000, Text: "world"},
				},
			},
		},
	}
	out := Stitch(parts, 0)
	if out.Text != "hello world" {
		t.Errorf("text = %q", out.Text)
	}
	if len(out.Segments) != 2 {
		t.Errorf("segments = %d, want 2", len(out.Segments))
	}
	if out.DetectedLanguage != "en" {
		t.Errorf("lang = %q", out.DetectedLanguage)
	}
}

func TestStitch_TrimsOverlapHalves(t *testing.T) {
	// Two chunks: [0, 25_000) and [20_000, 45_000) → overlap [20_000, 25_000).
	// overlapMS = 5000; midpoint = 22_500.
	// Chunk 0 keeps segments whose midpoint < 22_500.
	// Chunk 1 keeps segments whose midpoint >= 22_500.
	parts := []ChunkTranscript{
		{
			Chunk: TimeChunk{StartMS: 0, EndMS: 25_000},
			Transcript: ai.Transcript{
				Segments: []ai.TranscriptSegment{
					{StartMS: 18_000, EndMS: 19_000, Text: "before-overlap"},  // mid 18_500 < 22_500 → keep
					{StartMS: 21_000, EndMS: 22_000, Text: "boundary-near"},   // mid 21_500 < 22_500 → keep
					{StartMS: 23_000, EndMS: 24_500, Text: "boundary-far"},    // mid 23_750 >= 22_500 → drop
				},
			},
		},
		{
			Chunk: TimeChunk{StartMS: 20_000, EndMS: 45_000},
			Transcript: ai.Transcript{
				Segments: []ai.TranscriptSegment{
					// Provider returned chunk-relative; rebase to absolute.
					{StartMS: 1_000, EndMS: 2_500, Text: "boundary-far"},      // abs mid 21_750 < 22_500 → drop
					{StartMS: 5_000, EndMS: 7_000, Text: "after-overlap-1"},   // abs mid 26_000 >= 22_500 → keep
					{StartMS: 20_000, EndMS: 22_000, Text: "deep-into-chunk"}, // abs mid 41_000 → keep
				},
			},
		},
	}
	out := Stitch(parts, 5_000)

	// Expected survivors: before-overlap, boundary-near (chunk 0 side),
	// after-overlap-1, deep-into-chunk (chunk 1 side). 4 segments.
	if len(out.Segments) != 4 {
		t.Fatalf("expected 4 segments after trim; got %d (%+v)", len(out.Segments), out.Segments)
	}
	for _, s := range out.Segments {
		if s.Text == "boundary-far" {
			// boundary-far should be dropped on BOTH sides — chunk 0's
			// drop because mid >= cut, chunk 1's drop because mid < cut.
			// If any survives the trim is broken.
			t.Errorf("boundary-far should be dropped; got %+v", s)
		}
	}
	// Timecodes ascending.
	for i := 1; i < len(out.Segments); i++ {
		if out.Segments[i].StartMS < out.Segments[i-1].StartMS {
			t.Errorf("segments not ascending at %d: %+v vs %+v",
				i, out.Segments[i-1], out.Segments[i])
		}
	}
}

func TestStitch_TimecodesMonotonic(t *testing.T) {
	parts := []ChunkTranscript{
		{
			Chunk: TimeChunk{StartMS: 0, EndMS: 25_000},
			Transcript: ai.Transcript{Segments: []ai.TranscriptSegment{
				{StartMS: 0, EndMS: 5_000, Text: "a"},
				{StartMS: 5_000, EndMS: 10_000, Text: "b"},
			}},
		},
		{
			Chunk: TimeChunk{StartMS: 20_000, EndMS: 40_000},
			Transcript: ai.Transcript{Segments: []ai.TranscriptSegment{
				{StartMS: 8_000, EndMS: 12_000, Text: "c"}, // abs 28_000
			}},
		},
	}
	out := Stitch(parts, 5_000)
	for i := 1; i < len(out.Segments); i++ {
		if out.Segments[i].StartMS < out.Segments[i-1].StartMS {
			t.Errorf("non-monotonic at %d: %+v", i, out.Segments)
			break
		}
	}
}

func TestStitch_DetectedLanguage_FirstNonEmptyWins(t *testing.T) {
	parts := []ChunkTranscript{
		{Chunk: TimeChunk{StartMS: 0, EndMS: 25_000}, Transcript: ai.Transcript{DetectedLanguage: ""}},
		{Chunk: TimeChunk{StartMS: 20_000, EndMS: 45_000}, Transcript: ai.Transcript{DetectedLanguage: "es"}},
		{Chunk: TimeChunk{StartMS: 40_000, EndMS: 60_000}, Transcript: ai.Transcript{DetectedLanguage: "en"}},
	}
	if got := Stitch(parts, 5_000).DetectedLanguage; got != "es" {
		t.Errorf("DetectedLanguage = %q, want es (first non-empty)", got)
	}
}

func TestSynthesiseSegments_FillsForEmpty(t *testing.T) {
	parts := []ChunkTranscript{
		{
			Chunk: TimeChunk{StartMS: 0, EndMS: 25_000},
			Transcript: ai.Transcript{
				Text:     "gemini gave me text without segments",
				Segments: nil,
			},
		},
	}
	got := SynthesiseSegments(parts)
	if len(got[0].Transcript.Segments) != 1 {
		t.Fatalf("expected 1 synthesised segment; got %d", len(got[0].Transcript.Segments))
	}
	seg := got[0].Transcript.Segments[0]
	if seg.StartMS != 0 || seg.EndMS != 25_000 {
		t.Errorf("synthesised segment span = [%d, %d), want [0, 25000)", seg.StartMS, seg.EndMS)
	}
	if seg.Text != "gemini gave me text without segments" {
		t.Errorf("synthesised text = %q", seg.Text)
	}
}

func TestSynthesiseSegments_KeepsExistingSegments(t *testing.T) {
	parts := []ChunkTranscript{
		{
			Chunk: TimeChunk{StartMS: 0, EndMS: 25_000},
			Transcript: ai.Transcript{
				Segments: []ai.TranscriptSegment{
					{StartMS: 0, EndMS: 1000, Text: "real segment"},
				},
			},
		},
	}
	got := SynthesiseSegments(parts)
	if len(got[0].Transcript.Segments) != 1 {
		t.Errorf("existing segments should be preserved; got %d", len(got[0].Transcript.Segments))
	}
}
