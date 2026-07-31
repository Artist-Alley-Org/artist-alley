// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// The storage-column pin (#778)
// ---------------------------------------------------------------------------
//
// Every field type's value lives in exactly one value_* column, and
// every writer has to pick the same one or a value written through one
// surface is invisible through another.
//
// That invariant held only by luck until #778. Six call sites had
// drifted into three different answers for `tree` — this ADR said
// value_text, the asset writer said value_text, the collection writer
// said value_options, the display read value_ref — and nothing failed,
// because no `tree` field had ever carried a value. Six silent
// disagreements is what an unpinned invariant costs.
//
// So this file pins it BEHAVIOURALLY. It does not read the writers'
// switch statements or trust their comments; it calls each writer and
// observes which param field came back populated. A future edit that
// moves a type to a different column fails here regardless of how the
// switch is spelled, and regardless of whether anyone remembered to
// update a doc comment.
//
// Adding a field type? Add it to the table. A type absent from the
// table fails TestValueColumnTableIsExhaustive.

type valueColumn string

const (
	colText    valueColumn = "value_text"
	colNum     valueColumn = "value_num"
	colDate    valueColumn = "value_date"
	colOptions valueColumn = "value_options"
	colRef     valueColumn = "value_ref"
)

// valueColumnFor is the single source of truth this package is tested
// against: field type -> the column its value belongs in.
//
// `tree` is colText and holds ONE option slug — the node selected —
// not a path string like "europe/uk/london" and not the array of slugs
// along the path. See the 2026-07-31 tree-storage amendment to ADR
// 0012 for why: a path denormalises every ancestor's slug into the
// stored value, so renaming or re-parenting an ancestor would rewrite
// every descendant's value. Slugs are unique across a field's whole
// option tree, so the node's own slug addresses it and the path is
// reassembled at read time from the options document.
var valueColumnFor = map[string]valueColumn{
	"text":         colText,
	"longtext":     colText,
	"rich_text":    colText,
	"select":       colText,
	"tree":         colText,
	"number":       colNum,
	"boolean":      colNum,
	"date":         colDate,
	"datetime":     colDate,
	"multi_select": colOptions,
	"reference":    colRef,
}

// collectionValueColumnOverride records where the COLLECTION side
// deliberately differs from the asset side.
//
// There is exactly one entry, and it is not a decision this pin made:
// the asset path has always stored booleans as 0/1 in value_num while
// the collection path stores the strings "true"/"false" in value_text
// — and the asset DISPLAY reads value_text, so an asset boolean
// renders blank today. That is the same defect class as #778's tree
// bug and, like tree, it has never been hit because no boolean field
// has ever existed either (grep the baseline: there is no
// `'boolean'` field_definition row).
//
// It is recorded rather than fixed because unifying the two encodings
// is a write-contract change with an owner-level decision attached
// (0/1 vs "true"/"false"), not a drift repair. Pinning it here at
// least makes the divergence visible in code and stops it spreading:
// any NEW divergence fails TestCollectionColumnsMatchAssetColumns.
var collectionValueColumnOverride = map[string]valueColumn{
	"boolean": colText,
}

func collectionValueColumnFor(fieldType string) valueColumn {
	if c, ok := collectionValueColumnOverride[fieldType]; ok {
		return c
	}
	return valueColumnFor[fieldType]
}

// allFieldTypes is every type field_definition_type_check accepts.
// Kept literal so a schema change that adds a type has to come here.
var allFieldTypes = []string{
	"text", "longtext", "rich_text", "number", "boolean",
	"date", "datetime", "select", "multi_select", "tree", "reference",
}

func TestValueColumnTableIsExhaustive(t *testing.T) {
	for _, typ := range allFieldTypes {
		if _, ok := valueColumnFor[typ]; !ok {
			t.Errorf("field type %q has no entry in valueColumnFor — "+
				"every accepted type must name the column it stores in", typ)
		}
		if !validFieldType(typ) {
			t.Errorf("field type %q is in allFieldTypes but validFieldType rejects it", typ)
		}
	}
	if len(valueColumnFor) != len(allFieldTypes) {
		t.Errorf("valueColumnFor has %d entries, allFieldTypes has %d — "+
			"a type was added to one and not the other",
			len(valueColumnFor), len(allFieldTypes))
	}
}

