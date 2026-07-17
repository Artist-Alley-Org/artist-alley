// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — collection-side field value HTTP surface.
//
// Mirrors the asset-side endpoints (GetAssetFields,
// SetAssetFieldValue, ClearAssetFieldValue, GetAssetFieldValueHistory)
// but for collection_field_value + collection_field_value_history.
// The handler implementation deliberately stays close to the asset
// shape so a future polymorphic refactor can collapse the two with
// minimal surgery.
//
// # Capability filtering
//
// Reads run through the same field_definition.read_capability gate
// the asset path uses — callers who lack a field's read_cap don't
// see that value (the row is silently dropped from the response).
// Writes check write_capability before the upsert and 403 on miss.
//
// # Cache invalidation
//
// Cache for collection field values is per-collection (one entry =
// the full value list for one collection). Writes evict the entry
// and broadcast via the cache.Registry NOTIFY channel so peer
// instances drop their copies too. This is the ABC pattern from
// memory/feedback_core_principles.

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// cacheDomainCollectionFieldValues — NOTIFY channel for
// per-collection value caches. Lives next to cacheDomainFieldByID
// so peer-side LISTEN observes both via one Registry.
const cacheDomainCollectionFieldValues = "collection_field_value.list"

// collectionValueCache shape: keyed on the collection UUID string,
// value is the joined list of (field_def, value) pairs in API form.
// Stored pre-filter so the cache works the same regardless of who
// reads it; the per-caller read_capability filter runs on the way
// out.
type collectionValueCacheEntry struct {
	// values is the FULL value list for the collection, sourced
	// directly from collection_field_value joined with
	// field_definition. The handler filters per-caller on read.
	values []openapi.CollectionFieldValue
}

// SetCollectionValueCache enables the per-collection cache.
// Tests can leave this nil to bypass — the handler treats nil as
// "always read through".
func (h *Handler) setCollectionValueCache(registry *cache.Registry) {
	if registry == nil {
		return
	}
	// 10k entries fits a large catalogue of collections at ~1KB/entry.
	h.collectionValues = cache.Register[collectionValueCacheEntry](
		registry, cacheDomainCollectionFieldValues, 10_000,
	)
}

// ---------------------------------------------------------------------------
// GetCollectionFields — list values for a collection
// ---------------------------------------------------------------------------

