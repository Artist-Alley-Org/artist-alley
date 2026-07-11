// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 conversion tests. Pin the format-conversion
// invariants (timing preserved, WEBVTT header present, UTF-8
// output, error classification).

package subtitles

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConvert_SRT_PreservesTimings(t *testing.T) {
	src := `1
00:01:23,456 --> 00:01:25,789
Hello world

2
00:02:00,100 --> 00:02:03,200
Second cue
`
	got, err := Convert("srt", []byte(src))
	if err != nil {
		t.Fatalf("Convert(srt): %v", err)
	}
	out := string(got.VTT)
	if !strings.HasPrefix(out, "WEBVTT") {
		t.Errorf("output missing WEBVTT header: %q", out[:40])
	}
	if !strings.Contains(out, "00:01:23.456 --> 00:01:25.789") {
		t.Errorf("SRT timing not converted to VTT separator; got: %s", out)
	}
	if !strings.Contains(out, "Hello world") {
		t.Errorf("cue body missing")
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence=%v, want 1.0 for text source", got.Confidence)
	}
}

func TestConvert_SRT_EmptyInput_ErrPermanent(t *testing.T) {
	_, err := Convert("srt", []byte("not a real srt\n"))
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err=%v, want ErrPermanent", err)
	}
}

func TestConvert_VTT_RoundTrip(t *testing.T) {
	src := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHi\n"
	got, err := Convert("vtt", []byte(src))
	if err != nil {
		t.Fatalf("Convert(vtt): %v", err)
	}
	if !bytes.Equal(got.VTT, []byte(src)) {
		t.Errorf("VTT identity transform changed bytes")
	}
}

func TestConvert_VTT_MissingHeader_ErrPermanent(t *testing.T) {
	_, err := Convert("vtt", []byte("00:00:01.000 --> 00:00:02.000\nHi\n"))
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err=%v, want ErrPermanent for missing WEBVTT header", err)
	}
}

func TestConvert_SSA_Dialogue_Translated(t *testing.T) {
	src := `[Script Info]
Title: Example

[V4+ Styles]
Format: Name, Fontname, Fontsize

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.23,0:00:03.45,Default,,0,0,0,,Hello {\b1}world{\b0}
Dialogue: 0,0:00:04.00,0:00:05.50,Default,,0,0,0,,Second line\Nwith break
`
	got, err := Convert("ass", []byte(src))
	if err != nil {
		t.Fatalf("Convert(ass): %v", err)
	}
	out := string(got.VTT)
	if !strings.Contains(out, "00:00:01.230 --> 00:00:03.450") {
		t.Errorf("SSA timestamp not normalised; got: %s", out)
	}
	if !strings.Contains(out, "Hello world") {
		t.Errorf("override codes not stripped; got: %s", out)
	}
	if !strings.Contains(out, "Second line\nwith break") {
		t.Errorf("\\N not replaced with newline; got: %s", out)
	}
}

func TestConvert_SUB_FrameTimings(t *testing.T) {
	// 24 fps assumed. Frame 24 = 1s, frame 48 = 2s.
	src := "{24}{48}Hello world\n{72}{96}Second cue\n"
	got, err := Convert("sub", []byte(src))
	if err != nil {
		t.Fatalf("Convert(sub): %v", err)
	}
	out := string(got.VTT)
	if !strings.Contains(out, "00:00:01.000 --> 00:00:02.000") {
		t.Errorf("frame→time conversion wrong; got: %s", out)
	}
}

func TestConvert_IDX_ReturnsUnsupported(t *testing.T) {
	_, err := Convert("idx", []byte("any bytes"))
	if !errors.Is(err, ErrIDXUnsupported) {
		t.Errorf("err=%v, want ErrIDXUnsupported", err)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("ErrIDXUnsupported should also satisfy ErrPermanent")
	}
}

func TestConvert_UnknownFormat_ErrPermanent(t *testing.T) {
	_, err := Convert("xml", []byte("anything"))
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err=%v, want ErrPermanent for unknown format", err)
	}
}

func TestConvert_InvalidUTF8_ErrPermanent(t *testing.T) {
	src := []byte{0xFF, 0xFE, 0x00, 0xFE}
	_, err := Convert("srt", src)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err=%v, want ErrPermanent for invalid UTF-8", err)
	}
}

func TestConvert_OutputIsValidUTF8(t *testing.T) {
	src := `1
00:00:01,000 --> 00:00:02,000
日本語テスト
`
	got, err := Convert("srt", []byte(src))
	if err != nil {
		t.Fatalf("Convert(srt): %v", err)
	}
	if !utf8.Valid(got.VTT) {
		t.Errorf("output is not valid UTF-8")
	}
}

func TestConvert_OutputHasWEBVTTHeader(t *testing.T) {
	src := "1\n00:00:01,000 --> 00:00:02,000\nHi\n"
	got, err := Convert("srt", []byte(src))
	if err != nil {
		t.Fatalf("Convert(srt): %v", err)
	}
	if !bytes.HasPrefix(got.VTT, []byte("WEBVTT")) {
		t.Errorf("output first bytes=%q, want WEBVTT prefix", string(got.VTT[:20]))
	}
}

func TestConvert_BOM_TrimmedFromSRT(t *testing.T) {
	src := append([]byte{0xEF, 0xBB, 0xBF}, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n")...)
	got, err := Convert("srt", src)
	if err != nil {
		t.Fatalf("Convert(srt): %v", err)
	}
	if !bytes.HasPrefix(got.VTT, []byte("WEBVTT")) {
		t.Errorf("BOM not trimmed before conversion: %q", string(got.VTT[:10]))
	}
}