// sampleAssetWrite builds a write body carrying a plausible value in
// EVERY value_* slot, so the writer's choice of column is the writer's
// alone — nothing is forced by the input being absent.
func sampleAssetWrite() *openapi.AssetFieldValueWrite {
	text := "sample-slug"
	num := float32(1)
	date := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	opts := []string{"sample-slug"}
	ref := openapi_types.UUID(uuid.New())
	return &openapi.AssetFieldValueWrite{
		ValueText:    &text,
		ValueNum:     &num,
		ValueDate:    &date,
		ValueOptions: &opts,
		ValueRef:     &ref,
	}
}

func sampleCollectionWrite() *openapi.CollectionFieldValueWrite {
	text := "sample-slug"
	num := float32(1)
	date := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	opts := []string{"sample-slug"}
	ref := openapi_types.UUID(uuid.New())
	return &openapi.CollectionFieldValueWrite{
		ValueText:    &text,
		ValueNum:     &num,
		ValueDate:    &date,
		ValueOptions: &opts,
		ValueRef:     &ref,
	}
}

// populatedAssetColumns reports which value_* params the writer set.
// A writer that sets none, or more than one, is as broken as one that
// sets the wrong one — both are caught by comparing this to a
// single-element expectation.
func populatedAssetColumns(p UpsertAssetFieldValueParams) []valueColumn {
	var got []valueColumn
	if p.ValueText != nil {
		got = append(got, colText)
	}
	if p.ValueNum != nil {
		got = append(got, colNum)
	}
	if p.ValueDate.Valid {
		got = append(got, colDate)
	}
	if p.ValueOptions != nil {
		got = append(got, colOptions)
	}
	if p.ValueRef.Valid {
		got = append(got, colRef)
	}
	return got
}

func populatedCollectionColumns(p UpsertCollectionFieldValueParams) []valueColumn {
	var got []valueColumn
	if p.ValueText != nil {
		got = append(got, colText)
	}
	if p.ValueNum != nil {
		got = append(got, colNum)
	}
	if p.ValueDate.Valid {
		got = append(got, colDate)
	}
	if p.ValueOptions != nil {
		got = append(got, colOptions)
	}
	if p.ValueRef.Valid {
		got = append(got, colRef)
	}
	return got
}

func wantExactlyOne(t *testing.T, where, fieldType string, want valueColumn, got []valueColumn) {
	t.Helper()
	if len(got) != 1 {
		t.Errorf("%s: field type %q populated %v — must populate exactly one column (%s)",
			where, fieldType, got, want)
		return
	}
	if got[0] != want {
		t.Errorf("%s: field type %q stores in %s, want %s. "+
			"If this is a deliberate change, change valueColumnFor and every other writer with it — "+
			"a writer that disagrees with the others writes values nobody can read (#778).",
			where, fieldType, got[0], want)
	}
}

// TestAssetWriterUsesPinnedColumns pins metadata/handler.go's
// buildUpsertParams — the asset write path.
func TestAssetWriterUsesPinnedColumns(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	for _, typ := range allFieldTypes {
		t.Run(typ, func(t *testing.T) {
			p, err := buildUpsertParams(asset, field, typ, sampleAssetWrite(), nil)
			if err != nil {
				t.Fatalf("buildUpsertParams(%q): %v", typ, err)
			}
			wantExactlyOne(t, "asset write (buildUpsertParams)", typ,
				valueColumnFor[typ], populatedAssetColumns(p))
		})
	}
}

// TestCollectionWriterUsesPinnedColumns pins
// metadata/collection_handler.go's buildCollectionUpsertParams.
//
// This is the writer that had `tree` in the multi_select group, so a
// collection's tree value went to value_options while the same field's
// value on an asset went to value_text.
func TestCollectionWriterUsesPinnedColumns(t *testing.T) {
	coll := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	for _, typ := range allFieldTypes {
		t.Run(typ, func(t *testing.T) {
			p := buildCollectionUpsertParams(coll, field, typ, sampleCollectionWrite(), "manual", nil)
			wantExactlyOne(t, "collection write (buildCollectionUpsertParams)", typ,
				collectionValueColumnFor(typ), populatedCollectionColumns(p))
		})
	}
}