func (h *Handler) GetCollectionFields(
	ctx context.Context,
	req openapi.GetCollectionFieldsRequestObject,
) (openapi.GetCollectionFieldsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetCollectionFields401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	values, err := h.getCollectionValues(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("metadata: get collection values: %w", err)
	}

	// Capability filter: drop fields the caller lacks read_capability for.
	// We rebuild the slice rather than mutating the cached one (the cache
	// shape is the unfiltered superset).
	out := make([]openapi.CollectionFieldValue, 0, len(values))
	for _, v := range values {
		if !canReadField(ctx, h, v.FieldId, id) {
			continue
		}
		out = append(out, v)
	}
	return openapi.GetCollectionFields200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// SetCollectionFieldValue — upsert one value
// ---------------------------------------------------------------------------

func (h *Handler) SetCollectionFieldValue(
	ctx context.Context,
	req openapi.SetCollectionFieldValueRequestObject,
) (openapi.SetCollectionFieldValueResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetCollectionFieldValue401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetCollectionFieldValue400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	pgCollection := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgField := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}

	fieldRow, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetCollectionFieldValue404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, err
	}

	// 422 — wrong subject_kind. The discriminator gate: only
	// collection-side fields can be written on a collection.
	if fieldRow.SubjectKind != string(SubjectCollection) {
		field := fieldRow.Code
		return openapi.SetCollectionFieldValue422JSONResponse{
			Error:  fmt.Sprintf("field %q is not a collection field", fieldRow.Code),
			Reason: openapi.FieldNotForCollection,
			Field:  &field,
		}, nil
	}

	// Capability gate: write_capability if set.
	if fieldRow.WriteCapability != nil && *fieldRow.WriteCapability != "" {
		if !id.Can(*fieldRow.WriteCapability) {
			return openapi.SetCollectionFieldValue403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability for this field: " + *fieldRow.WriteCapability},
			}, nil
		}
	}

	// Validate the supplied value_* matches the field's type.
	if vErr := validateCollectionValueType(fieldRow.Type, req.Body); vErr != nil {
		field := fieldRow.Code
		return openapi.SetCollectionFieldValue422JSONResponse{
			Error:  vErr.Error(),
			Reason: openapi.ValueTypeMismatch,
			Field:  &field,
		}, nil
	}

	// set_by defaults to manual; the API allows callers to override
	// (e.g. an import pipeline). Validation already happens via the
	// openapi enum.
	setBy := "manual"
	if req.Body.SetBy != nil {
		setBy = string(*req.Body.SetBy)
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("metadata: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	prev, err := qTx.GetCollectionFieldValue(ctx, GetCollectionFieldValueParams{
		CollectionID: pgCollection,
		FieldID:      pgField,
	})
	hadOld := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("metadata: load previous: %w", err)
	}

	row, err := qTx.UpsertCollectionFieldValue(ctx, buildCollectionUpsertParams(
		pgCollection, pgField, fieldRow.Type, req.Body, setBy, &id.UserRef,
	))
	if err != nil {
		return nil, fmt.Errorf("metadata: upsert: %w", err)
	}

	// Append history row inside the same tx — same pattern as the
	// asset path, so cross-domain readers see one shape.
	var oldJSON, newJSON []byte
	if hadOld {
		oldJSON, _ = valueRowToJSON(prev.ValueText, prev.ValueNum, prev.ValueDate, prev.ValueOptions, prev.ValueRef, fieldRow.Type)
	}
	newJSON, _ = valueRowToJSON(row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef, fieldRow.Type)
	if err := qTx.AppendCollectionFieldValueHistory(ctx, AppendCollectionFieldValueHistoryParams{
		CollectionID:     pgCollection,
		FieldID:          pgField,
		OldValue:         oldJSON,
		NewValue:         newJSON,
		SetBy:            setBy,
		ChangedByUserRef: &id.UserRef,
	}); err != nil {
		return nil, fmt.Errorf("metadata: append history: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("metadata: commit: %w", err)
	}

	// Cache evict + broadcast. Best-effort — a NOTIFY failure logs
	// but doesn't propagate, matching invalidateField behavior.
	h.invalidateCollectionValues(ctx, pgCollection)

	return openapi.SetCollectionFieldValue200JSONResponse(
		buildCollectionValue(row.FieldID, fieldRow.Code, fieldRow.Label, fieldRow.Type,
			row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef,
			row.SetBy, row.SetAt, row.SetByUserRef),
	), nil
}

// ---------------------------------------------------------------------------
// ClearCollectionFieldValue — delete one value
// ---------------------------------------------------------------------------

