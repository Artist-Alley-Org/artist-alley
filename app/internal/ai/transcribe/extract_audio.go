// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.C — audio extraction via ffmpeg.
//
// The transcribe handler reads asset bytes from storage to a temp
// file, then this package wraps two ffmpeg/ffprobe invocations:
//
//   - ProbeDuration: ffprobe → ms duration. Pure read of the
//     container metadata; no re-encoding.
//   - ExtractAudio: ffmpeg → 16-kHz mono PCM WAV bytes for a time
//     slice. Whisper accepts the format natively; mono is what its
//     models train on; 16kHz is the lowest sample rate Whisper
//     understands and minimises payload size to the local /
//     cloud Whisper backend.
//
// Both helpers take the FFMPEG binary path as an argument so the
// caller can reuse the operator-configured path (AudioHandler /
// VideoHandler already centralise that lookup in preview/).

package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeDuration runs ffprobe against `srcPath` and returns the total
// playable duration in milliseconds. Used by the handler to decide
// whether to chunk + as the upper bound on chunk planning.
//
// Soft-fails on inputs ffprobe can't parse — returns 0 + a wrapped
// error so the caller can log + skip transcription rather than
// crashing the worker.
func ProbeDuration(ctx context.Context, ffprobeBin, srcPath string) (int, error) {
	if ffprobeBin == "" {
		ffprobeBin = "ffprobe"
	}
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "json",
		srcPath,
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w (%s)", srcPath, err, snippet(errBuf.Bytes(), 200))
	}
	var parsed struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return 0, fmt.Errorf("ffprobe %s: parse JSON: %w", srcPath, err)
	}
	if parsed.Format.Duration == "" {
		return 0, fmt.Errorf("ffprobe %s: no duration field in metadata", srcPath)
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: bad duration %q: %w", srcPath, parsed.Format.Duration, err)
	}
	return int(sec * 1000), nil
}

// ExtractAudio extracts a time slice of `srcPath` as 16-kHz mono PCM
// WAV bytes ready to ship to a transcription provider. Pass startMS
// = -1 + durationMS = -1 to extract the WHOLE file (no -ss / -t
// arguments; ffmpeg processes the full input).
//
// Output format choices:
//
//   - 16 kHz: Whisper's training sample rate; downsampling at our
//     end avoids the model wasting capacity on high frequencies it
//     ignores anyway. Cuts payload size to ~32 kB/sec.
//
//   - mono: Whisper trains on mono. Stereo channels get downmixed
//     internally anyway; doing it ourselves keeps the payload half
//     the size + dodges any decoder bug in the cloud path.
//
//   - s16le PCM (signed 16-bit little-endian): the lossless format
//     Whisper documents as the cleanest input. Bigger than mp3 but
//     guaranteed-correct + provider-decoder-bug-free.
//
//   - WAV container: standard, every Whisper client accepts it. We
//     emit a fresh WAV header per chunk so each chunk is a valid
//     standalone file.
func ExtractAudio(ctx context.Context, ffmpegBin, srcPath string, startMS, durationMS int) ([]byte, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-y",
	}
	if startMS >= 0 {
		// -ss BEFORE -i = fast seek (decodes from the nearest
		// keyframe), which is what we want for chunking — accuracy
		// to the ms isn't critical and the cost of accurate-seek
		// adds up across many chunks.
		args = append(args, "-ss", formatSec(startMS))
	}
	args = append(args, "-i", srcPath)
	if durationMS > 0 {
		args = append(args, "-t", formatSec(durationMS))
	}
	args = append(args,
		"-vn",                  // drop video stream
		"-ac", "1",             // mono
		"-ar", "16000",         // 16 kHz
		"-c:a", "pcm_s16le",    // signed 16-bit little-endian PCM
		"-f", "wav",            // WAV container
		"pipe:1",               // stdout
	)
	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg extract %s [%d, +%d]: %w (%s)",
			srcPath, startMS, durationMS, err, snippet(errBuf.Bytes(), 400))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg extract %s: produced empty output (%s)",
			srcPath, snippet(errBuf.Bytes(), 200))
	}
	return out.Bytes(), nil
}

// formatSec converts ms → a string ffmpeg's -ss / -t flags accept.
// Format: `SS.mmm` (seconds with millisecond fraction).
func formatSec(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
}

// snippet truncates a byte buffer for inclusion in error messages.
func snippet(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// nowMS is unused — kept here so the file's import set doesn't
// drift if a future commit adds time-based diagnostics.
var _ = time.Now
