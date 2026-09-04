// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// BATCH METADATA EDIT — selection, partition arithmetic, the four
// modes, and per-type value semantics (#1173, #1119, ADR 0019).
//
// Every assertion in this file FAILS ON dev@80028e36, where neither
// endpoint exists. "The endpoint is absent" is not an adequate proof of
// anything, so each case here is written against the behaviour the
// contract specifies rather than against the presence of a route: the
// partition a target lands in, the bytes that end up in the row, the
// number of history rows, and the reconciliation of two equations.
//
// Its counterweights are elsewhere and PASS TODAY:
// per_type_empty_value_e2e_test.go proves the single-target writer's
// per-type emptiness behaviour, which is the semantic baseline this
// file's batch layer must reproduce rather than invent.
package metadata_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// A1-A9 — selection, expansion, and the partition equation
// ---------------------------------------------------------------------------

// A1. An empty selection is 400 empty_selection, and NO PARTITION
// ARITHMETIC IS ASSERTED — there is nothing to partition, and a refusal
// that shipped a zero-filled counts block would invite a client to
// treat "refused" as "nothing would change".
func TestBatch_EmptySelection(t *testing.T) {
	f := newBatchFixture(t)
	_, ctx := f.bulkOperator("empty")
	field := f.textField(false)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), nil)
	f.wantPreviewRefusal(res, 400, openapi.BatchEmptySelection)
	if res.OK != nil {
		t.Fatal("a refused selection must carry no preview")
	}
}

// A2. One asset entry.
func TestBatch_SingleAssetTarget(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("one")
	asset := f.asset(&owner, nil)
	field := f.textField(false)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("alpha"), assetEntries(asset))
	if p.Counts.Expanded != 1 || p.Counts.WouldChange != 1 {
		t.Fatalf("want 1 expanded / 1 would_change, got %+v", p.Counts)
	}
	assertReconciles(t, p.Counts)
}

// A3. ONE POST entry expanding to MORE THAN ONE asset, which is the
// whole reason the selection is typed: the client sends a post id and
// the SERVER decides what it means.
func TestBatch_PostExpandsToManyAssets(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("post")
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)
	a3 := f.asset(&owner, nil)
	post := f.post(owner, a1, a2, a3)
	field := f.textField(false)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("alpha"), postEntries(post))
	if p.Counts.Expanded != 3 {
		t.Fatalf("one post holding three assets must expand to 3 targets, got %d", p.Counts.Expanded)
	}
	if p.SelectionEntryCount != 1 {
		t.Fatalf("the selection named 1 entry, reported %d", p.SelectionEntryCount)
	}
	assertReconciles(t, p.Counts)
	for _, want := range []uuid.UUID{a1, a2, a3} {
		if _, ok := partitionOf(p, want); !ok {
			t.Fatalf("asset %s missing from the expansion", want)
		}
	}
}

// A4-A6. The equation holds at N=1, and across ALL FOUR MODES at N>=2,
// on a fixture where EVERY ADJACENT PAIR OF PARTITION COUNTS DIFFERS —
// so an implementation that conflated two of them would be caught.
func TestBatch_PartitionEquationAcrossModes(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("eq")
	other := f.user("stranger")

	field := f.field("ms", fieldSpec{
		Type: "multi_select",
		Options: []map[string]any{
			vocabOption("a", "A", "active"),
			vocabOption("b", "B", "active"),
			vocabOption("old", "Old", "deprecated"),
		},
	})

	// A deliberately uneven world: distinct counts in every partition
	// that a mode can populate.
	mine1 := f.asset(&owner, nil)
	mine2 := f.asset(&owner, nil)
	mine3 := f.asset(&owner, nil)
	theirs := f.asset(&other, nil) // unauthorized: not owned, no team
	f.setValue(mine2, field, map[string]any{"options": []string{"a"}})
	f.setValue(mine3, field, map[string]any{"options": []string{"a", "b"}})

	selection := assetEntries(mine1, mine2, mine3, theirs)
	for _, mode := range []openapi.BatchAssetFieldMode{
		openapi.BatchModeOverwrite, openapi.BatchModeFillEmpties,
		openapi.BatchModeAppend, openapi.BatchModeRemove,
	} {
		p := f.mustPreview(ctx, mode, field, optionsValue("a"), selection)
		assertReconciles(t, p.Counts)
		if p.Counts.Expanded != 4 {
			t.Fatalf("%s: want 4 expanded, got %d", mode, p.Counts.Expanded)
		}
		if p.Counts.Unauthorized != 1 {
			t.Fatalf("%s: the asset owned by a stranger must be unauthorized, got %+v", mode, p.Counts)
		}
	}
}

