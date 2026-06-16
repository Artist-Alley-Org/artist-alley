// Phase 1.18.B-3 — subtitle format conversion to WebVTT.
//
// The conversion job (jobs.TypeSubtitleConvert) reads an uploaded
// sidecar from CAS, converts it to WebVTT, stores the VTT in CAS,
// and inserts the asset_subtitle_tracks row pointing at the new
// hash. WebVTT is the canonical on-disk format — browser-native
// <track> consumes it directly without further processing.
//
// # Why pure-Go text conversion (no ffmpeg) for text formats
//
// SRT, SSA, ASS, SUB (microdvd) are all text formats with simple
// cue-timing grammars. A focused Go converter is faster than
// shelling out to ffmpeg per file + has no binary dependency
// surface + produces deterministic output we can snapshot-test.
//
// IDX is the exception: it's a bitmap format (DVD subtitles)
// requiring OCR. That path needs a binary not in the default
// runtime image; until it lands the converter returns
// ErrIDXUnsupported (permanent — retry won't help).
//
// # Confidence
//
// Text-based conversions are deterministic — confidence = 1.0.
// IDX (when shipped) emits confidence < 1.0 reflecting OCR
// uncertainty. The UI surfaces a warning banner below 0.8.
//
// # Errors
//
// ErrPermanent wraps causes that don't benefit from retry
// (malformed input, unsupported format, invalid UTF-8). The
// jobs framework's TerminalError check tests for it. Other
// errors are surfaced as-is and the worker retries up to
// MaxAttempts.

package subtitles

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// ErrPermanent wraps conversion failures that the worker should
// NOT retry. The jobs framework's TerminalError check ascends
// errors.Is to match this.
var ErrPermanent = errors.New("subtitles: permanent conversion failure")

// ErrIDXUnsupported is the specific permanent-error case for IDX
// bitmap subtitles. Surfaced separately so the upload pipeline
// can offer a "upload pre-converted VTT instead" hint in the
// 422 response.
var ErrIDXUnsupported = fmt.Errorf("%w: idx (bitmap) conversion requires OCR capability not present in runtime", ErrPermanent)

// Converted is the conversion worker's output: the WebVTT bytes
// + the confidence score that should land in the
// asset_subtitle_tracks row.
type Converted struct {
	// WebVTT bytes ready for CAS storage. Always begins with the
	// WEBVTT header per RFC 8054.
	VTT []byte

	// 1.0 for text-based sources; lower for OCR'd bitmap sources.
	Confidence float64
}

// Convert dispatches by source format. The source bytes are the
// raw upload (post-CAS-fetch). The returned Converted.VTT is the
// final WebVTT to upload back into CAS.
//
// For source_format = "vtt" this is the identity transform —
// validation only. The migrate path uses it to round-trip + sanity
// check uploads.
func Convert(srcFormat string, src []byte) (*Converted, error) {
	if !utf8.Valid(src) {
		return nil, fmt.Errorf("%w: source bytes are not valid UTF-8", ErrPermanent)
	}
	switch srcFormat {
	case "vtt":
		return convertVTT(src)
	case "srt":
		return convertSRT(src)
	case "ssa", "ass":
		return convertSSA(src)
	case "sub":
		return convertSUB(src)
	case "idx":
		return nil, ErrIDXUnsupported
	default:
		return nil, fmt.Errorf("%w: unknown source format %q", ErrPermanent, srcFormat)
	}
}

// convertVTT validates the input + returns it unchanged. We
// require the WEBVTT header so a misdetected SRT doesn't sneak
// through as a VTT.
func convertVTT(src []byte) (*Converted, error) {
	// Allow optional UTF-8 BOM.
	body := bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	if !bytes.HasPrefix(body, []byte("WEBVTT")) {
		return nil, fmt.Errorf("%w: missing WEBVTT header", ErrPermanent)
	}
	out := make([]byte, 0, len(body))
	out = append(out, body...)
	return &Converted{VTT: out, Confidence: 1.0}, nil
}

// convertSRT converts SubRip (.srt) to WebVTT. The transform is
// straightforward — the only meaningful difference is the
// timestamp separator (SRT uses "," for the milliseconds; VTT
// uses ".").
func convertSRT(src []byte) (*Converted, error) {
	body := bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	cues := 0
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			out.WriteString("\n")
			continue
		}
		// SRT cue index line — drop. VTT permits cue identifiers
		// but they're optional and we don't need them.
		if isAllDigits(line) {
			continue
		}
		// SRT timing line: "00:01:23,456 --> 00:01:25,789"
		if strings.Contains(line, "-->") {
			converted := strings.ReplaceAll(line, ",", ".")
			out.WriteString(converted)
			out.WriteString("\n")
			cues++
			continue
		}
		// Cue body — passthrough.
		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scan SRT: %v", ErrPermanent, err)
	}
	if cues == 0 {
		return nil, fmt.Errorf("%w: SRT contained no timing lines", ErrPermanent)
	}
	return &Converted{VTT: out.Bytes(), Confidence: 1.0}, nil
}

