// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the extraction wiring shipped as #813 + #800 + #799:
// multi-extractor dispatch, per-extractor provenance, the two
// directions of skip_if_set against seeded values, controlled-
// vocabulary resolution, and the refusal of field types the applier
// has no column for.

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// countryOptions is migration 00024's `country` vocabulary, trimmed to
// two continents. Shape and depth match what ships: continent branches
// whose leaves are ISO 3166-1 alpha-2 slugs, so "United Kingdom" is a
// LABEL two levels down and `gb` is what a value stores.
const countryOptions = `{"values":[
    {"value":"europe","label":"Europe","children":[
        {"value":"fr","label":"France"},
        {"value":"gb","label":"United Kingdom"}
    ]},
    {"value":"americas","label":"Americas","children":[
        {"value":"us","label":"United States"}
    ]}
]}`

// namedExtractor is a stub whose Name is configurable, so a test can
// assert which extractor a value was attributed to.
type namedExtractor struct {
	name     string
	mimes    []string
	result   Result
	err      error
	extracts int
}

func (e *namedExtractor) Name() string { return e.name }

func (e *namedExtractor) Supports(mime string) bool {
	for _, m := range e.mimes {
		if m == mime {
			return true
		}
	}
	return false
}

func (e *namedExtractor) Extract(_ context.Context, r io.Reader, _ string) (Result, error) {
	e.extracts++
	// Drain, so a test would catch a handler that handed the same
	// already-consumed reader to every extractor.
	_, _ = io.ReadAll(r)
	if e.err != nil {
		return Result{}, e.err
	}
	return e.result, nil
}

func textResult(f CanonicalField, s string) Result {
	return Result{Format: "image/jpeg", Fields: map[CanonicalField]Value{
		f: {Kind: ValueKindText, Text: s},
	}}
}

// ---------------------------------------------------------------------------
// #800 — every supporting extractor runs, not just the first
// ---------------------------------------------------------------------------

// The dispatch this pins used to `break` on the first extractor whose
// Supports said yes. EXIF, IPTC and XMP all support image/jpeg and EXIF
// is registered first, so IPTC and XMP never ran on any JPEG ever
// uploaded — which is why wiring a field to an iptc_* or xmp_*
// canonical could not work no matter how the wiring was written.
func TestExtractJob_RunsEverySupportingExtractor(t *testing.T) {
	asset := AssetRef{ID: uuid.New(), MimeType: "image/jpeg"}
	exifX := &namedExtractor{name: "exif", mimes: []string{"image/jpeg"},
		result: textResult(FieldCameraMake, "Canon")}
	iptcX := &namedExtractor{name: "iptc", mimes: []string{"image/jpeg"},
		result: textResult(FieldIPTCCredit, "Aurora R&D")}
	xmpX := &namedExtractor{name: "xmp", mimes: []string{"image/jpeg"},
		result: textResult(FieldXMPRights, "CC0")}
	// Disjoint MIME range — must NOT be asked to extract.
	pdfX := &namedExtractor{name: "pdf", mimes: []string{"application/pdf"}}

	var got Result
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("jpegbytes"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		captureApplier{into: &got},
		nil,
		[]Extractor{exifX, iptcX, xmpX, pdfX},
		nil,
	)
	if _, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, e := range []*namedExtractor{exifX, iptcX, xmpX} {
		if e.extracts != 1 {
			t.Errorf("%s extracted %d times, want 1", e.name, e.extracts)
		}
	}
	if pdfX.extracts != 0 {
		t.Errorf("pdf extractor ran on a JPEG (%d times)", pdfX.extracts)
	}

	// All three namespaces reach the applier in one Result.
	for f, want := range map[CanonicalField]string{
		FieldCameraMake: "Canon",
		FieldIPTCCredit: "Aurora R&D",
		FieldXMPRights:  "CC0",
	} {
		if v, ok := got.Fields[f]; !ok || v.Text != want {
			t.Errorf("merged Fields[%s] = %q (present=%v), want %q", f, v.Text, ok, want)
		}
	}
}

