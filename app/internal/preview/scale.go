// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"image"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/sync/semaphore"

	"github.com/mscrnt/artist-alley/app/internal/memlimit"
)

// ---------------------------------------------------------------------------
// The one place the preview pipeline resamples an image.
//
// WHY THIS EXISTS. #888's instrumentation drove the CI-profile render
// storm against a 6 GiB ceiling and named the dominant allocator:
//
//	x/image/draw.(*kernelScaler).makeTmpBuf — 9.52 GB of 13.37 GB
//
// The heap retained 3.96 GB at peak and only 102 MB of it was live once
// the storm ended, so this was never a leak. The mechanism is worse
// than "a lot of garbage", though, and it is worth stating exactly
// because it explains why bounding CONCURRENCY is the fix and a bigger
// ceiling is not:
//
// x/image/draw's kernel scalers work in two passes. The horizontal pass
// writes into a [][4]float64 scratch buffer of dw × sh elements — the
// DESTINATION width by the SOURCE height, 32 bytes each — and the
// vertical pass reads it back out into dst. `Kernel.Scale` allocates
// that buffer fresh on every call and drops it on return.
//
// The buffer is therefore sized by the source, not by the thumbnail
// being produced. A single 6780×7071 source rendering its 4096-wide
// rung allocates 889 MB in one call (measured, not estimated). Eight
// preview workers doing that concurrently put multiple GB in the LIVE
// set at the same instant — and a live set that big drags the GC's own
// heap goal up with it (GOGC=100 targets 2× live), which is what lets
// several more GB of garbage pile up behind it. Measured peak on the
// unfixed storm: go_heap_live 2.14 GB, go_retained 4.68 GB, cgroup at
// 93.6 % of its ceiling.
//
// So the lever is the live set, and the live set is bounded by how many
// of these buffers may exist at once. Two mechanisms, in order of size:
//
//  1. [scaleGate] — a weighted semaphore whose unit is the scratch
//     buffer's byte count. It is not a count of operations: the storm's
//     scale ops span 2.4 MB to 889 MB (median 3.9 MB, p90 21 MB), and a
//     bound of "N at a time" would either throttle the 90 % that cost
//     nothing or fail to bound the 7 % that are 62 % of the bytes. A
//     byte budget throttles exactly the expensive ones.
//
//  2. [reusableScaler] — for the callers that resample many images at
//     IDENTICAL dimensions, `Kernel.NewScaler` returns a scaler that
//     pools its scratch buffer across calls. The turntable sprite sheet
//     is 36 cells of the same size from frames of the same size: 1,476
//     of the storm's 1,793 scale operations and 5.80 GB of its 23.31 GB
//     of scratch, collapsed to about one buffer per job.
//
// A sync.Pool cannot be reached from here — makeTmpBuf is unexported
// and internal to x/image/draw — so NewScaler is the only handle on its
// pooling, and it only helps where the dimensions actually repeat.
//
// NOT DONE, deliberately: a process-wide cache of scalers keyed by
// (dw, dh, sw, sh). The storm's hit rate for such a cache is 94.6 %
// (1,793 operations over 97 distinct tuples), which looks compelling
// until you price the retention: a cached scaler pins its pooled
// scratch buffer for as long as it stays cached, so caching the tuple
// that allocates 889 MB would hold 889 MB resident to avoid allocating
// it a second time — trading the churn this file exists to bound for
// permanent residency, which is strictly worse against a cgroup ceiling.
// The reuse that is safe is the reuse that is scoped to a job, and that
// is what reusableScaler gives.
// ---------------------------------------------------------------------------

// scratchBytesPerPixel is the size of one x/image/draw scratch element:
// a [4]float64 accumulator (RGBA at float64 precision).
const scratchBytesPerPixel = 32

// scaleScratchBytes is what one resample of a sr.Dy()-tall source into
// a dr.Dx()-wide destination will allocate inside x/image/draw.
//
// int64 throughout: dw × sh overflows int32 for perfectly ordinary
// inputs (a 4096-wide rung from a 7071-tall source is 2.9e7 elements),
// and a wrapped weight would hand the gate a negative budget request.
func scaleScratchBytes(dw, sh int) int64 {
	if dw <= 0 || sh <= 0 {
		return 0
	}
	return int64(dw) * int64(sh) * scratchBytesPerPixel
}

// scaleBudgetFraction is the share of the Go heap ceiling that all
// in-flight resample scratch may hold at once.
//
// A tenth, sized from the measured storm rather than from taste. The
// scratch is pure transient garbage, so what matters is not its total
// volume but how much of it is LIVE simultaneously — that is the term
// that drags the GC's heap goal up and lets the rest of the heap
// balloon behind it. Holding the live scratch to a tenth of the ceiling
// keeps its contribution to a GOGC=100 heap goal near a fifth, which
// leaves the goal dominated by real working set again.
//
// Expressed against GOMEMLIMIT rather than as a constant so it tracks
// AA_APP_MEM_LIMIT: an operator who gives the container 16g gets a
// proportionally larger budget without a second knob to discover, and
// the CI stack's 10g override moves it automatically.
const scaleBudgetFraction = 10

