// Phase 1.14.C — WebVTT marshaller.
//
// The subtitles package stores tracks as VTT bytes in storage_objects;
// this is the converter from ai.Transcript to that byte format.
//
// Format spec: https://www.w3.org/TR/webvtt1/
//   - File header: "WEBVTT\n\n"
//   - Cue: "<start> --> <end>\n<text>\n\n"
//   - Timestamp: HH:MM:SS.mmm (decimals always 3-digit ms)

package transcribe

import (
	"fmt"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/ai"
)

// ToWebVTT marshals a transcript's segments into WebVTT bytes.
// Empty transcript → just the WEBVTT header (still valid; renders as
// "no subtitles"). Segments with empty text are skipped so a
// pathological provider response doesn't produce blank cues.
func ToWebVTT(tx ai.Transcript) []byte {
	var sb strings.Builder
	sb.WriteString("WEBVTT\n\n")
	for _, seg := range tx.Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		// Clamp negative or out-of-order timestamps to a sane shape.
		// A malformed provider response shouldn't crash the marshaller.
		start := seg.StartMS
		if start < 0 {
			start = 0
		}
		end := seg.EndMS
		if end < start {
			end = start
		}
		fmt.Fprintf(&sb, "%s --> %s\n%s\n\n",
			formatVTTTime(start), formatVTTTime(end), text)
	}
	return []byte(sb.String())
}

// formatVTTTime renders milliseconds as HH:MM:SS.mmm. WebVTT
// requires the millisecond decimals + at least 1 hour digit but
// allows additional hour digits for long-form content.
func formatVTTTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	millis := ms % 1000
	sec := totalSec % 60
	min := (totalSec / 60) % 60
	hr := totalSec / 3600
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hr, min, sec, millis)
}
