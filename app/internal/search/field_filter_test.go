// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1157 — THE `field:` FILTER DIMENSION AND ITS READ GATE.
//
// The advanced search page filters on metadata field values through one
// new facet dimension, `filter=field:<code>=<value>`. Two things about
// it need holding down by a test rather than by a comment.
//
// # 1. It filters
//
// The value has to reach BOTH storage columns, because a vocabulary
// field's storage depends on its type: `select` and `tree` write one
// slug into `value_text`, `multi_select` writes an array into
// `value_options`. A client ticking a value does not know which, so one
// expression asks both — and a test that only covered `select` would
// pass on a dimension that silently ignored every multi-select field.
//
// # 2. It does not answer questions about a field the caller cannot read
//
// `field_definition.read_capability` names a capability without which a
// caller may not read that field. #907 settled the principle for the
// five original dimensions: a filter must not answer a question about a
// column the caller may not read, because "with a narrow enough
// selection, the filter IS the item". A caller without the capability
// who could still partition the corpus by that field's values would be
// running #902's recovery attack on the field plane, one bit at a time.
//
// The assertions are COMPARATIVE — the same filter, the same fixture,
// two callers, opposite verdicts — because a gate that refused everybody
// would satisfy a leak-only test and ship a dead feature.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	fdOwner int64 = 11571101
	// fdPhrase appears in the title of every fixture asset and nowhere
	// else in any developer's database, so a count is attributable to
	// this fixture alone.
	fdPhrase = "brindlequax"
	// fdOpenCode is readable by everyone; fdGatedCode needs a capability.
	fdOpenCode  = "ff_open_stage"
	fdGatedCode = "ff_gated_clearance"
	fdGatedCap  = "ff.clearance.read"
)

// fdSeed plants two searchable field definitions and three assets:
// one carrying the open field via value_text, one carrying it via
// value_options (the multi_select path), and one carrying the gated
// field. Returns nothing but its cleanup.
func fdSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	openField, gatedField := uuid.New(), uuid.New()
	for _, f := range []struct {
		id      uuid.UUID
		code    string
		typ     string
		readCap *string
	}{
		{openField, fdOpenCode, "select", nil},
		{gatedField, fdGatedCode, "select", fdStrPtr(fdGatedCap)},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO field_definition
			    (id, code, label, type, subject_kind, searchable, status, read_capability)
			VALUES ($1, $2, $2, $3, 'asset', TRUE, 'active', $4)`,
			f.id, f.code, f.typ, f.readCap); err != nil {
			t.Fatalf("seed field %s: %v", f.code, err)
		}
	}

	textAsset, optsAsset, gatedAsset := uuid.New(), uuid.New(), uuid.New()
	for _, a := range []struct {
		id    uuid.UUID
		title string
	}{
		{textAsset, fdPhrase + " one"},
		{optsAsset, fdPhrase + " two"},
		{gatedAsset, fdPhrase + " three"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
			a.id, a.title, fdOwner); err != nil {
			t.Fatalf("seed asset %s: %v", a.title, err)
		}
	}

	// value_text (the select/tree path).
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text)
		VALUES ($1, $2, 'Locked')`, textAsset, openField); err != nil {
		t.Fatalf("seed value_text: %v", err)
	}
	// value_options (the multi_select path) — SAME field code, so the
	// two storage columns are exercised by one filter and a dimension
	// that read only one of them fails here rather than in production.
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_options)
		VALUES ($1, $2, ARRAY['Locked','Other'])`, optsAsset, openField); err != nil {
		t.Fatalf("seed value_options: %v", err)
	}
	// The gated field.
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text)
		VALUES ($1, $2, 'Secret')`, gatedAsset, gatedField); err != nil {
		t.Fatalf("seed gated value: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		ids := []uuid.UUID{textAsset, optsAsset, gatedAsset}
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{openField, gatedField})
	})
}

func fdStrPtr(s string) *string { return &s }

