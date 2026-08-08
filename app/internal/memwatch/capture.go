// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"
)

// captureTimeLayout names capture directories so that lexicographic
// order IS chronological order. The ring's pruning depends on that:
// sorting names is cheaper and more reliable than stat-ing every
// directory for an mtime that a file copy could rewrite.
const captureTimeLayout = "20060102T150405Z"

const (
	heapFile      = "heap.pprof"
	goroutineFile = "goroutine.txt"
)

// maybeCapture decides whether this sample warrants a corpse, and takes
// one if so.
func (w *Watcher) maybeCapture(ctx context.Context, s Sample) {
	if w.cfg.Threshold <= 0 {
		return
	}
	frac := s.Cgroup.UsedFraction()
	if frac < w.cfg.Threshold {
		// Log the return below the line, once, so a reader can see the
		// excursion ended rather than inferring it from silence.
		if w.above {
			w.above = false
			w.logger.LogAttrs(ctx, slog.LevelInfo, "mem.threshold.cleared",
				slog.Float64("used_frac", round3(frac)),
				slog.Float64("threshold", w.cfg.Threshold),
			)
		}
		return
	}

	first := !w.above
	w.above = true

	now := w.now()
	if !w.lastCapture.IsZero() && now.Sub(w.lastCapture) < w.cfg.CaptureMinInterval {
		// Rate-limited. Report it at DEBUG on every sample but at INFO
		// on the first crossing, so an operator can always tell "the
		// threshold was crossed and we chose not to dump" from "the
		// threshold was never crossed".
		lvl := slog.LevelDebug
		if first {
			lvl = slog.LevelInfo
		}
		w.logger.LogAttrs(ctx, lvl, "mem.capture.rate_limited",
			slog.Float64("used_frac", round3(frac)),
			slog.Duration("since_last", now.Sub(w.lastCapture)),
			slog.Duration("min_interval", w.cfg.CaptureMinInterval),
		)
		return
	}
	w.lastCapture = now
	w.capture(ctx, s, now)
}

// capture writes one capture set and prunes the ring.
//
// It forces a GC first. That costs CPU at the worst possible moment,
// and it is worth it twice over: the heap profile then reflects the
// current mark rather than a stale one, and the before/after pair
// logged below is itself the answer to the question this issue asks —
// if the container's charge falls after a forced collection the memory
// was garbage the pacer had not got to, and if it does not, the memory
// is LIVE and no amount of GC tuning will save the process.
func (w *Watcher) capture(ctx context.Context, s Sample, at time.Time) {
	dir := filepath.Join(w.cfg.CaptureDir, at.UTC().Format(captureTimeLayout))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.logger.LogAttrs(ctx, slog.LevelError, "mem.capture.failed",
			slog.String("dir", dir), slog.String("err", err.Error()))
		return
	}

	// Profiles FIRST, before the forced GC below. A heap profile's
	// inuse_space reflects the most recent completed mark, so
	// collecting first would replace the picture of the process under
	// pressure with a picture of the process just after it was
	// cleaned up — discarding exactly the evidence being collected.
	// (alloc_space is cumulative and survives either ordering.)
	heapErr := writeProfile(filepath.Join(dir, heapFile), "heap", 0)
	goroErr := writeProfile(filepath.Join(dir, goroutineFile), "goroutine", 2)

	// Re-scan with the full process list — this is the one moment the
	// unaggregated table is worth its allocation.
	children := w.procs.scan(w.procRoot, w.self, true)

	runtime.GC()
	afterGo := w.goms.read()
	afterCg := readCgroup(w.cgroupRoot)

	attrs := []slog.Attr{
		slog.String("dir", dir),
		slog.Float64("used_frac", round3(s.Cgroup.UsedFraction())),
		slog.Float64("threshold", w.cfg.Threshold),
		slog.Int64("cg_current", s.Cgroup.CurrentBytes),
		slog.Int64("cg_anon", s.Cgroup.AnonBytes),
		slog.Int64("cg_limit", s.Cgroup.LimitBytes),
		slog.Int64("go_heap_live", s.Go.HeapLiveBytes),
		slog.Int64("go_retained", s.Go.RetainedBytes()),
		slog.Int64("nongo_anon", s.NonGoAnonBytes()),
		// The reclaimability verdict.
		slog.Int64("cg_current_after_gc", afterCg.CurrentBytes),
		slog.Int64("go_heap_live_after_gc", afterGo.HeapLiveBytes),
		slog.Int64("reclaimed_by_gc", s.Cgroup.CurrentBytes-afterCg.CurrentBytes),
		slog.Int("child_count", children.Count),
		slog.Int("child_zombies", children.Zombies),
		slog.Int64("child_rss", children.RSSBytes),
	}
	if heapErr != nil {
		attrs = append(attrs, slog.String("heap_err", heapErr.Error()))
	}
	if goroErr != nil {
		attrs = append(attrs, slog.String("goroutine_err", goroErr.Error()))
	}
	w.logger.LogAttrs(ctx, slog.LevelWarn, "mem.capture", attrs...)

	// The full child table, as its own line. Bounded by the number of
	// live descendants (tens), emitted only at capture time, and the
	// only place a per-PID figure appears at all.
	w.logger.LogAttrs(ctx, slog.LevelWarn, "mem.capture.children",
		slog.String("table", formatProcTable(children.All)),
	)

	w.pruneRing(ctx)
}

// writeProfile dumps a named pprof profile to path.
//
// debug=0 for the heap profile: that is the compressed protobuf form
// `go tool pprof` opens directly. debug=2 for goroutines: nobody reads
// a goroutine profile through pprof's UI at 3 a.m., they read the full
// text stacks, and that form needs no tooling to be useful inside a CI
// artifact.
func writeProfile(path, name string, debug int) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("memwatch: no %q profile", name)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := p.WriteTo(f, debug); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// pruneRing enforces CaptureKeep. The bound is enforced here, on every
// capture, rather than assumed from the rate limit: a bound that is
// only intended is a disk that fills during the one incident it was
// meant to survive.
func (w *Watcher) pruneRing(ctx context.Context) {
	entries, err := os.ReadDir(w.cfg.CaptureDir)
	if err != nil {
		w.logger.LogAttrs(ctx, slog.LevelWarn, "mem.capture.prune_failed",
			slog.String("err", err.Error()))
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only prune what we recognise as ours. An operator who copied
		// something into this directory should not have it deleted by
		// a background goroutine.
		if _, err := time.Parse(captureTimeLayout, e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= w.cfg.CaptureKeep {
		return
	}
	sort.Strings(names) // lexicographic == chronological, by construction
	drop := names[:len(names)-w.cfg.CaptureKeep]
	for _, n := range drop {
		if err := os.RemoveAll(filepath.Join(w.cfg.CaptureDir, n)); err != nil {
			w.logger.LogAttrs(ctx, slog.LevelWarn, "mem.capture.prune_failed",
				slog.String("name", n), slog.String("err", err.Error()))
		}
	}
	w.logger.LogAttrs(ctx, slog.LevelInfo, "mem.capture.pruned",
		slog.Int("removed", len(drop)),
		slog.Int("kept", w.cfg.CaptureKeep),
	)
}

// formatProcTable renders the full descendant list for the capture log
// line: "pid:comm:state:rss" tuples, largest first.
func formatProcTable(ps []procInfo) string {
	if len(ps) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%s:%c:%s", p.PID, p.Comm, p.State, humanBytes(p.RSSBytes))
	}
	return b.String()
}
