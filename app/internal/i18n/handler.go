// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package i18n is the server-side hook for the frontend's locale
// system. The translations themselves live on the frontend (bundled
// JSON catalogues); this package only:
//
//  1. Tells the frontend WHICH locales it can switch to.
//  2. (Future) accepts uploaded community translations to be persisted
//     in object storage and served back.
//
// For Phase 1.16 there's exactly one endpoint: `GET /i18n/locales`.
// The list is static — computed once at package init from a hand-
// maintained registry — because the catalogues themselves are baked
// into the frontend build. When we add an admin "manage translations"
// UI the list will become dynamic.
package i18n

import (
	"context"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Locale is the server-known form of a UI locale. Kept in sync by
// hand with the frontend `web/src/lib/i18n/locales.ts` registry —
// any new catalogue added on the frontend should add a matching row
// here.
type Locale struct {
	Code          string
	Name          string
	NativeName    string
	Region        string
	CompletionPct int
}

// registry — the supported locales. Order is the order the frontend
// renders in the picker.
var registry = []Locale{
	{Code: "en", Name: "English", NativeName: "English", CompletionPct: 100},
	{Code: "es", Name: "Spanish", NativeName: "Español", CompletionPct: 5},
	{Code: "fr", Name: "French", NativeName: "Français", CompletionPct: 5},
}

// Handler implements the i18n slice of the API.
type Handler struct {
	Logger *slog.Logger
}

func NewHandler(logger *slog.Logger) *Handler {
	return &Handler{Logger: logger}
}

// ListLocales returns every locale the frontend has a catalogue for.
// Authenticated callers only — anonymous browsing of the locale list
// could leak which translations are stubbed, which isn't sensitive
// but isn't useful either.
func (h *Handler) ListLocales(
	ctx context.Context,
	_ openapi.ListLocalesRequestObject,
) (openapi.ListLocalesResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListLocales401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	out := make([]openapi.Locale, 0, len(registry))
	for _, l := range registry {
		entry := openapi.Locale{
			Code:          l.Code,
			Name:          l.Name,
			NativeName:    l.NativeName,
			CompletionPct: l.CompletionPct,
		}
		if l.Region != "" {
			r := l.Region
			entry.Region = &r
		}
		out = append(out, entry)
	}
	return openapi.ListLocales200JSONResponse(out), nil
}
