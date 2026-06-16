// Phase 1.18.B-3 HTTP surface.
//
// Three endpoints under /assets/{id}/subtitle-tracks:
//
//   GET    /assets/{id}/subtitle-tracks         — list
//   POST   /assets/{id}/subtitle-tracks         — upload + convert + insert
//   DELETE /assets/{id}/subtitle-tracks/{lang}  — remove one
//
// The upload endpoint runs conversion synchronously (text formats
// are sub-millisecond) and inserts the row + returns 202 + the
// resulting track in one round-trip. Async-via-job is overhead
// without payoff for the current converters; if IDX/bitmap formats
// ever ship via the capability add-on (per ADR 0034) they'll route
// through the existing jobs queue and the 202 becomes a true
// "queued" response.

package subtitles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// StorageWriter is the subset of storage.Service the upload
// handler needs. Tests stub it; production wires *storage.Service.
//
// The pin tuple ensures storage GC doesn't reap the VTT bytes
// while the asset_subtitle_tracks row references them; passing
// (asset_id, lang) as the pin keys the bytes to their consumer.
type StorageWriter interface {
	PutBytes(ctx context.Context, b []byte, contentType, pinSubjectType, pinSubjectID string) (string, error)
}

// HTTPHandler adapts subtitles.Handler to the OpenAPI strict-server
// shim contract. Kept separate from Handler so the package's
// non-HTTP consumers (federation pass-through, admin tooling)
// don't drag the openapi import.
type HTTPHandler struct {
	domain  *Handler
	storage StorageWriter
	logger  *slog.Logger
}

// NewHTTPHandler wires the HTTP adapter. domain is required;
// storage may be nil if the build path doesn't ship the upload
// endpoint (test fixtures); when nil, upload returns 503.
func NewHTTPHandler(domain *Handler, storageWriter StorageWriter, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{domain: domain, storage: storageWriter, logger: logger}
}

// ListSubtitleTracks — GET /assets/{id}/subtitle-tracks.
//
// Surfaces the policy gate as 422 (not applicable for this asset
// kind). Auth required.
func (h *HTTPHandler) ListSubtitleTracks(
	ctx context.Context,
	req openapi.ListSubtitleTracksRequestObject,
) (openapi.ListSubtitleTracksResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListSubtitleTracks401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	assetID := uuid.UUID(req.Id)
	if err := RequiresAudioVideo(ctx, h.domain.queries, assetID); err != nil {
		return mapPolicyErrorToList(err), nil
	}
	tracks, err := h.domain.GetForAsset(ctx, assetID)
	if err != nil {
		return nil, err
	}
	out := make([]openapi.SubtitleTrack, len(tracks))
	for i, t := range tracks {
		out[i] = trackToAPI(t)
	}
	return openapi.ListSubtitleTracks200JSONResponse(out), nil
}

