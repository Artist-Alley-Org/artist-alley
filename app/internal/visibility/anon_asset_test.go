// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1209: the pin between the anonymous asset rule's two consumers.
//
// [Predicate.ToSQL] renders the rule for Postgres; [AnonymouslyVisible]
// evaluates it in Go so a cover picker can warn about a pick strangers
// will not be shown. Both now walk one list of conjuncts, and these
// tests are what stops that list quietly shrinking.
//
// TWO TESTS, ON PURPOSE, AND THE FIRST ONE IS THE ONE THAT ALWAYS RUNS.
// The oracle below is the stronger test and it SKIPS without
// AA_DB_PASSWORD, so on its own it would let a dropped conjunct reach a
// green local run. The literal pin needs no database and fails on the
// dropped conjunct by itself.

package visibility

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestAnonymousAssetConditions_Pinned states the rule as a literal, so
// a dropped or retitled conjunct is a test edit rather than a silent
// widening. Deriving the expectation from `anonymousAssetConditions`
// would be a test that asserts a list equals itself.
func TestAnonymousAssetConditions_Pinned(t *testing.T) {
	want := []struct{ column, value string }{
		{"status", "active"},
		{"sensitivity", "public"},
		{"processing_status", "ready"},
	}
	if len(anonymousAssetConditions) != len(want) {
		t.Fatalf("the anonymous asset rule changed shape: %d conjuncts, expected %d.\n"+
			"ADR 0063 governs this rule and ADR 0020 governs the tiers. If the change is "+
			"intended, edit this expectation deliberately and say why in the PR.",
			len(anonymousAssetConditions), len(want))
	}
	for i, w := range want {
		got := anonymousAssetConditions[i]
		if got.Column != w.column || got.Want != w.value {
			t.Errorf("conjunct %d: got %s = %q, want %s = %q",
				i, got.Column, got.Want, w.column, w.value)
		}
	}

	// The rendered fragment, including the soft-delete conjunct and the
	// alias handling, exactly as every splice site receives it.
	pred, err := Filter(context.Background(), EntityAsset, NewCaller(nil))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	frag, args := pred.ToSQL("a", 0)
	const wantFrag = " AND (a.deleted_at IS NULL AND a.status = 'active' AND " +
		"a.sensitivity = 'public' AND a.processing_status = 'ready')"
	if frag != wantFrag {
		t.Errorf("anonymous asset fragment:\n got %q\nwant %q", frag, wantFrag)
	}
	if len(args) != 0 {
		t.Errorf("anonymous branches must bind no arguments; got %d", len(args))
	}
}

// TestAnonymouslyVisible_EachConjunctIsLoadBearing flips one column at
// a time off an otherwise-visible row. A conjunct deleted from the
// table stops making its row invisible, and its case here goes red.
func TestAnonymouslyVisible_EachConjunctIsLoadBearing(t *testing.T) {
	base := FieldsRow{Status: "active", Sensitivity: "public", ProcessingStatus: "ready"}
	if !AnonymouslyVisible(base, false) {
		t.Fatalf("the baseline row must be anonymously visible; got false")
	}
	if AnonymouslyVisible(base, true) {
		t.Errorf("a soft-deleted row must never be anonymously visible")
	}

	// One deliberately-wrong value per column, chosen from the values
	// the column's CHECK constraint actually admits.
	broken := map[string]FieldsRow{
		"status":            {Status: "draft", Sensitivity: "public", ProcessingStatus: "ready"},
		"sensitivity":       {Status: "active", Sensitivity: "team", ProcessingStatus: "ready"},
		"processing_status": {Status: "active", Sensitivity: "public", ProcessingStatus: "pending"},
	}
	for _, c := range anonymousAssetConditions {
		row, ok := broken[c.Column]
		if !ok {
			t.Fatalf("conjunct %q has no failing case here; add one rather than "+
				"leaving the new column unpinned", c.Column)
		}
		if AnonymouslyVisible(row, false) {
			t.Errorf("%s: a row failing this conjunct was reported anonymously visible", c.Column)
		}
	}
}

// TestAnonymouslyVisible_MatchesSQL is the oracle: every combination of
// the four axes, seeded as real rows, filtered by the REAL predicate,
// and compared against the Go evaluator row by row.
//
// It catches the failure the literal pin cannot: the two consumers
// still holding three conjuncts each but disagreeing about what one of
// them means.
func TestAnonymouslyVisible_MatchesSQL(t *testing.T) {
	pool := matrixPool(t)
	ctx := context.Background()
	const owner int64 = 4120901

	statuses := []string{"draft", "active", "archived"}
	sensitivities := []string{"public", "team", "restricted", "embargo"}
	processing := []string{"pending", "processing", "ready", "failed"}
	deleted := []bool{false, true}

	type seeded struct {
		id  uuid.UUID
		row FieldsRow
		del bool
	}
	var all []seeded
	var ids []uuid.UUID
	for _, st := range statuses {
		for _, sv := range sensitivities {
			for _, ps := range processing {
				for _, d := range deleted {
					id := uuid.New()
					del := "NULL"
					if d {
						del = "NOW()"
					}
					if _, err := pool.Exec(ctx, fmt.Sprintf(`
						INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, deleted_at)
						VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), $4, $5, $6, %s)`, del),
						id, "anon-oracle-"+st+"-"+sv+"-"+ps, owner, st, sv, ps); err != nil {
						t.Fatalf("seed %s/%s/%s: %v", st, sv, ps, err)
					}
					ids = append(ids, id)
					all = append(all, seeded{
						id:  id,
						row: FieldsRow{Status: st, Sensitivity: sv, ProcessingStatus: ps},
						del: d,
					})
				}
			}
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
	})

	sqlSees := visibleIDs(t, pool, EntityAsset, NewCaller(nil), "assets", ids)

	// The denominator, stated: a run that seeded nothing would agree
	// with anything.
	if len(all) != len(statuses)*len(sensitivities)*len(processing)*len(deleted) {
		t.Fatalf("seeded %d rows, expected the full cross product", len(all))
	}
	visible := 0
	for _, s := range all {
		goSays := AnonymouslyVisible(s.row, s.del)
		if goSays {
			visible++
		}
		if goSays != sqlSees[s.id] {
			t.Errorf("status=%s sensitivity=%s processing=%s deleted=%v: "+
				"AnonymouslyVisible=%v but the SQL predicate says %v",
				s.row.Status, s.row.Sensitivity, s.row.ProcessingStatus, s.del,
				goSays, sqlSees[s.id])
		}
	}
	// Exactly one combination passes all four conjuncts. A run where
	// everything agreed on "invisible" would otherwise pass.
	if visible != 1 {
		t.Errorf("expected exactly one anonymously visible combination, got %d", visible)
	}
}

// TestAnonymousAssetSQL_AliasHandling covers the un-aliased form the
// matrix harness itself uses, so the shared renderer is exercised on
// both shapes.
func TestAnonymousAssetSQL_AliasHandling(t *testing.T) {
	got := strings.Join(anonymousAssetSQL(""), " AND ")
	const want = "status = 'active' AND sensitivity = 'public' AND processing_status = 'ready'"
	if got != want {
		t.Errorf("un-aliased render:\n got %q\nwant %q", got, want)
	}
}
