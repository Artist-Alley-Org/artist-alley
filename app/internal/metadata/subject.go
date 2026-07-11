// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — typed SubjectKind discriminator for field_definition.
//
// One field_definition row describes EITHER an asset-side field OR
// a collection-side field, never both. The discriminator is enforced
// at the schema layer by a CHECK constraint and surfaced here as a
// typed Go constant per ADR 0042 (distributed catalogues — typed in
// the same package that owns the data so the constant and the
// constraint can't drift).
package metadata

import "fmt"

// SubjectKind names what a field_definition describes. Closed set;
// extending it requires both a schema migration (relax the CHECK)
// and a code change here.
type SubjectKind string

const (
	// SubjectAsset — the field hangs off an individual asset row.
	// All pre-1.9.B fields are this kind via the migration's DEFAULT.
	SubjectAsset SubjectKind = "asset"

	// SubjectCollection — the field hangs off a whole collection.
	// Introduced in 1.9.B. Collections aren't federated yet (ADR
	// 0043), so collection-scoped values are local-instance only.
	SubjectCollection SubjectKind = "collection"
)

// AllSubjectKinds is the closed enumeration. Useful for tests +
// admin UI generators that need to render the picker without
// hardcoding the list in two places.
var AllSubjectKinds = []SubjectKind{SubjectAsset, SubjectCollection}

// Valid reports whether s is one of the known constants. The schema
// CHECK is the authoritative gate; this is a frontline check for
// API input.
func (s SubjectKind) Valid() bool {
	switch s {
	case SubjectAsset, SubjectCollection:
		return true
	}
	return false
}

// ParseSubjectKind round-trips a string through the typed constants.
// Returns a friendly error so the API handler can surface a 422
// without leaking the underlying CHECK constraint name.
func ParseSubjectKind(s string) (SubjectKind, error) {
	k := SubjectKind(s)
	if !k.Valid() {
		return "", fmt.Errorf("metadata: invalid subject_kind %q (want one of %v)", s, AllSubjectKinds)
	}
	return k, nil
}
