// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package memwatch logs this container's memory across three planes and
// dumps a readable corpse before the next OOM kill.
//
// # Why this exists
//
// The app was killed by its own cgroup ~11 times in 16 hours, every
// kill at 5.0-5.7 GB of anonymous RSS (#886, #887). From outside the
// process a legitimate high-water mark and a slow leak are the same
// picture: both climb, both die at the ceiling. #887 raised the ceiling
// to stop the bleeding; this package answers the question raising it
// defers — what holds 5+ GB, and is it bounded (#888).
//
// # Why three planes and not just the heap
//
// #781 already derives GOMEMLIMIT from the cgroup ceiling, and the
// kills continued anyway. That leaves two possibilities: the derivation
// is not taking effect, or the memory is not the kind a soft heap limit
// can reach. Only one of those is visible in runtime/metrics.
//
// The app container runs far more than the Go binary. Preview rendering
// shells out to ffmpeg, ghostscript, pdftoppm, unar, ImageMagick and a
// node + headless-chromium worker; every one of those lives in the app
// container's OWN cgroup, so the kernel charges their resident memory
// against the ceiling that does the killing while the Go runtime cannot
// see a byte of it. A measured render storm found ~1.0 GB of non-Go
// anonymous RSS at peak.
//
// So a sampler that read only runtime/metrics would report "heap is
// fine" in a steady voice while the container was being killed — a
// guard that cannot fail, which is worse than no guard at all. Every
// sample here therefore carries:
//
//  1. the Go runtime's own accounting (runtime/metrics, no STW pause);
//  2. the cgroup's accounting, which is the only view the OOM killer
//     has;
//  3. a rollup of every descendant process by command name, which
//     attributes the difference between the two.
//
// # What it produces
//
//   - "mem.limit" — one boot line stating the effective GOMEMLIMIT and
//     WHERE it came from (explicit env / derived from cgroup / no
//     limit). Permanently answers "is the derivation taking effect".
//   - "mem.sample" — one line per interval, all three planes.
//   - "mem.capture" + "mem.capture.children" — written when cgroup
//     usage crosses a fraction of the ceiling: a heap profile and a
//     goroutine dump into a bounded ring directory, plus the full
//     per-process table. This is the corpse: no debugger attached, no
//     operator awake, and the evidence outlives the kill.
//
// The always-on evidence trail is a different job from AA_PPROF_ADDR
// (internal/debugsrv), which stays exactly as it is: that one is an
// on-demand listener an operator opts into and reaches by hand, and it
// is useless for a process that died at 03:00 in CI.
package memwatch

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Defaults for every knob. Each number is argued rather than picked;
// see the constant's own comment.
const (
	// DefaultInterval — 15s.
	//
	// The two bounds: a measured render storm runs for a few minutes,
	// and telling a sawtooth (healthy: allocate, collect, repeat) from
	// a ratchet (a leak) needs enough points across that window to see
	// whether the troughs rise. 15s gives ~12-25 samples across a
	// typical storm, which is enough to draw the curve. At the other
	// end, a 16-hour CI day costs ~3,800 lines of a few hundred bytes
	// — around 1.5 MB, against a `docker compose logs` capture that
	// already tails thousands of lines. Collection itself is
	// sub-millisecond (see the benchmark), so the duty cycle is ~1e-5.
	DefaultInterval = 15 * time.Second

	// DefaultThreshold — 0.80 of the cgroup ceiling.
	//
	// Chosen to coincide with memlimit.DefaultRatio, and not by
	// accident: 80 % is the fraction of the container handed to the Go
	// heap as GOMEMLIMIT, so crossing it means the container holds
	// more than the Go runtime was ever told it could have. Below that
	// line, growth is the heap doing what it was configured to do;
	// above it, either the heap has overshot its soft limit or the
	// excess is non-Go — and both of those are exactly what is worth
	// a profile. It also leaves a fifth of the ceiling (1.6 GB at the
	// 8g default, 2 GB under the 10g CI override) as headroom for the
	// capture itself, which allocates to build the profile.
	DefaultThreshold = 0.80

	// DefaultKeep — 5 capture sets.
	//
	// The ring must answer "how did we get here", so it wants the
	// run-up and not just the final frame. Five sets at the rate limit
	// below span ten minutes of threshold-crossing, which covers the
	// climb of every measured storm. Each set is a heap profile
	// (~100 KB-1 MB) plus a goroutine dump (tens of KB), so the whole
	// ring is single-digit megabytes — small enough that the bound
	// protects the container's disk rather than being felt by it.
	DefaultKeep = 5

	// DefaultMinInterval — 2 minutes between captures.
	//
	// Without a floor, usage that parks just above the threshold
	// produces one capture per sample, which both spams the ring
	// (evicting the run-up it exists to hold) and adds a forced GC
	// every 15s to a process already under memory pressure. Two
	// minutes is short enough that a multi-minute storm still yields
	// several captures showing the trend, and long enough that a
	// steady-state hover costs at most 30 captures an hour.
	DefaultMinInterval = 2 * time.Minute

	// DefaultCaptureDir — under the OS temp dir, NOT under the storage
	// root. A heap profile contains live object contents: session
	// tokens, password hashes in flight, decoded file bytes, DB rows
	// (the same reasoning that keeps pprof off the application
	// router). The storage root is a mounted volume whose contents are
	// served; the temp dir is container-local and survives a restart
	// after an OOM kill, which is all the lifetime the corpse needs.
	DefaultCaptureDir = "/tmp/aa-memcapture"
)

