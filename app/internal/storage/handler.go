// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// PinSubjectTypeUser is the canonical pin subject for "this upload
// belongs to the uploading user". The asset entity handler swaps this
// out for `asset:<uuid>` once an asset is created on top of the bytes.
const PinSubjectTypeUser = "user"

// Handler implements the storage byte-plane slice of
// openapi.StrictServerInterface:
//
//	POST /storage/objects
//	GET  /storage/objects/{hash}
//	GET  /storage/objects/{hash}/variants/{variant}
//
// Asset-entity endpoints (`/assets`) live in the assets package and
// reach the storage layer through Service, not through this handler.
type Handler struct {
	Service *Service
	Logger  *slog.Logger
}

// NewHandler binds the HTTP handler to a storage Service.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{Service: svc, Logger: logger}
}

// UploadStorageObject implements POST /storage/objects.
func (h *Handler) UploadStorageObject(
	ctx context.Context,
	req openapi.UploadStorageObjectRequestObject,
) (openapi.UploadStorageObjectResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UploadStorageObject401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UploadStorageObject500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "missing request body"},
		}, nil
	}

	contentType := "application/octet-stream"
	if req.Params.XContentType != nil && *req.Params.XContentType != "" {
		contentType = *req.Params.XContentType
	}

	pin := PinRef{
		SubjectType: PinSubjectTypeUser,
		SubjectID:   strconv.FormatInt(id.UserRef, 10),
	}

	result, err := h.Service.UploadOriginal(ctx, req.Body, contentType, pin)
	if err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelError, "storage.upload.error",
			slog.Int64("user_ref", id.UserRef),
			slog.String("err", err.Error()),
		)
		return openapi.UploadStorageObject500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "could not store upload"},
		}, nil
	}

	return openapi.UploadStorageObject201JSONResponse(openapi.UploadResult{
		Hash:           result.Hash,
		Size:           result.Size,
		ContentType:    result.ContentType,
		Deduped:        result.Deduped,
		PinSubjectType: result.Pin.SubjectType,
		PinSubjectId:   result.Pin.SubjectID,
	}), nil
}

// DownloadStorageObject implements GET /storage/objects/{hash}.
func (h *Handler) DownloadStorageObject(
	ctx context.Context,
	req openapi.DownloadStorageObjectRequestObject,
) (openapi.DownloadStorageObjectResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DownloadStorageObject401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	return h.download(ctx, req.Hash, VariantOriginal, headerVal(req.Params.Range), storageOriginalResponder{})
}

// DownloadStorageObjectVariant implements
// GET /storage/objects/{hash}/variants/{variant}.
func (h *Handler) DownloadStorageObjectVariant(
	ctx context.Context,
	req openapi.DownloadStorageObjectVariantRequestObject,
) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DownloadStorageObjectVariant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	resp, err := h.download(ctx, req.Hash, req.Variant, headerVal(req.Params.Range), storageVariantResponder{})
	if err != nil || resp == nil {
		return nil, err
	}
	return resp.(openapi.DownloadStorageObjectVariantResponseObject), nil
}

// --- shared download path -------------------------------------------------

// downloadResponder bridges the shared download logic to the
// per-operation response types codegen wants.
type downloadResponder interface {
	full(body io.Reader, length int64) any
	partial(body io.Reader, length int64) any
	notFound() any
}

type storageOriginalResponder struct{}

func (storageOriginalResponder) full(body io.Reader, length int64) any {
	return openapi.DownloadStorageObject200ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (storageOriginalResponder) partial(body io.Reader, length int64) any {
	return openapi.DownloadStorageObject206ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (storageOriginalResponder) notFound() any {
	return openapi.DownloadStorageObject404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "object not found"},
	}
}

type storageVariantResponder struct{}

func (storageVariantResponder) full(body io.Reader, length int64) any {
	return openapi.DownloadStorageObjectVariant200ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (storageVariantResponder) partial(body io.Reader, length int64) any {
	return openapi.DownloadStorageObjectVariant206ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (storageVariantResponder) notFound() any {
	return openapi.DownloadStorageObjectVariant404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "object not found"},
	}
}

func (h *Handler) download(
	ctx context.Context,
	hash, variant, rangeHeader string,
	resp downloadResponder,
) (openapi.DownloadStorageObjectResponseObject, error) {
	if rangeHeader == "" {
		body, info, err := h.Service.Download(ctx, hash, variant)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return resp.notFound().(openapi.DownloadStorageObjectResponseObject), nil
			}
			return nil, err
		}
		return resp.full(body, info.Size).(openapi.DownloadStorageObjectResponseObject), nil
	}

	offset, length, ok := parseSingleRange(rangeHeader)
	if !ok {
		body, info, err := h.Service.Download(ctx, hash, variant)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return resp.notFound().(openapi.DownloadStorageObjectResponseObject), nil
			}
			return nil, err
		}
		return resp.full(body, info.Size).(openapi.DownloadStorageObjectResponseObject), nil
	}

	body, err := h.Service.DownloadRange(ctx, hash, variant, offset, length)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return resp.notFound().(openapi.DownloadStorageObjectResponseObject), nil
		}
		return nil, err
	}
	return resp.partial(body, length).(openapi.DownloadStorageObjectResponseObject), nil
}

// --- helpers ---------------------------------------------------------------

func headerVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// parseSingleRange parses "bytes=<off>-<end>" or "bytes=<off>-" into
// (offset, length, ok). Returns ok=false for any form we don't
// understand (multi-range, suffix-range), letting the caller fall
// back to a 200 full-body response.
func parseSingleRange(h string) (offset, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0, 0, false
	}
	startStr, endStr := spec[:dash], spec[dash+1:]
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if endStr == "" {
		return start, 0, true
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end - start + 1, true
}

// Compile-time assertion: catches openapi-codegen signature drift.
var _ interface {
	UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error)
	DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error)
	DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error)
} = (*Handler)(nil)
