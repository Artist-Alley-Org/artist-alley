// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 — `searchable` IS FULL-TEXT INDEX PARTICIPATION, AND NOTHING ELSE.
//
// An explicit `filter=field:<code><op><value>` predicate used to require
// `field_definition.searchable = TRUE` in two places: the Go gate
// (`facet.fieldGate`, consulted once per selected dimension by
// `Selection.Authorize`) and the execution `EXISTS` over
// `asset_field_value`. Neither had anything to do with what the flag
// means.
//
// `searchable` has exactly one functional consumer,
// `rebuild_asset_search_text()`, which folds a field's `value_text` /
// `value_options` into `assets.search_text`. It answers "does this
// field's TEXT feed the index". It was never an answer to "may a caller
// name this field in a structured predicate", and conflating the two had
// a specific, invisible failure mode: a refusal from `Selection.Authorize`
// is `{Hits: [], TypesMatched: types}, nil` — HTTP 200, zero hits, no
// error — so the filter looked applied and was not.
//
// The three tests here hold down three DIFFERENT properties, and none of
// them subsumes another.
//
//	§1 THE CONTRACT, on a controlled field. Any ACTIVE field the caller
//	   may read is filterable regardless of index participation. Portable:
//	   it drives the ordinary public search path and would compile
//	   unchanged against the code that had the bug (where it FAILS with
//	   an empty hit set).
//
//	§2 THE SHIPPED CONFIGURATION, on the real `pixel_width` definition.
//	   §1 could pass on a fixture while the stock install stayed broken.
//	   ⛔ It deliberately does NOT use `rating` or `polycount`: those are
//	   `searchable = true` and would have passed on the bug.
//
//	§3 THE BOUNDARIES. Removing a conjunct from a shared predicate is
//	   only safe if the conjuncts that carry meaning survived. Lifecycle
//	   (`status = 'active'`) and caller eligibility (`read_capability`,
//	   applied in Go) are asserted with their counterweights, because a
//	   gate that refused everybody would satisfy a refusal-only test.
//
// ⛔ NONE of these asserts the old behaviour. There is no
// characterization test here to rewrite later.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	fipOwner int64 = 11731801

	// One nonsense phrase per fixture, carried in every fixture asset's
	// title and nowhere else in any developer's database, so an exact-ID
	// assertion is attributable to that fixture alone and cannot be
	// disturbed by corpus rows that happen to carry the same value.
	fipContractPhrase = "quillomber"
	fipPixelPhrase    = "vandrelith"
	fipBoundaryPhrase = "throskadge"

	// §1's controlled field: numeric, active, readable, offered on the
	// advanced page, and DELIBERATELY out of the text index.
	fipGaugeCode = "fip_open_gauge"

	// §3's four field definitions.
	fipOpenCode     = "fip_bound_open"
	fipGatedCode    = "fip_bound_gated"
	fipGatedCap     = "fip.bound.clearance.read"
	fipArchivedCode = "fip_bound_archived"
	fipUnknownCode  = "fip_bound_no_such_field"
)

// fipRun executes one search for one caller through the ORDINARY public
// path — `facet.ParseSelection` then `Engine.Run` — and returns the hit
// IDs, sorted, as strings.
//
// No new helper on the production side, and nothing here that a client
// composing `filter=field:...` does not also get. That is what makes the
// artifact portable: it compiles against the code that had the defect.
//
// It repeats the #1165 count/result agreement on every call for the same
// reason `fdRunText` does: a count that narrowed differently from the
// result set would be an oracle the hits are not.
func fipRun(
	t *testing.T, pool *pgxpool.Pool, text string,
	caps visibility.CapabilityChecker, filters ...string,
) []string {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("parse %q: %v", filters, err)
	}
	ref := fipOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          text,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
		CapChecker:    caps,
	})
	if err != nil {
		t.Fatalf("run %q: %v", filters, err)
	}
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %q — the count and the result "+
			"set must narrow together", res.TotalCount, len(res.Hits), filters)
	}
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.ID.String())
	}
	sort.Strings(out)
	return out
}

// fipWantIDs compares an exact-ID result against an expected set.
func fipWantIDs(t *testing.T, what string, got []string, want ...uuid.UUID) {
	t.Helper()
	exp := make([]string, 0, len(want))
	for _, w := range want {
		exp = append(exp, w.String())
	}
	sort.Strings(exp)
	if len(got) != len(exp) {
		t.Errorf("%s returned %d hits %v, want %d %v", what, len(got), got, len(exp), exp)
		return
	}
	for i := range got {
		if got[i] != exp[i] {
			t.Errorf("%s returned %v, want %v", what, got, exp)
			return
		}
	}
}

