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
	// Anonymous is no longer rejected here (#415) — the checker decides,
	// so there is one place that knows which tiers an anonymous caller
	// may read. An anonymous caller carries a nil identity, so it also
	// carries no capabilities: nil CapabilityChecker, never admin.
	var (
		caller visibility.Caller
		caps   visibility.CapabilityChecker
	)
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		caller = visibility.NewCaller(&id.UserRef)
		caps = func(code string) bool { return id.Can(code) }
	} else {
		caller = visibility.NewCaller(nil)
	}
	// The mature axis rides the request context (#1116). This is the
	// single gate every byte-streaming handler shares, so wiring it here
	// covers asset_file, hls, archive_bundle and archive_entry at once —
	// and an unwired route yields the disqualified viewer rather than a
	// widened one (visibility.MatureFromContext).
	ok, err := visibility.CanReadContent(r.Context(), pool, caller, caps, assetID,
		visibility.MatureFromContext(r.Context()))
	if err != nil || !ok {
		// Fail closed: a lookup error is a denial, never a pass.
		http.NotFound(w, r)
		return false
	}
	return true
}