// fdRun executes one search with one field filter for one caller.
func fdRun(t *testing.T, pool *pgxpool.Pool, filter string, caps visibility.CapabilityChecker) int {
	t.Helper()
	return fdRunText(t, pool, fdPhrase, filter, caps)
}

// fdRunText is fdRun with the free-text term under the caller's control,
// so the filter-only path ("" text) can be driven through the same door.
func fdRunText(t *testing.T, pool *pgxpool.Pool, text, filter string, caps visibility.CapabilityChecker) int {
	t.Helper()
	sel, err := facet.ParseSelection([]string{filter})
	if err != nil {
		t.Fatalf("parse %q: %v", filter, err)
	}
	ref := fdOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          text,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
		CapChecker:    caps,
	})
	if err != nil {
		t.Fatalf("run %q: %v", filter, err)
	}
	return len(res.Hits)
}

// TestFieldFilter_MatchesBothStorageColumns is the "it filters" half.
func TestFieldFilter_MatchesBothStorageColumns(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	// Unfiltered: all three fixture assets.
	if got := fdRun(t, pool, "field:"+fdOpenCode+"=Locked", nil); got != 2 {
		t.Errorf("field:%s=Locked returned %d hits, want 2 — one stored in\n"+
			"  value_text and one in value_options. A dimension reading only\n"+
			"  one column returns 1 here and silently ignores every\n"+
			"  multi_select field in production.", fdOpenCode, got)
	}
	// A value nothing carries.
	if got := fdRun(t, pool, "field:"+fdOpenCode+"=Nonexistent", nil); got != 0 {
		t.Errorf("an unmatched value returned %d hits, want 0", got)
	}
	// A well-formed code for a field that does not exist: zero, not an
	// error — the shape check is deliberately not an existence oracle.
	if got := fdRun(t, pool, "field:ff_no_such_field=Locked", nil); got != 0 {
		t.Errorf("an unknown field code returned %d hits, want 0", got)
	}
}

// TestFieldFilter_GatedFieldRefusesWithoutTheCapability is the gate,
// with its counterweight.
func TestFieldFilter_GatedFieldRefusesWithoutTheCapability(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	filter := "field:" + fdGatedCode + "=Secret"
	holder := visibility.CapabilityChecker(func(code string) bool { return code == fdGatedCap })
	stranger := visibility.CapabilityChecker(func(string) bool { return false })

	if got := fdRun(t, pool, filter, stranger); got != 0 {
		t.Errorf("a caller WITHOUT %s got %d hits from a filter on that field, want 0.\n"+
			"  A filter must not answer a question about a column the caller\n"+
			"  may not read (#907) — with a narrow enough selection the filter\n"+
			"  IS the item.", fdGatedCap, got)
	}
	// ⭐ The counterweight. A gate that refused everyone would pass the
	// assertion above and ship a filter nobody can use.
	if got := fdRun(t, pool, filter, holder); got != 1 {
		t.Errorf("a caller WITH %s got %d hits, want 1 — this is a gate, not a\n"+
			"  removal of the feature.", fdGatedCap, got)
	}
	// And the open field is unaffected for the same stranger, so the
	// refusal is scoped to the gated field rather than to `field:`.
	if got := fdRun(t, pool, "field:"+fdOpenCode+"=Locked", stranger); got != 2 {
		t.Errorf("the ungated field returned %d hits for the stranger, want 2 —\n"+
			"  the gate keys on the FIELD, not on the dimension.", got)
	}
}

