// Package metadata implements the admin-extensible metadata layer
// described in ADR 0012:
//
//   - GET / POST / PATCH / DELETE on /fields          — schema admin
//   - GET on /assets/{id}/fields                       — read all values
//   - PUT / DELETE on /assets/{id}/fields/{field_id}   — write values
//   - GET on /assets/{id}/fields/{field_id}/history    — audit trail
//
// Storage rides on three tables: field_definition (the schema),
// asset_field_value (typed values, one column per primitive),
// asset_field_value_history (append-only audit). The search_text
// TSVECTOR on assets is maintained by Postgres triggers so search
// stays consistent even when PHP writes during the transition.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Cache domain names. Stable strings used as NOTIFY targets — peer
// instances key off these when dispatching invalidations.
const (
	cacheDomainFieldByID = "field_definition.id"
)

// codePattern matches the admin-supplied field code (federation slug).
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Capability gates for the field-management surface. Field-VALUE
// operations require asset write access, which today reduces to
// "authenticated"; per-asset ACL lands in a later phase.
const (
	CapFieldsAdmin = "fields.admin"
	CapSystemAdmin = "system.admin"
)

// Handler implements the metadata slice of openapi.StrictServerInterface.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// fieldsByID caches field_definition rows by UUID string. Reads
	// on the hot path (SetAssetFieldValue, GetField) go through it;
	// writes (Create/Update/Archive) call Invalidate which drops the
	// local copy AND broadcasts to peer instances via the cache
	// Registry's NOTIFY channel. Nil-safe: a Handler built without
	// a registry skips the cache and reads always go to the DB.
	fieldsByID *cache.Cache[FieldDefinition]

	// registry is held for cross-domain NOTIFYs — specifically the
	// metadata.extraction_config domain that the asset/metadata
	// extraction package owns. SetFieldExtraction emits on that
	// domain after a successful write so the asset/metadata cache
	// picks up the new wiring on the next extract job. Nil-safe.
	registry *cache.Registry

	// collectionValues caches the full collection_field_value list
	// per collection (Phase 1.9.B). Per-collection eviction on
	// upsert/delete; cross-instance NOTIFY via the same Registry.
	// Capability filtering runs on the way out so a single cache
	// entry serves every caller.
	collectionValues *cache.Cache[collectionValueCacheEntry]
}

// NewHandler binds the metadata handler to the DB pool and the
// shared cache registry. Passing a nil registry is legal — the
// handler falls back to direct DB reads (useful in tests that
// don't want to spin up the LISTEN goroutine).
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger, registry: registry}
	if registry != nil {
		// 5000 entries comfortably covers thousand-field installs
		// while staying ~1MB of resident memory. The LRU evicts
		// cold field defs without losing them — next read repopulates.
		h.fieldsByID = cache.Register[FieldDefinition](registry, cacheDomainFieldByID, 5000)
	}
	h.setCollectionValueCache(registry)
	return h
}

// getFieldByIDCached resolves a field_definition row, hitting the
// LRU on a warm cache and falling back to a DB read + populate on
// miss. Returns the same shape sqlc emits so callers don't care
// whether they got a cached copy.
func (h *Handler) getFieldByIDCached(ctx context.Context, id pgtype.UUID) (FieldDefinition, error) {
	idStr := uuidString(id)
	if h.fieldsByID != nil {
		if v, ok := h.fieldsByID.Get(idStr); ok {
			return v, nil
		}
	}
	row, err := New(h.Pool).GetFieldDefinitionByID(ctx, id)
	if err != nil {
		return row, err
	}
	if h.fieldsByID != nil {
		h.fieldsByID.Add(idStr, row)
	}
	return row, nil
}

// invalidateField drops the local LRU entry and broadcasts to
// peers. Best-effort — a NOTIFY failure logs but doesn't propagate
// so writers don't fail because of cache plumbing.
func (h *Handler) invalidateField(ctx context.Context, id pgtype.UUID) {
	if h.fieldsByID == nil {
		return
	}
	if err := h.fieldsByID.Invalidate(ctx, uuidString(id)); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "metadata.cache.invalidate.error",
			slog.String("domain", cacheDomainFieldByID),
			slog.String("key", uuidString(id)),
			slog.String("err", err.Error()),
		)
	}
}

