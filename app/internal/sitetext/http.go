// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP surface for operator string overrides (#794).
//
//	GET    /site-text        — the whole override map    (anonymous)
//	PUT    /site-text/{key}  — override one string        (system.config.write)
//	DELETE /site-text/{key}  — revert one string          (system.config.write)
//
// Kept in its own file so non-HTTP consumers of the package don't drag
// in the openapi import, matching the featured/requests convention.

package sitetext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// MaxOverrideLength caps a single override.
//
// The longest string that ships is comfortably under a kilobyte, and
// nothing downstream truncates, so the cap exists to stop a
// misdirected paste from wedging the boot payload every visitor
// downloads — not to express a UI constraint.
const MaxOverrideLength = 4096

// HTTPHandler adapts the domain Handler to the strict-server contract.
type HTTPHandler struct {
	domain *Handler
	logger *slog.Logger
}

// NewHTTPHandler builds the adapter.
func NewHTTPHandler(h *Handler, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{domain: h, logger: logger}
}

// ---------------------------------------------------------------------------
// GET /site-text
// ---------------------------------------------------------------------------

// GetSiteText serves the override map to anybody.
//
// No identity requirement and no capability check, deliberately: this
// is the wording of the UI itself. A logged-out visitor reads the same
// navbar as everyone else, so gating the read would make the operator's
// copy appear only after sign-in — the feature half-working in the case
// operators are most likely to check.
//
// Unlike the public rail this is NOT gated by public mode either. A
// closed install still renders a login page, and that login page is
// made of these strings.
func (h *HTTPHandler) GetSiteText(
	ctx context.Context,
	_ openapi.GetSiteTextRequestObject,
) (openapi.GetSiteTextResponseObject, error) {
	all, err := h.domain.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("sitetext: get: %w", err)
	}
	// All() guarantees a non-nil map, so a fresh install marshals as
	// `{"overrides":{}}` rather than `null` and no client has to
	// special-case it. Encoding only reads, so handing over the cached
	// map directly is safe — see All()'s read-only contract.
	return openapi.GetSiteText200JSONResponse{Overrides: all}, nil
}

// ---------------------------------------------------------------------------
// PUT /site-text/{key}
// ---------------------------------------------------------------------------

func (h *HTTPHandler) SetSiteText(
	ctx context.Context,
	req openapi.SetSiteTextRequestObject,
) (openapi.SetSiteTextResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetSiteText401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.SetSiteText403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetSiteText400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	key := strings.TrimSpace(req.Key)
	language := strings.TrimSpace(req.Body.Language)
	if key == "" || language == "" {
		return openapi.SetSiteText400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "key and language are required"},
		}, nil
	}
	if len(req.Body.Value) > MaxOverrideLength {
		return openapi.SetSiteText400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: fmt.Sprintf("value exceeds %d characters", MaxOverrideLength),
			},
		}, nil
	}

	userRef := id.UserRef
	row, err := h.domain.Set(ctx, key, language, req.Body.Value, &userRef)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			// The message NAMES THE KEY. An operator who mistypes
			// `nav.collection` has to be able to see that it was the
			// key that was wrong, not the save — the whole point of
			// ADR 0081's fail-loud rule.
			return openapi.SetSiteText422JSONResponse{
				UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{
					Error: fmt.Sprintf("no shipped string has the key %q — it cannot be overridden", key),
				},
			}, nil
		}
		return nil, fmt.Errorf("sitetext: set: %w", err)
	}

	out := openapi.SiteTextEntry{
		Key:      row.Key,
		Language: row.Language,
		Value:    row.Value,
	}
	if row.UpdatedAt.Valid {
		t := row.UpdatedAt.Time
		out.UpdatedAt = &t
	}
	return openapi.SetSiteText200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// DELETE /site-text/{key}
// ---------------------------------------------------------------------------

func (h *HTTPHandler) DeleteSiteText(
	ctx context.Context,
	req openapi.DeleteSiteTextRequestObject,
) (openapi.DeleteSiteTextResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeleteSiteText401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.DeleteSiteText403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	key := strings.TrimSpace(req.Key)
	language := strings.TrimSpace(req.Params.Language)
	if key == "" || language == "" {
		return openapi.DeleteSiteText400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "key and language are required"},
		}, nil
	}
	if err := h.domain.Delete(ctx, key, language); err != nil {
		if errors.Is(err, ErrNotFound) {
			return openapi.DeleteSiteText404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{
					Error: fmt.Sprintf("no override for %q in %q", key, language),
				},
			}, nil
		}
		return nil, fmt.Errorf("sitetext: delete: %w", err)
	}
	return openapi.DeleteSiteText204Response{}, nil
}
