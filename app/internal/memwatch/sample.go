// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/metrics"
	"strings"
	"time"
)

// goPlane is the first plane of a sample: what the Go runtime knows
// about itself.
//
// Read through runtime/metrics rather than runtime.ReadMemStats. Two
// reasons, both load-bearing here:
//
//   - ReadMemStats stops the world for the duration of the read. This
//     sampler runs on a fixed interval, forever, in a process whose
//     whole problem is memory pressure during heavy concurrent work;
//     a periodic STW pause is a self-inflicted latency regression on
//     exactly the workload under investigation. metrics.Read does not
//     stop the world.
//   - ReadMemStats cannot report GOMEMLIMIT or the GC's CPU limiter at
//     all. "/gc/gomemlimit:bytes" read from inside the process is the
//     only authoritative answer to "did #781's derivation take effect",
//     and it is the question this issue was opened on.
type goPlane struct {
	// HeapLiveBytes is the collector's own estimate of live (reachable)
	// heap as of the last mark. This is the figure that separates a
	// bounded high-water mark from a leak: garbage inflates
	// HeapObjects and total mapped, but only live memory ratchets.
	HeapLiveBytes int64
	// HeapObjectsBytes is heap memory occupied by objects, live or not
	// yet swept.
	HeapObjectsBytes int64
	// TotalMappedBytes is every byte of virtual memory the runtime has
	// mapped for any purpose — heap, stacks, metadata, the allocator's
	// own bookkeeping. The nearest Go-side analogue of RSS.
	TotalMappedBytes int64
	// ReleasedBytes is mapped-but-returned-to-the-OS memory. It counts
	// inside TotalMappedBytes but is NOT charged to the cgroup, so
	// TotalMapped minus Released is the honest Go-side contribution to
	// the container's charge.
	ReleasedBytes int64
	// StacksBytes is goroutine stacks (heap + OS), which grow with
	// goroutine count rather than with allocation and so ratchet
	// differently from the heap.
	StacksBytes int64
	// GCCycles is completed GC cycles. Flat across a climbing heap
	// means the collector is not running; climbing fast means it is
	// running and losing.
	GCCycles int64
	// Goroutines is the live goroutine count — a leak of goroutines
	// holding buffers looks identical to a heap leak in every other
	// field.
	Goroutines int64
	// MemLimitBytes is the runtime's effective GOMEMLIMIT.
	// math.MaxInt64 means "no limit set".
	MemLimitBytes int64
}

// RetainedBytes is TotalMapped minus Released: Go-attributable memory
// the container is actually being charged for. This is the term
// subtracted from cgroup anon to attribute the non-Go remainder.
func (g goPlane) RetainedBytes() int64 {
	r := g.TotalMappedBytes - g.ReleasedBytes
	if r < 0 {
		return 0
	}
	return r
}

// The metric names this package reads. Every one of them is checked
// against metrics.All() at construction: a name that a future Go
// release renames or drops must degrade to a zero field with a WARN,
// never to a silent zero that reads as "no memory used".
const (
	mHeapLive    = "/gc/heap/live:bytes"
	mHeapObjects = "/memory/classes/heap/objects:bytes"
	mTotal       = "/memory/classes/total:bytes"
	mReleased    = "/memory/classes/heap/released:bytes"
	mHeapStacks  = "/memory/classes/heap/stacks:bytes"
	mOSStacks    = "/memory/classes/os-stacks:bytes"
	mGCCycles    = "/gc/cycles/total:gc-cycles"
	mGoroutines  = "/sched/goroutines:goroutines"
	mMemLimit    = "/gc/gomemlimit:bytes"
)

var goMetricNames = []string{
	mHeapLive, mHeapObjects, mTotal, mReleased, mHeapStacks,
	mOSStacks, mGCCycles, mGoroutines, mMemLimit,
}

// goReader holds the pre-built metrics.Read sample slice. Reusing it
// across samples is what keeps a sample allocation-free on the steady
// path — metrics.Read fills the slice in place.
type goReader struct {
	samples []metrics.Sample
	idx     map[string]int
	missing []string
}

func newGoReader() *goReader {
	supported := map[string]bool{}
	for _, d := range metrics.All() {
		supported[d.Name] = true
	}
	r := &goReader{idx: make(map[string]int, len(goMetricNames))}
	for _, name := range goMetricNames {
		if !supported[name] {
			r.missing = append(r.missing, name)
			continue
		}
		r.idx[name] = len(r.samples)
		r.samples = append(r.samples, metrics.Sample{Name: name})
	}
	return r
}

func (r *goReader) value(name string) int64 {
	i, ok := r.idx[name]
	if !ok {
		return 0
	}
	return int64(r.samples[i].Value.Uint64())
}

