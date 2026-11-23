// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Portable field-schema export (#1576, ADR 0083 §1-§2 and amendments)
// ---------------------------------------------------------------------------
//
// An operator names the field codes to share and gets back a versioned,
// portable description of those definitions. The selection is made on
// every call (ADR 0083 §1: no persisted set, no "export everything").
//
// The projection is a POSITIVE whitelist: portableField copies the
// ADR 0083 IN properties one by one from the stored row into a struct
// that has no other members. Nothing is built from fieldDefToAPI or a
// generic map, so a column added to field_definition later cannot
// reach the export by default; it travels only when someone classifies
// it under ADR 0083 §2 and adds it here.

const (
	// FieldSchemaExportFormat is the one format this server writes. An
	// importer must refuse any other value rather than guess.
	FieldSchemaExportFormat = openapi.AaFieldSchema1

	// FieldSchemaExportMaxCodes bounds one selection, counted as sent
	// (before duplicates are folded).
	FieldSchemaExportMaxCodes = 100
	// FieldSchemaExportMaxRequestBytes is the request body cap. The
	// route's middleware enforces it before the body is decoded.
	FieldSchemaExportMaxRequestBytes = 64 << 10
	// FieldSchemaExportMaxResponseBytes bounds the response body: the
	// encoded export including the encoder's trailing newline. A larger
	// export is refused, never truncated.
	FieldSchemaExportMaxResponseBytes = 2 << 20
)

// ExportFieldSchema is POST /fields/export.
//
// Authorization comes first: no stored definition is read until the
// caller is known to hold fields.admin (or system.admin), so a refusal
// before that point says nothing about which codes exist. Read-only:
// the only query is a SELECT.
func (h *Handler) ExportFieldSchema(
	ctx context.Context,
	req openapi.ExportFieldSchemaRequestObject,
) (openapi.ExportFieldSchemaResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ExportFieldSchema401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.ExportFieldSchema403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.ExportFieldSchema400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	codes, msg := normalizeExportCodes(req.Body.Codes)
	if msg != "" {
		return openapi.ExportFieldSchema400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
		}, nil
	}

	rows, err := New(h.Pool).ListFieldDefinitionsByCodes(ctx, codes)
	if err != nil {
		return nil, fmt.Errorf("metadata: export select: %w", err)
	}
	doc, refusal, err := buildFieldSchemaExport(codes, rows)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return openapi.ExportFieldSchema422JSONResponse(*refusal), nil
	}
	return openapi.ExportFieldSchema200JSONResponse(doc), nil
}

