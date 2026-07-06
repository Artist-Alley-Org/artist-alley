package visualbackfill

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// buildRouter mounts the visual-backfill handler on a chi router for
// per-test invocation.
func buildRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

// withAdmin decorates the request context with a super-admin identity.
func withAdmin(r *http.Request) *http.Request {
	id := &auth.Identity{
		UserRef:      1,
		Username:     "admin",
		Capabilities: []string{auth.SuperAdminCapability},
	}
	return r.WithContext(auth.WithIdentity(r.Context(), id))
}

// withRegularUser decorates the request context with a non-admin identity.
func withRegularUser(r *http.Request) *http.Request {
	id := &auth.Identity{UserRef: 42, Username: "u", Capabilities: []string{"users.read"}}
	return r.WithContext(auth.WithIdentity(r.Context(), id))
}

// TestStart_NoAuth_401 — trigger without a resolved identity 401s.
func TestStart_NoAuth_401(t *testing.T) {
	h := &Handler{Store: &Store{}, Logger: slog.Default()}
	rr := httptest.NewRecorder()
	buildRouter(h).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/search/visual-backfill", strings.NewReader("{}")))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestStart_MissingCap_403 — trigger from a non-admin caller 403s.
func TestStart_MissingCap_403(t *testing.T) {
	h := &Handler{Store: &Store{}, Logger: slog.Default()}
	rr := httptest.NewRecorder()
	buildRouter(h).ServeHTTP(rr, withRegularUser(httptest.NewRequest(http.MethodPost, "/admin/search/visual-backfill", strings.NewReader("{}"))))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

// TestStart_ProviderNil_503 — when the sidecar isn't wired the trigger
// returns 503 with provider_not_registered so operators see the
// sysconfig gap.
func TestStart_ProviderNil_503(t *testing.T) {
	h := &Handler{Store: &Store{}, Logger: slog.Default()}
	rr := httptest.NewRecorder()
	buildRouter(h).ServeHTTP(rr, withAdmin(httptest.NewRequest(http.MethodPost, "/admin/search/visual-backfill", strings.NewReader("{}"))))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["error"] != "provider_not_registered" {
		t.Fatalf("expected provider_not_registered, got %v", body["error"])
	}
	if _, ok := body["message"]; !ok {
		t.Fatalf("expected diagnostic message field, got %v", body)
	}
}

// TestCancel_InvalidUUID_400 — malformed UUID at /cancel returns 400
// (guards the URL param before any store I/O).
func TestCancel_InvalidUUID_400(t *testing.T) {
	h := &Handler{Store: &Store{}, Logger: slog.Default()}
	rr := httptest.NewRecorder()
	buildRouter(h).ServeHTTP(rr, withAdmin(httptest.NewRequest(http.MethodPost, "/admin/search/visual-backfill/runs/not-a-uuid/cancel", nil)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestGet_InvalidUUID_400 — malformed UUID at GET /runs/{id} returns 400.
func TestGet_InvalidUUID_400(t *testing.T) {
	h := &Handler{Store: &Store{}, Logger: slog.Default()}
	rr := httptest.NewRecorder()
	buildRouter(h).ServeHTTP(rr, withAdmin(httptest.NewRequest(http.MethodGet, "/admin/search/visual-backfill/runs/not-a-uuid", nil)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}
