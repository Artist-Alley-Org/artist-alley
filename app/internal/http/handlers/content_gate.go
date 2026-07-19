// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// requireContentAccess is the one call every byte-streaming handler in
// this package makes after its identity guard (#433, ADR 0064).
//
// It writes the response and returns false when the caller must not
// receive the bytes, so handlers read as:
//
//	if !requireContentAccess(w, r, h.Pool, assetID) {
//	    return
//	}
//
// 404 rather than 403 on denial: a distinct 403 would confirm that a
// restricted asset exists at that id. The row plane may legitimately
// disclose existence (ADR 0020 keeps restricted assets listed), but
// this plane has no reason to.
func requireContentAccess(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, assetID uuid.UUID) bool {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return false
	}
	ok, err := visibility.CanReadContent(
		r.Context(), pool, visibility.NewCaller(&id.UserRef),
		func(code string) bool { return id.Can(code) }, assetID,
	)
	if err != nil || !ok {
		// Fail closed: a lookup error is a denial, never a pass.
		http.NotFound(w, r)
		return false
	}
	return true
}