// TestCollectionValidatorMatchesCollectionWriter pins the THIRD
// collection-side switch — validateCollectionValueType — against the
// writer it guards. A validator demanding value_options for a type the
// writer reads out of value_text rejects every correct request, which
// is what would have happened to `tree` the moment anyone tried.
//
// It works by supplying only the column the pin names and asserting
// the validator is satisfied, then supplying an empty body and
// asserting it is not.
func TestCollectionValidatorMatchesCollectionWriter(t *testing.T) {
	for _, typ := range allFieldTypes {
		t.Run(typ, func(t *testing.T) {
			body := &openapi.CollectionFieldValueWrite{}
			text := "sample-slug"
			num := float32(1)
			date := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
			opts := []string{"sample-slug"}
			ref := openapi_types.UUID(uuid.New())

			switch collectionValueColumnFor(typ) {
			case colText:
				body.ValueText = &text
			case colNum:
				body.ValueNum = &num
			case colDate:
				body.ValueDate = &date
			case colOptions:
				body.ValueOptions = &opts
			case colRef:
				body.ValueRef = &ref
			}

			if err := validateCollectionValueType(typ, body); err != nil {
				t.Errorf("validateCollectionValueType(%q) rejected the column the writer uses (%s): %v",
					typ, collectionValueColumnFor(typ), err)
			}
			if err := validateCollectionValueType(typ, &openapi.CollectionFieldValueWrite{}); err == nil {
				t.Errorf("validateCollectionValueType(%q) accepted an empty body — "+
					"a missing value must not reach the writer", typ)
			}
		})
	}
}

// TestCollectionColumnsMatchAssetColumns is the cross-surface half of
// the pin: the same field type must land in the same column whichever
// subject it is attached to, or a value set on a collection is
// unreadable by anything that reads the asset way round.
//
// Only the documented boolean divergence is tolerated. A NEW one fails
// here, which is precisely the check that was missing when `tree`
// split in two.
func TestCollectionColumnsMatchAssetColumns(t *testing.T) {
	for _, typ := range allFieldTypes {
		want := valueColumnFor[typ]
		got := collectionValueColumnFor(typ)
		if got == want {
			continue
		}
		if _, known := collectionValueColumnOverride[typ]; !known {
			t.Errorf("field type %q: asset side stores in %s but collection side stores in %s. "+
				"Same type, same value, two columns — that is bug #778 happening again. "+
				"Fix the writers, or if the split is deliberate add it to "+
				"collectionValueColumnOverride with the reason.", typ, want, got)
		}
	}
}

// TestTreeValueIsASingleSlugNotAPath is the semantic half of the tree
// decision, separate from the column it lives in.
//
// `tree` is SINGLE-valued: it names one node in a hierarchy, the way
// `select` names one term in a flat list. It is not a set (that is
// multi_select) and its value is not the path to the node (that would
// denormalise the ancestors and make an ancestor rename a cascading
// rewrite — the exact cost ADR 0012's slug indirection exists to
// avoid).
//
// So a tree write takes value_text and refuses value_options, and this
// pins both halves.
func TestTreeValueIsASingleSlugNotAPath(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	slug := "london"
	p, err := buildUpsertParams(asset, field, "tree",
		&openapi.AssetFieldValueWrite{ValueText: &slug}, nil)
	if err != nil {
		t.Fatalf("tree write with value_text: %v", err)
	}
	if p.ValueText == nil || *p.ValueText != slug {
		t.Fatalf("tree value_text: want %q, got %v", slug, p.ValueText)
	}
	if p.ValueOptions != nil {
		t.Errorf("tree write also populated value_options (%v) — a tree value is one slug, not a set",
			p.ValueOptions)
	}

	// value_options alone must be rejected: it is the shape the
	// collection path used to accept, and accepting it here would let
	// the old shape back in through the door it left by.
	opts := []string{"europe", "uk", "london"}
	if _, err := buildUpsertParams(asset, field, "tree",
		&openapi.AssetFieldValueWrite{ValueOptions: &opts}, nil); err == nil {
		t.Error("tree write accepted value_options with no value_text — " +
			"the path-as-array shape must be rejected, not silently stored")
	}

	if err := validateCollectionValueType("tree",
		&openapi.CollectionFieldValueWrite{ValueOptions: &opts}); err == nil {
		t.Error("collection tree write accepted value_options — same shape, same rejection")
	}
}

