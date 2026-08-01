// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Upload-default admin surface (#793)
// ---------------------------------------------------------------------------
//
// The wire type (openapi.FieldDefault) and the stored type
// (metadata.FieldDefault) have the same JSON shape on purpose, so the
// bridge between them is a marshal/unmarshal rather than a field-by-field
// copy that can silently lose a member when one side grows. The bytes
// that come out of encodeFieldDefault are exactly the bytes that go into
// the jsonb column, so what an operator sent and what validation saw
// cannot diverge.

// encodeFieldDefault normalises a submitted default into the stored
// document, validating it against the field's type and live options.
func encodeFieldDefault(fieldType string, options []byte, in *openapi.FieldDefault) ([]byte, error) {
	if in == nil {
		return nil, errors.New("default_value: missing body")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("default_value: %w", err)
	}
	parsed, ok, err := ParseFieldDefault(raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("default_value: empty document")
	}
	if err := ValidateFieldDefault(fieldType, options, parsed); err != nil {
		return nil, err
	}
	// Re-marshal from the parsed struct rather than passing `raw`
	// through: that drops any member the wire type carries and the
	// stored type does not, so a document that round-trips through the
	// API is byte-stable.
	return json.Marshal(parsed)
}

// apiFieldDefault renders a stored document on the wire. Returns nil for
// "no default" and for a document too broken to decode — a field whose
// stored default is unreadable shows as having none, which is what the
// apply path will do with it anyway.
func apiFieldDefault(raw []byte) *openapi.FieldDefault {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out openapi.FieldDefault
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

// ---------------------------------------------------------------------------
// ListFieldDefaultOverrides
// ---------------------------------------------------------------------------

func (h *Handler) ListFieldDefaultOverrides(
	ctx context.Context,
	req openapi.ListFieldDefaultOverridesRequestObject,
) (openapi.ListFieldDefaultOverridesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFieldDefaultOverrides401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.ListFieldDefaultOverrides403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if _, err := q.GetFieldDefinitionByID(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListFieldDefaultOverrides404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: default overrides: load field: %w", err)
	}

	rows, err := q.ListFieldDefaultOverrides(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("metadata: default overrides: list: %w", err)
	}
	out := make(openapi.ListFieldDefaultOverrides200JSONResponse, 0, len(rows))
	for _, r := range rows {
		d := apiFieldDefault(r.DefaultValue)
		if d == nil {
			continue
		}
		out = append(out, openapi.FieldDefaultOverride{
			FieldId:          openapi_types.UUID(r.FieldID.Bytes),
			TeamId:           openapi_types.UUID(r.TeamID.Bytes),
			TeamSlug:         r.TeamSlug,
			TeamName:         r.TeamName,
			DefaultValue:     *d,
			CreatedAt:        r.CreatedAt.Time,
			UpdatedAt:        r.UpdatedAt.Time,
			UpdatedByUserRef: r.UpdatedByUserRef,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SetFieldDefaultOverride
// ---------------------------------------------------------------------------

func (h *Handler) SetFieldDefaultOverride(
	ctx context.Context,
	req openapi.SetFieldDefaultOverrideRequestObject,
) (openapi.SetFieldDefaultOverrideResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetFieldDefaultOverride401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.SetFieldDefaultOverride403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetFieldDefaultOverride400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgTeam := pgtype.UUID{Bytes: uuid.UUID(req.TeamId), Valid: true}
	q := New(h.Pool)

	field, err := q.GetFieldDefinitionByID(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetFieldDefaultOverride404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: default override: load field: %w", err)
	}
	// A default only applies to an asset being created. Offering one on
	// a collection field would store a document nothing can ever read.
	if field.SubjectKind != string(SubjectAsset) {
		return openapi.SetFieldDefaultOverride400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "upload defaults apply to asset fields only; this field describes a collection",
			},
		}, nil
	}

	team, err := q.GetTeamForDefaultOverride(ctx, pgTeam)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetFieldDefaultOverride404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: default override: load team: %w", err)
	}

	stored, err := encodeFieldDefault(field.Type, field.Options, req.Body)
	if err != nil {
		return openapi.SetFieldDefaultOverride400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	row, err := q.UpsertFieldDefaultOverride(ctx, UpsertFieldDefaultOverrideParams{
		FieldID:          pgID,
		TeamID:           pgTeam,
		DefaultValue:     stored,
		UpdatedByUserRef: &id.UserRef,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: default override: upsert: %w", err)
	}
	h.invalidateField(ctx, pgID)

	d := apiFieldDefault(row.DefaultValue)
	if d == nil {
		return nil, errors.New("metadata: default override: stored document did not round-trip")
	}
	return openapi.SetFieldDefaultOverride200JSONResponse{
		FieldId:          openapi_types.UUID(row.FieldID.Bytes),
		TeamId:           openapi_types.UUID(row.TeamID.Bytes),
		TeamSlug:         team.Slug,
		TeamName:         team.Name,
		DefaultValue:     *d,
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
		UpdatedByUserRef: row.UpdatedByUserRef,
	}, nil
}

// ---------------------------------------------------------------------------
// DeleteFieldDefaultOverride
// ---------------------------------------------------------------------------

func (h *Handler) DeleteFieldDefaultOverride(
	ctx context.Context,
	req openapi.DeleteFieldDefaultOverrideRequestObject,
) (openapi.DeleteFieldDefaultOverrideResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeleteFieldDefaultOverride401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.DeleteFieldDefaultOverride403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgTeam := pgtype.UUID{Bytes: uuid.UUID(req.TeamId), Valid: true}

	n, err := New(h.Pool).DeleteFieldDefaultOverride(ctx, DeleteFieldDefaultOverrideParams{
		FieldID: pgID,
		TeamID:  pgTeam,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: default override: delete: %w", err)
	}
	if n == 0 {
		return openapi.DeleteFieldDefaultOverride404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "override not found"},
		}, nil
	}
	h.invalidateField(ctx, pgID)
	return openapi.DeleteFieldDefaultOverride204Response{}, nil
}