// ---------------------------------------------------------------------------
// ListFields
// ---------------------------------------------------------------------------

func (h *Handler) ListFields(
	ctx context.Context,
	req openapi.ListFieldsRequestObject,
) (openapi.ListFieldsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListFields401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)

	if req.Params.AssetType != nil {
		rows, err := q.ListFieldDefinitionsForAssetType(ctx, *req.Params.AssetType)
		if err != nil {
			return nil, fmt.Errorf("metadata: list by rt: %w", err)
		}
		out := make([]openapi.FieldDefinition, 0, len(rows))
		for _, r := range rows {
			out = append(out, fieldDefToAPI(r))
		}
		return openapi.ListFields200JSONResponse(out), nil
	}

	var statusFilter *string
	if req.Params.Status != nil {
		s := string(*req.Params.Status)
		statusFilter = &s
	}
	var subjectFilter *string
	if req.Params.SubjectKind != nil {
		s := string(*req.Params.SubjectKind)
		subjectFilter = &s
	}
	rows, err := q.ListFieldDefinitions(ctx, ListFieldDefinitionsParams{
		Status:      statusFilter,
		SubjectKind: subjectFilter,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: list: %w", err)
	}
	out := make([]openapi.FieldDefinition, 0, len(rows))
	for _, r := range rows {
		out = append(out, fieldDefToAPI(r))
	}
	return openapi.ListFields200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// CreateField
// ---------------------------------------------------------------------------

func (h *Handler) CreateField(
	ctx context.Context,
	req openapi.CreateFieldRequestObject,
) (openapi.CreateFieldResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateField401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.CreateField403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateField400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	code := strings.TrimSpace(in.Code)
	if !codePattern.MatchString(code) {
		return openapi.CreateField400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "code must match ^[a-z][a-z0-9_]*$"},
		}, nil
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return openapi.CreateField400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "label is required"},
		}, nil
	}
	if !validFieldType(string(in.Type)) {
		return openapi.CreateField400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "unknown field type"},
		}, nil
	}

	optsJSON, err := encodeJSON(in.Options, "{}")
	if err != nil {
		return nil, err
	}
	srcJSON, err := encodeJSONOptional(in.Source)
	if err != nil {
		return nil, err
	}

	// subject_kind discriminator (Phase 1.9.B). Defaults to asset for
	// callers that don't supply one — preserves the pre-1.9.B
	// "everything is an asset field" semantics.
	subject := SubjectAsset
	if in.SubjectKind != nil {
		parsed, err := ParseSubjectKind(string(*in.SubjectKind))
		if err != nil {
			return openapi.CreateField400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		subject = parsed
	}

	q := New(h.Pool)
	row, err := q.CreateFieldDefinition(ctx, CreateFieldDefinitionParams{
		Code:             code,
		Label:            label,
		Description:      strOr(in.Description, ""),
		Type:             string(in.Type),
		Options:          optsJSON,
		Required:         boolOr(in.Required, false),
		Searchable:       boolOr(in.Searchable, true),
		AppliesTo:        int64SliceOr(in.AppliesTo, []int64{}),
		FieldSetID:       uuidFromOpenAPIPtr(in.FieldSetId),
		ReadCapability:   in.ReadCapability,
		WriteCapability:  in.WriteCapability,
		DisplayOrder:     int32Or(in.DisplayOrder, 100),
		DisplayGroup:     strOr(in.DisplayGroup, "general"),
		Source:           srcJSON,
		Status:           "active",
		CreatedByUserRef: &id.UserRef,
		SubjectKind:      string(subject),
	})
	if err != nil {
		// Most likely a duplicate code violating the UNIQUE
		// constraint. Surface that as a friendly 400.
		if strings.Contains(err.Error(), "field_definition_code_key") {
			return openapi.CreateField400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "field code already exists"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: create: %w", err)
	}
	h.invalidateField(ctx, row.ID)
	return openapi.CreateField201JSONResponse(fieldDefToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// GetField
// ---------------------------------------------------------------------------

func (h *Handler) GetField(
	ctx context.Context,
	req openapi.GetFieldRequestObject,
) (openapi.GetFieldResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetField401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.getFieldByIDCached(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetField404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.GetField200JSONResponse(fieldDefToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// UpdateField
// ---------------------------------------------------------------------------

func (h *Handler) UpdateField(
	ctx context.Context,
	req openapi.UpdateFieldRequestObject,
) (openapi.UpdateFieldResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UpdateField401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.UpdateField403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateField400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	in := req.Body
	params := UpdateFieldDefinitionParams{
		ID:                pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		Label:             in.Label,
		Description:       in.Description,
		Required:          in.Required,
		Searchable:        in.Searchable,
		AppliesTo:         appliesToOrNil(in.AppliesTo),
		FieldSetID:        uuidFromOpenAPIPtr(in.FieldSetId),
		ReadCapability:    in.ReadCapability,
		WriteCapability:   in.WriteCapability,
		DisplayOrder:      int32PtrOpt(in.DisplayOrder),
		DisplayGroup:      in.DisplayGroup,
		DeprecatedReplacementID: uuidFromOpenAPIPtr(in.DeprecatedReplacementId),
		UpdatedByUserRef:  &id.UserRef,
	}
	if in.Status != nil {
		s := string(*in.Status)
		params.Status = &s
	}
	if in.Options != nil {
		b, err := json.Marshal(*in.Options)
		if err != nil {
			return nil, err
		}
		params.Options = b
	}
	if in.Source != nil {
		b, err := json.Marshal(*in.Source)
		if err != nil {
			return nil, err
		}
		params.Source = b
	}

	q := New(h.Pool)
	row, err := q.UpdateFieldDefinition(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateField404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: update: %w", err)
	}
	h.invalidateField(ctx, row.ID)
	return openapi.UpdateField200JSONResponse(fieldDefToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// SetFieldExtraction
// ---------------------------------------------------------------------------

// SetFieldExtraction wires (or unwires) the metadata-extraction
// pipeline against one field definition. Phase 1.18.A-2 follow-up B.
//
// source == "" clears extraction; mode is normalised to
// "skip_if_set" when empty so the DB constraint + the Applier's
// fall-through default stay aligned.
//
// On success: invalidates the field-by-id cache (so the next
// GetField reflects the new wiring) AND emits on the
// metadata.extraction_config domain so the asset/metadata
// extraction package's cache rebuilds on the next job.
func (h *Handler) SetFieldExtraction(
	ctx context.Context,
	req openapi.SetFieldExtractionRequestObject,
) (openapi.SetFieldExtractionResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetFieldExtraction401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.SetFieldExtraction403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetFieldExtraction400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	source := strings.TrimSpace(req.Body.Source)
	mode := ""
	if req.Body.Mode != nil {
		mode = strings.TrimSpace(string(*req.Body.Mode))
	}
	// DB CHECK requires extraction_mode IN the four-value enum even
	// when source is empty (unwired). Normalise empty to skip_if_set
	// so the row's default stays valid through clear/re-wire cycles.
	if mode == "" {
		mode = "skip_if_set"
	}
	if !validExtractionMode(mode) {
		return openapi.SetFieldExtraction400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "mode must be one of skip_if_set, replace, append, prepend"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if _, err := q.GetFieldDefinitionByID(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetFieldExtraction404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, err
	}
	row, err := q.SetFieldExtractionConfig(ctx, SetFieldExtractionConfigParams{
		ID:               pgID,
		ExtractionSource: source,
		ExtractionMode:   mode,
		UpdatedByUserRef: &id.UserRef,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: set extraction: %w", err)
	}
	h.invalidateField(ctx, row.ID)
	if h.registry != nil {
		// Best-effort: peers (and our own LISTEN goroutine) drop
		// the cached extraction-config list so the next extract
		// job rebuilds.
		if err := h.registry.Emit(ctx, "metadata.extraction_config", "all"); err != nil && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn,
				"metadata.cache.extraction_config.emit_error",
				slog.String("err", err.Error()),
			)
		}
	}
	return openapi.SetFieldExtraction200JSONResponse(fieldDefToAPI(row)), nil
}

func validExtractionMode(m string) bool {
	switch m {
	case "skip_if_set", "replace", "append", "prepend":
		return true
	}
	return false
}

// apiExtractionMode converts the DB string into the typed openapi
// pointer-enum. Empty string maps to a non-nil pointer with the
// empty value so the JSON shape is stable across "explicitly off"
// and "default unset".
func apiExtractionMode(s string) *openapi.FieldDefinitionExtractionMode {
	v := openapi.FieldDefinitionExtractionMode(s)
	return &v
}

// ---------------------------------------------------------------------------
// ArchiveField
// ---------------------------------------------------------------------------

func (h *Handler) ArchiveField(
	ctx context.Context,
	req openapi.ArchiveFieldRequestObject,
) (openapi.ArchiveFieldResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ArchiveField401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !canAdminFields(id) {
		return openapi.ArchiveField403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "field admin capability required"},
		}, nil
	}
	q := New(h.Pool)
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if _, err := q.GetFieldDefinitionByID(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ArchiveField404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, err
	}
	if err := q.ArchiveFieldDefinition(ctx, ArchiveFieldDefinitionParams{
		ID:                pgID,
		UpdatedByUserRef:  &id.UserRef,
	}); err != nil {
		return nil, fmt.Errorf("metadata: archive: %w", err)
	}
	h.invalidateField(ctx, pgID)
	return openapi.ArchiveField204Response{}, nil
}

// ---------------------------------------------------------------------------
// GetAssetFields
// ---------------------------------------------------------------------------

func (h *Handler) GetAssetFields(
	ctx context.Context,
	req openapi.GetAssetFieldsRequestObject,
) (openapi.GetAssetFieldsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetAssetFields401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListAssetFieldValues(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("metadata: list values: %w", err)
	}
	out := make([]openapi.AssetFieldValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, listAssetValueRowToAPI(r))
	}
	return openapi.GetAssetFields200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// SetAssetFieldValue
// ---------------------------------------------------------------------------

func (h *Handler) SetAssetFieldValue(
	ctx context.Context,
	req openapi.SetAssetFieldValueRequestObject,
) (openapi.SetAssetFieldValueResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetAssetFieldValue401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetAssetFieldValue400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgField := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}

	// Load the field definition so we can validate the incoming value
	// shape matches the declared type. The cached read avoids
	// hitting the DB for the same field on every asset edit.
	fieldRow, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetAssetFieldValue404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, err
	}
	if fieldRow.Status == "archived" {
		return openapi.SetAssetFieldValue400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "field is archived; values cannot be set"},
		}, nil
	}
	if fieldRow.WriteCapability != nil && *fieldRow.WriteCapability != "" {
		if !id.Can(*fieldRow.WriteCapability) {
			return openapi.SetAssetFieldValue403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability for this field: " + *fieldRow.WriteCapability},
			}, nil
		}
	}

	upsert, valErr := buildUpsertParams(pgAsset, pgField, fieldRow.Type, req.Body, &id.UserRef)
	if valErr != nil {
		return openapi.SetAssetFieldValue400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: valErr.Error()},
		}, nil
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("metadata: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	// Snapshot the old value (if any) for the history entry.
	prev, err := qTx.GetAssetFieldValue(ctx, GetAssetFieldValueParams{
		AssetID: pgAsset,
		FieldID: pgField,
	})
	hadOld := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("metadata: load previous: %w", err)
	}

	row, err := qTx.UpsertAssetFieldValue(ctx, upsert)
	if err != nil {
		return nil, fmt.Errorf("metadata: upsert: %w", err)
	}

	// Audit history.
	var oldJSON, newJSON []byte
	if hadOld {
		oldJSON, _ = valueRowToJSON(prev.ValueText, prev.ValueNum, prev.ValueDate, prev.ValueOptions, prev.ValueRef, fieldRow.Type)
	}
	newJSON, _ = valueRowToJSON(row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef, fieldRow.Type)
	if err := qTx.AppendAssetFieldValueHistory(ctx, AppendAssetFieldValueHistoryParams{
		AssetID:           pgAsset,
		FieldID:           pgField,
		OldValue:          oldJSON,
		NewValue:          newJSON,
		SetBy:             upsert.SetBy,
		ChangedByUserRef:  &id.UserRef,
	}); err != nil {
		return nil, fmt.Errorf("metadata: append history: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("metadata: commit: %w", err)
	}

	return openapi.SetAssetFieldValue200JSONResponse(
		buildAssetValue(row.FieldID, fieldRow.Code, fieldRow.Label, fieldRow.Type,
			row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef,
			row.SetBy, row.SetAt, row.SetByUserRef),
	), nil
}

// ---------------------------------------------------------------------------
// ClearAssetFieldValue
// ---------------------------------------------------------------------------

func (h *Handler) ClearAssetFieldValue(
	ctx context.Context,
	req openapi.ClearAssetFieldValueRequestObject,
) (openapi.ClearAssetFieldValueResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ClearAssetFieldValue401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgField := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("metadata: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	prev, err := qTx.GetAssetFieldValue(ctx, GetAssetFieldValueParams{
		AssetID: pgAsset,
		FieldID: pgField,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hadOld := err == nil

	if err := qTx.DeleteAssetFieldValue(ctx, DeleteAssetFieldValueParams{
		AssetID: pgAsset,
		FieldID: pgField,
	}); err != nil {
		return nil, fmt.Errorf("metadata: delete: %w", err)
	}

	if hadOld {
		oldJSON, _ := valueRowToJSON(prev.ValueText, prev.ValueNum, prev.ValueDate, prev.ValueOptions, prev.ValueRef, prev.Type)
		if err := qTx.AppendAssetFieldValueHistory(ctx, AppendAssetFieldValueHistoryParams{
			AssetID:          pgAsset,
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
	return openapi.ClearAssetFieldValue204Response{}, nil
}

// ---------------------------------------------------------------------------
// GetAssetFieldValueHistory
// ---------------------------------------------------------------------------

func (h *Handler) GetAssetFieldValueHistory(
	ctx context.Context,
	req openapi.GetAssetFieldValueHistoryRequestObject,
) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetAssetFieldValueHistory401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	limit := int32(50)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > 500 {
			l = 500
		}
		limit = int32(l)
	}
	q := New(h.Pool)
	fieldID := pgtype.UUID{Bytes: uuid.UUID(req.FieldId), Valid: true}
	rows, err := q.ListAssetFieldValueHistory(ctx, ListAssetFieldValueHistoryParams{
		AssetID:  pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		FieldID:  fieldID,
		RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: history: %w", err)
	}
	out := make([]openapi.FieldValueHistoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, historyRowToAPI(r))
	}
	return openapi.GetAssetFieldValueHistory200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// Compile-time assertion: catches openapi-codegen signature drift.
// ---------------------------------------------------------------------------

var _ interface {
	ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error)
	CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error)
	GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error)
	UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error)
	ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error)
	GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error)
	SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error)
	ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error)
	GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error)
} = (*Handler)(nil)