// A7. Mixed kinds in one selection are ordinary.
func TestBatch_MixedSelectionKinds(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mixed")
	loose := f.asset(&owner, nil)
	inPost := f.asset(&owner, nil)
	post := f.post(owner, inPost)
	field := f.textField(false)

	selection := append(assetEntries(loose), postEntries(post)...)
	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), selection)
	if p.Counts.Expanded != 2 {
		t.Fatalf("want 2 expanded from one asset and one single-member post, got %d", p.Counts.Expanded)
	}
	assertReconciles(t, p.Counts)
}

// A8. AN ASSET REACHED THROUGH TWO SELECTED POSTS IS WRITTEN ONCE.
// Asserted on the HISTORY ROW COUNT, not on the value: a double write
// stores the same bytes twice and is invisible to a value assertion.
func TestBatch_DuplicateReachWritesOnce(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("dup")
	shared := f.asset(&owner, nil)
	postA := f.post(owner, shared)
	postB := f.post(owner, shared)
	field := f.textField(false)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("once"), postEntries(postA, postB))
	if p.Counts.Expanded != 1 {
		t.Fatalf("an asset in two selected posts is ONE target, got %d", p.Counts.Expanded)
	}
	res := f.apply(ctx, p.Token, "dedupe check", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if n := f.historyCount(shared, field); n != 1 {
		t.Fatalf("want exactly 1 history row for one operator action, got %d", n)
	}
}

// A9. Expansion is SERVER-SIDE: the client sends post ids only and
// never learns membership except through the partition it gets back.
func TestBatch_ExpansionIsServerSide(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("srv")
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)
	post := f.post(owner, a1, a2)
	empty := f.post(owner)
	field := f.textField(false)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), postEntries(post, empty))
	if p.Counts.Expanded != 2 {
		t.Fatalf("want 2 expanded, got %d", p.Counts.Expanded)
	}
	// A post with no members contributes nothing and IS REPORTED.
	if p.EmptyPosts == nil || len(*p.EmptyPosts) != 1 || uuid.UUID((*p.EmptyPosts)[0]) != empty {
		t.Fatalf("the empty post must be reported, got %+v", p.EmptyPosts)
	}
}

// ---------------------------------------------------------------------------
// A10-A12 — the four modes and the ten types that refuse two of them
// ---------------------------------------------------------------------------

// A10-A11. Every mode's happy path on a type that supports it.
func TestBatch_FourModesHappyPath(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("modes")
	field := f.field("ms", fieldSpec{
		Type: "multi_select",
		Options: []map[string]any{
			vocabOption("a", "A", "active"), vocabOption("b", "B", "active"),
		},
	})

	cases := []struct {
		mode    openapi.BatchAssetFieldMode
		held    []string
		want    []string
		confirm bool
	}{
		{openapi.BatchModeOverwrite, []string{"b"}, []string{"a"}, true},
		{openapi.BatchModeFillEmpties, nil, []string{"a"}, false},
		{openapi.BatchModeAppend, []string{"b"}, []string{"b", "a"}, false},
		{openapi.BatchModeRemove, []string{"a", "b"}, []string{"b"}, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			asset := f.asset(&owner, nil)
			if tc.held != nil {
				f.setValue(asset, field, map[string]any{"options": tc.held})
			}
			p := f.mustPreview(ctx, tc.mode, field, optionsValue("a"), assetEntries(asset))
			var confirm *int
			if tc.confirm {
				confirm = intp(p.Counts.WouldChange)
			}
			res := f.apply(ctx, p.Token, "mode happy path", confirm)
			if res.OK == nil {
				t.Fatalf("apply refused: %+v", res.Refusal)
			}
			assertApplyReconciles(t, res.OK)
			got, ok := f.storedOptions(asset, field)
			if !ok {
				t.Fatalf("%s: no row stored", tc.mode)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%s: want %v, got %v", tc.mode, tc.want, got)
			}
		})
	}
}