// One extractor finding nothing is the COMMON case once three of them
// run on every JPEG, and must not discard what the others found.
func TestExtractJob_NoMetadataFromOneExtractorKeepsTheRest(t *testing.T) {
	asset := AssetRef{ID: uuid.New(), MimeType: "image/jpeg"}
	var got Result
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("jpegbytes"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		captureApplier{into: &got},
		nil,
		[]Extractor{
			&namedExtractor{name: "exif", mimes: []string{"image/jpeg"}, err: ErrNoMetadata},
			&namedExtractor{name: "iptc", mimes: []string{"image/jpeg"},
				result: textResult(FieldIPTCCredit, "Aurora R&D")},
		},
		nil,
	)
	if _, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if v, ok := got.Fields[FieldIPTCCredit]; !ok || v.Text != "Aurora R&D" {
		t.Fatalf("IPTC value lost when EXIF reported no metadata: %+v", got.Fields)
	}
	if got.FieldSources[FieldIPTCCredit] != "iptc" {
		t.Errorf("provenance = %q, want iptc", got.FieldSources[FieldIPTCCredit])
	}
}

// A malformed packet in ONE namespace is not a malformed file. It gets
// its own failure row and the other extractors' output still applies.
func TestExtractJob_MalformedOneExtractorStillAppliesOthers(t *testing.T) {
	asset := AssetRef{ID: uuid.New(), MimeType: "image/jpeg"}
	failures := &stubFailures{}
	var got Result
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("jpegbytes"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		captureApplier{into: &got},
		failures,
		[]Extractor{
			&namedExtractor{name: "exif", mimes: []string{"image/jpeg"},
				result: textResult(FieldCameraMake, "Canon")},
			&namedExtractor{name: "xmp", mimes: []string{"image/jpeg"}, err: ErrMalformedFile},
		},
		nil,
	)
	if _, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if v, ok := got.Fields[FieldCameraMake]; !ok || v.Text != "Canon" {
		t.Errorf("EXIF value lost because XMP was malformed: %+v", got.Fields)
	}
	if len(failures.calls) != 1 {
		t.Fatalf("failure rows = %d, want 1", len(failures.calls))
	}
	if k := failures.calls[0].ErrorKind; k != "malformed_file" {
		t.Errorf("error_kind = %q, want malformed_file", k)
	}
	// The row names WHICH extractor failed — otherwise an operator
	// staring at "malformed" on a file that opens fine has nothing.
	if !strings.Contains(failures.calls[0].Message, "xmp") {
		t.Errorf("failure message %q does not name the extractor", failures.calls[0].Message)
	}
}

// An unknown error class still aborts the whole job, because "retry
// half an extraction" is not something the job framework can express.
func TestExtractJob_UnknownErrorAborts(t *testing.T) {
	asset := AssetRef{ID: uuid.New(), MimeType: "image/jpeg"}
	h := NewExtractJobHandler(
		stubLoader{bytes: []byte("jpegbytes"), mime: "image/jpeg"},
		stubLookup{asset: asset, found: true},
		stubApplier{},
		nil,
		[]Extractor{
			&namedExtractor{name: "exif", mimes: []string{"image/jpeg"}, err: errors.New("connection reset")},
		},
		nil,
	)
	_, err := h.Handle(context.Background(), makeJob(ExtractJobPayload{AssetID: asset.ID}))
	if err == nil {
		t.Fatal("want a retryable error, got nil")
	}
	var terminal *jobs.TerminalError
	if errors.As(err, &terminal) {
		t.Fatal("unknown error classified terminal; the job must be retried")
	}
}

// ---------------------------------------------------------------------------
// #799 — set_by names the extractor that actually produced the value
// ---------------------------------------------------------------------------

// The pin the issue asks for: one value per extractor, each written
// with its own provenance. Before this, all five recorded "exif".
func TestApply_SetByIsPerExtractor(t *testing.T) {
	type wiring struct {
		canonical CanonicalField
		source    string
	}
	wirings := []wiring{
		{FieldCaptureDateTime, "exif"},
		{FieldIPTCCredit, "iptc"},
		{FieldXMPRights, "xmp"},
		{FieldPDFAuthor, "pdf"},
		{FieldCameraModel, "raw"},
	}

	cfgs := make([]FieldExtractionConfig, 0, len(wirings))
	ids := make(map[CanonicalField]uuid.UUID, len(wirings))
	res := Result{
		Format:       "image/jpeg",
		Fields:       map[CanonicalField]Value{},
		FieldSources: map[CanonicalField]string{},
	}
	for _, w := range wirings {
		id := uuid.New()
		ids[w.canonical] = id
		cfgs = append(cfgs, FieldExtractionConfig{
			FieldID: id, Source: w.canonical,
			Mode: ExtractionModeReplace, FieldType: "text",
		})
		res.Fields[w.canonical] = Value{Kind: ValueKindText, Text: "v-" + w.source}
		res.FieldSources[w.canonical] = w.source
	}

	writer := &stubWriter{}
	a := NewApplier(stubConfig{cfg: cfgs}, stubValues{}, writer, nil)
	if _, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != len(wirings) {
		t.Fatalf("writes = %d, want %d", len(writer.calls), len(wirings))
	}
	gotByField := map[uuid.UUID]string{}
	for _, c := range writer.calls {
		gotByField[c.FieldID] = c.SetBy
	}
	for _, w := range wirings {
		if got := gotByField[ids[w.canonical]]; got != w.source {
			t.Errorf("%s written with set_by=%q, want %q", w.canonical, got, w.source)
		}
	}
}

