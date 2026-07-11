// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Snapshot suite for the feedback HTTP surface (Phase 1.16.B-followup
// #185 retrofit).
//
// The retrofit swaps PoolVisibility's inline `SELECT EXISTS ...`
// query for a call into the shared visibility.CanSee helper. The
// entire value of the retrofit lives in "did observable behaviour
// survive." These tests capture every error-path response body
// byte-for-byte, so a future regression that accidentally changes
// error text, header shape, or status code fails loudly.
//
// Success paths (200 with a real DB row) aren't captured here —
// they need a live pool. The integration suite in ./scripts/test.sh
// covers those; the compose-stack smoke at PR time re-verifies.

package feedback

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func makeUserHandler() *Handler {
	return &Handler{Logger: slog.Default()}
}

func makeAdminHandler() *AdminHandler {
	return &AdminHandler{Logger: slog.Default()}
}

func serveUser(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	h.Mount(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func serveAdmin(h *AdminHandler, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	h.Mount(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func newAuthedRequest(method, path string, body string, userRef int64, caps ...string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet && method != http.MethodDelete {
		req.Header.Set("Content-Type", "application/json")
	}
	id := &auth.Identity{UserRef: userRef, Username: "u", Capabilities: caps}
	return req.WithContext(auth.WithIdentity(req.Context(), id))
}

// TestSnapshot_Submit_Anonymous_401Body — response bytes MUST match
// the pre-retrofit shape verbatim. Both the header set and the
// trailing newline from json.Encoder are load-bearing for downstream
// clients.
func TestSnapshot_Submit_Anonymous_401Body(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/search/feedback", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := serveUser(makeUserHandler(), req)

	want := []byte(`{"error":"authentication_required"}` + "\n")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", got)
	}
}

// TestSnapshot_Submit_InvalidBody_400 — malformed JSON path.
func TestSnapshot_Submit_InvalidBody_400(t *testing.T) {
	req := newAuthedRequest(http.MethodPost, "/search/feedback", `{not-json`, 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"invalid_body"}` + "\n")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Submit_MissingDSL_400.
func TestSnapshot_Submit_MissingDSL_400(t *testing.T) {
	body := `{"hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":1,"direction":"up"}`
	req := newAuthedRequest(http.MethodPost, "/search/feedback", body, 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"dsl_required"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Submit_NilHitAssetID_400.
func TestSnapshot_Submit_NilHitAssetID_400(t *testing.T) {
	body := `{"dsl":"cat","hit_asset_id":"00000000-0000-0000-0000-000000000000","hit_position":1,"direction":"up"}`
	req := newAuthedRequest(http.MethodPost, "/search/feedback", body, 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"hit_asset_id_required"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Submit_ZeroPosition_400.
func TestSnapshot_Submit_ZeroPosition_400(t *testing.T) {
	body := `{"dsl":"cat","hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":0,"direction":"up"}`
	req := newAuthedRequest(http.MethodPost, "/search/feedback", body, 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"hit_position_must_be_positive"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Submit_InvalidDirection_400.
func TestSnapshot_Submit_InvalidDirection_400(t *testing.T) {
	body := `{"dsl":"cat","hit_asset_id":"11111111-1111-1111-1111-111111111111","hit_position":1,"direction":"sideways"}`
	req := newAuthedRequest(http.MethodPost, "/search/feedback", body, 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"invalid_direction"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Delete_Anonymous_401 — DELETE-side anon body.
func TestSnapshot_Delete_Anonymous_401(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/search/feedback/"+uuid.New().String(), nil)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"authentication_required"}` + "\n")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Delete_InvalidUUID_400.
func TestSnapshot_Delete_InvalidUUID_400(t *testing.T) {
	req := newAuthedRequest(http.MethodDelete, "/search/feedback/not-a-uuid", "", 1)
	rr := serveUser(makeUserHandler(), req)
	want := []byte(`{"error":"invalid_id"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Admin_NoAuth_401.
func TestSnapshot_Admin_NoAuth_401(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/search/feedback", nil)
	rr := serveAdmin(makeAdminHandler(), req)
	want := []byte(`{"error":"authentication_required"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Admin_MissingCap_403.
func TestSnapshot_Admin_MissingCap_403(t *testing.T) {
	req := newAuthedRequest(http.MethodGet, "/admin/search/feedback", "", 1, "users.read")
	rr := serveAdmin(makeAdminHandler(), req)
	want := []byte(`{"error":"admin_required"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}

// TestSnapshot_Admin_PerUser_InvalidRef_400.
func TestSnapshot_Admin_PerUser_InvalidRef_400(t *testing.T) {
	req := newAuthedRequest(http.MethodGet, "/admin/search/feedback/audit/not-a-number", "", 1, auth.SuperAdminCapability)
	rr := serveAdmin(makeAdminHandler(), req)
	want := []byte(`{"error":"invalid_user_ref"}` + "\n")
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("body mismatch:\nwant %q\n got %q", want, rr.Body.Bytes())
	}
}
