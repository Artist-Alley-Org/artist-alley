// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.A — per-type concurrency cap on Pool.
//
// Pure-Go tests of the gate logic (tryReserve / confirmReservation
// / release) — no DB or worker spin-up needed. The full
// integration is exercised indirectly through Worker.Run when the
// boot wire constructs a Pool with a TypeConcurrency map.
package jobs

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestPool_TryReserve_NoTypeConcurrencyReturnsAllTypes(t *testing.T) {
	// Default behaviour when no caps are configured: every input
	// type is eligible.
	p := &Pool{}
	got := p.tryReserve([]JobType{"a", "b", "c"})
	if len(got) != 3 {
		t.Errorf("want all 3 types, got %v", got)
	}
}

func TestPool_TryReserve_EmptyMapAlsoNoCap(t *testing.T) {
	p := &Pool{TypeConcurrency: map[JobType]int{}}
	got := p.tryReserve([]JobType{"a", "b"})
	if len(got) != 2 {
		t.Errorf("empty map = no cap; want all types, got %v", got)
	}
}

func TestPool_TryReserve_ZeroCapMeansNoCap(t *testing.T) {
	// Per the docstring: zero entries mean "no cap" for that type
	// (operator-friendly default).
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 0}}
	got := p.tryReserve([]JobType{"ai.tag"})
	if len(got) != 1 {
		t.Errorf("zero cap should still allow claim; got %v", got)
	}
}

func TestPool_TryReserve_UnderCap_TypeEligible(t *testing.T) {
	p := &Pool{
		TypeConcurrency: map[JobType]int{"ai.tag": 4},
		typeRunning:     map[JobType]int{"ai.tag": 2},
	}
	got := p.tryReserve([]JobType{"ai.tag"})
	if len(got) != 1 {
		t.Errorf("running=2 cap=4 should be eligible, got %v", got)
	}
}

func TestPool_TryReserve_AtCap_TypeFilteredOut(t *testing.T) {
	p := &Pool{
		TypeConcurrency: map[JobType]int{"ai.tag": 4},
		typeRunning:     map[JobType]int{"ai.tag": 4}, // saturated
	}
	got := p.tryReserve([]JobType{"ai.tag"})
	if len(got) != 0 {
		t.Errorf("at-cap type should be filtered out, got %v", got)
	}
}

func TestPool_TryReserve_PerTypeIndependent(t *testing.T) {
	// One type at-cap shouldn't block another type with capacity.
	p := &Pool{
		TypeConcurrency: map[JobType]int{"ai.tag": 4, "ai.caption": 2},
		typeRunning:     map[JobType]int{"ai.tag": 4, "ai.caption": 1},
	}
	got := p.tryReserve([]JobType{"ai.tag", "ai.caption"})
	if len(got) != 1 || got[0] != "ai.caption" {
		t.Errorf("expected only ai.caption; got %v", got)
	}
}

func TestPool_TryReserve_UncappedTypePassesThrough(t *testing.T) {
	// A type not in the TypeConcurrency map gets no cap — passes
	// through regardless of how many of-cap types are saturated.
	p := &Pool{
		TypeConcurrency: map[JobType]int{"ai.tag": 4},
		typeRunning:     map[JobType]int{"ai.tag": 4},
	}
	got := p.tryReserve([]JobType{"ai.tag", "asset.transcode"})
	if len(got) != 1 || got[0] != "asset.transcode" {
		t.Errorf("uncapped type should pass through, got %v", got)
	}
}

func TestPool_ReserveAndRelease_AdjustCounter(t *testing.T) {
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 4}}
	p.tryReserve([]JobType{"ai.tag"})
	p.tryReserve([]JobType{"ai.tag"})
	if p.typeRunning["ai.tag"] != 2 {
		t.Errorf("reserve twice should set count to 2, got %d", p.typeRunning["ai.tag"])
	}
	p.release("ai.tag")
	if p.typeRunning["ai.tag"] != 1 {
		t.Errorf("release should decrement to 1, got %d", p.typeRunning["ai.tag"])
	}
}

func TestPool_Release_UncappedType_NoOp(t *testing.T) {
	// A type without an entry in TypeConcurrency never took a
	// reservation, so releasing it must not touch any counter.
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 4}}
	p.release("not.capped") // shouldn't panic, shouldn't change anything
	if p.typeRunning != nil && p.typeRunning["not.capped"] != 0 {
		t.Errorf("uncapped release shouldn't touch counter; got %v", p.typeRunning)
	}
}