// uuidString returns the canonical text form of a pgtype.UUID. Used
// for cache keys and NOTIFY payloads so writes and reads agree on
// the exact string representation.
func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// ---------------------------------------------------------------------------
// Helpers — input parsing, validation, type-specific marshalling.
// ---------------------------------------------------------------------------

func canAdminFields(id *auth.Identity) bool {
	return id.Can(CapFieldsAdmin) || id.Can(CapSystemAdmin)
}

func validFieldType(t string) bool {
	switch t {
	case "text", "longtext", "rich_text",
		"number", "boolean",
		"date", "datetime",
		"select", "multi_select", "tree",
		"reference":
		return true
	}
	return false
}

// buildUpsertParams validates the incoming AssetFieldValueWrite
// against the field's declared type and returns ready-to-go sqlc
// params. Each field type maps to exactly one value_* column;
// everything else is forced to NULL so old values from a prior type
// don't leak through.
func buildUpsertParams(asset, field pgtype.UUID, fieldType string, in *openapi.AssetFieldValueWrite, userRef *int64) (UpsertAssetFieldValueParams, error) {
	p := UpsertAssetFieldValueParams{
		AssetID:        asset,
		FieldID:        field,
		SetBy:          "manual",
		SetByUserRef:   userRef,
	}
	if in.SetBy != nil {
		p.SetBy = string(*in.SetBy)
	}

	switch fieldType {
	case "text", "longtext", "rich_text", "select", "tree":
		if in.ValueText == nil {
			return p, fmt.Errorf("field type %q requires value_text", fieldType)
		}
		p.ValueText = in.ValueText
	case "number":
		if in.ValueNum == nil {
			return p, fmt.Errorf("field type %q requires value_num", fieldType)
		}
		v := float64(*in.ValueNum)
		p.ValueNum = &v
	case "boolean":
		if in.ValueNum == nil {
			return p, fmt.Errorf("boolean field requires value_num (0 or 1)")
		}
		v := float64(*in.ValueNum)
		if v != 0 && v != 1 {
			return p, fmt.Errorf("boolean field accepts 0 or 1 only")
		}
		p.ValueNum = &v
	case "date", "datetime":
		if in.ValueDate == nil {
			return p, fmt.Errorf("field type %q requires value_date", fieldType)
		}
		p.ValueDate = pgtype.Timestamptz{Time: *in.ValueDate, Valid: true}
	case "multi_select":
		if in.ValueOptions == nil || len(*in.ValueOptions) == 0 {
			return p, fmt.Errorf("multi_select field requires non-empty value_options")
		}
		p.ValueOptions = *in.ValueOptions
	case "reference":
		if in.ValueRef == nil {
			return p, fmt.Errorf("reference field requires value_ref")
		}
		ref := uuid.UUID(*in.ValueRef)
		p.ValueRef = pgtype.UUID{Bytes: ref, Valid: true}
	default:
		return p, fmt.Errorf("unknown field type %q", fieldType)
	}
	return p, nil
}