// Config is the watcher's configuration, normally built from AA_MEM_*
// environment variables by internal/config.
type Config struct {
	// Interval between samples. Zero or negative disables the watcher
	// entirely — no goroutine, no ticker, no capture.
	Interval time.Duration
	// Threshold is the fraction of the cgroup ceiling that triggers a
	// capture. Zero or negative disables captures while leaving
	// sampling on. Values above 1 are clamped to 1: a threshold the
	// process can never reach is a capture that never fires, which is
	// the silent-guard failure this package is built to avoid.
	Threshold float64
	// CaptureDir is the ring directory. Each capture is a timestamped
	// subdirectory inside it.
	CaptureDir string
	// CaptureKeep is how many capture subdirectories survive pruning.
	// Values below 1 are treated as 1 — a ring that keeps nothing
	// would delete the corpse it just wrote.
	CaptureKeep int
	// CaptureMinInterval is the floor between two captures.
	CaptureMinInterval time.Duration
}

// Watcher samples memory on an interval and captures profiles when the
// container approaches its ceiling.
type Watcher struct {
	cfg    Config
	logger *slog.Logger
	goms   *goReader
	procs  *procScanner

	// Injection points for tests; production leaves them at their
	// package defaults.
	cgroupRoot string
	procRoot   string
	self       int
	now        func() time.Time

	seq int64
	// lastCapture is the rate-limit clock. Zero means "never
	// captured", which must trigger on the first crossing rather than
	// wait out one min-interval.
	lastCapture time.Time
	// above tracks whether the previous sample was already over the
	// threshold, so the log can distinguish a fresh crossing from a
	// sustained hover.
	above bool
}

// New builds a Watcher. It never fails: every input has a defensible
// fallback, and a misconfigured knob must degrade to "instrumentation
// still running" rather than "app refuses to boot".
func New(cfg Config, logger *slog.Logger) *Watcher {
	if cfg.CaptureDir == "" {
		cfg.CaptureDir = DefaultCaptureDir
	}
	if cfg.CaptureKeep < 1 {
		cfg.CaptureKeep = 1
	}
	if cfg.CaptureMinInterval < 0 {
		cfg.CaptureMinInterval = 0
	}
	if cfg.Threshold > 1 {
		cfg.Threshold = 1
	}
	w := &Watcher{
		cfg:        cfg,
		logger:     logger,
		goms:       newGoReader(),
		procs:      newProcScanner(),
		cgroupRoot: v2Root,
		procRoot:   "/proc",
		self:       selfPID,
		now:        time.Now,
	}
	if len(w.goms.missing) > 0 && logger != nil {
		// A renamed or dropped runtime metric would otherwise show up
		// as a permanent zero in a field an operator is reading as a
		// measurement. Say so once, loudly, at construction.
		logger.Warn("mem.metrics.unsupported",
			slog.Any("names", w.goms.missing),
			slog.String("effect", "these fields will read 0 in every mem.sample line"))
	}
	return w
}

