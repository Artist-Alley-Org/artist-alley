// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// DisplayValue renders one field value as the single line a card shows.
//
// # Why this is here and not in the assets package
//
// The card (#552) needs a STRING; every other reader of a field value gets
// the typed columns plus a `resolved_options` map and does its own
// formatting. Left to the caller, "how a field value reads" would have had a
// second implementation in the assets package — and it would have been the
// wrong one, because the interesting half is not formatting but VOCABULARY
// RESOLUTION: `select`, `multi_select` and `tree` store a SLUG and never the
// label (ADR 0012), precisely so relabelling a term rewrites nothing. A
// renderer that printed the stored value would put `pass-1` on the card
// where the operator wrote "Pass 1", which is the exact defect #775 fixed on
// the detail surface.
//
// So the resolution runs through the same resolveOptionSlugs the read path
// already uses. A slug that resolves to nothing degrades to the slug itself
// — the documented client fallback, applied here once rather than left to
// every surface.
//
// An empty result means "nothing to show", and a caller must drop the entry
// rather than render a blank row.
func DisplayValue(
	fieldType string,
	fieldOptions []byte,
	valueText *string,
	valueNum *float64,
	valueDate pgtype.Timestamptz,
	valueOptions []string,
) string {
	if slugs := vocabularySlugs(fieldType, valueText, valueOptions); len(slugs) > 0 {
		hits := resolveOptionSlugs(fieldOptions, slugs)
		parts := make([]string, 0, len(slugs))
		for _, s := range slugs {
			if o, ok := hits[s]; ok && o.Label != "" {
				parts = append(parts, o.Label)
				continue
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ", ")
	}
	switch {
	case valueText != nil:
		return strings.TrimSpace(*valueText)
	case valueNum != nil:
		// 'g' with -1 precision so 3 prints as "3" and 3.5 as "3.5" —
		// a card showing "3.000000" is a card showing a float64.
		return strconv.FormatFloat(*valueNum, 'g', -1, 64)
	case valueDate.Valid:
		// Date only. The card is scan-level: a timestamp to the second is
		// noise at this density, and the detail surface still carries the
		// full value.
		return valueDate.Time.Format("2006-01-02")
	}
	return ""
}