// normalizeExportCodes validates a selection and folds it into a
// sorted set. The message is empty when the selection is acceptable.
// Codes must match the field-code rule exactly: no trimming, because a
// code that needs trimming is not a code any definition has.
func normalizeExportCodes(in []string) ([]string, string) {
	if len(in) == 0 {
		return nil, "codes must name at least one field"
	}
	if len(in) > FieldSchemaExportMaxCodes {
		return nil, fmt.Sprintf("codes holds %d entries; at most %d may be exported at once",
			len(in), FieldSchemaExportMaxCodes)
	}
	set := make(map[string]struct{}, len(in))
	for i, c := range in {
		if !codePattern.MatchString(c) {
			return nil, fmt.Sprintf("codes[%d] must match ^[a-z][a-z0-9_]*$", i)
		}
		set[c] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	slices.Sort(out)
	return out, ""
}

// buildFieldSchemaExport assembles the export for a normalized
// selection, or the refusal that applies. The checks run in the order
// the API documents, and each reports every offending code (sorted), so
// the same request over the same schema always gets the same answer.
func buildFieldSchemaExport(codes []string, rows []FieldDefinition) (openapi.FieldSchemaExport, *openapi.FieldSchemaExportRefusal, error) {
	byCode := make(map[string]FieldDefinition, len(rows))
	for _, r := range rows {
		byCode[r.Code] = r
	}

	unknown := []string{}
	for _, c := range codes {
		if _, ok := byCode[c]; !ok {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) > 0 {
		return openapi.FieldSchemaExport{}, exportRefusal(openapi.UnknownCodes, unknown,
			"no field definition has these codes"), nil
	}

	retired := []string{}
	for _, c := range codes {
		if byCode[c].Status != "active" {
			retired = append(retired, c)
		}
	}
	if len(retired) > 0 {
		return openapi.FieldSchemaExport{}, exportRefusal(openapi.RetiredCodes, retired,
			"only active field definitions can be exported; these are deprecated or archived"), nil
	}

	fields := make([]openapi.FieldSchemaExportField, 0, len(codes))
	malformed := []string{}
	for _, c := range codes {
		f, err := portableField(byCode[c])
		if err != nil {
			malformed = append(malformed, c)
			continue
		}
		fields = append(fields, f)
	}
	if len(malformed) > 0 {
		return openapi.FieldSchemaExport{}, exportRefusal(openapi.MalformedDefinition, malformed,
			"these definitions hold stored data that does not decode, so no faithful export exists"), nil
	}

	doc := openapi.FieldSchemaExport{Format: FieldSchemaExportFormat, Fields: fields}
	// Measure the bytes the response will actually carry. The generated
	// 200 response writes with json.Encoder, which appends a newline that
	// json.Marshal does not, so encoding the same way here is what makes
	// the bound exact rather than one byte loose.
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(doc); err != nil {
		return openapi.FieldSchemaExport{}, nil, fmt.Errorf("metadata: export encode: %w", err)
	}
	if encoded.Len() > FieldSchemaExportMaxResponseBytes {
		return openapi.FieldSchemaExport{}, exportRefusal(openapi.ExportTooLarge, []string{},
			fmt.Sprintf("the export would be %d bytes, over the %d byte limit; select fewer fields",
				encoded.Len(), FieldSchemaExportMaxResponseBytes)), nil
	}
	return doc, nil, nil
}

func exportRefusal(reason openapi.FieldSchemaExportRefusalReason, codes []string, msg string) *openapi.FieldSchemaExportRefusal {
	return &openapi.FieldSchemaExportRefusal{Reason: reason, Codes: codes, Error: msg}
}

// errMalformedDefinition marks a stored row that cannot be exported
// faithfully.
var errMalformedDefinition = errors.New("metadata: malformed stored field definition")

// portableField is the whitelist. Every member of the portable shape is
// assigned here from one named column, and nothing else is.
//
// A stored value that does not decode, or that the schema's own CHECK
// constraints should have made impossible, fails the whole field: an
// export that silently left a member out would look complete and be
// wrong.
func portableField(r FieldDefinition) (openapi.FieldSchemaExportField, error) {
	if !validFieldType(r.Type) {
		return openapi.FieldSchemaExportField{}, fmt.Errorf("%w: %s: type %q", errMalformedDefinition, r.Code, r.Type)
	}
	if _, err := ParseSubjectKind(r.SubjectKind); err != nil {
		return openapi.FieldSchemaExportField{}, fmt.Errorf("%w: %s: %v", errMalformedDefinition, r.Code, err)
	}
	f := openapi.FieldSchemaExportField{
		Code:                 r.Code,
		Label:                r.Label,
		Description:          r.Description,
		Type:                 r.Type,
		SubjectKind:          openapi.FieldSchemaExportFieldSubjectKind(r.SubjectKind),
		Required:             r.Required,
		Searchable:           r.Searchable,
		DisplayGroup:         r.DisplayGroup,
		DisplayOrder:         int(r.DisplayOrder),
		RegexpFilter:         copyOptionalString(r.RegexpFilter),
		ShowOnCard:           r.ShowOnCard,
		ShowInAdvancedSearch: r.ShowInAdvancedSearch,
		ShowOnUpload:         r.ShowOnUpload,
		EditTab:              copyOptionalString(r.EditTab),
		OpenVocabulary:       r.OpenVocabulary,
	}

	// Terms name other fields by CODE and are exported verbatim, whether
	// or not the referent is part of this export (ADR 0083, 2026-09-03).
	cond, err := DecodeDisplayCondition(r.DisplayCondition)
	if err != nil {
		return openapi.FieldSchemaExportField{}, fmt.Errorf("%w: %s: %v", errMalformedDefinition, r.Code, err)
	}
	if len(cond) > 0 {
		for _, term := range cond {
			if strings.TrimSpace(term) == "" {
				return openapi.FieldSchemaExportField{}, fmt.Errorf("%w: %s: blank display_condition term", errMalformedDefinition, r.Code)
			}
		}
		f.DisplayCondition = &cond
	}

	if hasVocabulary(r.Type) {
		opts, err := portableOptions(r.Options)
		if err != nil {
			return openapi.FieldSchemaExportField{}, fmt.Errorf("%w: %s: %v", errMalformedDefinition, r.Code, err)
		}
		f.Options = &opts
	}
	return f, nil
}

func hasVocabulary(fieldType string) bool {
	switch fieldType {
	case "select", "multi_select", "tree":
		return true
	}
	return false
}

func copyOptionalString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// portableOptions exports the active terms of a stored options
// document. The whole stored vocabulary is validated first, with the
// same shape checks the write path runs, so a corrupt document is
// refused rather than exported in part.
//
// Only active terms travel. A deprecated or archived term is left out
// together with its subtree: lifecycle is local editorial history (ADR
// 0083 §2), and a child exported without its retired parent would
// claim a place in a tree that does not exist. Aliases, status and
// replaced_by never travel.
//
// A document with no `values` key is an empty vocabulary, not an error:
// a field is routinely created before its terms are.
func portableOptions(raw []byte) ([]openapi.FieldSchemaExportOption, error) {
	values, _, err := decodeOptionValues(raw)
	if errors.Is(err, errNoValues) {
		return []openapi.FieldSchemaExportOption{}, nil
	}
	if err != nil {
		return nil, err
	}
	all := make(map[string]FieldOption, len(values))
	if err := collectSlugs(values, all, ""); err != nil {
		return nil, err
	}
	if err := checkOptions(values, all, ""); err != nil {
		return nil, err
	}
	return portableOptionList(values), nil
}

func portableOptionList(opts []FieldOption) []openapi.FieldSchemaExportOption {
	out := make([]openapi.FieldSchemaExportOption, 0, len(opts))
	for _, o := range opts {
		if o.Status != "" && o.Status != OptionActive {
			continue
		}
		e := openapi.FieldSchemaExportOption{
			Value:    strings.TrimSpace(o.Value),
			Children: portableOptionList(o.Children),
		}
		if label := strings.TrimSpace(o.Label); label != "" {
			e.Label = &label
		}
		out = append(out, e)
	}
	return out
}
