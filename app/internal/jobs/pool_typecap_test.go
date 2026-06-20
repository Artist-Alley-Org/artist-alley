// Phase 1.14.A — per-type concurrency cap on Pool.
//
// Pure-Go tests of the gate logic (tryReserve / confirmReservation
// / release) — no DB or worker spin-up needed. The full
// integration is exercised indirectly through Worker.Run when the
// boot wire constructs a Pool with a TypeConcurrency map.
package jobs

import (
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

func TestPool_ConfirmAndRelease_AdjustCounter(t *testing.T) {
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 4}}
	p.confirmReservation("ai.tag")
	p.confirmReservation("ai.tag")
	if p.typeRunning["ai.tag"] != 2 {
		t.Errorf("confirm twice should set count to 2, got %d", p.typeRunning["ai.tag"])
	}
	p.release("ai.tag")
	if p.typeRunning["ai.tag"] != 1 {
		t.Errorf("release should decrement to 1, got %d", p.typeRunning["ai.tag"])
	}
}

func TestPool_Release_UncappedType_NoOp(t *testing.T) {
	// A type without an entry in TypeConcurrency should release
	// cleanly even though confirmReservation was a no-op for it.
	p := &Pool{TypeConcurrency: map[JobType]int{"ai.tag": 4}}
	p.release("not.capped") // shouldn't panic, shouldn't change anything
	if p.typeRunning != nil && p.typeRunning["not.capped"] != 0 {
		t.Errorf("uncapped release shouldn't touch counter; got %v", p.typeRunning)
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
