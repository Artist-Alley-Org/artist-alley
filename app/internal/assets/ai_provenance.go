// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

// The maker's AI declaration (#1167, ADR 0094).
//
// This file is deliberately the shape of mature.go's opposite number,
// and the difference between the two is the whole point:
//
//   - `mature` has an operator policy gate (matureWriteAllowed) because
//     the flag decides who is HANDED the work. Setting it on an install
//     that disallows mature content is refused with a 400.
//   - `ai_provenance` has NO gate and can have none. It is a statement
//     about the work, never a permission on it (ADR 0094 §4): nothing
//     is withheld from anybody on account of it, so there is no
//     operator switch it could contradict. If an operator ever wants AI
//     work off their instance that is MODERATION and produces an
//     ordinary withheld/removed state through the machinery that
//     already carries the derived-copies discipline.
//
// The other asymmetry is the zero value. `mature` reads an absent flag
// as `false`, which is true of a library that predates the feature.
// There is no such reading here: absent means UNDECLARED, and the one
// thing it must never be silently converted into is `none`, which is a
// positive claim the maker did not make.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// The three declared states. NULL — undeclared — is the fourth
// possibility and is represented by the absence of a value, never by a
// member of this set.
const (
	AIProvenanceNone      = "none"
	AIProvenanceAssisted  = "assisted"
	AIProvenanceGenerated = "generated"
)

// aiProvenanceValues is the accepted set, mirroring the CHECK constraint
// migration 00060 puts on both columns. Kept as a set rather than a
// slice so the error message can list it in a stable order without the
// lookup and the message disagreeing.
var aiProvenanceValues = map[string]struct{}{
	AIProvenanceNone:      {},
	AIProvenanceAssisted:  {},
	AIProvenanceGenerated: {},
}

// aiProvenanceValue validates one declared value and returns it as the
// pointer the query layer wants.
//
// Empty string is rejected rather than quietly treated as "undeclared".
// A client that means undeclared omits the field; one that sends `""`
// has a bug, and turning that bug into a silent NULL would make the
// difference between "declared nothing" and "sent nothing" invisible on
// exactly the axis where the difference matters.
func aiProvenanceValue(v string) (*string, error) {
	trimmed := strings.TrimSpace(v)
	if _, ok := aiProvenanceValues[trimmed]; !ok {
		accepted := make([]string, 0, len(aiProvenanceValues))
		for k := range aiProvenanceValues {
			accepted = append(accepted, k)
		}
		sort.Strings(accepted)
		return nil, fmt.Errorf(
			"ai_provenance must be one of %s (omit the field to leave the work undeclared)",
			strings.Join(accepted, "|"),
		)
	}
	out := trimmed
	return &out, nil
}

// aiProvenanceFromCreate maps the create body's optional enum onto the
// column. A nil input stays nil: the asset is born undeclared, which is
// the honest state for a work whose maker was not asked.
func aiProvenanceFromCreate(v *openapi.AssetCreateAiProvenance) (*string, error) {
	if v == nil {
		return nil, nil
	}
	return aiProvenanceValue(string(*v))
}

// aiProvenanceToAPI maps a stored column onto the read schema's enum.
// A NULL column yields nil — the wire form of undeclared — and callers
// must not substitute a value for it.
func aiProvenanceToAPI(v *string) *openapi.AssetAiProvenance {
	if v == nil || *v == "" {
		return nil
	}
	out := openapi.AssetAiProvenance(*v)
	return &out
}
