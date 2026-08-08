// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xdraw "golang.org/x/image/draw"
)

// testSource builds a deterministic image with enough structure that a
// resampling difference of a single pixel shows up in a byte compare.
func testSource(w, h int) *image.RGBA {
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8((x*7 + y*3) % 256),
				G: uint8((x*x + y) % 256),
				B: uint8((x + y*y) % 256),
				A: 255,
			})
		}
	}
	return src
}

// TestScaleHQMatchesUngatedCatmullRom is the correctness half of #887:
// the gate paces memory and must not touch a single pixel. If this ever
// fails, the fix has started producing different previews — which is a
// worse outcome than the OOM it was written to prevent.
func TestScaleHQMatchesUngatedCatmullRom(t *testing.T) {
	src := testSource(97, 61)
	for _, dim := range [][2]int{{40, 25}, {17, 17}, {200, 130}, {1, 1}} {
		want := image.NewRGBA(image.Rect(0, 0, dim[0], dim[1]))
		xdraw.CatmullRom.Scale(want, want.Bounds(), src, src.Bounds(), xdraw.Over, nil)

		got := image.NewRGBA(image.Rect(0, 0, dim[0], dim[1]))
		scaleHQ(context.Background(), got, got.Bounds(), src, src.Bounds(), xdraw.Over)

		if !bytes.Equal(want.Pix, got.Pix) {
			t.Fatalf("scaleHQ %dx%d differs from CatmullRom.Scale", dim[0], dim[1])
		}
	}
}

// TestReusableScalerMatchesUngatedCatmullRom proves the sprite-sheet
// optimisation is free: Kernel.NewScaler and Kernel.Scale build the same
// weight tables, so reusing one across a series is bit-identical to
// allocating one per call — including on the second and later calls,
// which are the ones that read a POOLED scratch buffer rather than a
// fresh zeroed one.
func TestReusableScalerMatchesUngatedCatmullRom(t *testing.T) {
	var r reusableScaler
	for i := 0; i < 5; i++ {
		src := testSource(64+i, 48)
		// Same destination and source dims every iteration except the
		// varying width, which also exercises the rebuild path below.
		want := image.NewRGBA(image.Rect(0, 0, 24, 24))
		xdraw.CatmullRom.Scale(want, want.Bounds(), src, src.Bounds(), xdraw.Over, nil)

		got := image.NewRGBA(image.Rect(0, 0, 24, 24))
		r.scale(context.Background(), got, got.Bounds(), src, src.Bounds(), xdraw.Over)

		if !bytes.Equal(want.Pix, got.Pix) {
			t.Fatalf("iteration %d: reusableScaler differs from CatmullRom.Scale", i)
		}
	}
}

// TestReusableScalerRepeatedIdenticalCalls is the sprite-sheet shape
// proper: identical dimensions every time, tiled into different
// sub-rectangles of one destination. Every cell must match the ungated
// result, and the scaler must be built exactly once.
func TestReusableScalerRepeatedIdenticalCalls(t *testing.T) {
	src := testSource(80, 80)
	const cell = 20

	want := image.NewRGBA(image.Rect(0, 0, cell*3, cell*3))
	for i := 0; i < 9; i++ {
		r := image.Rect((i%3)*cell, (i/3)*cell, (i%3)*cell+cell, (i/3)*cell+cell)
		xdraw.CatmullRom.Scale(want, r, src, src.Bounds(), xdraw.Over, nil)
	}

	var rs reusableScaler
	got := image.NewRGBA(image.Rect(0, 0, cell*3, cell*3))
	var built []xdraw.Scaler
	for i := 0; i < 9; i++ {
		r := image.Rect((i%3)*cell, (i/3)*cell, (i%3)*cell+cell, (i/3)*cell+cell)
		rs.scale(context.Background(), got, r, src, src.Bounds(), xdraw.Over)
		if len(built) == 0 || built[len(built)-1] != rs.s {
			built = append(built, rs.s)
		}
	}
	if len(built) != 1 {
		t.Fatalf("scaler rebuilt %d times across identical calls, want 1", len(built))
	}
	if !bytes.Equal(want.Pix, got.Pix) {
		t.Fatal("tiled sprite sheet differs from per-cell CatmullRom.Scale")
	}
}

