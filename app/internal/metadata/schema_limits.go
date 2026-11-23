// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import "github.com/mscrnt/artist-alley/app/internal/openapi"

// The `aa-field-schema/1` FORMAT limits (ADR 0083, 2026-11-22
// amendments). The exporter and the import preview enforce the same
// numbers, measured the same way, so no exported document is refused on
// import for a format limit. Import still applies the ordinary field
// rules, and can refuse an export whose source row breaks them (for
// example a label stored with surrounding spaces outside the API).
const (
	// FieldSchemaMaxOptionDepth bounds one option tree. A top-level
	// term is depth 1.
	FieldSchemaMaxOptionDepth = 64
	// FieldSchemaMaxOptionNodes bounds the option terms of a whole
	// document, every level of every field counted.
	FieldSchemaMaxOptionNodes = 100_000
	// FieldSchemaMaxDocumentBytes is the canonical encoded document,
	// trailing newline included: the export response cap.
	FieldSchemaMaxDocumentBytes = FieldSchemaExportMaxResponseBytes
)

// portableOptionStats reports the depth (1 for a flat list, 0 for none)
// and the node count of a portable option tree.
func portableOptionStats(opts []openapi.FieldSchemaExportOption) (depth, nodes int) {
	for _, o := range opts {
		d, n := portableOptionStats(o.Children)
		nodes += 1 + n
		if d+1 > depth {
			depth = d + 1
		}
	}
	return depth, nodes
}
