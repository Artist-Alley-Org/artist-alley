// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/saved"
)

func idFromHex(h string) uuid.UUID {
	u, err := uuid.Parse("00000000-0000-0000-0000-" + strings.Repeat("0", 12-len(h)) + h)
	if err != nil {
		panic(err)
	}
	return u
}

func TestDelta_FirstRun_HashChanged_AllAdded(t *testing.T) {
	prev := saved.Row{LastResultHash: nil}
	curr := saved.RunResult{
		HitIDs: []uuid.UUID{idFromHex("a"), idFromHex("b")},
		Hash:   "fake-hash",
	}
	d := saved.ComputeDelta(prev, curr)
	if !d.HashChanged {
		t.Errorf("HashChanged=false; want true on first run")
	}
	if len(d.Added) != 2 {
		t.Errorf("Added len=%d; want 2 on first run", len(d.Added))
	}
	if len(d.Removed) != 0 {
		t.Errorf("Removed len=%d; want 0 on first run", len(d.Removed))
	}
}

func TestDelta_IdenticalRerun_NoChange(t *testing.T) {
	h := "shared-hash"
	prev := saved.Row{
		LastResultHash: &h,
		LastResultIDs:  []uuid.UUID{idFromHex("a"), idFromHex("b")},
	}
	curr := saved.RunResult{
		HitIDs: []uuid.UUID{idFromHex("a"), idFromHex("b")},
		Hash:   h,
	}
	d := saved.ComputeDelta(prev, curr)
	if d.HashChanged {
		t.Errorf("HashChanged=true; want false on identical rerun")
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Errorf("delta drift on identical rerun: added=%d removed=%d", len(d.Added), len(d.Removed))
	}
	if d.Unchanged != 2 {
		t.Errorf("Unchanged=%d; want 2", d.Unchanged)
	}
}

func TestDelta_NewHit_HashChanged_OnlyNewInAdded(t *testing.T) {
	oldHash := "old-hash"
	prev := saved.Row{
		LastResultHash: &oldHash,
		LastResultIDs:  []uuid.UUID{idFromHex("a")},
	}
	curr := saved.RunResult{
		HitIDs: []uuid.UUID{idFromHex("a"), idFromHex("b")},
		Hash:   "new-hash",
	}
	d := saved.ComputeDelta(prev, curr)
	if !d.HashChanged {
		t.Errorf("HashChanged=false; want true after new hit")
	}
	if len(d.Added) != 1 || d.Added[0] != idFromHex("b") {
		t.Errorf("Added=%v; want [b]", d.Added)
	}
	if len(d.Removed) != 0 {
		t.Errorf("Removed=%v; want empty", d.Removed)
	}
	if d.Unchanged != 1 {
		t.Errorf("Unchanged=%d; want 1", d.Unchanged)
	}
}

func TestDelta_HitDropped_HashChanged_OnlyOldInRemoved(t *testing.T) {
	oldHash := "old-hash"
	prev := saved.Row{
		LastResultHash: &oldHash,
		LastResultIDs:  []uuid.UUID{idFromHex("a"), idFromHex("b")},
	}
	curr := saved.RunResult{
		HitIDs: []uuid.UUID{idFromHex("a")},
		Hash:   "smaller-hash",
	}
	d := saved.ComputeDelta(prev, curr)
	if !d.HashChanged {
		t.Errorf("HashChanged=false; want true after hit dropped")
	}
	if len(d.Added) != 0 {
		t.Errorf("Added=%v; want empty", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != idFromHex("b") {
		t.Errorf("Removed=%v; want [b]", d.Removed)
	}
}

func TestDelta_CompleteChange_AllAddedAllRemoved(t *testing.T) {
	oldHash := "old-hash"
	prev := saved.Row{
		LastResultHash: &oldHash,
		LastResultIDs:  []uuid.UUID{idFromHex("a"), idFromHex("b")},
	}
	curr := saved.RunResult{
		HitIDs: []uuid.UUID{idFromHex("c"), idFromHex("d")},
		Hash:   "different-hash",
	}
	d := saved.ComputeDelta(prev, curr)
	if !d.HashChanged {
		t.Errorf("HashChanged=false; want true")
	}
	if len(d.Added) != 2 || len(d.Removed) != 2 {
		t.Errorf("added=%v removed=%v; want both len 2", d.Added, d.Removed)
	}
	if d.Unchanged != 0 {
		t.Errorf("Unchanged=%d; want 0", d.Unchanged)
	}
}