// fipAsset plants one active, publicly visible asset owned by fipOwner.
func fipAsset(t *testing.T, pool *pgxpool.Pool, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
		id, title, fipOwner); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	return id
}

// fipNumValue hangs one numeric value on one asset for one field.
func fipNumValue(t *testing.T, pool *pgxpool.Pool, asset, field uuid.UUID, v float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO asset_field_value (asset_id, field_id, value_num)
		VALUES ($1, $2, $3)`, asset, field, v); err != nil {
		t.Fatalf("seed value %v: %v", v, err)
	}
}

// fipCleanup removes a fixture's assets, their values and any field
// definitions it created. Through testdb.Purge, so a teardown that
// cannot do its job says so instead of leaving the next run to find out.
func fipCleanup(t *testing.T, pool *pgxpool.Pool, assets, fields []uuid.UUID) {
	t.Cleanup(func() {
		testdb.Purge(t, pool, assets,
			`DELETE FROM asset_field_value WHERE asset_id = ANY($1::uuid[])`,
			`DELETE FROM assets WHERE id = ANY($1::uuid[])`,
		)
		if len(fields) > 0 {
			testdb.Purge(t, pool, fields,
				`DELETE FROM field_definition WHERE id = ANY($1::uuid[])`,
			)
		}
	})
}

// =====================================================================
// §1 THE CONTRACT
// =====================================================================

// TestFieldFilter_NonIndexedFieldIsStillFilterable is the portable
// execution regression.
//
// The field is everything a filterable field has to be — `status =
// 'active'`, no `read_capability`, offered on the advanced page — and
// `searchable = false`, which is the operator's statement about the TEXT
// INDEX and about nothing else. A structured bound on it must select
// rows.
//
// ⛔ It asserts EXACT IDs rather than a count. "Two hits" is satisfied by
// two wrong rows, and the whole failure mode being corrected here is a
// predicate that returns a set nobody checked.
//
// FAIL-BEFORE: run unchanged against the code that conjuncted
// `searchable = TRUE`, both bounds return zero hits and both assertions
// fail. There is no version of this test that expects that.
func TestFieldFilter_NonIndexedFieldIsStillFilterable(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()

	gauge := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO field_definition
		    (id, code, label, type, subject_kind, status,
		     searchable, show_in_advanced_search, read_capability)
		VALUES ($1, $2, $2, 'number', 'asset', 'active', FALSE, TRUE, NULL)`,
		gauge, fipGaugeCode); err != nil {
		t.Fatalf("seed field %s: %v", fipGaugeCode, err)
	}

	low := fipAsset(t, pool, fipContractPhrase+" low")
	high := fipAsset(t, pool, fipContractPhrase+" high")
	fipCleanup(t, pool, []uuid.UUID{low, high}, []uuid.UUID{gauge})

	fipNumValue(t, pool, low, gauge, 101)
	fipNumValue(t, pool, high, gauge, 202)

	// The premise, asserted rather than assumed: without the field
	// filter both rows are reachable by this caller. An empty result
	// below would otherwise be indistinguishable from a fixture that
	// never became visible.
	fipWantIDs(t, "the unfiltered fixture",
		fipRun(t, pool, fipContractPhrase, nil), low, high)

	// The premise about the FLAG, likewise asserted: if this row were
	// somehow searchable = true the test would be checking nothing.
	var searchable bool
	if err := pool.QueryRow(ctx,
		`SELECT searchable FROM field_definition WHERE id = $1`, gauge).
		Scan(&searchable); err != nil {
		t.Fatalf("read back searchable: %v", err)
	}
	if searchable {
		t.Fatal("the controlled field is searchable = true; the rest of this " +
			"test would pass on the defect it exists to catch")
	}

	fipWantIDs(t, "field:"+fipGaugeCode+">=200",
		fipRun(t, pool, fipContractPhrase, nil, "field:"+fipGaugeCode+">=200"), high)
	fipWantIDs(t, "field:"+fipGaugeCode+"<=150",
		fipRun(t, pool, fipContractPhrase, nil, "field:"+fipGaugeCode+"<=150"), low)
}

// =====================================================================
// §2 THE SHIPPED CONFIGURATION
// =====================================================================

// TestFieldFilter_StockPixelDimensionsFilter proves sprint 18d's pixel
// control works on the configuration a fresh install actually has.
//
// ⛔ THE FIELD IS THE SHIPPED `pixel_width` ROW AND NOTHING IS MUTATED.
// Migration 00017 ships it `searchable = false` on purpose — "a number in
// a tsvector is noise" — and that intent is correct and untouched. A
// substitute like `rating` or `polycount` is `searchable = true` and
// would have passed while the defect was live, which is exactly what
// makes this a separate test from §1 rather than a second case in it.
func TestFieldFilter_StockPixelDimensionsFilter(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()

	// THE SHIPPED CONFIGURATION IS THE PREMISE. Read it, do not assume
	// it, and fail loudly rather than skipping: a missing or reconfigured
	// row means this test is not measuring what it claims.
	var (
		pixelID   uuid.UUID
		ftype     string
		status    string
		indexed   bool
		onAdv     bool
		readCap   *string
		appliesTo []int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT id, type, status, searchable, show_in_advanced_search,
		       read_capability, applies_to
		  FROM field_definition WHERE code = 'pixel_width'`).
		Scan(&pixelID, &ftype, &status, &indexed, &onAdv, &readCap, &appliesTo); err != nil {
		t.Fatalf("the shipped pixel_width definition (migration 00017) is not "+
			"present, so this test cannot measure the stock install: %v", err)
	}
	if ftype != "number" || status != "active" {
		t.Fatalf("pixel_width is type=%q status=%q, want number/active", ftype, status)
	}
	if indexed {
		t.Fatal("pixel_width is searchable = true in this database. The whole " +
			"point of this test is the searchable = false configuration " +
			"migration 00017 ships; against a searchable field it would " +
			"have passed on the defect too")
	}
	if !onAdv {
		t.Fatal("pixel_width has show_in_advanced_search = false; the advanced " +
			"page would not offer the control this test drives the wire for")
	}
	if readCap != nil && *readCap != "" {
		t.Fatalf("pixel_width carries read_capability %q; the shipped row has none "+
			"and the caller below holds no capabilities", *readCap)
	}
	if len(appliesTo) != 0 {
		t.Fatalf("pixel_width carries applies_to %v; the shipped row is unscoped", appliesTo)
	}

	// Values far above anything a real image carries, so the exact-ID
	// assertions cannot be disturbed by corpus rows — and the text term
	// confines the population to this fixture regardless.
	const (
		aVal = 700100.0
		bVal = 700200.0
		cVal = 700300.0
	)
	a := fipAsset(t, pool, fipPixelPhrase+" below")
	b := fipAsset(t, pool, fipPixelPhrase+" inside")
	c := fipAsset(t, pool, fipPixelPhrase+" above")
	fipCleanup(t, pool, []uuid.UUID{a, b, c}, nil)

	fipNumValue(t, pool, a, pixelID, aVal)
	fipNumValue(t, pool, b, pixelID, bVal)
	fipNumValue(t, pool, c, pixelID, cVal)

	// ⛔ THE A/B/C PREMISE IS ESTABLISHED INDEPENDENTLY, BEFORE THE
	// OUTSIDE CASE. An empty result proves the range excluded the rows
	// only if the rows exist, carry the values claimed and are reachable
	// by this caller. Without this, "outside -> empty" is satisfied by a
	// fixture that never landed.
	for _, want := range []struct {
		id  uuid.UUID
		val float64
	}{{a, aVal}, {b, bVal}, {c, cVal}} {
		var got float64
		if err := pool.QueryRow(ctx, `
			SELECT value_num FROM asset_field_value
			 WHERE asset_id = $1 AND field_id = $2`, want.id, pixelID).Scan(&got); err != nil {
			t.Fatalf("premise: no pixel_width value stored for %s: %v", want.id, err)
		}
		if got != want.val {
			t.Fatalf("premise: %s carries pixel_width %v, want %v", want.id, got, want.val)
		}
	}
	fipWantIDs(t, "premise: the unfiltered fixture",
		fipRun(t, pool, fipPixelPhrase, nil), a, b, c)

	// The four cases the control can express.
	fipWantIDs(t, "lower bound only",
		fipRun(t, pool, fipPixelPhrase, nil, "field:pixel_width>=700150"), b, c)
	fipWantIDs(t, "upper bound only",
		fipRun(t, pool, fipPixelPhrase, nil, "field:pixel_width<=700250"), a, b)
	fipWantIDs(t, "both bounds",
		fipRun(t, pool, fipPixelPhrase, nil,
			"field:pixel_width>=700150", "field:pixel_width<=700250"), b)
	fipWantIDs(t, "a range outside every fixture value",
		fipRun(t, pool, fipPixelPhrase, nil,
			"field:pixel_width>=700400", "field:pixel_width<=700500"))
}

