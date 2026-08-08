// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureLogger mirrors the pattern already used in internal/seed's
// tests: a JSON handler over a buffer, so assertions are on the fields
// an operator will actually grep rather than on a formatted string.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func lines(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// testWatcher builds a Watcher pointed at fixture /sys and /proc trees,
// with a controllable clock.
func testWatcher(t *testing.T, cfg Config, cgroupFiles map[string]string, procs []fakeProcEntry) (*Watcher, *bytes.Buffer, *time.Time) {
	t.Helper()
	cgRoot := t.TempDir()
	if cgroupFiles != nil {
		writeFiles(t, cgRoot, cgroupFiles)
	}
	procRoot := writeFakeProc(t, procs)

	buf := &bytes.Buffer{}
	if cfg.CaptureDir == "" {
		cfg.CaptureDir = filepath.Join(t.TempDir(), "captures")
	}
	w := New(cfg, captureLogger(buf))
	w.cgroupRoot = cgRoot
	w.procRoot = procRoot
	w.self = 1
	clock := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	return w, buf, &clock
}

func fixtureCgroup(current, limit int64) map[string]string {
	return map[string]string{
		"memory.current": fmt.Sprint(current),
		"memory.peak":    fmt.Sprint(current),
		"memory.max":     fmt.Sprint(limit),
		"memory.stat":    fmt.Sprintf("anon %d\nfile 0\n", current),
	}
}

var fixtureProcs = []fakeProcEntry{
	{pid: 1, ppid: 0, comm: "aa", state: "S", rssPages: 1000},
	{pid: 2, ppid: 1, comm: "ffmpeg", state: "R", rssPages: 500},
	{pid: 3, ppid: 2, comm: "chrome", state: "R", rssPages: 900},
}

// TestSampleCarriesAllThreePlanes is the anti-vacuity test. A sampler
// that reported only runtime/metrics would say "heap is fine" while the
// container was being killed by memory the runtime cannot see — so the
// assertion is that every line carries all three planes, not merely
// that a line was emitted.
func TestSampleCarriesAllThreePlanes(t *testing.T) {
	w, buf, _ := testWatcher(t, Config{Interval: time.Second}, fixtureCgroup(1000, 4000), fixtureProcs)
	w.tick(context.Background())

	got := lines(t, buf, "mem.sample")
	if len(got) != 1 {
		t.Fatalf("mem.sample lines = %d, want 1", len(got))
	}
	m := got[0]
	for _, plane := range [][]string{
		{"go_heap_live", "go_total_mapped", "go_retained", "go_gc_cycles", "go_goroutines", "go_memlimit"},
		{"cg_current", "cg_peak", "cg_anon", "cg_limit", "cg_used_frac", "nongo_anon"},
		{"child_count", "child_zombies", "child_rss", "child_top"},
	} {
		for _, k := range plane {
			if _, ok := m[k]; !ok {
				t.Errorf("sample line is missing %q — a plane is unreported", k)
			}
		}
	}
	// The Go plane must be real numbers read from this process, not
	// zeroes: a runtime metric that silently disappeared would read as
	// "no memory used" rather than as an error.
	if v, _ := m["go_total_mapped"].(float64); v <= 0 {
		t.Errorf("go_total_mapped = %v, want > 0", m["go_total_mapped"])
	}
	if v, _ := m["go_goroutines"].(float64); v <= 0 {
		t.Errorf("go_goroutines = %v, want > 0", m["go_goroutines"])
	}
	if got, want := m["child_count"], float64(2); got != want {
		t.Errorf("child_count = %v, want %v", got, want)
	}
	if s, _ := m["child_top"].(string); !strings.Contains(s, "chrome=") {
		t.Errorf("child_top = %q, want a chrome entry", s)
	}
}

// TestNegativeControlBelowThreshold is the reachability half of the
// pair: it is only meaningful because TestCaptureFiresAboveThreshold
// proves the same code path CAN fire.
func TestNegativeControlBelowThreshold(t *testing.T) {
	cfg := Config{Interval: time.Second, Threshold: 0.8}
	w, buf, _ := testWatcher(t, cfg, fixtureCgroup(1000, 10000), fixtureProcs) // 10 %
	w.tick(context.Background())

	if got := lines(t, buf, "mem.capture"); len(got) != 0 {
		t.Fatalf("capture fired at 10 %% of the ceiling: %v", got)
	}
	if entries, err := os.ReadDir(w.cfg.CaptureDir); err == nil && len(entries) > 0 {
		t.Fatalf("capture dir has %d entries at idle", len(entries))
	}
}