// valueRowToJSON encodes a typed value row into a JSONB blob suitable
// for the history table — the single shape both old and new values
// share regardless of underlying column.
func valueRowToJSON(
	text *string,
	num *float64,
	date pgtype.Timestamptz,
	options []string,
	ref pgtype.UUID,
	fieldType string,
) ([]byte, error) {
	v := map[string]any{"type": fieldType}
	switch {
	case text != nil:
		v["value"] = *text
	case num != nil:
		v["value"] = *num
	case date.Valid:
		v["value"] = date.Time.Format(time.RFC3339Nano)
	case options != nil:
		v["value"] = options
	case ref.Valid:
		v["value"] = uuid.UUID(ref.Bytes).String()
	default:
		v["value"] = nil
	}
	return json.Marshal(v)
}

// ---------------------------------------------------------------------------
// Row-to-API conversions
// ---------------------------------------------------------------------------

// fieldDefToAPI converts an sqlc-generated FieldDefinition model
// row into the OpenAPI response shape. The five field-def queries
// all return the same model, so one helper covers them.
func fieldDefToAPI(r FieldDefinition) openapi.FieldDefinition {
	def := openapi.FieldDefinition{
		Id:               openapi_types.UUID(r.ID.Bytes),
		Code:             r.Code,
		Label:            r.Label,
		Description:      &r.Description,
		Type:             openapi.FieldDefinitionType(r.Type),
		SubjectKind:      openapi.FieldDefinitionSubjectKind(r.SubjectKind),
		Required:         r.Required,
		Searchable:       r.Searchable,
		AppliesTo:        r.AppliesTo,
		ReadCapability:   r.ReadCapability,
		WriteCapability:  r.WriteCapability,
		DisplayOrder:     int(r.DisplayOrder),
		DisplayGroup:     r.DisplayGroup,
		Status:           openapi.FieldDefinitionStatus(r.Status),
		CreatedAt:        r.CreatedAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
		ExtractionSource: &r.ExtractionSource,
		ExtractionMode:   apiExtractionMode(r.ExtractionMode),
	}
	if r.FieldSetID.Valid {
		v := openapi_types.UUID(r.FieldSetID.Bytes)
		def.FieldSetId = &v
	}
	if r.DeprecatedReplacementID.Valid {
		v := openapi_types.UUID(r.DeprecatedReplacementID.Bytes)
		def.DeprecatedReplacementId = &v
	}
	if len(r.Options) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Options, &m); err == nil {
			def.Options = &m
		}
	}
	if len(r.Source) > 0 && string(r.Source) != "null" {
		var m map[string]any
		if err := json.Unmarshal(r.Source, &m); err == nil {
			def.Source = &m
		}
	}
	return def
}

