// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// A vocabulary is a searchable resource (ADR 0092 §1, #789)
// ---------------------------------------------------------------------------
//
// The premise this endpoint exists to fix, in the owner's words:
//
//	The metadata fields are all just listed. Which isn't a problem in
//	our local because we have only like a dozen at most in some fields.
//	However, real production will have 1000s.
//
// Every surface that offers a term today reads `options.values` off the
// field definition and filters it in the browser. That is correct only
// while the client provably holds the WHOLE list, and it fails silently
// rather than loudly when it stops being true: the picker keeps working
// and simply stops offering terms past whatever prefix of the
// vocabulary happened to be shipped.
//
// So the search moves to the server and the client gets a BOUND. What
// comes back is capped, and the response says how many terms actually
// matched, so a surface can tell its reader it is showing 50 of 340
// instead of implying 50 is the answer.
//
// # Why the filter is in Go and not in SQL
//
// A vocabulary is ONE jsonb document on ONE row, not a table of terms.
// There is no per-term row to index and no query plan to improve:
// `jsonb_path_query` over `options` would deserialise the same document
// this does, in the database, and then ship the matches back anyway. The
// expensive thing about the old design was never the scan — a few
// thousand strings is microseconds — it was shipping the document to
// every browser that rendered the field. That is what this removes.
//
// The document is read through the field-by-id LRU, so a hot field
// costs no query at all. If a vocabulary ever outgrows one document
// (hundreds of thousands of terms), the answer is a `field_option`
// table, and THIS endpoint is what makes that change invisible to every
// client — which is the other half of why the endpoint is the contract.

// vocabSearchDefaultLimit is what a caller that says nothing gets. Big
// enough to fill a dropdown without scrolling into uselessness, small
// enough that twenty fields on one page is still a small payload.
const vocabSearchDefaultLimit = 50

// vocabSearchMaxLimit is the hard ceiling, mirroring the spec. A caller
// asking for more is clamped rather than refused: the point of the cap
// is that a response has a known worst case, and 400ing a client that
// wanted everything just moves the unbounded fetch somewhere else.
const vocabSearchMaxLimit = 200

// SearchFieldValues answers one bounded query against one field's
// vocabulary.
func (h *Handler) SearchFieldValues(
	ctx context.Context,
	req openapi.SearchFieldValuesRequestObject,
) (openapi.SearchFieldValuesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SearchFieldValues401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	pgField := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	field, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SearchFieldValues404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: vocabulary search: %w", err)
	}

	// read_capability gates the field's VALUES on every other read path
	// (ListAssetFieldValues filters on it, facet selection refuses a
	// search naming a field the caller cannot read). Its vocabulary is
	// the set of values it can hold, so leaving this ungated would make
	// the term list the disclosure the value gate exists to prevent —
	// "which artists are on the roster" is answerable from a picker.
	if field.ReadCapability != nil && *field.ReadCapability != "" {
		if !id.Can(*field.ReadCapability) {
			return openapi.SearchFieldValues403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
					Error: "missing capability for this field: " + *field.ReadCapability,
				},
			}, nil
		}
	}

	limit := vocabSearchDefaultLimit
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 {
		limit = 1
	}
	if limit > vocabSearchMaxLimit {
		limit = vocabSearchMaxLimit
	}

	q := ""
	if req.Params.Q != nil {
		q = strings.ToLower(strings.TrimSpace(*req.Params.Q))
	}
	substring := req.Params.Match != nil && *req.Params.Match == openapi.Substring

	wantStatus := openapi.SearchFieldValuesParamsStatusActive
	if req.Params.Status != nil {
		wantStatus = *req.Params.Status
	}

	values, _, decErr := decodeOptionValues(field.Options)
	if decErr != nil && !errors.Is(decErr, errNoValues) {
		// A document that will not parse is a broken field, not a
		// broken request. Reported as empty rather than 500 for the
		// same reason resolveOptionSlugs is tolerant: a read surface
		// must not fail an entire page over one malformed definition,
		// and the operator sees the problem in the options editor.
		values = nil
	}

	page := searchVocabulary(values, q, substring, wantStatus, limit)
	page.OpenVocabulary = openVocabularyApplies(field.Type, field.OpenVocabulary)
	// can_extend is the SERVER's answer to "may this caller create a
	// term here", not the client's guess from two separate facts. A
	// picker renders its create row from this alone, which is what
	// makes ADR 0092 §2's "the same control with the create arm absent"
	// hold without the browser reimplementing the write rule.
	page.CanExtend = page.OpenVocabulary && canExtendVocabulary(id)
	return openapi.SearchFieldValues200JSONResponse(page), nil
}

// vocabCandidate is one term plus the rank its match earned.
type vocabCandidate struct {
	value openapi.VocabularyValue
	rank  int
	// key is the lowercased label the sort breaks ties on, precomputed
	// because the comparator runs O(n log n) times.
	key string
}

// Match ranks, best first. The whole point of ranking rather than
// merely filtering is that "type it and press Enter" must land on the
// term the person meant: an exact hit outranks a term that merely
// starts with what they typed, which outranks one that happens to
// contain it.
const (
	rankExact = iota
	rankPrefix
	rankSubstring
	rankNone
)