// Enabled reports whether sampling is on. A nil Watcher is disabled, so
// callers need no branch.
func (w *Watcher) Enabled() bool { return w != nil && w.cfg.Interval > 0 }

// Collect takes one observation across all three planes.
//
// full requests the unaggregated per-process list, which only the
// threshold capture needs; ordinary samples pass false so a sample
// never retains a slice sized by the process table.
func (w *Watcher) Collect(full bool) Sample {
	start := w.now()
	s := Sample{
		At:       start,
		Go:       w.goms.read(),
		Cgroup:   readCgroup(w.cgroupRoot),
		Children: w.procs.scan(w.procRoot, w.self, full),
	}
	w.seq++
	s.Seq = w.seq
	s.Cost = time.Since(start)
	return s
}

// Run samples until ctx is cancelled. It is meant to be called in its
// own goroutine and returns when the context ends.
//
// The first sample is taken immediately rather than after one interval:
// a process that dies 8 seconds into a 15-second interval would
// otherwise leave no memory line at all, and "the crash produced no
// evidence" is precisely the condition this package removes.
func (w *Watcher) Run(ctx context.Context) {
	if !w.Enabled() {
		return
	}
	w.logStart(ctx)

	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick takes one sample, logs it, and captures if the ceiling is close.
func (w *Watcher) tick(ctx context.Context) {
	s := w.Collect(false)
	w.logger.LogAttrs(ctx, slog.LevelInfo, "mem.sample", s.attrs()...)
	w.maybeCapture(ctx, s)
}

// logStart states the watcher's own configuration once, so a reader of
// a failed run can tell "the threshold never fired" from "captures were
// switched off".
func (w *Watcher) logStart(ctx context.Context) {
	w.logger.LogAttrs(ctx, slog.LevelInfo, "mem.watch.start",
		slog.Duration("interval", w.cfg.Interval),
		slog.Float64("capture_threshold", w.cfg.Threshold),
		slog.String("capture_dir", w.cfg.CaptureDir),
		slog.Int("capture_keep", w.cfg.CaptureKeep),
		slog.Duration("capture_min_interval", w.cfg.CaptureMinInterval),
		slog.Bool("captures_enabled", w.cfg.Threshold > 0),
	)
}

// LimitReport is the boot-time answer to "is the GOMEMLIMIT derivation
// taking effect", which #781 shipped and #886/#887 could not confirm
// from outside the process.
type LimitReport struct {
	// Effective is the runtime's live GOMEMLIMIT, read from the
	// runtime itself rather than from the environment or from what we
	// intended to set. math.MaxInt64 means no limit.
	Effective int64
	// SourceKind is one of the memlimit.Kind* values: which of the
	// three mutually exclusive paths produced Effective.
	SourceKind string
	// Detail is the human explanation — the cgroup file read, or the
	// reason the runtime was left alone.
	Detail string
	// CgroupLimit is the ceiling the container was given, 0 if none.
	CgroupLimit int64
	// Ratio is the fraction applied to CgroupLimit.
	Ratio float64
}

// LogLimit emits the single boot line. Kept in this package rather than
// inline in main so the boot line and the sample lines cannot drift
// apart in naming.
func LogLimit(ctx context.Context, logger *slog.Logger, r LimitReport) {
	unlimited := r.Effective == math.MaxInt64
	logger.LogAttrs(ctx, slog.LevelInfo, "mem.limit",
		slog.Int64("gomemlimit_bytes", r.Effective),
		slog.Bool("gomemlimit_unlimited", unlimited),
		slog.String("source", r.SourceKind),
		slog.String("detail", r.Detail),
		slog.Int64("cgroup_limit_bytes", r.CgroupLimit),
		slog.Float64("ratio", r.Ratio),
	)
}
