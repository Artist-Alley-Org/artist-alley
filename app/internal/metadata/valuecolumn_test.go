// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"fmt"
	"strconv"
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

// There is no per-subject override table. There was one, holding a
// single entry — `boolean`, which the collection side stored as the
// strings "true"/"false" in value_text while the asset side stored
// 0/1 in value_num. #791 moved the collection side onto value_num,
// which is what ADR 0012 always specified, and the table went with it.
//
// It is not coming back as an empty map "for later". A mechanism for
// recording deliberate divergence is an invitation to record one, and
// the entire lesson of #778 and #791 is that a divergence which is
// merely documented is a divergence that ships. Both surfaces read
// valueColumnFor; a writer that disagrees fails, with nowhere to
// register an exemption.

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

// The one sample every writer is driven with. Fixed, not generated:
// TestWritersAgreeOnColumnAndEncoding compares the two writers'
// OUTPUT byte for byte, so a randomly generated UUID (or any other
// per-call value) would report a false disagreement. Defined once so
// the two body builders below cannot drift apart either.
var (
	sampleText    = "sample-slug"
	sampleNum     = float32(1)
	sampleDate    = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	sampleOptions = []string{"sample-slug"}
	sampleRef     = openapi_types.UUID(uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"))
)

// sampleAssetWrite builds a write body carrying a plausible value in
// EVERY value_* slot, so the writer's choice of column is the writer's
// alone — nothing is forced by the input being absent.
func sampleAssetWrite() *openapi.AssetFieldValueWrite {
	opts := sampleOptions
	return &openapi.AssetFieldValueWrite{
		ValueText:    &sampleText,
		ValueNum:     &sampleNum,
		ValueDate:    &sampleDate,
		ValueOptions: &opts,
		ValueRef:     &sampleRef,
	}
}

func sampleCollectionWrite() *openapi.CollectionFieldValueWrite {
	opts := sampleOptions
	return &openapi.CollectionFieldValueWrite{
		ValueText:    &sampleText,
		ValueNum:     &sampleNum,
		ValueDate:    &sampleDate,
		ValueOptions: &opts,
		ValueRef:     &sampleRef,
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
				valueColumnFor[typ], populatedCollectionColumns(p))
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

			switch valueColumnFor[typ] {
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
					typ, valueColumnFor[typ], err)
			}
			if err := validateCollectionValueType(typ, &openapi.CollectionFieldValueWrite{}); err == nil {
				t.Errorf("validateCollectionValueType(%q) accepted an empty body — "+
					"a missing value must not reach the writer", typ)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The cross-surface pin, over COLUMN **and** ENCODING (#791)
// ---------------------------------------------------------------------------
//
// #778's version of this compared each writer's column against a
// table. That is only half the invariant, and #791 is the half it
// missed: the asset and collection writers BOTH had a defensible
// answer for `boolean` and still disagreed, because agreeing on a
// column says nothing about what goes in it. Two writers can both
// pick value_num and disagree about 1 versus 1.0; both pick value_text
// and disagree about "true" versus "1" versus "yes". Column agreement
// is necessary and not sufficient.
//
// So this compares the writers to EACH OTHER, on the rendered stored
// value, given byte-identical input. No table sits in the middle to
// be updated on both sides at once and hide the drift — the same
// failure mode as a doc comment that gets reworded along with the bug.

func renderStored(col valueColumn, text *string, num *float64, date pgtype.Timestamptz, opts []string, ref pgtype.UUID) string {
	switch col {
	case colText:
		if text == nil {
			return "value_text=<nil>"
		}
		return fmt.Sprintf("value_text=%q", *text)
	case colNum:
		if num == nil {
			return "value_num=<nil>"
		}
		return "value_num=" + strconv.FormatFloat(*num, 'g', -1, 64)
	case colDate:
		if !date.Valid {
			return "value_date=<nil>"
		}
		return "value_date=" + date.Time.UTC().Format(time.RFC3339Nano)
	case colOptions:
		return fmt.Sprintf("value_options=%q", opts)
	case colRef:
		if !ref.Valid {
			return "value_ref=<nil>"
		}
		return "value_ref=" + uuid.UUID(ref.Bytes).String()
	}
	return "unknown column " + string(col)
}

func assetStored(col valueColumn, p UpsertAssetFieldValueParams) string {
	return renderStored(col, p.ValueText, p.ValueNum, p.ValueDate, p.ValueOptions, p.ValueRef)
}

func collectionStored(col valueColumn, p UpsertCollectionFieldValueParams) string {
	return renderStored(col, p.ValueText, p.ValueNum, p.ValueDate, p.ValueOptions, p.ValueRef)
}

// TestWritersAgreeOnColumnAndEncoding is the cross-surface pin: the
// same field type given the same value must land in the same column
// AND in the same representation whichever subject it is attached to,
// or a value set on a collection is unreadable by anything that reads
// the asset way round.
//
// There is no exemption list. A divergence recorded as deliberate is
// still a value nobody can read — that is what the old
// collectionValueColumnOverride bought us, and what #791 removed.
func TestWritersAgreeOnColumnAndEncoding(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	coll := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	for _, typ := range allFieldTypes {
		t.Run(typ, func(t *testing.T) {
			col := valueColumnFor[typ]

			ap, err := buildUpsertParams(asset, field, typ, sampleAssetWrite(), nil)
			if err != nil {
				t.Fatalf("asset writer refused the sample: %v", err)
			}
			cp := buildCollectionUpsertParams(coll, field, typ, sampleCollectionWrite(), "manual", nil)

			got, want := collectionStored(col, cp), assetStored(col, ap)
			if got != want {
				t.Errorf("field type %q: an asset stores %s but a collection stores %s. "+
					"Same type, same input, two encodings — a value written through one "+
					"surface is invisible to anything reading the other (#778 for the column, "+
					"#791 for the encoding). Fix the writers; there is no exemption list.",
					typ, want, got)
			}
		})
	}
}

// TestBooleanIsZeroOrOneInValueNum states #791's decision outright,
// separately from the agreement check above — because two writers can
// agree with each other and both be wrong.
//
// ADR 0012 has always specified `boolean -> value_num`, 0/1, so the
// partial index on (field_id, value_num) serves a "where flag = true"
// filter. The asset writer and the seeder complied; the collection
// writer and every display surface drifted onto the strings
// "true"/"false" in value_text, so a boolean rendered blank.
//
// The negatives matter as much as the positives: "true" in value_text
// is the shape the drifted surfaces sent, and it must be REJECTED
// rather than quietly stored somewhere nothing reads.
func TestBooleanIsZeroOrOneInValueNum(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	coll := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	for _, tc := range []struct {
		name string
		in   float32
		want string
	}{
		{"true", 1, "value_num=1"},
		{"false", 0, "value_num=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			num := tc.in
			p, err := buildUpsertParams(asset, field, "boolean",
				&openapi.AssetFieldValueWrite{ValueNum: &num}, nil)
			if err != nil {
				t.Fatalf("asset boolean write: %v", err)
			}
			if got := assetStored(colNum, p); got != tc.want {
				t.Errorf("asset boolean %s stored %s, want %s", tc.name, got, tc.want)
			}

			body := &openapi.CollectionFieldValueWrite{ValueNum: &num}
			if err := validateCollectionValueType("boolean", body); err != nil {
				t.Fatalf("collection validator rejected value_num=%v: %v", tc.in, err)
			}
			cp := buildCollectionUpsertParams(coll, field, "boolean", body, "manual", nil)
			if got := collectionStored(colNum, cp); got != tc.want {
				t.Errorf("collection boolean %s stored %s, want %s", tc.name, got, tc.want)
			}
		})
	}

	// The drifted shape: "true" in value_text and nothing in value_num.
	// Both writers must refuse it. Storing it would put the value in a
	// column the reader does not consult — the bug itself.
	drifted := "true"
	if _, err := buildUpsertParams(asset, field, "boolean",
		&openapi.AssetFieldValueWrite{ValueText: &drifted}, nil); err == nil {
		t.Error(`asset boolean write accepted value_text "true" — ` +
			"that is the pre-#791 encoding and must be rejected, not stored")
	}
	if err := validateCollectionValueType("boolean",
		&openapi.CollectionFieldValueWrite{ValueText: &drifted}); err == nil {
		t.Error(`collection boolean write accepted value_text "true" — ` +
			"that is exactly what this path used to write (#791)")
	}

	// Out of range. NUMERIC will hold 2 happily; the contract will not.
	two := float32(2)
	if _, err := buildUpsertParams(asset, field, "boolean",
		&openapi.AssetFieldValueWrite{ValueNum: &two}, nil); err == nil {
		t.Error("asset boolean write accepted value_num=2")
	}
	if err := validateCollectionValueType("boolean",
		&openapi.CollectionFieldValueWrite{ValueNum: &two}); err == nil {
		t.Error("collection boolean write accepted value_num=2 — the range check must " +
			"live on the collection side too, since buildCollectionUpsertParams cannot fail")
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
	if _, err := NormalizeOptionsDoc(dupAcrossLevels); err == nil {
		t.Error("a slug duplicated across levels was accepted — " +
			"tree-wide slug uniqueness is what makes a bare leaf slug a complete address")
	}

	dupInBranch := []byte(`{"values":[
		{"value":"europe","label":"Europe","children":[
			{"value":"london","label":"London"},
			{"value":"london","label":"London again"}
		]}
	]}`)
	if _, err := NormalizeOptionsDoc(dupInBranch); err == nil {
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

// ---------------------------------------------------------------------------
// The defaults writer joins the pin (#793)
// ---------------------------------------------------------------------------
//
// An upload default is a THIRD writer of asset_field_value, and a third
// writer with its own opinion about which column a type uses is exactly
// what #778 cost us the first two times. So it is pinned the same way:
// drive it, observe which param came back populated, compare to the
// table AND to the asset writer byte for byte.
//
// In practice the defaults path cannot disagree, because it resolves a
// default into an AssetFieldValueWrite and hands it to buildUpsertParams
// — the same function the manual PUT uses. That is the design, and this
// test is what stops a future "small optimisation" from replacing it
// with a switch statement that looks equivalent.

// defaultForColumn builds a literal FieldDefault carrying the shared
// sample in the column the pin names for this type, so the two writers
// are given byte-identical input.
func defaultForColumn(col valueColumn) FieldDefault {
	d := FieldDefault{Kind: DefaultKindLiteral}
	switch col {
	case colText:
		v := sampleText
		d.ValueText = &v
	case colNum:
		v := float64(sampleNum)
		d.ValueNum = &v
	case colDate:
		v := sampleDate
		d.ValueDate = &v
	case colOptions:
		d.ValueOptions = append([]string(nil), sampleOptions...)
	case colRef:
		v := uuid.UUID(sampleRef)
		d.ValueRef = &v
	}
	return d
}

func TestDefaultsWriterUsesPinnedColumns(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	for _, typ := range allFieldTypes {
		t.Run(typ, func(t *testing.T) {
			col := valueColumnFor[typ]
			def := defaultForColumn(col)

			write, ok := ResolveFieldDefault(typ, def, DefaultResolveContext{})
			if !ok {
				t.Fatalf("a literal default for %q did not resolve", typ)
			}
			p, err := buildUpsertParams(asset, field, typ, write, nil)
			if err != nil {
				t.Fatalf("defaults write (%q): %v", typ, err)
			}
			wantExactlyOne(t, "defaults write (ResolveFieldDefault → buildUpsertParams)",
				typ, col, populatedAssetColumns(p))

			// And byte-identical to what the manual path stores, given
			// the same value — a default that renders differently is a
			// value the asset page shows one way and the default
			// editor shows another.
			ap, err := buildUpsertParams(asset, field, typ, sampleAssetWrite(), nil)
			if err != nil {
				t.Fatalf("asset writer refused the sample: %v", err)
			}
			if got, want := assetStored(col, p), assetStored(col, ap); got != want {
				t.Errorf("field type %q: a default stores %s but a manual write stores %s. "+
					"Same type, same input, two encodings (#778 for the column, #791 for the encoding)",
					typ, got, want)
			}
		})
	}
}

// TestDefaultContextTargetsMatchThePin is the other half: a CONTEXT
// default names no column, so the mapping from context to storage shape
// has to agree with valueColumnFor or `current_date` would resolve into
// value_text on a datetime field and vanish.
func TestDefaultContextTargetsMatchThePin(t *testing.T) {
	asset := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	field := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	rc := DefaultResolveContext{
		UserDisplay: "Ada Lovelace",
		TeamName:    "Textures",
		Now:         sampleDate,
	}

	var covered int
	for _, typ := range allFieldTypes {
		for _, ctxName := range ContextsForFieldType(typ) {
			covered++
			t.Run(typ+"/"+string(ctxName), func(t *testing.T) {
				write, ok := ResolveFieldDefault(typ,
					FieldDefault{Kind: DefaultKindContext, Context: ctxName}, rc)
				if !ok {
					t.Fatalf("context %q did not resolve for %q with a fully-populated context", ctxName, typ)
				}
				p, err := buildUpsertParams(asset, field, typ, write, nil)
				if err != nil {
					t.Fatalf("context %q for %q: %v", ctxName, typ, err)
				}
				wantExactlyOne(t, "context default", typ, valueColumnFor[typ], populatedAssetColumns(p))
			})
		}
	}
	if covered == 0 {
		t.Fatal("no field type accepts any context value — ContextsForFieldType " +
			"is returning nothing and the whole context half of ADR 0081 §3 is dead code")
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