// The regression this whole change exists for (#777).
//
// tryReserve used to be a pure read, with the increment deferred to a
// separate confirmReservation after the DB claim. Every worker polling
// inside that window saw the same stale count and passed the same gate.
// Observed in CI run 30595183336 attempt 1: five workers running
// preview.3d against a cap of 2.
//
// Reserving is now part of the check, so the Nth caller past the cap
// gets nothing back, no matter how many callers there are.
func TestPool_TryReserve_ConcurrentCallersCannotExceedCap(t *testing.T) {
	const (
		cap     = 2
		callers = 16
	)
	p := &Pool{TypeConcurrency: map[JobType]int{"preview.3d": cap}}

	var granted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them all into the same window
			if len(p.tryReserve([]JobType{"preview.3d"})) > 0 {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != cap {
		t.Errorf("cap=%d with %d concurrent callers: %d were granted a slot; "+
			"the gate must hand out at most the cap", cap, callers, got)
	}
	if got := p.typeRunning["preview.3d"]; got != cap {
		t.Errorf("running counter = %d, want %d", got, cap)
	}
}

func TestPool_ReleaseReserved_KeepsExactlyOneForClaimedType(t *testing.T) {
	p := &Pool{TypeConcurrency: map[JobType]int{"preview.3d": 2, "preview.video": 2}}

	// A poll reserves every eligible type, because the worker cannot
	// know which one the claim query will return.
	reserved := p.tryReserve([]JobType{"preview.3d", "preview.video"})
	if len(reserved) != 2 {
		t.Fatalf("expected both types reserved, got %v", reserved)
	}

	// The claim came back as preview.video: that one stays held, the
	// speculative preview.3d slot goes straight back.
	if kept := p.releaseReserved(reserved, "preview.video"); !kept {
		t.Error("releaseReserved should report it kept the claimed type")
	}
	if got := p.typeRunning["preview.3d"]; got != 0 {
		t.Errorf("unused preview.3d reservation leaked: %d", got)
	}
	if got := p.typeRunning["preview.video"]; got != 1 {
		t.Errorf("claimed preview.video should stay held: %d", got)
	}

	p.release("preview.video")
	if got := p.typeRunning["preview.video"]; got != 0 {
		t.Errorf("counter should return to zero after the run, got %d", got)
	}
}

func TestPool_ReleaseReserved_EmptyKeepReturnsEverything(t *testing.T) {
	// Claim failed or the queue was empty: nothing runs, so the poll
	// must not leave the type looking busy.
	p := &Pool{TypeConcurrency: map[JobType]int{"preview.3d": 2}}
	reserved := p.tryReserve([]JobType{"preview.3d"})
	if kept := p.releaseReserved(reserved, ""); kept {
		t.Error("no type was claimed; releaseReserved should report kept=false")
	}
	if got := p.typeRunning["preview.3d"]; got != 0 {
		t.Errorf("reservation leaked on the empty-claim path: %d", got)
	}
}

// A leak here wedges the type at its cap for the life of the process,
// so walk a full poll cycle many times and assert it returns to zero.
func TestPool_ReserveReleaseCycle_DoesNotLeak(t *testing.T) {
	p := &Pool{TypeConcurrency: map[JobType]int{"preview.video": 2}}
	for i := 0; i < 100; i++ {
		reserved := p.tryReserve([]JobType{"preview.video", "preview.raster"})
		if kept := p.releaseReserved(reserved, "preview.video"); kept {
			p.release("preview.video")
		}
	}
	if got := p.typeRunning["preview.video"]; got != 0 {
		t.Errorf("after 100 clean cycles the counter should be 0, got %d", got)
	}
	// And the type must still be claimable.
	if got := p.tryReserve([]JobType{"preview.video"}); len(got) != 1 {
		t.Error("type wedged at cap after repeated reserve/release cycles")
	}
}

func TestPool_Release_DoesNotGoNegative(t *testing.T) {
	// Belt-and-braces: even if release is called without a prior
	// confirm (a code bug), the counter shouldn't underflow.
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 4}}
	p.release("ai.tag")
	p.release("ai.tag")
	if p.typeRunning["ai.tag"] != 0 {
		t.Errorf("counter went negative: %d", p.typeRunning["ai.tag"])
	}
}

func TestPool_TryReserve_PreservesInputOrder(t *testing.T) {
	// The router-equivalent for ordering: the worker passes types
	// in a particular order, and the gate shouldn't shuffle them.
	p := &Pool{TypeConcurrency: map[JobType]int{}}
	got := p.tryReserve([]JobType{"zzz", "aaa", "mmm"})
	if len(got) != 3 || got[0] != "zzz" || got[2] != "mmm" {
		t.Errorf("order changed: %v", got)
	}
}

// Sanity: *Pool satisfies the typeCapGate interface Worker depends on.
var _ typeCapGate = (*Pool)(nil)
