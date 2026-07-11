// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved

import "github.com/google/uuid"

// ComputeDelta produces the set-diff between a Row's previously
// stored IDs + the fresh RunResult's IDs. HashChanged is the
// notification trigger; Added lists the IDs the digest email
// surfaces; Removed lists the IDs that vanished (used only for
// audit + admin diagnostics — the digest deliberately doesn't
// call these out to keep the email surface small).
//
// The Unchanged count is the size of the intersection — cheap to
// compute since both slices are sorted (see [sortByID]) and lets
// tests + observability assert stability across runs.
func ComputeDelta(prev Row, current RunResult) Delta {
	hashChanged := true
	if prev.LastResultHash != nil && *prev.LastResultHash == current.Hash {
		hashChanged = false
	}
	// Never-run rows (LastResultHash == nil) always fire as
	// hash-changed. That means the first successful run of a
	// saved search sends an initial digest with every hit as
	// Added — the "here's what your search matched today" welcome
	// email. Operators tuning this can set notify_channel='none'
	// at create time then flip it later to skip the initial.
	if prev.LastResultHash == nil {
		hashChanged = true
	}

	// Both slices are sorted ascending by UUID.String() thanks to
	// sortByID; we can merge them in one linear pass.
	prevIDs := prev.LastResultIDs
	currIDs := current.HitIDs
	added := make([]uuid.UUID, 0, len(currIDs))
	removed := make([]uuid.UUID, 0, len(prevIDs))
	unchanged := 0
	i, j := 0, 0
	for i < len(prevIDs) && j < len(currIDs) {
		a := prevIDs[i].String()
		b := currIDs[j].String()
		switch {
		case a == b:
			unchanged++
			i++
			j++
		case a < b:
			removed = append(removed, prevIDs[i])
			i++
		default: // a > b
			added = append(added, currIDs[j])
			j++
		}
	}
	for ; i < len(prevIDs); i++ {
		removed = append(removed, prevIDs[i])
	}
	for ; j < len(currIDs); j++ {
		added = append(added, currIDs[j])
	}

	return Delta{
		Added:       added,
		Removed:     removed,
		Unchanged:   unchanged,
		HashChanged: hashChanged,
	}
}