// convertSSA / .ass: SubStation Alpha + Advanced SSA. Both share
// the [Events] Dialogue: rows + a per-line cue. We strip the SSA
// styling overrides ({\\bN}, {\\fs24}, etc.) and emit unstyled VTT.
//
// SSA timestamps are "H:MM:SS.cc" (centiseconds, no leading zero
// hour). VTT wants "HH:MM:SS.mmm". Normalise both ends of the cue.
func convertSSA(src []byte) (*Converted, error) {
	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")

	body := bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)

	inEvents := false
	cues := 0
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trim := strings.TrimSpace(line)
		switch {
		case strings.EqualFold(trim, "[Events]"):
			inEvents = true
			continue
		case strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]"):
			inEvents = false
			continue
		}
		if !inEvents {
			continue
		}
		if !strings.HasPrefix(trim, "Dialogue:") {
			continue
		}
		// Dialogue: Layer,Start,End,Style,Name,MarginL,MarginR,MarginV,Effect,Text
		fields := strings.SplitN(trim[len("Dialogue:"):], ",", 10)
		if len(fields) < 10 {
			continue
		}
		start := normalizeSSATime(strings.TrimSpace(fields[1]))
		end := normalizeSSATime(strings.TrimSpace(fields[2]))
		text := stripSSAOverrides(fields[9])
		if start == "" || end == "" || text == "" {
			continue
		}
		out.WriteString(start)
		out.WriteString(" --> ")
		out.WriteString(end)
		out.WriteString("\n")
		out.WriteString(text)
		out.WriteString("\n\n")
		cues++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scan SSA/ASS: %v", ErrPermanent, err)
	}
	if cues == 0 {
		return nil, fmt.Errorf("%w: SSA/ASS contained no Dialogue rows", ErrPermanent)
	}
	return &Converted{VTT: out.Bytes(), Confidence: 1.0}, nil
}

// convertSUB handles microdvd-style .sub files where each cue
// is a single line of the form {start_frame}{end_frame}Text.
// We treat the frame counts as 24fps (the most common assumption
// in the wild); operators with a different framerate can re-upload
// the source as VTT.
func convertSUB(src []byte) (*Converted, error) {
	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")

	body := bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)

	cues := 0
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimRight(scanner.Text(), "\r"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		// Find the closing braces of the two frame counters.
		startEnd := strings.Index(line, "}")
		if startEnd < 1 {
			continue
		}
		startFrame := line[1:startEnd]
		rest := line[startEnd+1:]
		if !strings.HasPrefix(rest, "{") {
			continue
		}
		endEnd := strings.Index(rest, "}")
		if endEnd < 2 {
			continue
		}
		endFrame := rest[1:endEnd]
		text := rest[endEnd+1:]
		if text == "" {
			continue
		}
		start, err := framesToVTTTime(startFrame, 24.0)
		if err != nil {
			continue
		}
		end, err := framesToVTTTime(endFrame, 24.0)
		if err != nil {
			continue
		}
		out.WriteString(start)
		out.WriteString(" --> ")
		out.WriteString(end)
		out.WriteString("\n")
		// microdvd uses "|" as the line separator.
		out.WriteString(strings.ReplaceAll(text, "|", "\n"))
		out.WriteString("\n\n")
		cues++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scan SUB: %v", ErrPermanent, err)
	}
	if cues == 0 {
		return nil, fmt.Errorf("%w: SUB contained no frame-timed cues", ErrPermanent)
	}
	return &Converted{VTT: out.Bytes(), Confidence: 1.0}, nil
}

// --- helpers ---------------------------------------------------------

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalizeSSATime converts "H:MM:SS.cc" → "0H:MM:SS.cc0" form
// (so it parses as VTT's HH:MM:SS.mmm). Returns "" on parse
// failure; the caller drops the cue.
func normalizeSSATime(t string) string {
	parts := strings.Split(t, ":")
	if len(parts) != 3 {
		return ""
	}
	h := parts[0]
	if len(h) == 1 {
		h = "0" + h
	}
	mm := parts[1]
	ss := parts[2]
	// SSA centiseconds → VTT milliseconds (×10).
	if i := strings.Index(ss, "."); i >= 0 && i < len(ss)-1 {
		ms := ss[i+1:]
		for len(ms) < 3 {
			ms = ms + "0"
		}
		ss = ss[:i] + "." + ms[:3]
	} else {
		ss = ss + ".000"
	}
	return h + ":" + mm + ":" + ss
}

// stripSSAOverrides removes SSA inline style codes ({\...})
// and replaces SSA's \N line break with a real newline.
func stripSSAOverrides(s string) string {
	var b strings.Builder
	skip := 0
	for _, r := range s {
		if r == '{' {
			skip++
			continue
		}
		if r == '}' && skip > 0 {
			skip--
			continue
		}
		if skip > 0 {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ReplaceAll(b.String(), `\N`, "\n")
}

// framesToVTTTime converts a frame counter at the given fps into
// a VTT timestamp string.
func framesToVTTTime(frameStr string, fps float64) (string, error) {
	var frame int64
	for _, r := range frameStr {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("non-digit in frame count: %q", frameStr)
		}
		frame = frame*10 + int64(r-'0')
	}
	if fps <= 0 {
		return "", fmt.Errorf("fps must be > 0")
	}
	totalSec := float64(frame) / fps
	hours := int64(totalSec) / 3600
	mins := (int64(totalSec) / 60) % 60
	secs := int64(totalSec) % 60
	ms := int64((totalSec-float64(int64(totalSec)))*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, mins, secs, ms), nil
}

// Read is a small convenience wrapper for callers that want to
// hand the converter an io.Reader (job workers fetching from CAS).
func Read(srcFormat string, r io.Reader) (*Converted, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read source: %v", ErrPermanent, err)
	}
	return Convert(srcFormat, buf)
}
