// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// The precedence chain, asserted at the point where it inverted (#793)
// ---------------------------------------------------------------------------
//
// ADR 0081 §3 specifies extracted > team default > field default >
// empty, and "a default never overwrites a value that is already set,
// and never overwrites an extracted one". The top of that chain is
// decided HERE, in the applier, and until #793 it was decided
// backwards: skip_if_set tested whether a value was PRESENT, and a
// default is present, so extraction would skip a field a default had
// filled — a default outranking the extraction it is supposed to yield
// to.
//
// Every case below runs against ExtractionModeSkipIfSet on purpose. The
// same assertions against `replace` would all pass with the bug intact,
// because replace never consults the existing value at all — and 13 of
// the 15 live field definitions are skip_if_set, so `replace` is the
// case that does not matter.

// snapshotFor builds the shape metaValueReaderAdapter returns for a row
// that exists. set_by is NOT NULL DEFAULT 'manual' in the schema, so
// every present row carries a real provenance and a test fixture with
// an empty one would be a state production cannot reach.
func snapshotFor(text, setBy string) FieldValueSnapshot {
	v := text
	return FieldValueSnapshot{ValueText: &v, SetBy: setBy}
}

func applyOne(t *testing.T, mode ExtractionMode, existing *FieldValueSnapshot, extracted string) (*stubWriter, ApplySummary) {
	t.Helper()
	fid := uuid.New()
	cfg := stubConfig{cfg: []FieldExtractionConfig{
		{FieldID: fid, Source: FieldCameraMake, Mode: mode},
	}}
	values := stubValues{}
	if existing != nil {
		values.byField = map[uuid.UUID]FieldValueSnapshot{fid: *existing}
	}
	writer := &stubWriter{}
	a := NewApplier(cfg, values, writer, nil)

	summary, err := a.Apply(context.Background(), AssetRef{ID: uuid.New()}, Result{
		Format: "image/jpeg",
		Fields: map[CanonicalField]Value{
			FieldCameraMake: {Kind: ValueKindText, Text: extracted},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return writer, summary
}

// Position 1 — extracted beats a default.
//
// This is the assertion that failed before #793. The field holds a
// value an upload default put there; extraction has a better answer;
// skip_if_set must NOT protect the placeholder.
func TestPrecedence_ExtractionOverwritesADefault(t *testing.T) {
	before := snapshotFor("Placeholder Co", SetByDefault)
	writer, summary := applyOne(t, ExtractionModeSkipIfSet, &before, "Canon")

	if len(writer.calls) != 1 {
		t.Fatalf("extraction did not overwrite a default under skip_if_set "+
			"(writes=%d, skipped-by-mode=%v). That is ADR 0081 §3's precedence inverted: "+
			"the skip is a provenance check, not a presence check", len(writer.calls), summary.FieldsSkippedMode)
	}
	if got := writer.calls[0].Value.Text; got != "Canon" {
		t.Errorf("wrote %q, want the extracted value", got)
	}
}

// Position 2, negative — a default never overwrites a value already set.
//
// Asserted from the applier's side as the mirror of the writer-side
// guarantee (metadata.InsertAssetFieldValueIfAbsent's ON CONFLICT DO
// NOTHING): whatever a person chose survives extraction under
// skip_if_set, so a default written before them cannot be resurrected
// by an extraction pass either.
func TestPrecedence_ManualValueSurvivesExtraction(t *testing.T) {
	before := snapshotFor("Nikon", "manual")
	writer, summary := applyOne(t, ExtractionModeSkipIfSet, &before, "Canon")

	if len(writer.calls) != 0 {
		t.Errorf("extraction overwrote a manually-set value under skip_if_set — "+
			"the provenance check must widen the skip's exemption to defaults ONLY, "+
			"not remove it (wrote %+v)", writer.calls)
	}
	if len(summary.FieldsSkippedMode) != 1 {
		t.Errorf("FieldsSkippedMode = %v, want the field recorded as skipped", summary.FieldsSkippedMode)
	}
}

// Position 2, negative (the other provenances). Every non-default
// provenance the CHECK constraint permits must be protected. Table-
// driven so adding a provenance to the constraint without deciding what
// it means here shows up as a gap rather than as silent data loss.
func TestPrecedence_OnlyDefaultsAreOverwritable(t *testing.T) {
	// Every value asset_field_value_set_by_check accepts except
	// 'default' itself. Kept literal so a schema change has to come
	// here.
	protected := []string{"manual", "exif", "iptc", "xmp", "api", "import", "computed"}

	for _, setBy := range protected {
		t.Run(setBy, func(t *testing.T) {
			before := snapshotFor("Nikon", setBy)
			writer, _ := applyOne(t, ExtractionModeSkipIfSet, &before, "Canon")
			if len(writer.calls) != 0 {
				t.Errorf("skip_if_set overwrote a value with set_by=%q — "+
					"only 'default' is a placeholder; everything else was chosen by someone or something",
					setBy)
			}
		})
	}

	t.Run(SetByDefault, func(t *testing.T) {
		before := snapshotFor("Nikon", SetByDefault)
		writer, _ := applyOne(t, ExtractionModeSkipIfSet, &before, "Canon")
		if len(writer.calls) != 1 {
			t.Error("skip_if_set protected a value with set_by='default' — " +
				"that is the one provenance extraction outranks")
		}
	})
}

// Position 4 — an empty field takes the extracted value. Unchanged by
// #793 and asserted here so the whole chain reads in one file.
func TestPrecedence_EmptyFieldTakesExtraction(t *testing.T) {
	writer, _ := applyOne(t, ExtractionModeSkipIfSet, nil, "Canon")
	if len(writer.calls) != 1 {
		t.Fatalf("empty field did not take the extracted value (writes=%d)", len(writer.calls))
	}
}

// The equal-value short-circuit must not strand a placeholder.
//
// A default that happens to match what the file says is still labelled
// "nobody chose this". Left short-circuited it would keep that label
// forever, and every later pass would keep re-examining it. One write
// promotes the provenance; the pass after that takes the short-circuit
// normally, which is what keeps backfill idempotent.
func TestPrecedence_EqualValuePlaceholderIsStillPromoted(t *testing.T) {
	before := snapshotFor("Canon", SetByDefault)
	writer, summary := applyOne(t, ExtractionModeSkipIfSet, &before, "Canon")
	if len(writer.calls) != 1 {
		t.Fatalf("a default equal to the extracted value was short-circuited (skipped-no-change=%v) — "+
			"the value is right but its provenance is a lie that never gets corrected",
			summary.FieldsSkippedNoChange)
	}

	// And once promoted, it converges: the same pass over the
	// now-'exif' row writes nothing.
	after := snapshotFor("Canon", "exif")
	writer2, _ := applyOne(t, ExtractionModeSkipIfSet, &after, "Canon")
	if len(writer2.calls) != 0 {
		t.Error("the promoted row was written again — the equal-value short-circuit " +
			"must still fire for non-placeholders, or backfill stops being idempotent")
	}
}

// `replace` is unaffected: it overwrites regardless of provenance,
// which is what it has always meant.
func TestPrecedence_ReplaceIsUnchangedByProvenance(t *testing.T) {
	for _, setBy := range []string{"manual", SetByDefault} {
		t.Run(setBy, func(t *testing.T) {
			before := snapshotFor("Nikon", setBy)
			writer, _ := applyOne(t, ExtractionModeReplace, &before, "Canon")
			if len(writer.calls) != 1 {
				t.Errorf("replace skipped a set_by=%q value — replace never consulted "+
					"the existing value and must not start now", setBy)
			}
		})
	}
}