// UploadSubtitleTrack — POST /assets/{id}/subtitle-tracks.
//
// Synchronous convert + CAS-store + DB upsert. Returns 202 with
// the resulting track in the body.
func (h *HTTPHandler) UploadSubtitleTrack(
	ctx context.Context,
	req openapi.UploadSubtitleTrackRequestObject,
) (openapi.UploadSubtitleTrackResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.UploadSubtitleTrack401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UploadSubtitleTrack400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	if h.storage == nil {
		return nil, fmt.Errorf("subtitles HTTP: storage writer not wired")
	}
	assetID := uuid.UUID(req.Id)
	if err := RequiresAudioVideo(ctx, h.domain.queries, assetID); err != nil {
		return mapPolicyErrorToUpload(err), nil
	}
	if err := ValidateLang(req.Body.Lang); err != nil {
		return openapi.UploadSubtitleTrack400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	// Convert source bytes to WebVTT. Body's Content field is
	// the UTF-8 source (post-multipart decoding); pure-Go
	// converters give us deterministic output.
	srcBytes := []byte(req.Body.Content)
	conv, err := Convert(string(req.Body.SourceFormat), srcBytes)
	if err != nil {
		if errors.Is(err, ErrIDXUnsupported) {
			return openapi.UploadSubtitleTrack501Response{}, nil
		}
		if errors.Is(err, ErrPermanent) {
			return openapi.UploadSubtitleTrack400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		return nil, fmt.Errorf("subtitles convert: %w", err)
	}

	// Store the VTT in CAS. The hash is what lives on the track row.
	// Pin tuple = (asset_id, lang) so storage GC doesn't reap the
	// bytes while the track row points at them.
	hash, err := h.storage.PutBytes(
		ctx, conv.VTT,
		"text/vtt; charset=utf-8",
		"subtitle_track",
		assetID.String()+"-"+req.Body.Lang,
	)
	if err != nil {
		return nil, fmt.Errorf("subtitles store VTT: %w", err)
	}

	label := ""
	if req.Body.Label != nil {
		label = *req.Body.Label
	}
	track, err := h.domain.Upsert(ctx, Track{
		AssetID:      assetID,
		Lang:         req.Body.Lang,
		Label:        label,
		FileHash:     hash,
		SourceFormat: string(req.Body.SourceFormat),
		Confidence:   conv.Confidence,
	})
	if err != nil {
		return mapPolicyErrorToUpload(err), nil
	}
	return openapi.UploadSubtitleTrack202JSONResponse(trackToAPI(track)), nil
}

// DeleteSubtitleTrack — DELETE /assets/{id}/subtitle-tracks/{lang}.
func (h *HTTPHandler) DeleteSubtitleTrack(
	ctx context.Context,
	req openapi.DeleteSubtitleTrackRequestObject,
) (openapi.DeleteSubtitleTrackResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DeleteSubtitleTrack401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	assetID := uuid.UUID(req.Id)
	if err := h.domain.Delete(ctx, assetID, req.Lang); err != nil {
		return mapPolicyErrorToDelete(err), nil
	}
	return openapi.DeleteSubtitleTrack204Response{}, nil
}

// --- mapping helpers -------------------------------------------------

// TrackToAPI converts a domain Track to the OpenAPI shape. Exported
// so the api.go shim can splice subtitle tracks into asset responses
// without duplicating the conversion.
func TrackToAPI(t Track) openapi.SubtitleTrack { return trackToAPI(t) }

func trackToAPI(t Track) openapi.SubtitleTrack {
	conf := float32(t.Confidence)
	createdAt := t.CreatedAt
	label := t.Label
	return openapi.SubtitleTrack{
		Lang:         t.Lang,
		Label:        &label,
		FileHash:     t.FileHash,
		SourceFormat: openapi.SubtitleTrackSourceFormat(t.SourceFormat),
		Confidence:   conf,
		CreatedAt:    &createdAt,
	}
}

func mapPolicyErrorToList(err error) openapi.ListSubtitleTracksResponseObject {
	switch {
	case errors.Is(err, ErrAssetNotFound):
		return openapi.ListSubtitleTracks404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
		}
	case errors.Is(err, ErrSubtitlesNotApplicable):
		return openapi.ListSubtitleTracks422JSONResponse{
			UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{Error: err.Error()},
		}
	default:
		return nil
	}
}

func mapPolicyErrorToUpload(err error) openapi.UploadSubtitleTrackResponseObject {
	switch {
	case errors.Is(err, ErrAssetNotFound):
		return openapi.UploadSubtitleTrack404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
		}
	case errors.Is(err, ErrSubtitlesNotApplicable):
		return openapi.UploadSubtitleTrack422JSONResponse{
			UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{Error: err.Error()},
		}
	case errors.Is(err, ErrInvalidLang):
		return openapi.UploadSubtitleTrack400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}
	default:
		return nil
	}
}

func mapPolicyErrorToDelete(err error) openapi.DeleteSubtitleTrackResponseObject {
	switch {
	case errors.Is(err, ErrAssetNotFound):
		return openapi.DeleteSubtitleTrack404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
		}
	case errors.Is(err, ErrSubtitlesNotApplicable):
		return openapi.DeleteSubtitleTrack422JSONResponse{
			UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{Error: err.Error()},
		}
	case errors.Is(err, ErrTrackNotFound):
		return openapi.DeleteSubtitleTrack404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
		}
	default:
		return nil
	}
}

// storageServiceAdapter bridges storage.Service to the StorageWriter
// interface this package needs. Kept here (not on the storage
// package) so storage doesn't grow a subtitle-specific surface.
type storageServiceAdapter struct {
	svc *storage.Service
}

// NewStorageAdapter constructs the bridge. Production passes the
// real *storage.Service from boot wiring; tests pass nil + a
// custom fake.
func NewStorageAdapter(svc *storage.Service) StorageWriter {
	return &storageServiceAdapter{svc: svc}
}

func (a *storageServiceAdapter) PutBytes(ctx context.Context, b []byte, contentType, pinSubjectType, pinSubjectID string) (string, error) {
	res, err := a.svc.UploadOriginal(ctx, bytes.NewReader(b), contentType, storage.PinRef{
		SubjectType: pinSubjectType,
		SubjectID:   pinSubjectID,
	})
	if err != nil {
		return "", err
	}
	return res.Hash, nil
}
