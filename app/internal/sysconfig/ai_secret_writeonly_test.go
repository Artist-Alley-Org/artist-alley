// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #711 — a stored AI provider API key is write-only. It is never
// returned to ANY caller, `system.admin` included, and an unrelated
// save never wipes it.
//
// Written against the RAW response JSON through a real router, not
// against openapi.AIConfig, and deliberately so. The bug this pins is
// a CONVERTER that copies the stored key into the response struct —
// decoding the body back into that same struct would round-trip the
// leak and pass. `map[string]any` is the only assertion that sees
// what a client sees, and it is also the only one that distinguishes
// "field absent" from "field present and empty" (ADR 0072: omitted,
// not blanked).
//
// The read gate is `system.config.read`; setting a key needs
// `system.ai.write`. That asymmetry is why the key cannot come back
// on the read — otherwise the narrower write cap protects nothing.
//
// Skips without AA_DB_PASSWORD, same convention as the rest of the
// sysconfig suite.

package sysconfig_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

const (
	aiSecretSentinel    = "sk-SUPER-SECRET-STORED-KEY-711"
	aiSecretRotated     = "sk-ROTATED-KEY-711"
	aiSecretProviderID  = "11111111-1111-4111-8111-111111111711"
	aiSecretProviderAlt = "22222222-2222-4222-8222-222222222711"
)

// zeroElem yields the zero value of a slice's element type without
// naming that type. openapi.AIConfig.Providers has an INLINE
// anonymous element type, so an ordinary composite literal has to
// re-spell all nine fields and tags — and every such literal breaks
// the moment the schema gains a field. (handler_audit_test.go had one
// and this change broke it; that is the whole reason this exists.)
func zeroElem[T any](_ []T) T {
	var z T
	return z
}

// addAIProvider appends one provider to a request body.
func addAIProvider(cfg *openapi.AIConfig, id, kind, displayName string, apiKey *string) {
	cfg.Providers = append(cfg.Providers, zeroElem(cfg.Providers))
	e := &cfg.Providers[len(cfg.Providers)-1]
	if id != "" {
		e.Id = &id
	}
	e.Kind = openapi.AIConfigProvidersKind(kind)
	e.Enabled = true
	e.DisplayName = displayName
	e.ApiKey = apiKey
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type aiSecretShim struct {
	*strictservershim.PanicShim
	h *sysconfig.Handler
}

func (s aiSecretShim) GetAIConfig(ctx context.Context, req openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	return s.h.GetAIConfig(ctx, req)
}

func (s aiSecretShim) UpdateAIConfig(ctx context.Context, req openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	return s.h.UpdateAIConfig(ctx, req)
}

func aiSecretRouter(h *sysconfig.Handler, caps ...string) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 1, Username: "probe", AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(aiSecretShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// rawAIConfig returns the GET body as raw JSON plus the decoded map,
// so assertions can check both the field set and the whole payload
// for the sentinel — a key smuggled into any other field still fails.
func rawAIConfig(t *testing.T, router chi.Router) (string, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/system/ai", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/system/ai = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode body: %v (%s)", err, body)
	}
	return body, out
}

func firstProvider(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	list, ok := decoded["providers"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("no providers in response: %v", decoded)
	}
	p, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("providers[0] is not an object: %v", list[0])
	}
	return p
}

func patchAI(t *testing.T, router chi.Router, body openapi.AIConfig) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/system/ai", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/system/ai = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), aiSecretSentinel) || strings.Contains(rr.Body.String(), aiSecretRotated) {
		t.Errorf("PATCH response echoed a stored API key: %s", rr.Body.String())
	}
}

func aiSecretHandler(t *testing.T, pool *pgxpool.Pool) *sysconfig.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return sysconfig.NewHTTPHandler(pool, sysconfig.NewStore(pool), logger)
}

func seedAIKey(t *testing.T, ctx context.Context, h *sysconfig.Handler, key string) {
	t.Helper()
	if err := h.Store.SetAI(ctx, sysconfig.AIConfig{
		DefaultProviderID: aiSecretProviderID,
		Providers: []sysconfig.AIProvider{{
			ID: aiSecretProviderID, Kind: "openai", Enabled: true,
			DisplayName: "Probe", Model: "gpt-4o", APIKey: key,
		}},
	}); err != nil {
		t.Fatalf("seed ai config: %v", err)
	}
}

func storedAIKey(t *testing.T, ctx context.Context, h *sysconfig.Handler, id string) string {
	t.Helper()
	cfg, err := h.Store.GetAI(ctx)
	if err != nil {
		t.Fatalf("GetAI: %v", err)
	}
	for _, p := range cfg.Providers {
		if p.ID == id {
			return p.APIKey
		}
	}
	t.Fatalf("provider %s missing from stored config: %+v", id, cfg.Providers)
	return ""
}

// ---------------------------------------------------------------------------
// The invariant
// ---------------------------------------------------------------------------