// A12. THE TEN TYPES THAT REFUSE append AND remove, from an explicit
// table whose length is asserted — so a type added later that quietly
// dropped off this list would fail here rather than silently acquiring
// an invented set semantics.
func TestBatch_AppendRemoveUnsupportedTypes(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("unsup")
	asset := f.asset(&owner, nil)

	unsupported := []string{
		"text", "longtext", "rich_text", "select", "tree",
		"number", "boolean", "date", "datetime", "reference",
	}
	if len(unsupported) != 10 {
		t.Fatalf("the contract names TEN unsupported types; this table has %d", len(unsupported))
	}

	for _, typ := range unsupported {
		for _, mode := range []openapi.BatchAssetFieldMode{openapi.BatchModeAppend, openapi.BatchModeRemove} {
			t.Run(typ+"/"+string(mode), func(t *testing.T) {
				spec := fieldSpec{Type: typ}
				if typ == "select" || typ == "tree" {
					spec.Options = []map[string]any{vocabOption("a", "A", "active")}
				}
				field := f.field("t", spec)
				res := f.preview(ctx, mode, field, optionsValue("a"), assetEntries(asset))
				f.wantPreviewRefusal(res, 422, openapi.BatchModeNotSupportedForType)
			})
		}
	}

	// And the one type that DOES support them, so the table above is a
	// statement about ten types rather than about eleven.
	ms := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{vocabOption("a", "A", "active")}})
	if p := f.preview(ctx, openapi.BatchModeAppend, ms, optionsValue("a"), assetEntries(asset)); p.OK == nil {
		t.Fatalf("multi_select must accept append, got %d %+v", p.Status, p.Refusal)
	}
}

// ---------------------------------------------------------------------------
// A13-A15 — emptiness, per family
// ---------------------------------------------------------------------------

// A13-A14. fill_empties over every emptiness family, and BOOLEAN FALSE
// IS NOT EMPTY. A rule that tested truthiness would overwrite every
// deliberate "no" in the catalogue, and it would pass every other
// assertion in this file.
func TestBatch_FillEmpties_EmptinessFamilies(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("fill")

	t.Run("boolean false is a real value", func(t *testing.T) {
		field := f.field("bool", fieldSpec{Type: "boolean"})
		holdsFalse := f.asset(&owner, nil)
		absent := f.asset(&owner, nil)
		f.setValue(holdsFalse, field, map[string]any{"num": 0})

		p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, numValue(1),
			assetEntries(holdsFalse, absent))
		if got, _ := partitionOf(p, holdsFalse); got != openapi.BatchPartitionNoOp {
			t.Fatalf("a stored FALSE is not empty; want no_op, got %s", got)
		}
		if got, _ := partitionOf(p, absent); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("an absent row is empty; want would_change, got %s", got)
		}
	})

	t.Run("multi_select empty set", func(t *testing.T) {
		field := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{vocabOption("a", "A", "active")}})
		absent := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, optionsValue("a"), assetEntries(absent))
		if got, _ := partitionOf(p, absent); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("want would_change, got %s", got)
		}
	})

	t.Run("number null is empty, zero is not", func(t *testing.T) {
		field := f.field("num", fieldSpec{Type: "number"})
		holdsZero := f.asset(&owner, nil)
		absent := f.asset(&owner, nil)
		f.setValue(holdsZero, field, map[string]any{"num": 0})
		p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, numValue(7),
			assetEntries(holdsZero, absent))
		if got, _ := partitionOf(p, holdsZero); got != openapi.BatchPartitionNoOp {
			t.Fatalf("a stored 0 is not empty; want no_op, got %s", got)
		}
		if got, _ := partitionOf(p, absent); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("want would_change, got %s", got)
		}
	})
}

