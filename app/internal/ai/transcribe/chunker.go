// Package transcribe orchestrates audio transcription end-to-end —
// planning chunk time-ranges for long-form audio, calling the
// transcription router per chunk, stitching the per-chunk transcripts
// back into a single timeline, and handing the result to the
// subtitles package as a WebVTT track.
//
// Lives under app/internal/ai/ so it can depend on ai.Router +
// ai.Transcript without creating a cycle. Consumers (the
// ai.transcribe job handler in commit 3) call TranscribeAsset.

package transcribe

import (
	"fmt"
	"time"
)

// TimeChunk is one planned-chunk time slice within the source audio.
// The chunker returns these from PlanChunks; the handler then asks
// the audio extractor for the bytes covering each slice.
type TimeChunk struct {
	StartMS int // absolute start offset in the parent audio
	EndMS   int // absolute end offset (exclusive)
}

// Duration returns the slice's time-span.
func (c TimeChunk) Duration() time.Duration {
	return time.Duration(c.EndMS-c.StartMS) * time.Millisecond
}

// PlanChunks computes the chunk time slices for a stream of length
// `totalDurationMS`. Audio that fits inside `windowSec` returns a
// single slice [0, totalDurationMS) — pass-through, no extraction
// per chunk needed.
//
// Longer audio produces slices stepped by `windowSec - overlapSec`
// with each slice spanning `windowSec`. The overlap region is the
// hand-off zone the stitcher trims later to avoid duplicate
// transcription at chunk boundaries (Whisper's 30s context window
// + ~5s typical utterance length means a 5s overlap reliably
// straddles whole words).
func PlanChunks(totalDurationMS, windowSec, overlapSec int) ([]TimeChunk, error) {
	if windowSec <= 0 {
		return nil, fmt.Errorf("chunker: windowSec must be > 0; got %d", windowSec)
	}
	if overlapSec < 0 || overlapSec >= windowSec {
		return nil, fmt.Errorf("chunker: overlapSec must be in [0, windowSec); got %d (window=%d)", overlapSec, windowSec)
	}
	if totalDurationMS <= 0 {
		return nil, fmt.Errorf("chunker: totalDurationMS must be > 0; got %d", totalDurationMS)
	}

	windowMS := windowSec * 1000
	overlapMS := overlapSec * 1000

	// Short audio (≤ window) → single chunk, no splitting.
	if totalDurationMS <= windowMS {
		return []TimeChunk{{StartMS: 0, EndMS: totalDurationMS}}, nil
	}

	strideMS := windowMS - overlapMS
	chunks := make([]TimeChunk, 0, totalDurationMS/strideMS+1)
	for startMS := 0; startMS < totalDurationMS; startMS += strideMS {
		endMS := startMS + windowMS
		if endMS > totalDurationMS {
			endMS = totalDurationMS
		}
		chunks = append(chunks, TimeChunk{StartMS: startMS, EndMS: endMS})
		if endMS >= totalDurationMS {
			break
		}
	}
	return chunks, nil
}