// searchVocabulary walks a field's option tree once, ranks what the
// query admits, sorts, and returns the capped page.
//
// Matching keys are slug, label AND aliases — the same three the write
// path resolves through (indexVocabulary). A term offered here is
// therefore a term a value write will accept, which is the property
// that makes a picker's suggestions trustworthy. Offering by label
// alone would hide a term whose alias is the only thing a person knows
// it by, and offering by slug alone would hide every term whose slug is
// an abbreviation of its label.
func searchVocabulary(
	values []FieldOption,
	q string,
	substring bool,
	want openapi.SearchFieldValuesParamsStatus,
	limit int,
) openapi.VocabularyValuePage {
	page := openapi.VocabularyValuePage{
		Values: []openapi.VocabularyValue{},
		Limit:  limit,
	}

	cands := make([]vocabCandidate, 0, limit*2)
	walkOptions(values, nil, func(o FieldOption, ancestors []string) {
		slug := strings.TrimSpace(o.Value)
		if slug == "" {
			return
		}
		// vocabulary_size counts EVERY term at every depth in every
		// state, before any filter. It is the number a client needs to
		// decide whether fetching this field whole is defensible, and a
		// number that moved with the query could not answer that.
		page.VocabularySize++

		status := o.Status
		if status == "" {
			status = OptionActive
		}
		if !statusWanted(status, want) {
			return
		}
		label := strings.TrimSpace(o.Label)
		if label == "" {
			label = slug
		}
		rank := rankTerm(q, slug, label, o.Aliases, substring)
		if rank == rankNone {
			return
		}
		page.Matched++
		// Only the terms that could still make the cut are kept. The
		// sort is over what matched, and a query matching every term of
		// a five-thousand-term field would otherwise build a
		// five-thousand-element slice to return fifty of it. Matched is
		// counted above, so the honest total survives the pruning.
		if len(cands) >= limit*4 {
			// Cheap prune: keep the buffer bounded by sorting and
			// trimming when it grows past four pages' worth. Four
			// rather than one so the sort runs rarely; the result is
			// identical because the comparator is a total order.
			sortCandidates(cands)
			cands = cands[:limit]
		}
		cands = append(cands, vocabCandidate{
			value: vocabValue(o, slug, label, status, ancestors),
			rank:  rank,
			key:   strings.ToLower(label),
		})
	})

	sortCandidates(cands)
	if len(cands) > limit {
		cands = cands[:limit]
	}
	for _, c := range cands {
		page.Values = append(page.Values, c.value)
	}
	page.Returned = len(page.Values)
	page.Truncated = page.Matched > page.Returned
	return page
}

// sortCandidates orders by rank, then label, then slug. The slug
// tiebreak is what makes the order TOTAL: two terms can share a rank
// and a label (legitimately, in different branches of a tree), and
// without it the same query could return them in either order and a
// caller raising `limit` would see rows reshuffle.
func sortCandidates(c []vocabCandidate) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].rank != c[j].rank {
			return c[i].rank < c[j].rank
		}
		if c[i].key != c[j].key {
			return c[i].key < c[j].key
		}
		return c[i].value.Value < c[j].value.Value
	})
}

// rankTerm scores one term against the query across all three key
// kinds, keeping the BEST score any key earned. An empty query admits
// everything at one rank, which is the browse case.
func rankTerm(q, slug, label string, aliases []string, substring bool) int {
	if q == "" {
		return rankExact
	}
	best := rankNone
	consider := func(key string) {
		r := rankKey(q, strings.ToLower(strings.TrimSpace(key)), substring)
		if r < best {
			best = r
		}
	}
	consider(slug)
	consider(label)
	for _, a := range aliases {
		consider(a)
	}
	return best
}

func rankKey(q, key string, substring bool) int {
	switch {
	case key == "":
		return rankNone
	case key == q:
		return rankExact
	case strings.HasPrefix(key, q):
		return rankPrefix
	case substring && strings.Contains(key, q):
		return rankSubstring
	default:
		return rankNone
	}
}

// statusWanted applies the `status` filter. `any` is the curation view
// — a tombstone is only useful to someone who can see it — and the
// default is `active`, the set a picker may OFFER.
func statusWanted(got OptionStatus, want openapi.SearchFieldValuesParamsStatus) bool {
	switch want {
	case openapi.SearchFieldValuesParamsStatusAny:
		return true
	case openapi.SearchFieldValuesParamsStatusDeprecated:
		return got == OptionDeprecated
	case openapi.SearchFieldValuesParamsStatusArchived:
		return got == OptionArchived
	default:
		return got == OptionActive
	}
}

// vocabValue renders one option in the wire shape.
func vocabValue(o FieldOption, slug, label string, status OptionStatus, ancestors []string) openapi.VocabularyValue {
	// ancestors is reused across the walk — copy before keeping it.
	path := make([]string, len(ancestors), len(ancestors)+1)
	copy(path, ancestors)
	v := openapi.VocabularyValue{
		Value:  slug,
		Label:  label,
		Path:   append(path, label),
		Status: openapi.VocabularyValueStatus(status),
	}
	if o.ReplacedBy != "" {
		rb := o.ReplacedBy
		v.ReplacedBy = &rb
	}
	if len(o.Aliases) > 0 {
		a := append([]string(nil), o.Aliases...)
		v.Aliases = &a
	}
	return v
}
