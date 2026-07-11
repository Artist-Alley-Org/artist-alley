// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package feedback

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// buildUserRouter mounts the user Handler on a fresh router.
func buildUserRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

// buildAdminRouter mounts the admin Handler.
func buildAdminRouter(h *AdminHandler) chi.Router {
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func withAuth(r *http.Request, userRef int64, caps ...string) *http.Request {
	id := &auth.Identity{UserRef: userRef, Username: "u", Capabilities: caps}
	return r.WithContext(auth.WithIdentity(r.Context(), id))
}

// TestSubmit_Anonymous_Returns401 — no identity → 401.
func TestSubmit_Anonymous_Returns401(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	body := `{"dsl":"cat","hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":1,"direction":"up"}`
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestSubmit_InvalidBody_Returns400 — malformed JSON.
func TestSubmit_InvalidBody_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader("{garbage"))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSubmit_MissingDSL_Returns400 — dsl required.
func TestSubmit_MissingDSL_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	body := `{"hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":1,"direction":"up"}`
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["error"] != "dsl_required" {
		t.Fatalf("expected error=dsl_required, got %v", out)
	}
}

// TestSubmit_InvalidPosition_Returns400 — hit_position must be >= 1.
func TestSubmit_InvalidPosition_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	body := `{"dsl":"cat","hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":0,"direction":"up"}`
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSubmit_InvalidDirection_Returns400 — direction must be up|down.
func TestSubmit_InvalidDirection_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	body := `{"dsl":"cat","hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":1,"direction":"sideways"}`
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSubmit_MissingHitAssetID_Returns400 — nil UUID rejected.
func TestSubmit_MissingHitAssetID_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	body := `{"dsl":"cat","hit_asset_id":"00000000-0000-0000-0000-000000000000","hit_position":1,"direction":"up"}`
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestDelete_Anonymous_Returns401 — no identity → 401.
func TestDelete_Anonymous_Returns401(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodDelete, "/search/feedback/"+uuid.New().String(), nil)
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestDelete_InvalidUUID_Returns400 — malformed URL param.
func TestDelete_InvalidUUID_Returns400(t *testing.T) {
	h := &Handler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodDelete, "/search/feedback/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	buildUserRouter(h).ServeHTTP(rr, withAuth(req, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestAdmin_Aggregation_NoAuth_Returns401 — anonymous → 401.
func TestAdmin_Aggregation_NoAuth_Returns401(t *testing.T) {
	h := &AdminHandler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/admin/search/feedback", nil)
	rr := httptest.NewRecorder()
	buildAdminRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestAdmin_Aggregation_MissingCap_Returns403 — non-admin → 403.
func TestAdmin_Aggregation_MissingCap_Returns403(t *testing.T) {
	h := &AdminHandler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/admin/search/feedback", nil)
	rr := httptest.NewRecorder()
	buildAdminRouter(h).ServeHTTP(rr, withAuth(req, 1, "users.read"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

// TestAdmin_PerUser_InvalidUserRef_Returns400 — malformed URL param.
func TestAdmin_PerUser_InvalidUserRef_Returns400(t *testing.T) {
	h := &AdminHandler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/admin/search/feedback/audit/not-a-number", nil)
	rr := httptest.NewRecorder()
	buildAdminRouter(h).ServeHTTP(rr, withAuth(req, 1, auth.SuperAdminCapability))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}