// A15. rich_text emptiness IN THE CORRECT DIRECTION. The sanitiser
// strips no empty elements, so `<p><br></p>` survives it and reads
// EMPTY — an implementation that trimmed the markup as text would call
// it non-empty and skip exactly the targets fill_empties exists for.
func TestBatch_FillEmpties_RichTextDirection(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("rt")
	field := f.field("rt", fieldSpec{Type: "rich_text"})

	semanticallyEmpty := f.asset(&owner, nil)
	visibleProse := f.asset(&owner, nil)
	f.setValue(semanticallyEmpty, field, map[string]any{"text": "<p><br></p>"})
	f.setValue(visibleProse, field, map[string]any{"text": "<p>real words</p>"})

	p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field,
		textValue("<p>filled</p>"), assetEntries(semanticallyEmpty, visibleProse))

	if got, _ := partitionOf(p, semanticallyEmpty); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("<p><br></p> is semantically EMPTY and must be filled; got %s", got)
	}
	if got, _ := partitionOf(p, visibleProse); got != openapi.BatchPartitionNoOp {
		t.Fatalf("visible prose is not empty; want no_op, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// A16-A19 — required, and the three outcomes of an empty overwrite
// ---------------------------------------------------------------------------

// A16. REQUIRED + overwrite with a semantically empty value, PER TYPE.
//
// ⚠️ The two whitespace select/tree rows assert required_value_empty and
// NOT unknown_slug. On the shipped single-target path R1 sits ABOVE the
// transaction the vocabulary gate lives in, so a required field refuses
// the empty value before anything asks whether "   " is a term. The
// batch reproduces that precedence rather than inventing its own.
func TestBatch_RequiredOverwriteEmpty_PerType(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("req")
	asset := f.asset(&owner, nil)

	vocab := []map[string]any{vocabOption("a", "A", "active")}
	cases := []struct {
		name  string
		typ   string
		opts  []map[string]any
		value openapi.BatchAssetFieldValue
	}{
		{"text empty", "text", nil, textValue("")},
		{"text whitespace", "text", nil, textValue("   ")},
		{"longtext empty", "longtext", nil, textValue("")},
		{"longtext whitespace", "longtext", nil, textValue("   ")},
		{"rich_text semantic empty", "rich_text", nil, textValue("<p><br></p>")},
		{"select empty", "select", vocab, textValue("")},
		{"select whitespace", "select", vocab, textValue("   ")},
		{"tree empty", "tree", vocab, textValue("")},
		{"tree whitespace", "tree", vocab, textValue("   ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := f.field("rq", fieldSpec{Type: tc.typ, Required: true, Options: tc.opts})
			res := f.preview(ctx, openapi.BatchModeOverwrite, field, tc.value, assetEntries(asset))
			f.wantPreviewRefusal(res, 422, openapi.BatchRequiredValueEmpty)
			if f.rowExists(asset, field) {
				t.Fatal("a refused preview must write nothing")
			}
		})
	}
}

// A17. Required + a real value is allowed, so A16 is a statement about
// emptiness rather than about `required`.
func TestBatch_RequiredOverwriteNonEmpty(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("reqok")
	asset := f.asset(&owner, nil)

	text := f.field("rq", fieldSpec{Type: "text", Required: true})
	if p := f.preview(ctx, openapi.BatchModeOverwrite, text, textValue("prose"), assetEntries(asset)); p.OK == nil {
		t.Fatalf("required + prose must be allowed, got %d %+v", p.Status, p.Refusal)
	}
	sel := f.field("rqs", fieldSpec{Type: "select", Required: true,
		Options: []map[string]any{vocabOption("a", "A", "active")}})
	if p := f.preview(ctx, openapi.BatchModeOverwrite, sel, textValue("a"), assetEntries(asset)); p.OK == nil {
		t.Fatalf("required + an active slug must be allowed, got %d %+v", p.Status, p.Refusal)
	}
}

// A18. fill_empties with a semantically empty value is refused
// BATCH-WIDE on REQUIRED AND OPTIONAL alike, on every type. The mode
// means "give the empty ones a value"; a value that is itself empty
// makes it a contradiction, and on an optional field it would write
// empty rows over precisely the targets that had none.
func TestBatch_FillEmptiesValueEmpty(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("fev")
	asset := f.asset(&owner, nil)

	for _, required := range []bool{true, false} {
		for _, tc := range []struct {
			name  string
			typ   string
			opts  []map[string]any
			value openapi.BatchAssetFieldValue
		}{
			{"text", "text", nil, textValue("")},
			{"text ws", "text", nil, textValue("  ")},
			{"longtext", "longtext", nil, textValue("")},
			{"rich_text", "rich_text", nil, textValue("<p><br></p>")},
			{"select", "select", []map[string]any{vocabOption("a", "A", "active")}, textValue("")},
			{"tree", "tree", []map[string]any{vocabOption("a", "A", "active")}, textValue("")},
		} {
			name := tc.name
			if required {
				name += "/required"
			} else {
				name += "/optional"
			}
			t.Run(name, func(t *testing.T) {
				field := f.field("fe", fieldSpec{Type: tc.typ, Required: required, Options: tc.opts})
				res := f.preview(ctx, openapi.BatchModeFillEmpties, field, tc.value, assetEntries(asset))
				f.wantPreviewRefusal(res, 422, openapi.BatchFillEmptiesValueEmpty)
				if f.rowExists(asset, field) {
					t.Fatal("zero writes")
				}
				if f.historyCount(asset, field) != 0 {
					t.Fatal("zero history rows")
				}
			})
		}
	}
}

// A19. OPTIONAL + overwrite with a semantically empty value: THREE
// DIFFERENT OUTCOMES IN ONE TABLE, which is why one assertion could not
// cover them. Nothing trims, so "   " is stored untrimmed; select ""
// never enters the vocabulary pipeline and stores as ”; select "   "
// enters it as a slug and is refused as unknown.
func TestBatch_OptionalOverwriteEmpty_ThreeOutcomes(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("opt")

	vocab := []map[string]any{vocabOption("a", "A", "active")}
	cases := []struct {
		name       string
		typ        string
		opts       []map[string]any
		send       string
		wantStored *string
		wantReason openapi.BatchAssetFieldRefusalReason
	}{
		{name: "text empty stores ''", typ: "text", send: "", wantStored: strp("")},
		{name: "text whitespace stores UNTRIMMED", typ: "text", send: "   ", wantStored: strp("   ")},
		{name: "longtext empty stores ''", typ: "longtext", send: "", wantStored: strp("")},
		{name: "longtext whitespace stores UNTRIMMED", typ: "longtext", send: "   ", wantStored: strp("   ")},
		{name: "rich_text stores the SANITISED markup", typ: "rich_text", send: "<p><br></p>", wantStored: strp("<p><br></p>")},
		{name: "select empty STORES", typ: "select", opts: vocab, send: "", wantStored: strp("")},
		{name: "select whitespace is UNKNOWN, not stored", typ: "select", opts: vocab, send: "   ", wantReason: openapi.BatchUnknownSlug},
		{name: "tree empty STORES", typ: "tree", opts: vocab, send: "", wantStored: strp("")},
		{name: "tree whitespace is UNKNOWN, not stored", typ: "tree", opts: vocab, send: "   ", wantReason: openapi.BatchUnknownSlug},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := f.field("ov", fieldSpec{Type: tc.typ, Options: tc.opts})
			asset := f.asset(&owner, nil)

			res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue(tc.send), assetEntries(asset))
			if tc.wantReason != "" {
				f.wantPreviewRefusal(res, 422, tc.wantReason)
				if f.rowExists(asset, field) {
					t.Fatal("a refused value must not be stored")
				}
				return
			}
			if res.OK == nil {
				t.Fatalf("want a preview, got %d %+v", res.Status, res.Refusal)
			}
			apply := f.apply(ctx, res.OK.Token, "optional empty overwrite", intp(res.OK.Counts.WouldChange))
			if apply.OK == nil {
				t.Fatalf("apply refused: %+v", apply.Refusal)
			}
			got, present := f.storedText(asset, field)
			if !present {
				t.Fatal("an empty OVERWRITE is a SET: the row must exist afterwards")
			}
			if got != *tc.wantStored {
				t.Fatalf("want %q stored, got %q", *tc.wantStored, got)
			}
			if n := f.historyCount(asset, field); n != 1 {
				t.Fatalf("a set writes one history row, got %d", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A20-A22 — malformed values, no accidental empty rows, alias storage
// ---------------------------------------------------------------------------

// A20. A multi_select with an empty option set is 400 value_type_mismatch
// on EVERY mode, mirroring buildUpsertParams rather than becoming an
// empty write.
func TestBatch_MultiSelectEmptyOptionSet(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mse")
	asset := f.asset(&owner, nil)
	field := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{vocabOption("a", "A", "active")}})

	for _, mode := range []openapi.BatchAssetFieldMode{
		openapi.BatchModeOverwrite, openapi.BatchModeFillEmpties,
		openapi.BatchModeAppend, openapi.BatchModeRemove,
	} {
		res := f.preview(ctx, mode, field, optionsValue(), assetEntries(asset))
		f.wantPreviewRefusal(res, 400, openapi.BatchValueTypeMismatch)
	}
}

// A21. NO ACCIDENTAL EMPTY-ROW CREATION: a fill_empties refused by A18
// leaves absent targets ABSENT.
func TestBatch_NoAccidentalEmptyRows(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("noempty")
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)
	field := f.field("t", fieldSpec{Type: "text"})

	res := f.preview(ctx, openapi.BatchModeFillEmpties, field, textValue(""), assetEntries(a1, a2))
	f.wantPreviewRefusal(res, 422, openapi.BatchFillEmptiesValueEmpty)
	for _, a := range []uuid.UUID{a1, a2} {
		if f.rowExists(a, field) {
			t.Fatalf("asset %s gained an empty row from a refused fill_empties", a)
		}
	}
}

// A22. select/tree overwrite reaching a term via an ALIAS or a MERGE
// TOMBSTONE stores THE TARGET SLUG — on a CLOSED vocabulary. Aliases
// and tombstones are curation, not extension, and restricting them to
// open fields would make the normalisation tooling useless on precisely
// the fields most likely to be curated.
func TestBatch_AliasAndTombstoneStoreTargetSlug(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("alias")

	for _, typ := range []string{"select", "tree"} {
		t.Run(typ+"/alias", func(t *testing.T) {
			field := f.field("al", fieldSpec{Type: typ, Options: []map[string]any{
				vocabOptionWith("gb", "United Kingdom", "active", map[string]any{"aliases": []string{"uk"}}),
			}})
			asset := f.asset(&owner, nil)
			p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("uk"), assetEntries(asset))
			if p.ResolvedValue.ValueText == nil || *p.ResolvedValue.ValueText != "gb" {
				t.Fatalf("the preview must bind the CANONICAL slug, got %v", p.ResolvedValue.ValueText)
			}
			res := f.apply(ctx, p.Token, "alias redirect", intp(p.Counts.WouldChange))
			if res.OK == nil {
				t.Fatalf("apply refused: %+v", res.Refusal)
			}
			if got, _ := f.storedText(asset, field); got != "gb" {
				t.Fatalf("want the TARGET slug gb stored, got %q", got)
			}
		})

		t.Run(typ+"/tombstone", func(t *testing.T) {
			field := f.field("tb", fieldSpec{Type: typ, Options: []map[string]any{
				vocabOption("united-kingdom", "United Kingdom", "active"),
				vocabOptionWith("uk", "UK", "archived", map[string]any{"replaced_by": "united-kingdom"}),
			}})
			asset := f.asset(&owner, nil)
			p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("uk"), assetEntries(asset))
			if p.ResolvedValue.ValueText == nil || *p.ResolvedValue.ValueText != "united-kingdom" {
				t.Fatalf("a merge tombstone must redirect; got %v", p.ResolvedValue.ValueText)
			}
			res := f.apply(ctx, p.Token, "tombstone redirect", intp(p.Counts.WouldChange))
			if res.OK == nil {
				t.Fatalf("apply refused: %+v", res.Refusal)
			}
			if got, _ := f.storedText(asset, field); got != "united-kingdom" {
				t.Fatalf("want united-kingdom stored, got %q", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A23-A27 — the three-way case, removal, applicability
// ---------------------------------------------------------------------------

// A23. THE THREE-WAY CASE IN ONE BATCH. A required multi_select, a
// removal of ["a"], and three targets holding ["a"], ["a","b"] and
// ["b"]: refused, would_change and no_op — all three coexisting, each
// with its own machine reason, and the equation summing.
func TestBatch_RemoveThreeWayCase(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("three")
	field := f.field("ms", fieldSpec{
		Type: "multi_select", Required: true,
		Options: []map[string]any{vocabOption("a", "A", "active"), vocabOption("b", "B", "active")},
	})

	onlyA := f.asset(&owner, nil)
	both := f.asset(&owner, nil)
	onlyB := f.asset(&owner, nil)
	f.setValue(onlyA, field, map[string]any{"options": []string{"a"}})
	f.setValue(both, field, map[string]any{"options": []string{"a", "b"}})
	f.setValue(onlyB, field, map[string]any{"options": []string{"b"}})

	p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("a"),
		assetEntries(onlyA, both, onlyB))
	assertReconciles(t, p.Counts)

	want := map[uuid.UUID]openapi.BatchAssetFieldPartition{
		onlyA: openapi.BatchPartitionRefused,
		both:  openapi.BatchPartitionWouldChange,
		onlyB: openapi.BatchPartitionNoOp,
	}
	for id, expect := range want {
		got, ok := partitionOf(p, id)
		if !ok || got != expect {
			t.Fatalf("asset %s: want %s, got %s", id, expect, got)
		}
	}
	// The refused target names WHY.
	for _, tgt := range p.Targets {
		if uuid.UUID(tgt.AssetId) == onlyA {
			if tgt.RefusalReason == nil || *tgt.RefusalReason != openapi.BatchRefusalRequiredWouldEmpty {
				t.Fatalf("the refused target must carry required_would_empty, got %v", tgt.RefusalReason)
			}
		}
	}
	if p.Counts.Refused != 1 || p.Counts.WouldChange != 1 || p.Counts.NoOp != 1 {
		t.Fatalf("want 1/1/1 refused/would_change/no_op, got %+v", p.Counts)
	}

	// A24. Refused targets are NOT WRITTEN at apply, asserted on the
	// ROW and on the HISTORY.
	beforeHistory := f.historyCount(onlyA, field)
	res := f.apply(ctx, p.Token, "three-way removal", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if got, _ := f.storedOptions(onlyA, field); strings.Join(got, ",") != "a" {
		t.Fatalf("the refused target must keep its value, got %v", got)
	}
	if f.historyCount(onlyA, field) != beforeHistory {
		t.Fatal("the refused target must gain no history row")
	}
	if got, _ := f.storedOptions(both, field); strings.Join(got, ",") != "b" {
		t.Fatalf("the would_change target must become [b], got %v", got)
	}
}

// A25. remove emptying an OPTIONAL multi_select DELETES THE ROW. It
// never writes `[]`: that is a shape the single-target writer refuses,
// and the batch has no reason to invent it.
func TestBatch_RemoveEmptyingOptionalDeletesRow(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("del")
	field := f.field("ms", fieldSpec{Type: "multi_select",
		Options: []map[string]any{vocabOption("a", "A", "active")}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"a"}})

	p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("a"), assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("want would_change, got %s", got)
	}
	res := f.apply(ctx, p.Token, "empty an optional set", intp(1))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if f.rowExists(asset, field) {
		t.Fatal("emptying an OPTIONAL multi_select must DELETE the row, not store []")
	}
	if n := f.historyCount(asset, field); n != 1 {
		t.Fatalf("the removal writes one history row, got %d", n)
	}
}

// A26. required + fill_empties with a real value writes.
func TestBatch_RequiredFillEmptiesWrites(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("rfe")
	field := f.field("t", fieldSpec{Type: "text", Required: true})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, textValue("value"), assetEntries(asset))
	res := f.apply(ctx, p.Token, "fill a required field", nil)
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if got, _ := f.storedText(asset, field); got != "value" {
		t.Fatalf("want value stored, got %q", got)
	}
}

// A27. A field that does not apply to an asset's type partitions
// `inapplicable`, WHICH IS NOT AN ERROR. Selecting a mixed bag and
// editing a field that covers only some of it is ordinary.
func TestBatch_Inapplicable(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("app")
	field := f.field("t", fieldSpec{Type: "text", AppliesTo: []int64{1}})
	matching := f.assetOfType(&owner, nil, 1, "active")
	other := f.assetOfType(&owner, nil, 2, "active")

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"),
		assetEntries(matching, other))
	assertReconciles(t, p.Counts)
	if got, _ := partitionOf(p, other); got != openapi.BatchPartitionInapplicable {
		t.Fatalf("want inapplicable, got %s", got)
	}
	if got, _ := partitionOf(p, matching); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("want would_change, got %s", got)
	}
	if p.Counts.Inapplicable != 1 {
		t.Fatalf("want 1 inapplicable, got %+v", p.Counts)
	}
}

// A80. no_op per mode — and OVERWRITE REPORTS no_op == 0 EVEN AGAINST
// AN IDENTICAL VALUE. A set advances set_at and writes a history row,
// so it changes the record even where it does not change the value, and
// calling it a no-op would under-report the count the operator confirms.
func TestBatch_OverwriteAgainstIdenticalValueIsNotNoOp(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("same")
	field := f.field("t", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"text": "same"})
	before := f.setAt(asset, field)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("same"), assetEntries(asset))
	if p.Counts.NoOp != 0 || p.Counts.WouldChange != 1 {
		t.Fatalf("overwrite against an identical value is still a change; got %+v", p.Counts)
	}
	res := f.apply(ctx, p.Token, "identical overwrite", intp(1))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if after := f.setAt(asset, field); !after.After(before) {
		t.Fatal("a set must advance set_at even when the bytes are unchanged")
	}
	if n := f.historyCount(asset, field); n != 1 {
		t.Fatalf("a set writes a history row, got %d", n)
	}
}
