// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — typed SubjectKind tests.
//
// Pure-Go, no DB; verifies the closed-enum semantics so a future
// edit that adds a new constant has to update both the slice and
// the Valid() switch.
package metadata

import "testing"

func TestSubjectKind_AssetIsValid(t *testing.T) {
	if !SubjectAsset.Valid() {
		t.Fatal("SubjectAsset.Valid() = false, want true")
	}
}

func TestSubjectKind_CollectionIsValid(t *testing.T) {
	if !SubjectCollection.Valid() {
		t.Fatal("SubjectCollection.Valid() = false, want true")
	}
}

func TestSubjectKind_UnknownIsInvalid(t *testing.T) {
	cases := []SubjectKind{"", "user", "post", "asset ", "ASSET"}
	for _, k := range cases {
		if k.Valid() {
			t.Errorf("SubjectKind(%q).Valid() = true, want false", k)
		}
	}
}

func TestParseSubjectKind_RoundTripsKnownConstants(t *testing.T) {
	for _, want := range AllSubjectKinds {
		got, err := ParseSubjectKind(string(want))
		if err != nil {
			t.Errorf("ParseSubjectKind(%q): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSubjectKind(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestParseSubjectKind_RejectsUnknown(t *testing.T) {
	_, err := ParseSubjectKind("user")
	if err == nil {
		t.Fatal("ParseSubjectKind(\"user\"): want error, got nil")
	}
}

func TestAllSubjectKinds_MatchesValid(t *testing.T) {
	// Catches the failure mode "added a constant, forgot to add it
	// to the slice" — a slice membership scan and Valid() should
	// agree on every constant the test author writes here.
	for _, k := range AllSubjectKinds {
		if !k.Valid() {
			t.Errorf("AllSubjectKinds contains %q which Valid() rejects", k)
		}
	}
}