const (
	// scaleBudgetMin — 128 MiB. Below this the p99 operation (126 MB of
	// scratch on the measured storm) would serialise against itself,
	// which converts a memory bound into a throughput cliff.
	scaleBudgetMin = 128 << 20
	// scaleBudgetMax — 1 GiB. The ceiling matters for the uncapped case:
	// with no cgroup limit GOMEMLIMIT reads as math.MaxInt64, and a
	// tenth of that is not a bound. An uncapped host still should not
	// let every worker hold most of a gigabyte of scratch at once.
	scaleBudgetMax = 1 << 30
)

// scaleGate bounds the total scratch held by concurrent resamples.
//
// Weight is bytes, not operations — see the package note above. A
// single operation whose scratch exceeds the whole budget is clamped to
// the budget rather than rejected: it then runs alone, which is exactly
// the desired behaviour for the 889 MB outlier, and never turns a
// legal (if enormous) source into a failed preview.
type scaleGate struct {
	sem    *semaphore.Weighted
	budget int64
}

func newScaleGate(budget int64) *scaleGate {
	return &scaleGate{sem: semaphore.NewWeighted(budget), budget: budget}
}

// gate is the process-wide budget, built on first use rather than at
// package init: main applies GOMEMLIMIT during boot, and a package-level
// initialiser would read the ceiling before it had been set.
var (
	gateOnce sync.Once
	gateInst *scaleGate
)

func gate() *scaleGate {
	gateOnce.Do(func() { gateInst = newScaleGate(scaleBudgetBytes(memlimit.Effective())) })
	return gateInst
}

// scaleBudgetBytes derives the byte budget from a Go heap ceiling.
// Split out from init so the derivation is testable without touching
// the process-wide runtime limit.
func scaleBudgetBytes(memLimit int64) int64 {
	if memLimit <= 0 {
		return scaleBudgetMax
	}
	b := memLimit / scaleBudgetFraction
	if b < scaleBudgetMin {
		return scaleBudgetMin
	}
	if b > scaleBudgetMax {
		return scaleBudgetMax
	}
	return b
}

// ScaleBudgetBytes reports the in-flight scratch budget this process
// will enforce. Exported so the server can state it in the boot log
// next to the effective GOMEMLIMIT it is derived from — a limit nobody
// can read is one nobody can tell apart from a limit that isn't there.
func ScaleBudgetBytes() int64 { return gate().budget }

// acquire reserves n bytes of the budget and returns the release.
//
// A context error does NOT skip the resample. The gate exists to pace
// memory, not to police cancellation: callers below it return images
// rather than errors, so refusing to scale would silently produce a
// blank or unscaled variant. A cancelled job is about to fail on its
// next real checkpoint anyway; letting its final resample run ungated
// costs one buffer.
func (g *scaleGate) acquire(ctx context.Context, n int64) func() {
	if n <= 0 {
		return func() {}
	}
	if n > g.budget {
		n = g.budget
	}
	if err := g.sem.Acquire(ctx, n); err != nil {
		return func() {}
	}
	return func() { g.sem.Release(n) }
}

// scaleHQ is the one-shot high-quality resample: CatmullRom, gated on
// the scratch budget. Every preview resample goes through here or
// through reusableScaler.
func scaleHQ(ctx context.Context, dst xdraw.Image, dr image.Rectangle, src image.Image, sr image.Rectangle, op xdraw.Op) {
	release := gate().acquire(ctx, scaleScratchBytes(dr.Dx(), sr.Dy()))
	defer release()
	xdraw.CatmullRom.Scale(dst, dr, src, sr, op, nil)
}

// reusableScaler resamples a SERIES of images that share source and
// destination dimensions, reusing x/image/draw's pooled scratch buffer
// across the series instead of allocating one per call.
//
// Not safe for concurrent use, and that is the point: its lifetime is
// one job's loop, so the pooled buffer is unreachable — and collectable
// — the moment that loop returns. A shared instance would keep the
// buffer resident for the life of the process.
//
// The dimensions are re-checked on every call and the scaler rebuilt if
// they change. x/image/draw would otherwise silently fall back to an
// unpooled scale for the mismatched call, so a caller whose inputs turn
// out not to be uniform gets correct output either way — it just stops
// getting the reuse.
type reusableScaler struct {
	s              xdraw.Scaler
	dw, dh, sw, sh int
}

func (r *reusableScaler) scale(ctx context.Context, dst xdraw.Image, dr image.Rectangle, src image.Image, sr image.Rectangle, op xdraw.Op) {
	dw, dh, sw, sh := dr.Dx(), dr.Dy(), sr.Dx(), sr.Dy()
	if r.s == nil || r.dw != dw || r.dh != dh || r.sw != sw || r.sh != sh {
		r.s = xdraw.CatmullRom.NewScaler(dw, dh, sw, sh)
		r.dw, r.dh, r.sw, r.sh = dw, dh, sw, sh
	}
	release := gate().acquire(ctx, scaleScratchBytes(dw, sh))
	defer release()
	r.s.Scale(dst, dr, src, sr, op, nil)
}