// A Result assembled without provenance writes the honest "extract"
// rather than guessing at the first extractor's name.
func TestApply_SetByFallsBackToExtract(t *testing.T) {
	fid := uuid.New()
	writer := &stubWriter{}
	a := NewApplier(stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: ExtractionModeReplace, FieldType: "text"},
	}}, stubValues{}, writer, nil)

	_, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()},
		textResult(FieldCameraMake, "Canon"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 1 || writer.calls[0].SetBy != SetByExtraction {
		t.Fatalf("SetBy = %q, want %q", writer.calls[0].SetBy, SetByExtraction)
	}
}

// MergeResults attributes each field to the extractor that produced it
// and resolves a collision deterministically by registration order.
func TestMergeResults_ProvenanceAndPrecedence(t *testing.T) {
	merged := MergeResults([]SourcedResult{
		{Source: "exif", Result: textResult(FieldImageDescription, "from exif")},
		{Source: "iptc", Result: textResult(FieldIPTCCaption, "from iptc")},
		{Source: "xmp", Result: textResult(FieldImageDescription, "from xmp")},
	})
	if got := merged.Fields[FieldImageDescription].Text; got != "from exif" {
		t.Errorf("collision resolved to %q, want the first registered (%q)", got, "from exif")
	}
	if got := merged.FieldSources[FieldImageDescription]; got != "exif" {
		t.Errorf("provenance = %q, want exif", got)
	}
	if got := merged.FieldSources[FieldIPTCCaption]; got != "iptc" {
		t.Errorf("provenance = %q, want iptc", got)
	}
}

// ---------------------------------------------------------------------------
// ADR 0081 §3 — extraction fills gaps and never clobbers a seeded value
// ---------------------------------------------------------------------------
//
// Both directions, because either alone passes for the wrong reason: an
// applier that never writes passes the first, and one that always
// writes passes the second.

func TestApply_SkipIfSet_SeededValueSurvives(t *testing.T) {
	fid := uuid.New()
	seeded := "Photo by a person"
	writer := &stubWriter{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{
			{FieldID: fid, Source: FieldIPTCCredit, Mode: ExtractionModeSkipIfSet, FieldType: "text"},
		}},
		// set_by='import' is what `aa seed` writes. Not a placeholder:
		// somebody chose it, so extraction must leave it alone.
		stubValues{byField: map[uuid.UUID]FieldValueSnapshot{
			fid: {ValueText: &seeded, SetBy: "import"},
		}},
		writer, nil,
	)
	summary, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()},
		Result{
			Format:       "image/jpeg",
			Fields:       map[CanonicalField]Value{FieldIPTCCredit: {Kind: ValueKindText, Text: "Aurora R&D"}},
			FieldSources: map[CanonicalField]string{FieldIPTCCredit: "iptc"},
		})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("extraction overwrote a seeded value with %q", writer.calls[0].Value.Text)
	}
	if len(summary.FieldsSkippedMode) != 1 {
		t.Errorf("FieldsSkippedMode = %v, want the one skip", summary.FieldsSkippedMode)
	}
}

func TestApply_SkipIfSet_EmptyFieldGainsExtractedValue(t *testing.T) {
	fid := uuid.New()
	writer := &stubWriter{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{
			{FieldID: fid, Source: FieldIPTCCredit, Mode: ExtractionModeSkipIfSet, FieldType: "text"},
		}},
		stubValues{}, // no row for this asset+field
		writer, nil,
	)
	if _, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()},
		Result{
			Format:       "image/jpeg",
			Fields:       map[CanonicalField]Value{FieldIPTCCredit: {Kind: ValueKindText, Text: "Aurora R&D"}},
			FieldSources: map[CanonicalField]string{FieldIPTCCredit: "iptc"},
		}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 1 {
		t.Fatalf("writes = %d, want 1 — an empty field must gain the extracted value", len(writer.calls))
	}
	if got := writer.calls[0].Value.Text; got != "Aurora R&D" {
		t.Errorf("wrote %q, want %q", got, "Aurora R&D")
	}
	if got := writer.calls[0].SetBy; got != "iptc" {
		t.Errorf("set_by = %q, want iptc", got)
	}
}

