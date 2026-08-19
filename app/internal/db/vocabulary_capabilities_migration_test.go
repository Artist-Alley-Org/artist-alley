// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #789 / ADR 0092 — migration 00057 seeds the two vocabulary
// capabilities. This proves the Up inserts both rows and grants them to
// the roles the migration's own header names, that the Down removes
// both cleanly, and that up + down + up is idempotent-clean.
//
// The standing reachability invariant lives in
// capability_reachability_test.go and covers these automatically — a
// capability no role holds fails CI (#958). What THAT test cannot say
// is which roles, and the distinction is the whole design here: extend
// goes to `Base` so the dial's default preserves the behaviour #830
// shipped, merge goes to `Admin` alone because it rewrites records
// their owners did not touch. Seeding extend to Admin only would pass
// the reachability guard and still be the regression.

package db

import (
	"testing"
)

const (
	vocabCapsBeforeVersion = 56 // 00056_collection_cover_zoom
	vocabCapsAtVersion     = 57 // 00057_vocabulary_capabilities
)

// baseRoleID is declared in auditor_role_migration_test.go — the same
// built-in Base role, and it is the role sysconfig's `default_role`
// names, so every fresh signup lands in it.

const (
	capVocabExtend = "fields.vocabulary.extend"
	capVocabMerge  = "fields.vocabulary.merge"
)

func TestMigration00057_VocabularyCapabilities_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, vocabCapsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", vocabCapsBeforeVersion, err)
	}
	for _, code := range []string{capVocabExtend, capVocabMerge} {
		if capExists(t, sqlDB, code) {
			t.Fatalf("%s exists before 00057", code)
		}
	}

	if _, err := p.UpTo(ctx, vocabCapsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", vocabCapsAtVersion, err)
	}
	for _, code := range []string{capVocabExtend, capVocabMerge} {
		if !capExists(t, sqlDB, code) {
			t.Errorf("%s missing after 00057 Up", code)
		}
	}

	// Extend defaults to EVERYONE. Before this migration minting was
	// ungated, so an install where only admins hold it is a silent
	// regression for every artist — the friction #789 exists to remove,
	// reintroduced under the banner of adding a control.
	if !roleHasCap(t, sqlDB, baseRoleID, capVocabExtend) {
		t.Errorf("Base role missing %s — the default must preserve the pre-00057 "+
			"behaviour; an operator who wants the librarian model REVOKES it", capVocabExtend)
	}
	if !roleHasCap(t, sqlDB, adminRoleID, capVocabExtend) {
		t.Errorf("Admin role missing %s (Admin does not inherit Base's grants)", capVocabExtend)
	}

	// Merge does NOT. It rewrites other people's records.
	if !roleHasCap(t, sqlDB, adminRoleID, capVocabMerge) {
		t.Errorf("Admin role missing %s", capVocabMerge)
	}
	if roleHasCap(t, sqlDB, baseRoleID, capVocabMerge) {
		t.Errorf("Base role holds %s — every signed-in user could rewrite stored "+
			"values across the catalogue", capVocabMerge)
	}

	if _, err := p.DownTo(ctx, vocabCapsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", vocabCapsBeforeVersion, err)
	}
	for _, code := range []string{capVocabExtend, capVocabMerge} {
		if capExists(t, sqlDB, code) {
			t.Errorf("%s still present after 00057 Down", code)
		}
		if roleHasCap(t, sqlDB, baseRoleID, code) || roleHasCap(t, sqlDB, adminRoleID, code) {
			t.Errorf("a role still holds %s after 00057 Down", code)
		}
	}

	if _, err := p.UpTo(ctx, vocabCapsAtVersion); err != nil {
		t.Fatalf("re-apply 00057 after down: %v", err)
	}
	if !capExists(t, sqlDB, capVocabExtend) || !roleHasCap(t, sqlDB, baseRoleID, capVocabExtend) {
		t.Errorf("%s not restored after re-Up", capVocabExtend)
	}
	if !capExists(t, sqlDB, capVocabMerge) || !roleHasCap(t, sqlDB, adminRoleID, capVocabMerge) {
		t.Errorf("%s not restored after re-Up", capVocabMerge)
	}
}