// TestFieldFilter_SelectionIsUncacheable pins the cache decision, which
// is otherwise invisible and would rot silently.
//
// A `field:` result cannot be keyed: `read_capability` names a
// capability from an open set that keyForQuery cannot enumerate, and the
// cache is consulted BEFORE Engine.Run, where the gate lives. So a
// caller whose capability was revoked would keep being served the
// narrowed page for the rest of the TTL — the revoke direction, which
// serves MORE than the caller is owed.
func TestFieldFilter_SelectionIsUncacheable(t *testing.T) {
	for _, c := range []struct {
		name   string
		filter string
		want   bool
	}{
		{"a field selection", "field:x=y", true},
		{"an ordinary selection", "extension:png", false},
		{"a collection selection", "collection:" + uuid.New().String(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sel, err := facet.ParseSelection([]string{c.filter})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := sel.NamesFieldDimension(); got != c.want {
				t.Errorf("NamesFieldDimension() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFieldFilter_FilterOnlySearchRuns is the #1157 review finding: a
// selection with NO free text is a runnable search.
//
// It was not, for ANY dimension. `/search?filter=extension:png` with no
// `q` answered 400 `query_required` — the HTTP handler rejected on empty
// text before `filter=` was parsed at all, so this predates the `field:`
// dimension and had been dead since the rail shipped in #907. "Everything
// at pipeline stage Final" is the primary question an advanced search
// page exists to ask.
//
// Two halves, and the second is the one that could go wrong quietly:
// the text predicate has to go (`plainto_tsquery('english',”)` is the
// empty tsquery and matches no row, so keeping it would return nothing
// however the caller filtered) while the FIELD PLANE has to stay
// (AssetSearchMatchSQL is `search_text @@ q AND FieldsReadableSQL`, and
// only the first half is about text).
func TestFieldFilter_FilterOnlySearchRuns(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	// The fixture's own field, no text at all.
	if got := fdRunText(t, pool, "", "field:"+fdOpenCode+"=Locked", nil); got != 2 {
		t.Errorf("a filter-only search returned %d hits, want 2.\n"+
			"  Zero here means the empty tsquery is still being ANDed in;\n"+
			"  an error means the request never reached the Engine.", got)
	}
	// And it is still a SEARCH, not "everything": a value nothing carries
	// returns nothing rather than the whole table.
	if got := fdRunText(t, pool, "", "field:"+fdOpenCode+"=Nonexistent", nil); got != 0 {
		t.Errorf("a filter-only search on an unmatched value returned %d hits, want 0 —\n"+
			"  dropping the text predicate must not drop the FILTER with it.", got)
	}
}

// TestFieldFilter_FilterOnlyKeepsTheFieldPlane is the security half,
// asserted on the new path specifically.
//
// ⚠️ Without this, the natural implementation — skip the whole match
// fragment when the text is empty — silently sheds FieldsReadableSQL,
// because that conjunct lives INSIDE AssetSearchMatchSQL. The gate would
// still pass every existing test, all of which run with text present.
func TestFieldFilter_FilterOnlyKeepsTheFieldPlane(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	filter := "field:" + fdGatedCode + "=Secret"
	holder := visibility.CapabilityChecker(func(code string) bool { return code == fdGatedCap })
	stranger := visibility.CapabilityChecker(func(string) bool { return false })

	if got := fdRunText(t, pool, "", filter, stranger); got != 0 {
		t.Errorf("a filter-only search by a caller WITHOUT %s returned %d hits, want 0.\n"+
			"  The read gate must not depend on there being text to match.", fdGatedCap, got)
	}
	if got := fdRunText(t, pool, "", filter, holder); got != 1 {
		t.Errorf("a filter-only search by a caller WITH %s returned %d hits, want 1.", fdGatedCap, got)
	}
}

// TestEmptyRequestIsStillRejected pins the other side of the widened
// rule: text, similarity hint and selection all absent is still 400.
func TestEmptyRequestIsStillRejected(t *testing.T) {
	pool := coPool(t)
	_, err := NewEngine(pool).Run(context.Background(), Query{
		Types: []HitType{HitTypeAsset},
		Limit: 10,
	})
	if !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("a request with no text, no hint and no filters returned err=%v,\n"+
			"  want ErrEmptyQuery — widening what counts as a search must not\n"+
			"  make the genuinely empty request run.", err)
	}
}