func TestCaptureFiresAboveThreshold(t *testing.T) {
	cfg := Config{Interval: time.Second, Threshold: 0.8}
	w, buf, _ := testWatcher(t, cfg, fixtureCgroup(9000, 10000), fixtureProcs) // 90 %
	w.tick(context.Background())

	caps := lines(t, buf, "mem.capture")
	if len(caps) != 1 {
		t.Fatalf("mem.capture lines = %d, want 1\n%s", len(caps), buf.String())
	}
	if _, ok := caps[0]["reclaimed_by_gc"]; !ok {
		t.Error("capture line lacks reclaimed_by_gc — the live-vs-garbage verdict")
	}
	if tbl := lines(t, buf, "mem.capture.children"); len(tbl) != 1 {
		t.Fatalf("child table lines = %d, want 1", len(tbl))
	} else if s, _ := tbl[0]["table"].(string); !strings.Contains(s, "chrome") {
		t.Errorf("child table = %q, want the full per-PID list", s)
	}

	dir, _ := caps[0]["dir"].(string)
	heap := filepath.Join(dir, heapFile)
	fi, err := os.Stat(heap)
	if err != nil {
		t.Fatalf("heap profile not written: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("heap profile is empty — a file that exists but says nothing")
	}
	if fi, err := os.Stat(filepath.Join(dir, goroutineFile)); err != nil || fi.Size() == 0 {
		t.Fatalf("goroutine dump missing or empty: %v", err)
	}
}

func TestCaptureRateLimited(t *testing.T) {
	cfg := Config{Interval: time.Second, Threshold: 0.8, CaptureMinInterval: 2 * time.Minute}
	w, buf, clock := testWatcher(t, cfg, fixtureCgroup(9000, 10000), fixtureProcs)

	// Hovering above the line for four samples 30s apart must produce
	// exactly one capture: without the floor, a parked usage level
	// evicts the run-up the ring exists to hold.
	for i := 0; i < 4; i++ {
		w.tick(context.Background())
		*clock = clock.Add(30 * time.Second)
	}
	if got := lines(t, buf, "mem.capture"); len(got) != 1 {
		t.Fatalf("captures = %d over 90s of hovering, want 1", len(got))
	}
	if got := lines(t, buf, "mem.capture.rate_limited"); len(got) != 3 {
		t.Fatalf("rate-limit lines = %d, want 3", len(got))
	}

	// Past the floor it fires again — the limit is a floor, not a
	// one-shot.
	*clock = clock.Add(2 * time.Minute)
	w.tick(context.Background())
	if got := lines(t, buf, "mem.capture"); len(got) != 2 {
		t.Fatalf("captures = %d after the floor elapsed, want 2", len(got))
	}
}

// TestRingBoundIsEnforced writes N+2 capture sets and counts N. The
// bound has to be enforced on every capture, not implied by the rate
// limit: a disk that fills during the one incident the ring exists for
// is the failure this guards.
func TestRingBoundIsEnforced(t *testing.T) {
	const keep = 3
	cfg := Config{Interval: time.Second, Threshold: 0.5, CaptureKeep: keep}
	w, _, clock := testWatcher(t, cfg, fixtureCgroup(9000, 10000), fixtureProcs)

	var written []string
	for i := 0; i < keep+2; i++ {
		w.tick(context.Background())
		written = append(written, clock.UTC().Format(captureTimeLayout))
		*clock = clock.Add(time.Second)
	}

	entries, err := os.ReadDir(w.cfg.CaptureDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != keep {
		t.Fatalf("ring holds %d sets after %d captures, want %d: %v", len(got), keep+2, keep, got)
	}
	// The NEWEST survive: a ring that kept the oldest would preserve
	// the calm before the storm and discard the death.
	want := written[len(written)-keep:]
	for _, w := range want {
		if !contains(got, w) {
			t.Fatalf("newest capture %q was pruned; kept %v", w, got)
		}
	}
}

func TestPruneLeavesForeignDirectoriesAlone(t *testing.T) {
	cfg := Config{Interval: time.Second, Threshold: 0.5, CaptureKeep: 1}
	w, _, clock := testWatcher(t, cfg, fixtureCgroup(9000, 10000), fixtureProcs)
	if err := os.MkdirAll(filepath.Join(w.cfg.CaptureDir, "operator-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w.tick(context.Background())
		*clock = clock.Add(time.Second)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.CaptureDir, "operator-notes")); err != nil {
		t.Fatalf("pruner deleted a directory it did not create: %v", err)
	}
}

func TestCapturesDisabledByZeroThreshold(t *testing.T) {
	cfg := Config{Interval: time.Second, Threshold: 0}
	w, buf, _ := testWatcher(t, cfg, fixtureCgroup(9999, 10000), fixtureProcs)
	w.tick(context.Background())
	if got := lines(t, buf, "mem.capture"); len(got) != 0 {
		t.Fatal("capture fired with the threshold switched off")
	}
	if got := lines(t, buf, "mem.sample"); len(got) != 1 {
		t.Fatal("sampling stopped when captures were switched off")
	}
}

func TestNoCgroupStillSamples(t *testing.T) {
	// Bare-metal and dev runs have no memory controller. Losing the
	// cgroup plane must cost the cgroup plane and nothing else.
	cfg := Config{Interval: time.Second, Threshold: 0.8}
	w, buf, _ := testWatcher(t, cfg, nil, fixtureProcs)
	w.tick(context.Background())

	got := lines(t, buf, "mem.sample")
	if len(got) != 1 {
		t.Fatalf("sample lines = %d, want 1", len(got))
	}
	if avail, ok := got[0]["cg_available"].(bool); !ok || avail {
		t.Error("expected cg_available:false rather than silent zeroes")
	}
	if _, ok := got[0]["go_heap_live"]; !ok {
		t.Error("Go plane lost along with the cgroup plane")
	}
	if caps := lines(t, buf, "mem.capture"); len(caps) != 0 {
		t.Error("capture fired with no ceiling to be a fraction of")
	}
}

func TestRunStopsWithContext(t *testing.T) {
	cfg := Config{Interval: 10 * time.Millisecond}
	w, buf, _ := testWatcher(t, cfg, fixtureCgroup(100, 1000), fixtureProcs)
	// The fake clock is fine for the capture rate limit, but Run's
	// ticker is real; restore a real clock so timestamps advance.
	w.now = time.Now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on context cancellation")
	}
	// The first sample is taken immediately, not after one interval —
	// a process that dies inside its first interval would otherwise
	// leave no memory line at all.
	if got := lines(t, buf, "mem.sample"); len(got) == 0 {
		t.Fatal("no samples emitted")
	}
	if got := lines(t, buf, "mem.watch.start"); len(got) != 1 {
		t.Fatal("watcher did not state its own configuration at start")
	}
}

func TestDisabledWatcher(t *testing.T) {
	if New(Config{Interval: 0}, slog.New(slog.NewTextHandler(io.Discard, nil))).Enabled() {
		t.Fatal("zero interval should disable the watcher")
	}
	var nilW *Watcher
	if nilW.Enabled() {
		t.Fatal("nil watcher should be disabled")
	}
	nilW.Run(context.Background()) // must not panic
}

func TestNewClampsNonsense(t *testing.T) {
	w := New(Config{Interval: time.Second, Threshold: 5, CaptureKeep: 0, CaptureMinInterval: -time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A threshold above 1 is a capture that can never fire — the
	// silent-guard failure this package exists to avoid.
	if w.cfg.Threshold != 1 {
		t.Errorf("threshold = %v, want clamped to 1", w.cfg.Threshold)
	}
	if w.cfg.CaptureKeep != 1 {
		t.Errorf("keep = %d, want at least 1", w.cfg.CaptureKeep)
	}
	if w.cfg.CaptureMinInterval != 0 {
		t.Errorf("min interval = %v, want 0", w.cfg.CaptureMinInterval)
	}
	if w.cfg.CaptureDir != DefaultCaptureDir {
		t.Errorf("capture dir = %q, want the default", w.cfg.CaptureDir)
	}
}

func TestNonGoAnonAttribution(t *testing.T) {
	s := Sample{
		Cgroup: cgroupPlane{Available: true, AnonBytes: 5_390_000_000},
		Go:     goPlane{TotalMappedBytes: 4_600_000_000, ReleasedBytes: 200_000_000},
	}
	// 5.39 GB anon - (4.6 - 0.2) GB retained = 990 MB non-Go, which is
	// the shape #887 measured.
	if got := s.NonGoAnonBytes(); got != 990_000_000 {
		t.Fatalf("non-Go anon = %d, want 990000000", got)
	}
	// Slightly negative arithmetic (mapped-but-not-resident on an idle
	// process) must clamp rather than report a negative "memory".
	s.Go.TotalMappedBytes = 9_000_000_000
	if got := s.NonGoAnonBytes(); got != 0 {
		t.Fatalf("non-Go anon = %d, want clamped to 0", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// BenchmarkCollect measures the sampler's own cost, so "cheap enough to
// leave on always" is a number in the record rather than a claim. It
// runs against this process's real /proc and /sys, which is the load
// production actually pays.
func BenchmarkCollect(b *testing.B) {
	w := New(Config{Interval: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.ReportAllocs()
	for b.Loop() {
		_ = w.Collect(false)
	}
}

// BenchmarkCollectAndLog measures the whole per-interval cost including
// rendering the line, which is what the duty cycle is computed from.
func BenchmarkCollectAndLog(b *testing.B) {
	w := New(Config{Interval: time.Second}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		w.tick(ctx)
	}
}