// listAssetValueRowToAPI builds the API shape from the JOIN row that
// list-values returns (carries the field code/label/type alongside
// the value columns).
func listAssetValueRowToAPI(r ListAssetFieldValuesRow) openapi.AssetFieldValue {
	return buildAssetValue(
		r.FieldID, r.Code, r.Label, r.Type,
		r.ValueText, r.ValueNum, r.ValueDate, r.ValueOptions, r.ValueRef,
		r.SetBy, r.SetAt, r.SetByUserRef,
	)
}

// buildAssetValue is the single helper for assembling an
// openapi.AssetFieldValue from sqlc-side columns. Used by both the
// list path (joined rows) and the upsert path (where we have the
// field metadata loaded separately).
func buildAssetValue(
	fieldID pgtype.UUID,
	code, label, fieldType string,
	valueText *string,
	valueNum *float64,
	valueDate pgtype.Timestamptz,
	valueOptions []string,
	valueRef pgtype.UUID,
	setBy string,
	setAt pgtype.Timestamptz,
	setByUserRef *int64,
) openapi.AssetFieldValue {
	out := openapi.AssetFieldValue{
		FieldId:      openapi_types.UUID(fieldID.Bytes),
		FieldCode:    code,
		FieldLabel:   &label,
		Type:         openapi.AssetFieldValueType(fieldType),
		SetBy:        openapi.AssetFieldValueSetBy(setBy),
		SetAt:        setAt.Time,
		SetByUserRef: setByUserRef,
	}
	if valueText != nil {
		out.ValueText = valueText
	}
	if valueNum != nil {
		v := float32(*valueNum)
		out.ValueNum = &v
	}
	if valueDate.Valid {
		t := valueDate.Time
		out.ValueDate = &t
	}
	if len(valueOptions) > 0 {
		opts := valueOptions
		out.ValueOptions = &opts
	}
	if valueRef.Valid {
		ref := openapi_types.UUID(valueRef.Bytes)
		out.ValueRef = &ref
	}
	return out
}