// ---------------------------------------------------------------------------
// Controlled vocabulary — a label in the file becomes a slug in the row
// ---------------------------------------------------------------------------

func countryApplier(t *testing.T, fid uuid.UUID) (*DefaultApplier, *stubWriter, *stubFailures) {
	t.Helper()
	writer, failures := &stubWriter{}, &stubFailures{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{{
			FieldID: fid, Source: FieldIPTCCountry,
			Mode: ExtractionModeSkipIfSet, FieldType: "tree",
			Options: []byte(countryOptions),
		}}},
		stubValues{}, writer, failures,
	)
	return a, writer, failures
}

func applyCountry(t *testing.T, a *DefaultApplier, raw string) ApplySummary {
	t.Helper()
	s, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, Result{
		Format:       "image/jpeg",
		Fields:       map[CanonicalField]Value{FieldIPTCCountry: {Kind: ValueKindText, Text: raw}},
		FieldSources: map[CanonicalField]string{FieldIPTCCountry: "iptc"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return s
}

// IPTC 2:101 carries a LABEL. The column stores a SLUG. Storing the
// label would produce a value that renders plausibly and addresses
// nothing.
func TestApply_CountryLabelResolvesToSlug(t *testing.T) {
	fid := uuid.New()
	a, writer, failures := countryApplier(t, fid)
	applyCountry(t, a, "United Kingdom")

	if len(writer.calls) != 1 {
		t.Fatalf("writes = %d, want 1", len(writer.calls))
	}
	if got := writer.calls[0].Value.Text; got != "gb" {
		t.Errorf("value_text = %q, want %q — the label was stored instead of the slug", got, "gb")
	}
	if len(failures.calls) != 0 {
		t.Errorf("unexpected failure rows: %+v", failures.calls)
	}
}

func TestApply_CountryMatchIsCaseAndSpaceInsensitive(t *testing.T) {
	for _, raw := range []string{"united kingdom", "  UNITED KINGDOM  ", "gb", "GB"} {
		t.Run(raw, func(t *testing.T) {
			fid := uuid.New()
			a, writer, _ := countryApplier(t, fid)
			applyCountry(t, a, raw)
			if len(writer.calls) != 1 || writer.calls[0].Value.Text != "gb" {
				t.Fatalf("%q did not resolve to gb: %+v", raw, writer.calls)
			}
		})
	}
}

// No term, no write — and a row saying so, because the operator is the
// only one who can add the term or fix the file.
func TestApply_CountryNoMatchIsDroppedWithAWarning(t *testing.T) {
	fid := uuid.New()
	a, writer, failures := countryApplier(t, fid)
	summary := applyCountry(t, a, "Ruritania")

	if len(writer.calls) != 0 {
		t.Fatalf("an unresolvable country was written as %q", writer.calls[0].Value.Text)
	}
	if len(failures.calls) != 1 {
		t.Fatalf("failure rows = %d, want 1", len(failures.calls))
	}
	f := failures.calls[0]
	if f.FieldKey != FieldIPTCCountry {
		t.Errorf("field_key = %q, want %q", f.FieldKey, FieldIPTCCountry)
	}
	// The raw value must survive onto the row; it is the whole point.
	if f.RawValue != "Ruritania" {
		t.Errorf("raw_value = %v, want Ruritania", f.RawValue)
	}
	if !strings.Contains(f.Message, "vocabulary") {
		t.Errorf("message %q does not explain the drop", f.Message)
	}
	if len(summary.FieldsSkippedValid) != 1 {
		t.Errorf("summary did not record the skip: %+v", summary)
	}
}

// An archived term is retired hard; it must not acquire new values.
func TestApply_CountryArchivedTermDoesNotMatch(t *testing.T) {
	fid := uuid.New()
	writer, failures := &stubWriter{}, &stubFailures{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{{
			FieldID: fid, Source: FieldIPTCCountry, Mode: ExtractionModeReplace, FieldType: "tree",
			Options: []byte(`{"values":[{"value":"europe","label":"Europe","children":[
                {"value":"su","label":"Soviet Union","status":"archived"}]}]}`),
		}}},
		stubValues{}, writer, failures,
	)
	if _, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, Result{
		Format: "image/jpeg",
		Fields: map[CanonicalField]Value{FieldIPTCCountry: {Kind: ValueKindText, Text: "Soviet Union"}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("archived term acquired a new value: %q", writer.calls[0].Value.Text)
	}
}

// The bare-slug entry form the seeder writes must resolve too.
func TestResolveVocabularySlug_BareSlugEntries(t *testing.T) {
	got, ok := resolveVocabularySlug([]byte(`{"values":["sRGB","Linear"]}`), "srgb")
	if !ok || got != "sRGB" {
		t.Fatalf("resolveVocabularySlug = (%q, %v), want (sRGB, true)", got, ok)
	}
}

func TestResolveVocabularySlug_EmptyVocabularyMatchesNothing(t *testing.T) {
	for _, doc := range []string{``, `{}`, `{"values":[]}`, `not json`} {
		if _, ok := resolveVocabularySlug([]byte(doc), "United Kingdom"); ok {
			t.Errorf("options %q resolved a term it does not have", doc)
		}
	}
}

// ---------------------------------------------------------------------------
// The guard that stops `keywords` being wired before #789
// ---------------------------------------------------------------------------

// /admin/fields will let an operator point a multi_select field at
// iptc_keywords. The applier has no value_options column, so the write
// would go to value_text and the field would stay visibly empty while
// every log line said it was set. Refuse it loudly instead.
func TestApply_MultiSelectTargetIsRefused(t *testing.T) {
	fid := uuid.New()
	writer, failures := &stubWriter{}, &stubFailures{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{{
			FieldID: fid, Source: FieldIPTCKeywords,
			Mode: ExtractionModeReplace, FieldType: "multi_select",
		}}},
		stubValues{}, writer, failures,
	)
	summary, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, Result{
		Format: "image/jpeg",
		Fields: map[CanonicalField]Value{
			FieldIPTCKeywords: {Kind: ValueKindText, Text: "landscape, nature"},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("a multi_select field was written as text: %q", writer.calls[0].Value.Text)
	}
	if len(failures.calls) != 1 {
		t.Fatalf("failure rows = %d, want 1", len(failures.calls))
	}
	if !strings.Contains(failures.calls[0].Message, "multi_select") {
		t.Errorf("message %q does not name the offending type", failures.calls[0].Message)
	}
	if len(summary.FieldsSet) != 0 {
		t.Errorf("summary claims fields were set: %+v", summary.FieldsSet)
	}
}

func TestApply_ReferenceTargetIsRefused(t *testing.T) {
	fid := uuid.New()
	writer := &stubWriter{}
	a := NewApplier(
		stubConfig{cfg: []FieldExtractionConfig{{
			FieldID: fid, Source: FieldIPTCByline,
			Mode: ExtractionModeReplace, FieldType: "reference",
		}}},
		stubValues{}, writer, &stubFailures{},
	)
	if _, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()},
		textResult(FieldIPTCByline, "someone")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("a reference field was written as text")
	}
}