func (r *goReader) read() goPlane {
	metrics.Read(r.samples)
	return goPlane{
		HeapLiveBytes:    r.value(mHeapLive),
		HeapObjectsBytes: r.value(mHeapObjects),
		TotalMappedBytes: r.value(mTotal),
		ReleasedBytes:    r.value(mReleased),
		StacksBytes:      r.value(mHeapStacks) + r.value(mOSStacks),
		GCCycles:         r.value(mGCCycles),
		Goroutines:       r.value(mGoroutines),
		MemLimitBytes:    r.value(mMemLimit),
	}
}

// Sample is one observation across all three planes, taken as close to
// simultaneously as three unsynchronised sources allow.
type Sample struct {
	At       time.Time
	Seq      int64
	Go       goPlane
	Cgroup   cgroupPlane
	Children childPlane
	// Cost is how long collection itself took. Carried in every line
	// so "cheap enough to leave on" stays a measured claim rather than
	// an assertion made once at review time.
	Cost time.Duration
}

// NonGoAnonBytes attributes the part of the container's anonymous
// memory the Go runtime does not account for: children, cgo buffers,
// anything mapped outside the runtime's allocator.
//
// This is an ESTIMATE and the arithmetic is stated so it can be argued
// with: cgroup anon minus (Go total mapped minus Go released). Both
// terms are measured over slightly different windows and the runtime's
// mapped memory is not all resident, so the result can go slightly
// negative on an idle process; it is clamped at zero. Treat a figure in
// the hundreds of megabytes as real and a figure in the tens as noise.
func (s Sample) NonGoAnonBytes() int64 {
	if !s.Cgroup.Available {
		return 0
	}
	n := s.Cgroup.AnonBytes - s.Go.RetainedBytes()
	if n < 0 {
		return 0
	}
	return n
}

// attrs renders a sample as the flat attribute list of one log line.
//
// Flat, not nested groups: an operator reading a failed CI run greps
// these out of a wall of JSON, and `grep mem.sample | jq .cg_current`
// has to keep working without knowing the group nesting.
func (s Sample) attrs() []slog.Attr {
	a := []slog.Attr{
		slog.Int64("seq", s.Seq),
		slog.Int64("cost_us", s.Cost.Microseconds()),

		// Plane 1: Go runtime.
		slog.Int64("go_heap_live", s.Go.HeapLiveBytes),
		slog.Int64("go_heap_objects", s.Go.HeapObjectsBytes),
		slog.Int64("go_total_mapped", s.Go.TotalMappedBytes),
		slog.Int64("go_released", s.Go.ReleasedBytes),
		slog.Int64("go_retained", s.Go.RetainedBytes()),
		slog.Int64("go_stacks", s.Go.StacksBytes),
		slog.Int64("go_gc_cycles", s.Go.GCCycles),
		slog.Int64("go_goroutines", s.Go.Goroutines),
		slog.Int64("go_memlimit", s.Go.MemLimitBytes),
	}

	// Plane 2: cgroup.
	if s.Cgroup.Available {
		a = append(a,
			slog.Int("cg_version", s.Cgroup.Version),
			slog.Int64("cg_current", s.Cgroup.CurrentBytes),
			slog.Int64("cg_peak", s.Cgroup.PeakBytes),
			slog.Int64("cg_anon", s.Cgroup.AnonBytes),
			slog.Int64("cg_file", s.Cgroup.FileBytes),
			slog.Int64("cg_limit", s.Cgroup.LimitBytes),
			slog.Float64("cg_used_frac", round3(s.Cgroup.UsedFraction())),
			slog.Int64("nongo_anon", s.NonGoAnonBytes()),
		)
	} else {
		a = append(a, slog.Bool("cg_available", false))
	}

	// Plane 3: descendant processes.
	if s.Children.Available {
		a = append(a,
			slog.Int("child_count", s.Children.Count),
			slog.Int("child_zombies", s.Children.Zombies),
			slog.Int64("child_rss", s.Children.RSSBytes),
			slog.String("child_top", formatRollup(s.Children.Top)),
		)
	} else {
		a = append(a, slog.Bool("child_available", false))
	}
	return a
}

// formatRollup renders the per-command table as one compact string:
// "chrome=812.4M/3,ffmpeg=210.1M/2". One field instead of one field per
// command name is the cardinality bound made structural — a log
// consumer cannot be surprised by a field it has never seen.
func formatRollup(rs []commRollup) string {
	if len(rs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range rs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s/%d", r.Comm, humanBytes(r.RSSBytes), r.Count)
	}
	return b.String()
}

// humanBytes formats for a human skimming a log, not for a parser —
// every field a tool consumes is emitted as raw bytes elsewhere in the
// same line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTP"[exp])
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

// selfPID is a variable so tests can point the descendant scan at a
// synthetic tree.
var selfPID = os.Getpid()