func historyRowToAPI(r AssetFieldValueHistory) openapi.FieldValueHistoryEntry {
	e := openapi.FieldValueHistoryEntry{
		Id:                  openapi_types.UUID(r.ID.Bytes),
		AssetId:             openapi_types.UUID(r.AssetID.Bytes),
		FieldId:             openapi_types.UUID(r.FieldID.Bytes),
		ChangedAt:           r.ChangedAt.Time,
		ChangedByUserRef:    r.ChangedByUserRef,
		SetBy:               r.SetBy,
	}
	if len(r.OldValue) > 0 && string(r.OldValue) != "null" {
		var m map[string]any
		if err := json.Unmarshal(r.OldValue, &m); err == nil {
			e.OldValue = &m
		}
	}
	if len(r.NewValue) > 0 && string(r.NewValue) != "null" {
		var m map[string]any
		if err := json.Unmarshal(r.NewValue, &m); err == nil {
			e.NewValue = &m
		}
	}
	return e
}

// ---------------------------------------------------------------------------
// Small option-extraction helpers — keep the handler bodies tidy.
// ---------------------------------------------------------------------------

func encodeJSON(m *map[string]any, fallback string) ([]byte, error) {
	if m == nil || len(*m) == 0 {
		return []byte(fallback), nil
	}
	return json.Marshal(*m)
}

func encodeJSONOptional(m *map[string]any) ([]byte, error) {
	if m == nil || len(*m) == 0 {
		return nil, nil
	}
	return json.Marshal(*m)
}

func uuidFromOpenAPIPtr(u *openapi_types.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: uuid.UUID(*u), Valid: true}
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func int32Or(p *int, def int32) int32 {
	if p == nil {
		return def
	}
	return int32(*p)
}

func int32PtrOpt(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func int64SliceOr(p *[]int64, def []int64) []int64 {
	if p == nil {
		return def
	}
	return *p
}

// appliesToOrNil — for PATCH, returns nil when caller didn't send
// the array (= keep current), or the array itself when they did (=
// replace).
func appliesToOrNil(p *[]int64) []int64 {
	if p == nil {
		return nil
	}
	return *p
}

// urlSafe ensures we never accidentally interpolate user input into
// pg_notify payloads without escaping.
var _ = url.QueryEscape