// The read capability holder — who cannot set a key — gets no
// credential material back. This is the whole bug: `system.ai.write`
// guarded the write while `system.config.read` handed the value out.
func TestGetAIConfig_NeverReturnsStoredAPIKey(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, aiSecretSentinel)

		// Deliberately WITHOUT system.ai.write.
		router := aiSecretRouter(h, sysconfig.CapConfigRead)
		body, decoded := rawAIConfig(t, router)

		if strings.Contains(body, aiSecretSentinel) {
			t.Fatalf("stored API key returned to a system.config.read caller: %s", body)
		}
		p := firstProvider(t, decoded)
		if _, present := p["api_key"]; present {
			t.Errorf("api_key present in the response (omitted, not blanked): %v", p)
		}
		if set, _ := p["api_key_set"].(bool); !set {
			t.Errorf("api_key_set should be true when a key is stored: %v", p)
		}
		// The rest of the provider still comes back — this is a
		// field-level redaction, not a row-level one.
		if p["display_name"] != "Probe" || p["model"] != "gpt-4o" {
			t.Errorf("non-secret fields dropped: %v", p)
		}
	})
}

// system.admin is not a bypass. There is no role that may read a
// credential but not set one, so no capability unlocks it.
func TestGetAIConfig_SystemAdminIsNotAnExemption(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, aiSecretSentinel)

		router := aiSecretRouter(h,
			sysconfig.CapSystemAdmin, sysconfig.CapConfigRead, sysconfig.CapAIWrite)
		body, decoded := rawAIConfig(t, router)

		if strings.Contains(body, aiSecretSentinel) {
			t.Fatalf("stored API key returned to a system.admin caller: %s", body)
		}
		if _, present := firstProvider(t, decoded)["api_key"]; present {
			t.Errorf("api_key present for system.admin: %s", body)
		}
	})
}

// api_key_set is false — not absent — when nothing is on file, so the
// UI can tell "no key configured" from "key on file".
func TestGetAIConfig_ApiKeySetFalseWhenUnset(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, "")

		router := aiSecretRouter(h, sysconfig.CapConfigRead)
		_, decoded := rawAIConfig(t, router)
		p := firstProvider(t, decoded)
		set, ok := p["api_key_set"].(bool)
		if !ok {
			t.Fatalf("api_key_set missing: %v", p)
		}
		if set {
			t.Errorf("api_key_set true with no key stored: %v", p)
		}
	})
}

// #708's shape: saving an unrelated field must not wipe the secret.
// The UI can no longer round-trip the key (it never receives it), so
// every ordinary save now arrives with api_key absent — if that meant
// "clear it", the first rename would destroy every provider key.
func TestUpdateAIConfig_UnrelatedFieldSaveKeepsStoredKey(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, aiSecretSentinel)
		router := aiSecretRouter(h, sysconfig.CapAIWrite)

		// api_key omitted entirely — what the admin page now posts.
		body := openapi.AIConfig{DefaultProviderId: strPtr(aiSecretProviderID)}
		addAIProvider(&body, aiSecretProviderID, "openai", "Renamed", nil)
		patchAI(t, router, body)

		if got := storedAIKey(t, ctx, h, aiSecretProviderID); got != aiSecretSentinel {
			t.Errorf("display-name save wiped the API key: got %q, want %q", got, aiSecretSentinel)
		}

		// api_key present but empty — what a form that binds the input
		// to a blank string posts. Same meaning: keep.
		body = openapi.AIConfig{DefaultProviderId: strPtr(aiSecretProviderID)}
		addAIProvider(&body, aiSecretProviderID, "openai", "Renamed Twice", strPtr(""))
		patchAI(t, router, body)

		if got := storedAIKey(t, ctx, h, aiSecretProviderID); got != aiSecretSentinel {
			t.Errorf("empty-string api_key wiped the stored key: got %q", got)
		}
		cfg, err := h.Store.GetAI(ctx)
		if err != nil {
			t.Fatalf("GetAI: %v", err)
		}
		if cfg.Providers[0].DisplayName != "Renamed Twice" {
			t.Errorf("the non-secret edit did not apply: %+v", cfg.Providers[0])
		}
	})
}

// Merge must not become "ignore the field": a supplied key replaces.
func TestUpdateAIConfig_NewKeyReplacesStoredKey(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, aiSecretSentinel)
		router := aiSecretRouter(h, sysconfig.CapAIWrite)

		body := openapi.AIConfig{DefaultProviderId: strPtr(aiSecretProviderID)}
		addAIProvider(&body, aiSecretProviderID, "openai", "Probe", strPtr(aiSecretRotated))
		patchAI(t, router, body)

		if got := storedAIKey(t, ctx, h, aiSecretProviderID); got != aiSecretRotated {
			t.Errorf("rotation did not apply: got %q, want %q", got, aiSecretRotated)
		}
	})
}

// A provider added in the same save gets its own key and does not
// inherit — or leak — the existing provider's. The merge is keyed on
// provider ID, and a new provider has none.
func TestUpdateAIConfig_NewProviderDoesNotInheritAnotherKey(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := aiSecretHandler(t, pool)
		seedAIKey(t, ctx, h, aiSecretSentinel)
		router := aiSecretRouter(h, sysconfig.CapAIWrite)

		body := openapi.AIConfig{DefaultProviderId: strPtr(aiSecretProviderID)}
		addAIProvider(&body, aiSecretProviderID, "openai", "Probe", nil)
		addAIProvider(&body, aiSecretProviderAlt, "anthropic", "Second", nil)
		patchAI(t, router, body)

		if got := storedAIKey(t, ctx, h, aiSecretProviderID); got != aiSecretSentinel {
			t.Errorf("existing provider lost its key: got %q", got)
		}
		if got := storedAIKey(t, ctx, h, aiSecretProviderAlt); got != "" {
			t.Errorf("new provider inherited a key it was never given: %q", got)
		}
	})
}