func (h *Handler) ClearCollectionFieldValue(
	ctx context.Context,
	req openapi.ClearCollectionFieldValueRequestObject,
) (openapi.ClearCollectionFieldValueResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ClearCollectionFieldValue401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgCollection := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgField := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("metadata: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	prev, err := qTx.GetCollectionFieldValue(ctx, GetCollectionFieldValueParams{
		CollectionID: pgCollection,
		FieldID:      pgField,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hadOld := err == nil

	if err := qTx.DeleteCollectionFieldValue(ctx, DeleteCollectionFieldValueParams{
		CollectionID: pgCollection,
		FieldID:      pgField,
	}); err != nil {
		return nil, fmt.Errorf("metadata: delete: %w", err)
	}

	if hadOld {
		fieldRow, fErr := h.getFieldByIDCached(ctx, pgField)
		if fErr != nil && !errors.Is(fErr, pgx.ErrNoRows) {
			return nil, fErr
		}
		var fieldType string
		if fErr == nil {
			fieldType = fieldRow.Type
		}
		oldJSON, _ := valueRowToJSON(prev.ValueText, prev.ValueNum, prev.ValueDate, prev.ValueOptions, prev.ValueRef, fieldType)
		if err := qTx.AppendCollectionFieldValueHistory(ctx, AppendCollectionFieldValueHistoryParams{
			CollectionID:     pgCollection,
			FieldID:          pgField,
			OldValue:         oldJSON,
			NewValue:         nil,
			SetBy:            "manual",
			ChangedByUserRef: &id.UserRef,
		}); err != nil {
			return nil, fmt.Errorf("metadata: append history: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("metadata: commit: %w", err)
	}
	h.invalidateCollectionValues(ctx, pgCollection)
	return openapi.ClearCollectionFieldValue204Response{}, nil
}

// ---------------------------------------------------------------------------
// GetCollectionFieldValueHistory — read audit trail
// ---------------------------------------------------------------------------

func (h *Handler) GetCollectionFieldValueHistory(
	ctx context.Context,
	req openapi.GetCollectionFieldValueHistoryRequestObject,
) (openapi.GetCollectionFieldValueHistoryResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetCollectionFieldValueHistory401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgCollection := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgField := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}

	limit := int32(50)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}

	rows, err := New(h.Pool).ListCollectionFieldValueHistory(ctx, ListCollectionFieldValueHistoryParams{
		CollectionID: pgCollection,
		FieldID:      pgtype.UUID{Bytes: pgField.Bytes, Valid: true},
		RowLimit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: list history: %w", err)
	}

	out := make([]openapi.CollectionFieldValueHistoryEntry, 0, len(rows))
	for _, r := range rows {
		entry := openapi.CollectionFieldValueHistoryEntry{
			Id:           openapi_types.UUID(r.ID.Bytes),
			CollectionId: openapi_types.UUID(r.CollectionID.Bytes),
			FieldId:      openapi_types.UUID(r.FieldID.Bytes),
			ChangedAt:    r.ChangedAt.Time,
			SetBy:        r.SetBy,
		}
		if r.ChangedByUserRef != nil {
			entry.ChangedByUserRef = r.ChangedByUserRef
		}
		if len(r.OldValue) > 0 && string(r.OldValue) != "null" {
			var m map[string]any
			if jErr := json.Unmarshal(r.OldValue, &m); jErr == nil {
				entry.OldValue = &m
			}
		}
		if len(r.NewValue) > 0 && string(r.NewValue) != "null" {
			var m map[string]any
			if jErr := json.Unmarshal(r.NewValue, &m); jErr == nil {
				entry.NewValue = &m
			}
		}
		out = append(out, entry)
	}
	return openapi.GetCollectionFieldValueHistory200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// getCollectionValues reads the full value list for a collection.
// Cache-fronted: warm reads skip the DB; cold reads populate.
func (h *Handler) getCollectionValues(ctx context.Context, collectionID pgtype.UUID) ([]openapi.CollectionFieldValue, error) {
	key := uuidString(collectionID)

	if h.collectionValues != nil {
		if entry, ok := h.collectionValues.Get(key); ok {
			return entry.values, nil
		}
	}

	rows, err := New(h.Pool).ListCollectionFieldValues(ctx, collectionID)
	if err != nil {
		return nil, err
	}

	// Join with field_definition to surface code/label/type per row.
	values := make([]openapi.CollectionFieldValue, 0, len(rows))
	for _, r := range rows {
		fieldRow, fErr := h.getFieldByIDCached(ctx, r.FieldID)
		if fErr != nil {
			// Definition gone (CASCADE delete in flight, race?).
			// Skip silently; cache repopulates on next request.
			continue
		}
		values = append(values, buildCollectionValue(
			r.FieldID, fieldRow.Code, fieldRow.Label, fieldRow.Type,
			r.ValueText, r.ValueNum, r.ValueDate, r.ValueOptions, r.ValueRef,
			r.SetBy, r.SetAt, r.SetByUserRef,
		))
	}

	if h.collectionValues != nil {
		h.collectionValues.Add(key, collectionValueCacheEntry{values: values})
	}
	return values, nil
}

// invalidateCollectionValues drops the local entry and broadcasts to
// peers. Best-effort.
func (h *Handler) invalidateCollectionValues(ctx context.Context, collectionID pgtype.UUID) {
	if h.collectionValues == nil {
		return
	}
	if err := h.collectionValues.Invalidate(ctx, uuidString(collectionID)); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "metadata.cache.invalidate.error",
			slog.String("domain", cacheDomainCollectionFieldValues),
			slog.String("key", uuidString(collectionID)),
			slog.String("err", err.Error()),
		)
	}
}

// InvalidateCollectionValues is the cross-package entry point for
// callers that own the collections table (collections.HardDelete)
// to clear the cache after a cascade-delete. The collection ID is
// passed as a uuid.UUID for ergonomics — callers don't have to
// build a pgtype.UUID just to invalidate.
func (h *Handler) InvalidateCollectionValues(ctx context.Context, collectionID uuid.UUID) {
	h.invalidateCollectionValues(ctx, pgtype.UUID{Bytes: collectionID, Valid: true})
}

// buildCollectionValue projects the typed-value row to the API shape.
// Pulled out for re-use by both the read path (list) and the write
// path (the upsert returns the new value).
func buildCollectionValue(
	fieldID pgtype.UUID, code, label, fieldType string,
	text *string, num *float64, date pgtype.Timestamptz,
	options []string, refUUID pgtype.UUID,
	setBy string, setAt pgtype.Timestamptz, setByUserRef *int64,
) openapi.CollectionFieldValue {
	v := openapi.CollectionFieldValue{
		FieldId:      openapi_types.UUID(fieldID.Bytes),
		FieldCode:    code,
		Type:         openapi.CollectionFieldValueType(fieldType),
		SetBy:        openapi.CollectionFieldValueSetBy(setBy),
		SetAt:        setAt.Time,
		SetByUserRef: setByUserRef,
		ValueText:    text,
	}
	if label != "" {
		v.FieldLabel = &label
	}
	if num != nil {
		// OpenAPI ships float32 by default — narrow the DB float64
		// to match (same pattern buildAssetValue uses).
		n := float32(*num)
		v.ValueNum = &n
	}
	if date.Valid {
		t := date.Time
		v.ValueDate = &t
	}
	if len(options) > 0 {
		opts := options
		v.ValueOptions = &opts
	}
	if refUUID.Valid {
		u := openapi_types.UUID(refUUID.Bytes)
		v.ValueRef = &u
	}
	return v
}

// buildCollectionUpsertParams maps the API write shape to the sqlc
// params. Mirrors buildUpsertParams in handler.go — kept separate
// because the openapi types are different.
func buildCollectionUpsertParams(
	collectionID, fieldID pgtype.UUID,
	fieldType string,
	body *openapi.CollectionFieldValueWrite,
	setBy string,
	userRef *int64,
) UpsertCollectionFieldValueParams {
	p := UpsertCollectionFieldValueParams{
		CollectionID: collectionID,
		FieldID:      fieldID,
		SetBy:        setBy,
		SetByUserRef: userRef,
	}
	switch fieldType {
	case "text", "longtext", "rich_text":
		p.ValueText = body.ValueText
	case "number":
		if body.ValueNum != nil {
			n := float64(*body.ValueNum)
			p.ValueNum = &n
		}
	case "boolean":
		// Booleans store in value_text as the literal "true"/"false"
		// — same encoding the asset path uses (see handler.go
		// buildUpsertParams). Keeps the storage row count stable
		// regardless of whether the operator picks a bool or text
		// field for a yes/no question.
		if body.ValueText != nil {
			p.ValueText = body.ValueText
		}
	case "date", "datetime":
		if body.ValueDate != nil {
			p.ValueDate = pgtype.Timestamptz{Time: *body.ValueDate, Valid: true}
		}
	case "select":
		p.ValueText = body.ValueText
	case "multi_select", "tree":
		if body.ValueOptions != nil {
			p.ValueOptions = *body.ValueOptions
		}
	case "reference":
		if body.ValueRef != nil {
			p.ValueRef = pgtype.UUID{Bytes: uuid.UUID(*body.ValueRef), Valid: true}
		}
	}
	return p
}

// validateCollectionValueType enforces "the value_* the caller
// supplied matches the field's declared type". Mirrors the asset
// side's value validation.
func validateCollectionValueType(fieldType string, body *openapi.CollectionFieldValueWrite) error {
	switch fieldType {
	case "text", "longtext", "rich_text", "select":
		if body.ValueText == nil {
			return fmt.Errorf("value_text required for field type %q", fieldType)
		}
	case "number":
		if body.ValueNum == nil {
			return fmt.Errorf("value_num required for field type %q", fieldType)
		}
	case "boolean":
		if body.ValueText == nil {
			return fmt.Errorf("value_text (\"true\"/\"false\") required for field type %q", fieldType)
		}
	case "date", "datetime":
		if body.ValueDate == nil {
			return fmt.Errorf("value_date required for field type %q", fieldType)
		}
	case "multi_select", "tree":
		if body.ValueOptions == nil {
			return fmt.Errorf("value_options required for field type %q", fieldType)
		}
	case "reference":
		if body.ValueRef == nil {
			return fmt.Errorf("value_ref required for field type %q", fieldType)
		}
	}
	return nil
}

// canReadField checks the caller's identity against the field's
// read_capability. Empty/nil capability = no gate.
func canReadField(_ context.Context, h *Handler, fieldUUID openapi_types.UUID, id *auth.Identity) bool {
	pgID := pgtype.UUID{Bytes: uuid.UUID(fieldUUID), Valid: true}
	row, err := h.getFieldByIDCached(context.Background(), pgID)
	if err != nil {
		// Field gone — drop conservatively rather than leak.
		return false
	}
	if row.ReadCapability == nil || *row.ReadCapability == "" {
		return true
	}
	return id.Can(*row.ReadCapability)
}

// ---------------------------------------------------------------------------
// Cross-package gate helpers for collections.MetadataGate (Phase 1.9.B).
//
// These are the raw building blocks; an adapter in app/internal/http
// converts the metadata-local input shape to the collections-package
// interface shape so neither package has to know about the other's
// types directly. Keeps metadata a leaf.
// ---------------------------------------------------------------------------

// SeedCollectionFieldValueInTx writes one value inside the caller's
// transaction. Used by collections.Create to seed the initial values
// supplied in the create body — same tx as the collection INSERT so
// a write failure rolls back together.
//
// Type validation: enforced via the field_definition.type lookup.
// Capability validation is the caller's responsibility.
// 422-class errors are returned as plain Go errors; the calling
// package decides how to surface them. Required-field validation
// is the caller's job (already happened pre-tx).
//
// History is NOT written here — the create flow's collection
// activity emission is sufficient audit; per-value history rows
// land when operators edit via PUT /collections/{id}/fields/{field_id}.
// Keeps the create path lean.
func (h *Handler) SeedCollectionFieldValueInTx(
	ctx context.Context,
	tx pgx.Tx,
	collectionID, fieldID uuid.UUID,
	valueText *string,
	valueNum *float64,
	valueDate *time.Time,
	valueOptions []string,
	valueRef *uuid.UUID,
	callerRef int64,
) error {
	pgField := pgtype.UUID{Bytes: fieldID, Valid: true}
	fieldRow, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		return fmt.Errorf("metadata: lookup field for seed: %w", err)
	}
	if fieldRow.SubjectKind != string(SubjectCollection) {
		return fmt.Errorf("field %q is not a collection field", fieldRow.Code)
	}

	params := UpsertCollectionFieldValueParams{
		CollectionID: pgtype.UUID{Bytes: collectionID, Valid: true},
		FieldID:      pgField,
		SetBy:        "manual",
		SetByUserRef: &callerRef,
	}
	switch fieldRow.Type {
	case "text", "longtext", "rich_text", "select", "boolean":
		params.ValueText = valueText
	case "number":
		params.ValueNum = valueNum
	case "date", "datetime":
		if valueDate != nil {
			params.ValueDate = pgtype.Timestamptz{Time: *valueDate, Valid: true}
		}
	case "multi_select", "tree":
		params.ValueOptions = valueOptions
	case "reference":
		if valueRef != nil {
			params.ValueRef = pgtype.UUID{Bytes: *valueRef, Valid: true}
		}
	}

	if _, err := New(tx).UpsertCollectionFieldValue(ctx, params); err != nil {
		return fmt.Errorf("metadata: upsert collection field value: %w", err)
	}
	return nil
}

// ListRequiredCollectionFieldsRaw returns the (id, code, label, type)
// tuples for every active collection-scoped field where required=TRUE.
// Adapter in app/internal/http maps the result into
// collections.RequiredField.
func (h *Handler) ListRequiredCollectionFieldsRaw(ctx context.Context) ([]ListRequiredCollectionFieldsRow, error) {
	return New(h.Pool).ListRequiredCollectionFields(ctx)
}
