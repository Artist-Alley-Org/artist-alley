// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package transcribe

import "testing"

func TestPlanChunks_ShortAudio_SinglePassThrough(t *testing.T) {
	got, err := PlanChunks(10_000, 25, 5) // 10s audio, 25/5 chunking
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk for short audio; got %d", len(got))
	}
	if got[0].StartMS != 0 || got[0].EndMS != 10_000 {
		t.Errorf("pass-through chunk = [%d, %d), want [0, 10000)", got[0].StartMS, got[0].EndMS)
	}
}

func TestPlanChunks_LongAudio_OverlappingWindows(t *testing.T) {
	// 90s @ 25/5 → stride 20s.
	// Starts: 0, 20, 40, 60, 80 → 5 chunks.
	// Last chunk's end clamps to 90s.
	got, err := PlanChunks(90_000, 25, 5)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 chunks; got %d (%+v)", len(got), got)
	}

	wantStarts := []int{0, 20_000, 40_000, 60_000, 80_000}
	for i, w := range wantStarts {
		if got[i].StartMS != w {
			t.Errorf("chunk %d start = %d, want %d", i, got[i].StartMS, w)
		}
	}
	// Each chunk except the last spans windowMS.
	for i := 0; i < 4; i++ {
		if got[i].EndMS-got[i].StartMS != 25_000 {
			t.Errorf("chunk %d span = %d, want 25000", i, got[i].EndMS-got[i].StartMS)
		}
	}
	// Last chunk clamps to total duration.
	if got[4].EndMS != 90_000 {
		t.Errorf("last chunk EndMS = %d, want 90000 (clamped)", got[4].EndMS)
	}
}

func TestPlanChunks_ExactlyWindow_SinglePassThrough(t *testing.T) {
	got, err := PlanChunks(25_000, 25, 5)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	if len(got) != 1 || got[0].EndMS != 25_000 {
		t.Errorf("25s audio with 25s window should be 1 pass-through chunk; got %+v", got)
	}
}

func TestPlanChunks_InvalidArgs(t *testing.T) {
	cases := []struct {
		name                                string
		totalDurationMS, windowSec, overlap int
	}{
		{"zero window", 60_000, 0, 5},
		{"negative window", 60_000, -1, 5},
		{"overlap >= window", 60_000, 25, 25},
		{"zero duration", 0, 25, 5},
		{"negative duration", -1, 25, 5},
		{"negative overlap", 60_000, 25, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanChunks(c.totalDurationMS, c.windowSec, c.overlap)
			if err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestTimeChunk_Duration(t *testing.T) {
	c := TimeChunk{StartMS: 1000, EndMS: 4500}
	if got := c.Duration(); got.Milliseconds() != 3500 {
		t.Errorf("Duration() = %v, want 3500ms", got)
	}
}