// ---------------------------------------------------------------------------
// The shipped wiring itself
// ---------------------------------------------------------------------------

// Migration 00025 writes four CanonicalField names into
// extraction_source. A name that is not one of the extractor's
// constants routes nothing at all, silently — the applier looks it up
// in Result.Fields and simply never finds it. This pins the four
// against the constants so a typo cannot ship.
func TestShippedWiring_NamesRealCanonicalFields(t *testing.T) {
	shipped := map[string]CanonicalField{
		"capture_date": FieldCaptureDateTime,
		"copyright":    FieldXMPRights,
		"credit":       FieldIPTCCredit,
		"country":      FieldIPTCCountry,
	}
	want := map[string]string{
		"capture_date": "capture_datetime",
		"copyright":    "xmp_rights",
		"credit":       "iptc_credit",
		"country":      "iptc_country",
	}
	for code, canonical := range shipped {
		if string(canonical) != want[code] {
			t.Errorf("%s wired to %q, but the migration writes %q",
				code, canonical, want[code])
		}
	}
}

// countryOptions must stay a document NormalizeOptionsDoc would accept,
// or the vocabulary this test resolves against is not the one that
// ships.
func TestCountryOptionsFixtureIsWellFormed(t *testing.T) {
	var doc struct {
		Values []json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal([]byte(countryOptions), &doc); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if len(doc.Values) != 2 {
		t.Fatalf("fixture has %d branches, want 2", len(doc.Values))
	}
}

// captureApplier records the Result the job handler assembled so a
// dispatch test can assert on the merge rather than on a write.
type captureApplier struct{ into *Result }

func (c captureApplier) Apply(_ context.Context, _ AssetRef, r Result) (ApplySummary, error) {
	*c.into = r
	return ApplySummary{}, nil
}