// TestReusableScalerRebuildsOnDimensionChange: a caller whose inputs
// turn out not to be uniform must still get correct output. x/image
// would fall back internally, but then the pooled buffer would be the
// wrong size forever after — so we rebuild and assert we did.
func TestReusableScalerRebuildsOnDimensionChange(t *testing.T) {
	var rs reusableScaler
	src1, src2 := testSource(40, 40), testSource(80, 30)

	dst := image.NewRGBA(image.Rect(0, 0, 16, 16))
	rs.scale(context.Background(), dst, dst.Bounds(), src1, src1.Bounds(), xdraw.Over)
	first := rs.s
	rs.scale(context.Background(), dst, dst.Bounds(), src2, src2.Bounds(), xdraw.Over)
	if rs.s == first {
		t.Fatal("scaler was not rebuilt after the source dimensions changed")
	}

	want := image.NewRGBA(image.Rect(0, 0, 16, 16))
	xdraw.CatmullRom.Scale(want, want.Bounds(), src2, src2.Bounds(), xdraw.Over, nil)
	if !bytes.Equal(want.Pix, dst.Pix) {
		t.Fatal("output after rebuild differs from CatmullRom.Scale")
	}
}

func TestScaleScratchBytes(t *testing.T) {
	// The case that motivates int64: 4096 × 7071 × 32 is 927 MB, which
	// is fine, but the element count alone (2.9e7) times 32 overflows a
	// 32-bit intermediate on a 32-bit build.
	if got, want := scaleScratchBytes(4096, 7071), int64(4096)*7071*32; got != want {
		t.Fatalf("scaleScratchBytes(4096, 7071) = %d, want %d", got, want)
	}
	// Degenerate inputs must not produce a negative weight — the gate
	// would treat that as "release more than you took".
	for _, c := range [][2]int{{0, 100}, {100, 0}, {-1, 10}} {
		if got := scaleScratchBytes(c[0], c[1]); got != 0 {
			t.Fatalf("scaleScratchBytes(%d, %d) = %d, want 0", c[0], c[1], got)
		}
	}
}

func TestScaleBudgetBytes(t *testing.T) {
	cases := []struct {
		name  string
		limit int64
		want  int64
	}{
		// The 6g stack this was measured against: 0.8 × 6 GiB = 5.15 GB.
		{"six_gig_container", 5153960755, 5153960755 / 10},
		// A tiny ceiling must not derive a budget smaller than one
		// ordinary rung, or the pipeline serialises on itself.
		{"tiny_ceiling", 256 << 20, scaleBudgetMin},
		// No cgroup limit: the runtime reports MaxInt64 and a tenth of
		// that is not a bound.
		{"unlimited", math.MaxInt64, scaleBudgetMax},
		{"zero", 0, scaleBudgetMax},
		{"negative", -1, scaleBudgetMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scaleBudgetBytes(c.limit); got != c.want {
				t.Fatalf("scaleBudgetBytes(%d) = %d, want %d", c.limit, got, c.want)
			}
		})
	}
}

// TestScaleGateBoundsConcurrentScratch is the point of the whole file:
// however many goroutines pile in, the sum of the scratch they hold at
// any instant stays inside the budget.
func TestScaleGateBoundsConcurrentScratch(t *testing.T) {
	const budget = 1000
	const weight = 300 // 3 fit, the 4th must wait
	g := newScaleGate(budget)

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := g.acquire(context.Background(), weight)
			defer release()
			cur := inFlight.Add(weight)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-weight)
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > budget {
		t.Fatalf("peak in-flight scratch %d exceeded budget %d", p, budget)
	}
	if p := peak.Load(); p == 0 {
		t.Fatal("nothing ever ran")
	}
}

// TestScaleGateOversizedRequestRunsAlone: an operation whose scratch
// exceeds the entire budget must still run — clamped, holding the whole
// budget — rather than block forever. The 889 MB outlier on the measured
// storm is a real asset, not a hypothetical.
func TestScaleGateOversizedRequestRunsAlone(t *testing.T) {
	g := newScaleGate(1000)
	done := make(chan struct{})
	go func() {
		release := g.acquire(context.Background(), 10_000_000)
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an over-budget acquire blocked instead of clamping")
	}
}

// TestScaleGateCancelledContextStillScales: the gate paces memory, it
// does not police cancellation. Callers below it return images rather
// than errors, so a refused acquire must not become a blank variant.
func TestScaleGateCancelledContextStillScales(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	src := testSource(50, 50)
	want := image.NewRGBA(image.Rect(0, 0, 20, 20))
	xdraw.CatmullRom.Scale(want, want.Bounds(), src, src.Bounds(), xdraw.Over, nil)

	got := image.NewRGBA(image.Rect(0, 0, 20, 20))
	scaleHQ(ctx, got, got.Bounds(), src, src.Bounds(), xdraw.Over)

	if !bytes.Equal(want.Pix, got.Pix) {
		t.Fatal("a cancelled context skipped the resample")
	}
}
