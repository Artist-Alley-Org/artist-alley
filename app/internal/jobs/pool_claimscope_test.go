// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #278 — an unrestricted worker (Types == nil, the single-process
// install shape) must still honour per-type concurrency caps. These
// pure-Go tests pin claimScope's behaviour without a DB or worker
// spin-up.
package jobs

import (
	"context"
	"encoding/json"
	"testing"
)

type scopeHandler struct{ typ JobType }

func (h scopeHandler) Type() JobType { return h.typ }
func (h scopeHandler) Handle(context.Context, *Claim) (json.RawMessage, error) {
	return nil, nil
}

func newScopeWorker(caps map[JobType]int, registered ...JobType) (*Worker, *Pool) {
	reg := NewRegistry()
	for _, t := range registered {
		reg.Register(scopeHandler{typ: t})
	}
	p := &Pool{TypeConcurrency: caps}
	w := &Worker{Service: &Service{Registry: reg}, Gate: p}
	return w, p
}

func contains(s []JobType, t JobType) bool {
	for _, x := range s {
		if x == t {
			return true
		}
	}
	return false
}

// No caps: the unrestricted worker uses a nil scope (ClaimNext claims
// any type) and never touches the gate — the cheap common path.
func TestClaimScope_UnrestrictedNoCaps_ClaimsAnything(t *testing.T) {
	w, _ := newScopeWorker(nil, "ai.embed", "preview.raster")
	got, saturated := w.claimScope()
	if got != nil || saturated {
		t.Fatalf("claimScope() = (%v, %v), want (nil, false)", got, saturated)
	}
}

// With caps, the unrestricted worker scopes its claim to the registered
// types and drops any that are at their cap — the crux of #278.
func TestClaimScope_UnrestrictedWithCaps_ExcludesTypesAtCap(t *testing.T) {
	w, p := newScopeWorker(map[JobType]int{"ai.embed": 1}, "ai.embed", "preview.raster")

	got, saturated := w.claimScope()
	if saturated {
		t.Fatal("unexpectedly saturated with nothing running")
	}
	if !contains(got, "ai.embed") || !contains(got, "preview.raster") {
		t.Fatalf("both types should be eligible, got %v", got)
	}

	// claimScope RESERVES what it returns (#777), so this poll is now
	// holding ai.embed's only slot. Hand it back the way Run does when
	// a claim comes up empty, or the next assertion would pass for the
	// wrong reason — an unreleased reservation rather than the cap.
	p.releaseReserved(got, "")

	// Saturate ai.embed (cap 1) by taking its slot for real.
	if len(p.tryReserve([]JobType{"ai.embed"})) != 1 {
		t.Fatal("ai.embed should have been free to reserve")
	}

	got, saturated = w.claimScope()
	if saturated {
		t.Fatal("should not be saturated — preview.raster is uncapped")
	}
	if contains(got, "ai.embed") {
		t.Fatalf("ai.embed at cap must be excluded, got %v", got)
	}
	if !contains(got, "preview.raster") {
		t.Fatalf("uncapped preview.raster must remain claimable, got %v", got)
	}
}

// A type-restricted worker whose only type is at cap reports saturated
// so Run backs off without a DB hit.
func TestClaimScope_TypeRestrictedAllAtCap_Saturated(t *testing.T) {
	w, p := newScopeWorker(map[JobType]int{"ai.embed": 1}, "ai.embed")
	w.Types = []JobType{"ai.embed"}
	if len(p.tryReserve([]JobType{"ai.embed"})) != 1 {
		t.Fatal("ai.embed should have been free to reserve")
	}

	got, saturated := w.claimScope()
	if !saturated {
		t.Fatalf("want saturated, got types=%v", got)
	}
}
