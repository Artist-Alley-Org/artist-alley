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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/richtext"
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
			FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
				Error:  fmt.Sprintf("field %q is not a collection field", fieldRow.Code),
				Reason: openapi.FieldNotForCollection,
				Field:  &field,
			},
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

	// READ-ONLY (#1173), the collection twin of the asset gate. The
	// asymmetry between the two is deliberate and lives elsewhere: a
	// collection's create body MAY seed an initial value, because
	// `CollectionCreate.field_values` is a real human first-write seam
	// that the asset side has no equivalent of. That seam is
	// collections.Create's, pre-transaction. THIS handler is every write
	// after it, and every write after it is refused.
	if msg := readOnlyRefusal(fieldRow, "set"); msg != "" {
		code := fieldRow.Code
		return openapi.SetCollectionFieldValue422JSONResponse{
			FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
				Error:  msg,
				Reason: openapi.FieldReadOnly,
				Field:  &code,
			},
		}, nil
	}

	// Validate the supplied value_* matches the field's type.
	if vErr := validateCollectionValueType(fieldRow.Type, req.Body); vErr != nil {
		field := fieldRow.Code
		return openapi.SetCollectionFieldValue422JSONResponse{
			FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
				Error:  vErr.Error(),
				Reason: openapi.ValueTypeMismatch,
				Field:  &field,
			},
		}, nil
	}

	// INPUT PATTERN (#1173). `req.Body.ValueText` is what
	// buildCollectionUpsertParams stores for the two supported types:
	// SanitizeValueText is a no-op outside `rich_text`, which does not
	// honour a pattern. Checked before the vocabulary gate below because
	// the two supported types have no vocabulary to consult.
	if msg := patternRefusal(fieldRow, req.Body.ValueText); msg != "" {
		code := fieldRow.Code
		return openapi.SetCollectionFieldValue422JSONResponse{
			FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
				Error:  msg,
				Reason: openapi.PatternMismatch,
				Field:  &code,
			},
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

	// Controlled-vocabulary gate (#824) + the accept-and-create branch
	// an open vocabulary takes (#830) — the SAME call the asset writer
	// makes, on the same helper, so a collection cannot accept a slug an
	// asset refuses, or refuse one an asset creates. Placed after the
	// snapshot because the lifecycle half of the rule needs the held
	// value, which prev already is.
	var held []string
	if hadOld {
		held = vocabularySlugs(fieldRow.Type, prev.ValueText, prev.ValueOptions)
	}
	var incomingOptions []string
	if req.Body.ValueOptions != nil {
		incomingOptions = *req.Body.ValueOptions
	}
	vocab, rej, err := openOrCheckVocabulary(ctx, qTx, fieldRow,
		vocabularySlugs(fieldRow.Type, req.Body.ValueText, incomingOptions), held,
		canExtendVocabulary(id))
	if err != nil {
		return nil, err
	}
	if rej != nil {
		return openapi.SetCollectionFieldValue422JSONResponse{
			FieldValueUnprocessableJSONResponse: rejectionBody(fieldRow.Code, rej),
		}, nil
	}
	// Canonical slugs, not the text a client sent — same reason the
	// asset path rewrites its upsert params. Here the params are built
	// from req.Body a line below, so the normalisation lands on the body.
	switch fieldRow.Type {
	case "multi_select":
		req.Body.ValueOptions = &vocab.Slugs
	case "select", "tree":
		// See SetAssetFieldValue for why the single-slug types are
		// written back too: alias and tombstone redirects move a value
		// on a closed vocabulary, and value_text is where a closed
		// vocabulary keeps it.
		if len(vocab.Slugs) == 1 {
			req.Body.ValueText = &vocab.Slugs[0]
		}
	}

	// Reference-existence gate (#842) — the collection sibling of the
	// asset path's gate, on the same GetReferencedAsset query so a
	// collection cannot store a dangling ref an asset would refuse. A
	// WRITE gate only: the read path tolerates a since-deleted target
	// and degrades to the bare id (the #839 interlock). See
	// SetAssetFieldValue for the full argument. The resolved target is
	// reused for the 200 body's resolved_reference (#840), so the write
	// response carries the same shape the list path does.
	var ref resolvedRef
	if fieldRow.Type == "reference" && req.Body.ValueRef != nil {
		refUUID := pgtype.UUID{Bytes: uuid.UUID(*req.Body.ValueRef), Valid: true}
		target, refErr := qTx.GetReferencedAsset(ctx, refUUID)
		if refErr != nil {
			if errors.Is(refErr, pgx.ErrNoRows) {
				field := fieldRow.Code
				return openapi.SetCollectionFieldValue422JSONResponse{
					FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
						Error:  fmt.Sprintf("%s: referenced asset %s does not exist", fieldRow.Code, uuid.UUID(*req.Body.ValueRef)),
						Reason: openapi.DanglingReference,
						Field:  &field,
					},
				}, nil
			}
			return nil, fmt.Errorf("metadata: verify reference target: %w", refErr)
		}
		ref = resolvedRef{ID: target.ID, Title: target.Title}
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
	if len(vocab.Created) > 0 {
		// The vocabulary grew; see the asset path for why this is after
		// the commit and why it drops the extraction cache too.
		h.InvalidateFieldVocabulary(ctx, pgField)
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelInfo, "metadata.vocabulary.terms_created",
				slog.String("field", fieldRow.Code),
				slog.Int("count", len(vocab.Created)),
				slog.String("terms", strings.Join(vocab.Created, ",")),
			)
		}
	}

	return openapi.SetCollectionFieldValue200JSONResponse(
		buildCollectionValue(row.FieldID, fieldRow.Code, fieldRow.Label, fieldRow.Type,
			row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef,
			row.SetBy, row.SetAt, row.SetByUserRef,
			// vocab.Options, not fieldRow.Options: on an open-vocabulary
			// write that just minted a term, the loaded field row predates
			// it and the response would resolve every term but the new one.
			// Same reason the asset path passes vocab.Options here.
			vocab.Options, ref),
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

	// READ-ONLY (#1173) refuses the clear as well as the set, for the
	// reason ClearAssetFieldValue gives. The field is loaded before the
	// transaction opens: nothing here needs the tx, and refusing without
	// opening one keeps the cheap answer cheap. A field that has since
	// been deleted falls through to the delete below, which is a no-op
	// answering 204, exactly as it did before.
	if fieldRow, fErr := h.getFieldByIDCached(ctx, pgField); fErr == nil {
		if msg := readOnlyRefusal(fieldRow, "cleared"); msg != "" {
			code := fieldRow.Code
			return openapi.ClearCollectionFieldValue422JSONResponse{
				FieldValueUnprocessableJSONResponse: openapi.FieldValueUnprocessableJSONResponse{
					Error:  msg,
					Reason: openapi.FieldReadOnly,
					Field:  &code,
				},
			}, nil
		}
	} else if !errors.Is(fErr, pgx.ErrNoRows) {
		return nil, fmt.Errorf("metadata: load field: %w", fErr)
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

	// code/label/type/options and the reference title now ride on the
	// row itself (#840 joins), so no per-row getFieldByIDCached and no
	// N+1 — the query resolves everything buildCollectionValue needs.
	// A value whose field definition was deleted is dropped by the
	// INNER JOIN to field_definition, which is the join's equivalent of
	// the "definition gone" skip this loop used to do by hand.
	values := make([]openapi.CollectionFieldValue, 0, len(rows))
	for _, r := range rows {
		values = append(values, buildCollectionValue(
			r.FieldID, r.Code, r.Label, r.Type,
			r.ValueText, r.ValueNum, r.ValueDate, r.ValueOptions, r.ValueRef,
			r.SetBy, r.SetAt, r.SetByUserRef,
			r.Options, resolvedRef{ID: r.RefAssetID, Title: r.RefAssetTitle},
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
//
// fieldOptions and ref are the #840 additions: the collection side now
// resolves select/tree slugs and reference targets exactly the way
// buildAssetValue does, so collection metadata renders labels and
// linked titles instead of raw slugs and bare UUIDs. Both callers
// already hold both — the list path off its joins, the write path off
// the loaded field def and the reference gate — so, as on the asset
// side, the resolution costs no extra query and no consumer can forget
// it. See buildAssetValue for the full argument (#775/#817).
func buildCollectionValue(
	fieldID pgtype.UUID, code, label, fieldType string,
	text *string, num *float64, date pgtype.Timestamptz,
	options []string, refUUID pgtype.UUID,
	setBy string, setAt pgtype.Timestamptz, setByUserRef *int64,
	fieldOptions []byte, ref resolvedRef,
) openapi.CollectionFieldValue {
	v := openapi.CollectionFieldValue{
		FieldId:      openapi_types.UUID(fieldID.Bytes),
		FieldCode:    code,
		Type:         openapi.CollectionFieldValueType(fieldType),
		SetBy:        openapi.CollectionFieldValueSetBy(setBy),
		SetAt:        setAt.Time,
		SetByUserRef: setByUserRef,
		// #816's read boundary — see buildAssetValue for the argument.
		// A collection value has the same writers-that-are-not-handlers
		// problem the asset side does.
		ValueText: richtext.SanitizeValueText(fieldType, text),
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
	// The same resolution the asset path does (#840), through the same
	// shared helper, so the two subject kinds cannot render a value
	// differently.
	if resolved := resolveValueOptions(fieldType, text, options, fieldOptions); len(resolved) > 0 {
		v.ResolvedOptions = &resolved
	}
	// Gate on fieldType as well as ref.ID — same narrowness as
	// buildAssetValue — so a stray value_ref on a non-reference field
	// cannot start emitting a resolved target. A target that did not
	// resolve (soft-deleted, or a since-deleted dangling ref) leaves
	// this absent and the client falls back to the bare id (#839).
	if fieldType == "reference" && ref.ID.Valid {
		v.ResolvedReference = &openapi.ResolvedReference{
			Id:    openapi_types.UUID(ref.ID.Bytes),
			Title: ref.Title,
		}
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
		// #816's write boundary, same one line the asset side runs in
		// buildUpsertParams. Two call sites, one implementation — the
		// two handlers cannot drift about what HTML is allowed because
		// neither of them decides it. See internal/richtext.
		p.ValueText = richtext.SanitizeValueText(fieldType, body.ValueText)
	case "number", "boolean":
		// `boolean` is 0/1 in value_num, exactly as ADR 0012 specifies
		// and exactly what handler.go's buildUpsertParams has always
		// written on the asset side. This path wrote the strings
		// "true"/"false" into value_text until #791, so a boolean set
		// on a collection landed in a different column from the same
		// field's value on an asset. The 0/1-only range is enforced by
		// validateCollectionValueType before this runs.
		if body.ValueNum != nil {
			n := float64(*body.ValueNum)
			p.ValueNum = &n
		}
	case "date", "datetime":
		if body.ValueDate != nil {
			p.ValueDate = pgtype.Timestamptz{Time: *body.ValueDate, Valid: true}
		}
	case "select", "tree":
		// `tree` stores ONE vocabulary slug in value_text, exactly like
		// `select` — see the 2026-07-31 tree-storage amendment to ADR
		// 0012. It used to sit with multi_select and write
		// value_options, which put a collection's tree value in a
		// different column from an asset's. Nothing caught it because
		// no tree field has ever carried a value.
		p.ValueText = body.ValueText
	case "multi_select":
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
	case "text", "longtext", "rich_text", "select", "tree":
		if body.ValueText == nil {
			return fmt.Errorf("value_text required for field type %q", fieldType)
		}
	case "number":
		if body.ValueNum == nil {
			return fmt.Errorf("value_num required for field type %q", fieldType)
		}
	case "boolean":
		// Same contract as the asset side's buildUpsertParams: 0/1 in
		// value_num, and nothing else. The range check lives here
		// because buildCollectionUpsertParams cannot fail — the
		// collection write path validates first and then marshals.
		if body.ValueNum == nil {
			return fmt.Errorf("value_num (0 or 1) required for field type %q", fieldType)
		}
		if v := float64(*body.ValueNum); v != 0 && v != 1 {
			return fmt.Errorf("boolean field accepts 0 or 1 only")
		}
	case "date", "datetime":
		if body.ValueDate == nil {
			return fmt.Errorf("value_date required for field type %q", fieldType)
		}
	case "multi_select":
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
	case "text", "longtext", "rich_text", "select", "tree":
		// Seeds bypass the HTTP handler, so they get the write-side
		// sanitise explicitly (#816). The read side would cover them
		// anyway; this keeps the stored row clean too.
		params.ValueText = richtext.SanitizeValueText(fieldRow.Type, valueText)
	case "number", "boolean":
		// `boolean` moved out of the value_text group in #791 — see
		// buildCollectionUpsertParams. A seeded collection boolean is
		// 0/1 in value_num like every other writer's.
		params.ValueNum = valueNum
	case "date", "datetime":
		if valueDate != nil {
			params.ValueDate = pgtype.Timestamptz{Time: *valueDate, Valid: true}
		}
	case "multi_select":
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

// CollectionSeedValueProbe is one value from a collection-create body,
// as the pre-transaction gate needs to see it.
//
// Only `ValueText` travels, because it is the only member an input
// pattern can be written about: `regexp_filter` is honoured for `text`
// and `longtext`, and both store there. Widening the pattern to another
// type would widen this struct with it.
type CollectionSeedValueProbe struct {
	FieldID   uuid.UUID
	ValueText *string
}

// SeedValueRefusal names the field that refused a seeded value and the
// sentence to refuse it with.
type SeedValueRefusal struct {
	Code    string
	Label   string
	Message string
}

// ValidateCollectionSeedValues checks the values a collection-create
// body proposes, BEFORE anything is written.
//
// It runs pre-transaction for the reason the required-field gate does:
// a refusal must not leave a half-created collection behind. That is
// also why the check cannot live inside SeedCollectionFieldValueInTx,
// which by then is already inside the caller's transaction and one
// statement away from the collection row.
//
// `read_only` is deliberately NOT checked here. The create body is the
// collection side's human first-write seam, and seeding an initial
// value is the one write a read-only collection field permits: every
// later set and clear is refused by the handlers above. The pattern
// still applies to that first value, because it is a person's input
// like any other.
//
// Probes are checked in the order given, so the refusal an operator
// sees is stable across identical requests.
func (h *Handler) ValidateCollectionSeedValues(
	ctx context.Context,
	probes []CollectionSeedValueProbe,
) (*SeedValueRefusal, error) {
	for _, p := range probes {
		fieldRow, err := h.getFieldByIDCached(ctx, pgtype.UUID{Bytes: p.FieldID, Valid: true})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Not this gate's refusal to make. An unknown field id
				// fails inside the create transaction with the message
				// that path already produces.
				continue
			}
			return nil, fmt.Errorf("metadata: seed value gate: %w", err)
		}
		if msg := patternRefusal(fieldRow, p.ValueText); msg != "" {
			return &SeedValueRefusal{
				Code:    fieldRow.Code,
				Label:   fieldRow.Label,
				Message: msg,
			}, nil
		}
	}
	return nil, nil
}

// ListRequiredCollectionFieldsRaw returns the (id, code, label, type)
// tuples for every active collection-scoped field where required=TRUE.
// Adapter in app/internal/http maps the result into
// collections.RequiredField.
func (h *Handler) ListRequiredCollectionFieldsRaw(ctx context.Context) ([]ListRequiredCollectionFieldsRow, error) {
	return New(h.Pool).ListRequiredCollectionFields(ctx)
}
