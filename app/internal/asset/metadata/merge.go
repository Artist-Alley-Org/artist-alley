// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

// ---------------------------------------------------------------------------
// Multi-extractor dispatch
// ---------------------------------------------------------------------------
//
// One asset can be read by more than one extractor, and until this
// file existed it never was. The job handler picked the FIRST
// registered extractor whose Supports said yes and stopped there:
//
//	for _, e := range h.extractors {
//	        if e.Supports(mimeType) { ext = e; break }
//	}
//
// EXIF, IPTC and XMP all answer yes for image/jpeg, and EXIF is
// registered first. So on every JPEG ever uploaded, only the EXIF
// extractor ran; the IPTC and XMP extractors — their parsers, their
// carrier walkers, their twenty-odd CanonicalFields — could not be
// reached from production at all. They were exercised solely by their
// own unit tests, which is why nothing said so.
//
// That is not a missed optimisation, it is the reason wiring a field
// to an IPTC or XMP canonical could not work. `credit` wants IPTC
// 2:110 and `copyright` wants dc:rights; a JPEG carrying both would
// have routed neither, and the field would have stayed empty with no
// failure row and no log line to explain it. Four of the dataset's
// images carry a rights statement in XMP and nothing in EXIF — the
// files that most need reading were exactly the files nothing read.
//
// The three namespaces are complements, not alternatives. EXIF
// describes the CAMERA, IPTC the NEWSROOM, XMP the RIGHTS; a
// photograph routinely carries all three, saying different things.
// Choosing one and discarding the rest throws away most of what a file
// knows about itself.
//
// So: run every extractor that supports the type, and fold the results
// together. Which raises the question the single-extractor design
// never had to answer — who wrote this value? — and that question is
// #799. The answer has to be carried per field, because after a merge
// the Result is no longer one extractor's word.

// SourcedResult pairs one extractor's Result with the name of the
// extractor that produced it. The job handler builds one per
// supporting extractor and hands the slice to [MergeResults].
type SourcedResult struct {
	// Source is [Extractor.Name] — "exif" / "iptc" / "xmp" / …
	Source string
	Result Result
}

// SetByExtraction is the provenance written when a value's originating
// extractor is not known. It exists for Results assembled by hand
// (tests, and any future caller that builds a Result without going
// through [MergeResults]) — the honest answer to "which extractor?"
// when there is no answer is not "exif".
const SetByExtraction = "extract"

// MergeResults folds several extractors' Results into the single
// Result the applier consumes, recording per-field provenance in
// [Result.FieldSources].
//
// # Precedence
//
// FIRST writer wins, so the slice order is the precedence order, and
// the job handler builds it in extractor-registration order — EXIF,
// then IPTC, then XMP. That is the order the registration site already
// documented ("EXIF first (largest catalog), then IPTC + XMP") for a
// dispatch that never actually consulted more than one of them.
//
// In practice nothing collides: each extractor namespaces its output
// (`capture_datetime` is EXIF's, `iptc_credit` is IPTC's, `xmp_rights`
// is XMP's), which is the whole point of CanonicalField being
// per-source rather than per-semantic. Two extractors' views of the
// same semantic field are reconciled by the OPERATOR, one level up, by
// choosing which canonical a field-definition's extraction_source
// names. The rule here is a determinism guarantee for a case that
// should not arise, not a policy — if it ever fires, the right fix is
// a distinct CanonicalField, not a cleverer merge.
//
// # Side channels
//
// ICCProfile, Orientation, PreviewImageBytes and PageCount are
// single-valued and follow the same first-non-zero-wins rule. Format
// comes from the first Result that names one.
func MergeResults(parts []SourcedResult) Result {
	out := Result{
		Fields:       map[CanonicalField]Value{},
		FieldSources: map[CanonicalField]string{},
	}
	for _, p := range parts {
		if out.Format == "" {
			out.Format = p.Result.Format
		}
		for k, v := range p.Result.Fields {
			if _, taken := out.Fields[k]; taken {
				continue
			}
			out.Fields[k] = v
			// Prefer the per-Result provenance when the extractor
			// somehow set one (a nested merge); otherwise the
			// extractor's own name.
			if s := p.Result.FieldSources[k]; s != "" {
				out.FieldSources[k] = s
			} else {
				out.FieldSources[k] = p.Source
			}
		}
		if len(out.ICCProfile) == 0 {
			out.ICCProfile = p.Result.ICCProfile
		}
		if out.Orientation == 0 {
			out.Orientation = p.Result.Orientation
		}
		if len(out.PreviewImageBytes) == 0 {
			out.PreviewImageBytes = p.Result.PreviewImageBytes
		}
		if out.PageCount == 0 {
			out.PageCount = p.Result.PageCount
		}
	}
	return out
}

// sourceFor reports the provenance to record for one canonical field,
// falling back to [SetByExtraction] when the Result carries none.
func (r Result) sourceFor(f CanonicalField) string {
	if s := r.FieldSources[f]; s != "" {
		return s
	}
	return SetByExtraction
}