// TestAncestorRenameDoesNotRewriteStoredValues is acceptance item 4,
// as an executable claim rather than a promise in a doc.
//
// Renaming an ancestor's LABEL, and re-parenting a node outright, both
// change only the options document. The stored value is the leaf slug
// and is untouched — it still resolves, and the path it resolves to
// reflects the new shape immediately. Had the value been the path
// string ADR 0012 originally specified, both operations would have
// required rewriting every descendant row.
func TestAncestorRenameDoesNotRewriteStoredValues(t *testing.T) {
	const stored = "london" // what asset_field_value holds. Never changes.

	before := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[
			{"value":"uk","label":"UK","children":[
				{"value":"london","label":"London"}
			]}
		]}
	]}`)

	got := resolveOptionSlugs(before, []string{stored})
	if len(got) != 1 {
		t.Fatalf("nested slug %q did not resolve: %v", stored, got)
	}
	assertPath(t, "before rename", got[stored].Path, []string{"Europe", "UK", "London"})

	// (1) Rename an ancestor's label. Only the options document moves.
	renamed := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[
			{"value":"uk","label":"United Kingdom","children":[
				{"value":"london","label":"London"}
			]}
		]}
	]}`)
	got = resolveOptionSlugs(renamed, []string{stored})
	if len(got) != 1 {
		t.Fatalf("slug %q stopped resolving after an ancestor rename", stored)
	}
	assertPath(t, "after ancestor rename", got[stored].Path,
		[]string{"Europe", "United Kingdom", "London"})

	// (2) Re-parent the node. Also free, for the same reason.
	reparented := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[
			{"value":"uk","label":"United Kingdom"},
			{"value":"london","label":"London"}
		]}
	]}`)
	got = resolveOptionSlugs(reparented, []string{stored})
	if len(got) != 1 {
		t.Fatalf("slug %q stopped resolving after a re-parent", stored)
	}
	assertPath(t, "after re-parent", got[stored].Path, []string{"Europe", "London"})
}

// TestNestedSlugsMustBeUniqueTreeWide pins the property the
// single-slug storage decision rests on. If two nodes in one field
// could share a slug, a stored slug would be an ambiguous address and
// the value WOULD have to carry its path.
func TestNestedSlugsMustBeUniqueTreeWide(t *testing.T) {
	dupAcrossLevels := []byte(`{"values":[
		{"value":"london","label":"London"},
		{"value":"europe","label":"Europe","children":[
			{"value":"london","label":"London, UK"}
		]}
	]}`)
	if _, err := normalizeOptionsDoc(dupAcrossLevels); err == nil {
		t.Error("a slug duplicated across levels was accepted — " +
			"tree-wide slug uniqueness is what makes a bare leaf slug a complete address")
	}

	dupInBranch := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[
			{"value":"london","label":"London"},
			{"value":"london","label":"London again"}
		]}
	]}`)
	if _, err := normalizeOptionsDoc(dupInBranch); err == nil {
		t.Error("a slug duplicated within one branch was accepted")
	}
}

// TestResolvedPathOmittedForFlatVocabularies keeps the wire shape of
// every existing select / multi_select response unchanged: a
// one-element path carries nothing the label does not.
func TestResolvedPathOmittedForFlatVocabularies(t *testing.T) {
	flat := []byte(`{"values":["sRGB","Linear"]}`)
	text := "sRGB"

	got := resolveValueOptions("select", &text, nil, flat)
	if len(got) != 1 {
		t.Fatalf("want 1 resolved, got %v", got)
	}
	if got["sRGB"].Path != nil {
		t.Errorf("top-level term shipped a path (%v) — flat vocabularies must be unchanged on the wire",
			*got["sRGB"].Path)
	}

	nested := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[{"value":"london","label":"London"}]}
	]}`)
	leaf := "london"
	got = resolveValueOptions("tree", &leaf, nil, nested)
	if len(got) != 1 {
		t.Fatalf("tree: want 1 resolved, got %v", got)
	}
	if got["london"].Path == nil {
		t.Fatal("nested term shipped no path — the reader cannot show the hierarchy without it")
	}
	assertPath(t, "wire path", *got["london"].Path, []string{"Europe", "London"})
	if got["london"].Label != "London" {
		t.Errorf("want label London, got %q", got["london"].Label)
	}
}

func assertPath(t *testing.T, where string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: path %v, want %v", where, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: path %v, want %v", where, got, want)
			return
		}
	}
}
