// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The SHIPPED FIELD CATALOGUE — the field definitions every install has
// because a migration inserted them, as opposed to the ones a seed, an
// operator or a federation peer added later (#812).
//
// # Why this list exists in Go at all
//
// The migrations own the ROWS. This file owns only the CLASSIFICATION:
// "these codes are ours, everything else in field_definition belongs to
// whoever put it there". Nothing here duplicates a definition's label,
// type, group or extraction wiring — that would be a second source of
// truth for the same nine rows, and the one that drifts.
//
// One consumer needs the classification and cannot derive it:
// seed.Reset. `aa seed --reset` used to name field_definition in its
// TRUNCATE list, which deleted the shipped catalogue on every dev, CI
// and demo instance and replaced it with the 20-entry studio catalogue
// in seed/profiles/dataset.field_definitions.json. Production ran one
// field catalogue and every instance we tested ran a different one,
// neither a subset of the other — a production state no test ever
// constructed. Reset now SWEEPS this table instead: the shipped rows
// survive, everything else goes. That rule needs a name for "shipped",
// and no query can supply one. A row inserted by 00001 and a row
// inserted by the seed are indistinguishable in the table.
//
// # Why it is a hand-written list and not a heuristic
//
// Every candidate discriminator is wrong:
//
//   - created_by_user_ref IS NULL — true of the seven baseline product
//     rows, but ALSO true of anything inserted by a background job, and
//     it was true of the six fixtures' siblings for exactly the wrong
//     reason (they carry 420000, a test ref, which is what identified
//     them, not what a rule could).
//   - "inserted before goose version N" — field_definition has no
//     provenance column, and created_at is a fold artefact: the six
//     fixtures carry timestamps from June and July 2026, months apart
//     and interleaved with the product rows' own.
//   - "is referenced by app code" — pixel_width/pixel_height are, the
//     other seven are not, and country least of all. Shipping a
//     catalogue nothing hardcodes is the point of a catalogue.
//
// So the classification is a judgement call, written by hand, for the
// same reason seed.polymorphicRefs is.
//
// # And why the enforcement is derived
//
// A hand-maintained list rots by omission: someone adds a shipped
// definition in migration 00030, nobody adds it here, and the very next
// `aa seed --reset` silently deletes it — reintroducing #812 one row at
// a time and with no error anywhere. TestShippedFieldCatalogue_
// MatchesMigrations reads the codes out of a FRESHLY MIGRATED database
// and fails if the two sets differ in either direction. Adding a
// definition without registering it is therefore a CI failure that says
// "classify me", which is the half a hand-written list cannot do for
// itself. Same shape as TestPolymorphicRegistry_CoversSchema.

package db

import "sort"

// ShippedField is one definition the migrations insert, plus the reason
// it is part of the product. Reason is mandatory: a bare list of codes
// invites the next reader to add one "because it looked shipped".
type ShippedField struct {
	Code   string
	Reason string
}

// shippedFields is the classified set. It is EXACTLY what a fresh
// install's field_definition table contains — no more, no less — and
// TestShippedFieldCatalogue_MatchesMigrations proves it against a real
// migration run.
var shippedFields = []ShippedField{
	{"title", "core editorial field; baseline 00001, mapped to IPTC ObjectName (the mapping RS ships)"},
	{"description", "core editorial field; baseline 00001"},
	{"credit", "rights field; baseline 00001, mapped to IPTC Credit"},
	{"copyright", "rights field; baseline 00001, mapped to XMP dc:rights"},
	{"capture_date", "technical field; baseline 00001, mapped to EXIF DateTimeOriginal"},
	{"keywords", "core tagging field; baseline 00001, mapped to IPTC Keywords"},
	{"country", "location field; baseline 00001, mapped to IPTC Country-PrimaryLocationName. " +
		"Typed `tree` here where RS types its equivalent as a node-backed keyword list — a " +
		"divergence worth revisiting deliberately (#812 flags it), but not by deleting the row"},
	{"pixel_width", "computed image dimension; migration 00017. IIIF info.json and the masonry " +
		"tile shape resolve it by code (pixeldims.SelectColumnsSQL, iiif/http.go)"},
	{"pixel_height", "computed image dimension; migration 00017. Same two consumers as pixel_width"},
}

// ShippedFieldCodes returns the registered codes, sorted, as a fresh
// slice. Sorted so a caller that renders them into SQL or a log line
// produces a reproducible result; a copy so no caller can mutate the
// registry.
func ShippedFieldCodes() []string {
	out := make([]string, 0, len(shippedFields))
	for _, f := range shippedFields {
		out = append(out, f.Code)
	}
	sort.Strings(out)
	return out
}

// ShippedFields returns the registry entries with their reasons, in
// declaration order. For tests and diagnostics.
func ShippedFields() []ShippedField {
	out := make([]ShippedField, len(shippedFields))
	copy(out, shippedFields)
	return out
}