// =====================================================================
// §3 THE BOUNDARIES
// =====================================================================

// TestFieldFilter_IndexIndependenceKeepsItsGates proves the correction
// removed the conjunct that carried no meaning and none of the ones that
// do.
//
// Four cases, and the first is the counterweight the other three need: a
// gate that refused everybody would satisfy every refusal assertion here
// and ship a dead dimension.
//
//  1. active + readable + searchable = false -> SELECTS (the correction)
//  2. active + denied read_capability        -> empty, and its holder
//     still gets the row
//  3. archived field                         -> empty
//  4. unknown code                           -> empty
//
// Cases 2, 3 and 4 must be INDISTINGUISHABLE from one another: `nil`
// error and an empty hit set, never an error that separates "no such
// field" from "a field you may not read". That is the existence oracle
// `validFieldCode` refuses one level up, and the correction must not have
// opened it.
func TestFieldFilter_IndexIndependenceKeepsItsGates(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()

	open, gated, archived := uuid.New(), uuid.New(), uuid.New()
	gatedCap := fipGatedCap
	for _, f := range []struct {
		id      uuid.UUID
		code    string
		status  string
		readCap *string
	}{
		{open, fipOpenCode, "active", nil},
		{gated, fipGatedCode, "active", &gatedCap},
		{archived, fipArchivedCode, "archived", nil},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO field_definition
			    (id, code, label, type, subject_kind, status,
			     searchable, show_in_advanced_search, read_capability)
			VALUES ($1, $2, $2, 'number', 'asset', $3, FALSE, TRUE, $4)`,
			f.id, f.code, f.status, f.readCap); err != nil {
			t.Fatalf("seed field %s: %v", f.code, err)
		}
	}

	// One asset carrying a value for all three fields, so every case
	// below differs ONLY in the field's configuration and the caller's
	// capabilities.
	subject := fipAsset(t, pool, fipBoundaryPhrase+" subject")
	fipCleanup(t, pool, []uuid.UUID{subject},
		[]uuid.UUID{open, gated, archived})
	for _, f := range []uuid.UUID{open, gated, archived} {
		fipNumValue(t, pool, subject, f, 500)
	}

	stranger := visibility.CapabilityChecker(func(string) bool { return false })
	holder := visibility.CapabilityChecker(func(c string) bool { return c == fipGatedCap })

	fipWantIDs(t, "premise: the unfiltered fixture",
		fipRun(t, pool, fipBoundaryPhrase, stranger), subject)

	// 1. THE CORRECTION. Active, readable, out of the text index, and
	//    the bound selects the row.
	fipWantIDs(t, "an active readable non-indexed field",
		fipRun(t, pool, fipBoundaryPhrase, stranger, "field:"+fipOpenCode+">=100"),
		subject)

	// 2. THE CAPABILITY GATE, both directions. Losing `searchable` from
	//    the gate must not have taken `read_capability` with it, and the
	//    holder's arm is what proves this is a gate rather than a
	//    removal of the feature.
	fipWantIDs(t, "a gated field for a caller without the capability",
		fipRun(t, pool, fipBoundaryPhrase, stranger, "field:"+fipGatedCode+">=100"))
	fipWantIDs(t, "a gated field for the capability holder",
		fipRun(t, pool, fipBoundaryPhrase, holder, "field:"+fipGatedCode+">=100"),
		subject)

	// 3. LIFECYCLE. An archived definition's values stop answering, the
	//    same rule rebuild_asset_search_text() applies on the other half
	//    of its WHERE.
	fipWantIDs(t, "an archived field",
		fipRun(t, pool, fipBoundaryPhrase, holder, "field:"+fipArchivedCode+">=100"))

	// 4. NON-ORACULAR. A well-formed code for a field that does not
	//    exist answers the way a denied one and an archived one do.
	fipWantIDs(t, "an unknown field code",
		fipRun(t, pool, fipBoundaryPhrase, holder, "field:"+fipUnknownCode+">=100"))
}
