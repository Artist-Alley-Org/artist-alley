// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #718 — a stored SSO provider secret is write-only. It is never
// returned to ANY caller, `system.admin` included, and an unrelated
// save never wipes it.
//
// Same construction as ai_secret_writeonly_test.go (#711) and for the
// same reason: these assertions run against the RAW response JSON
// through a real router, not against openapi.AuthConfig. The bug being
// pinned is a CONVERTER that copies stored secrets into the response
// struct — decoding the body back into that same struct would
// round-trip the leak and pass. `map[string]any` is the only assertion
// that sees what a client sees, and the only one that distinguishes
// "field absent" from "field present and empty" (ADR 0072: omitted,
// not blanked).
//
// The read gate is `system.config.read`; setting a secret needs
// `system.auth.write`. That asymmetry is the whole bug — the narrower
// write capability protected nothing while the read handed the values
// out.
//
// The converter-level invariants (including the merge that stops this
// fix from becoming a data-loss bug) live in
// handler_convert_auth_internal_test.go and need no database.
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
	ssoSecretClientSecret = "oauth-client-secret-SENTINEL-718"
	ssoSecretBindPassword = "ldap-bind-password-SENTINEL-718"
	ssoSecretPrivateKey   = "saml-sp-private-key-SENTINEL-718"
	ssoSecretRotated      = "rotated-client-secret-718"
	ssoSecretProviderID   = "44444444-4444-4444-8444-444444444718"
	ssoSecretProviderAlt  = "55555555-5555-4555-8555-555555555718"
)

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type ssoSecretShim struct {
	*strictservershim.PanicShim
	h *sysconfig.Handler
}

func (s ssoSecretShim) GetAuthConfig(ctx context.Context, req openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	return s.h.GetAuthConfig(ctx, req)
}

func (s ssoSecretShim) UpdateAuthConfig(ctx context.Context, req openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	return s.h.UpdateAuthConfig(ctx, req)
}

func ssoSecretRouter(h *sysconfig.Handler, caps ...string) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 1, Username: "probe", AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(ssoSecretShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// rawAuthConfig returns the GET body as raw JSON plus the decoded map,
// so assertions can check both the field set and the whole payload for
// the sentinels — a secret smuggled into any other field still fails.
func rawAuthConfig(t *testing.T, router chi.Router) (string, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/system/auth", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/system/auth = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode body: %v (%s)", err, body)
	}
	return body, out
}

func firstSSOProviderConfig(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	list, ok := decoded["sso_providers"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("no sso_providers in response: %v", decoded)
	}
	p, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("sso_providers[0] is not an object: %v", list[0])
	}
	cfg, ok := p["config"].(map[string]any)
	if !ok {
		t.Fatalf("sso_providers[0].config missing or not an object: %v", p)
	}
	return cfg
}

func patchAuth(t *testing.T, router chi.Router, body openapi.AuthConfig) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/system/auth", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/system/auth = %d, body=%s", rr.Code, rr.Body.String())
	}
	assertNoSSOSecrets(t, "PATCH response", rr.Body.String())
}

func assertNoSSOSecrets(t *testing.T, what, body string) {
	t.Helper()
	for _, s := range []string{ssoSecretClientSecret, ssoSecretBindPassword, ssoSecretPrivateKey, ssoSecretRotated} {
		if strings.Contains(body, s) {
			t.Errorf("%s echoed a stored SSO secret (%s): %s", what, s, body)
		}
	}
}

func ssoSecretHandler(t *testing.T, pool *pgxpool.Pool) *sysconfig.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return sysconfig.NewHTTPHandler(pool, sysconfig.NewStore(pool), logger)
}

func seedSSOSecrets(t *testing.T, ctx context.Context, h *sysconfig.Handler, cfg sysconfig.SSOProviderConfig) {
	t.Helper()
	if err := h.Store.SetAuth(ctx, sysconfig.AuthConfig{
		SSOProviders: []sysconfig.SSOProvider{{
			ID:          ssoSecretProviderID,
			Kind:        sysconfig.SSOKindGoogle,
			Enabled:     true,
			DisplayName: "Probe SSO",
			Config:      cfg,
		}},
	}); err != nil {
		t.Fatalf("seed auth config: %v", err)
	}
}

func storedSSOConfig(t *testing.T, ctx context.Context, h *sysconfig.Handler, id string) sysconfig.SSOProviderConfig {
	t.Helper()
	cfg, err := h.Store.GetAuth(ctx)
	if err != nil {
		t.Fatalf("GetAuth: %v", err)
	}
	for _, p := range cfg.SSOProviders {
		if p.ID == id {
			return p.Config
		}
	}
	t.Fatalf("provider %s missing from stored config: %+v", id, cfg.SSOProviders)
	return sysconfig.SSOProviderConfig{}
}

func allThreeSecrets() sysconfig.SSOProviderConfig {
	return sysconfig.SSOProviderConfig{
		ClientID:       "client-id-is-public",
		ClientSecret:   ssoSecretClientSecret,
		BindDN:         "cn=svc,dc=example,dc=org",
		BindPassword:   ssoSecretBindPassword,
		IDPCertificate: "-----BEGIN CERTIFICATE-----public",
		SPPrivateKey:   ssoSecretPrivateKey,
	}
}

// ---------------------------------------------------------------------------
// The invariant
// ---------------------------------------------------------------------------

// The read-capability holder — who cannot set a secret — gets no
// credential material back. This is the whole bug: `system.auth.write`
// guarded the write while `system.config.read` handed the values out.
func TestGetAuthConfig_NeverReturnsProviderSecrets(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, allThreeSecrets())

		// Deliberately WITHOUT system.auth.write.
		router := ssoSecretRouter(h, sysconfig.CapConfigRead)
		body, decoded := rawAuthConfig(t, router)

		assertNoSSOSecrets(t, "GET body", body)
		cfg := firstSSOProviderConfig(t, decoded)
		for _, field := range []string{"client_secret", "bind_password", "sp_private_key"} {
			if _, present := cfg[field]; present {
				t.Errorf("%s present in the response (omitted, not blanked): %v", field, cfg)
			}
		}
		for _, field := range []string{"client_secret_set", "bind_password_set", "sp_private_key_set"} {
			if set, _ := cfg[field].(bool); !set {
				t.Errorf("%s should be true when a secret is stored: %v", field, cfg)
			}
		}
		// Field-level redaction, not row-level. The non-secrets — and
		// the two look-alikes that are public by nature — still come
		// back, or every edit becomes a retype-everything.
		if cfg["client_id"] != "client-id-is-public" ||
			cfg["bind_dn"] != "cn=svc,dc=example,dc=org" ||
			cfg["idp_certificate"] != "-----BEGIN CERTIFICATE-----public" {
			t.Errorf("non-secret config fields dropped: %v", cfg)
		}
	})
}

// system.admin is not a bypass. There is no role that may read a
// credential but not set one, so no capability unlocks it.
func TestGetAuthConfig_SystemAdminIsNotAnExemption(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, allThreeSecrets())

		router := ssoSecretRouter(h,
			sysconfig.CapSystemAdmin, sysconfig.CapConfigRead, sysconfig.CapAuthWrite)
		body, decoded := rawAuthConfig(t, router)

		assertNoSSOSecrets(t, "GET body for system.admin", body)
		cfg := firstSSOProviderConfig(t, decoded)
		for _, field := range []string{"client_secret", "bind_password", "sp_private_key"} {
			if _, present := cfg[field]; present {
				t.Errorf("%s present for system.admin: %s", field, body)
			}
		}
	})
}

// *_set is false — not absent — when nothing is on file, so the UI can
// tell "not configured" from "configured".
func TestGetAuthConfig_SecretSetFalseWhenUnset(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, sysconfig.SSOProviderConfig{ClientID: "public-only"})

		router := ssoSecretRouter(h, sysconfig.CapConfigRead)
		_, decoded := rawAuthConfig(t, router)
		cfg := firstSSOProviderConfig(t, decoded)
		for _, field := range []string{"client_secret_set", "bind_password_set", "sp_private_key_set"} {
			set, ok := cfg[field].(bool)
			if !ok {
				t.Fatalf("%s missing: %v", field, cfg)
			}
			if set {
				t.Errorf("%s true with no secret stored: %v", field, cfg)
			}
		}
	})
}

// #708's shape, end to end: the admin page loads, edits one unrelated
// field, and PATCHes the whole provider list back. It can no longer
// round-trip the secrets (it never receives them), so if absent meant
// "clear it" the first rename would destroy every credential.
func TestUpdateAuthConfig_UnrelatedSaveKeepsStoredSecrets(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, allThreeSecrets())
		router := ssoSecretRouter(h, sysconfig.CapConfigRead, sysconfig.CapAuthWrite)

		// Take the GET body verbatim — the exact payload the admin page
		// has to work from — rename, and post it back.
		_, decoded := rawAuthConfig(t, router)
		var body openapi.AuthConfig
		raw, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode GET body into the PATCH shape: %v", err)
		}
		body.SsoProviders[0].DisplayName = "Renamed"
		patchAuth(t, router, body)

		got := storedSSOConfig(t, ctx, h, ssoSecretProviderID)
		if got.ClientSecret != ssoSecretClientSecret {
			t.Errorf("display-name save wiped client_secret: %q", got.ClientSecret)
		}
		if got.BindPassword != ssoSecretBindPassword {
			t.Errorf("display-name save wiped bind_password: %q", got.BindPassword)
		}
		if got.SPPrivateKey != ssoSecretPrivateKey {
			t.Errorf("display-name save wiped sp_private_key: %q", got.SPPrivateKey)
		}

		// And the second save — the one that actually proves it. The
		// first could pass on a stale in-memory copy; this one starts
		// from a fresh GET of what the first save persisted.
		_, decoded2 := rawAuthConfig(t, router)
		var body2 openapi.AuthConfig
		raw2, err := json.Marshal(decoded2)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if err := json.Unmarshal(raw2, &body2); err != nil {
			t.Fatalf("decode second GET body: %v", err)
		}
		body2.SsoProviders[0].DisplayName = "Renamed Twice"
		patchAuth(t, router, body2)

		got2 := storedSSOConfig(t, ctx, h, ssoSecretProviderID)
		if got2.ClientSecret != ssoSecretClientSecret || got2.BindPassword != ssoSecretBindPassword ||
			got2.SPPrivateKey != ssoSecretPrivateKey {
			t.Errorf("second save wiped a secret: %+v", got2)
		}
		cfg, err := h.Store.GetAuth(ctx)
		if err != nil {
			t.Fatalf("GetAuth: %v", err)
		}
		if cfg.SSOProviders[0].DisplayName != "Renamed Twice" {
			t.Errorf("the non-secret edit did not apply: %+v", cfg.SSOProviders[0])
		}
	})
}

// Merge must not become "ignore the field": a supplied secret replaces.
func TestUpdateAuthConfig_SuppliedSecretRotates(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, allThreeSecrets())
		router := ssoSecretRouter(h, sysconfig.CapConfigRead, sysconfig.CapAuthWrite)

		body := openapi.AuthConfig{}
		body.SsoProviders = append(body.SsoProviders, zeroElem(body.SsoProviders))
		e := &body.SsoProviders[0]
		id := ssoSecretProviderID
		e.Id = &id
		e.Kind = openapi.AuthConfigSsoProvidersKind(sysconfig.SSOKindGoogle)
		e.Enabled = true
		e.DisplayName = "Probe SSO"
		rotated := ssoSecretRotated
		e.Config = &openapi.SSOProviderConfig{ClientSecret: &rotated}
		patchAuth(t, router, body)

		got := storedSSOConfig(t, ctx, h, ssoSecretProviderID)
		if got.ClientSecret != ssoSecretRotated {
			t.Errorf("rotation did not apply: %q", got.ClientSecret)
		}
		if got.BindPassword != ssoSecretBindPassword {
			t.Errorf("rotating one secret disturbed another: %q", got.BindPassword)
		}
	})
}

// A provider added in the same save gets its own secrets and does not
// inherit — or leak — the existing provider's. The merge is keyed on
// provider ID.
func TestUpdateAuthConfig_NewProviderDoesNotInheritSecrets(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, _ *sysconfig.Handler, pool *pgxpool.Pool) {
		h := ssoSecretHandler(t, pool)
		seedSSOSecrets(t, ctx, h, allThreeSecrets())
		router := ssoSecretRouter(h, sysconfig.CapConfigRead, sysconfig.CapAuthWrite)

		_, decoded := rawAuthConfig(t, router)
		var body openapi.AuthConfig
		raw, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode GET body: %v", err)
		}
		// A second provider carrying an explicit id the store has never
		// seen — the closest a caller can get to "guess your way into
		// someone else's secret".
		body.SsoProviders = append(body.SsoProviders, body.SsoProviders[0])
		alt := ssoSecretProviderAlt
		body.SsoProviders[1].Id = &alt
		body.SsoProviders[1].DisplayName = "Second"
		patchAuth(t, router, body)

		if got := storedSSOConfig(t, ctx, h, ssoSecretProviderID); got.ClientSecret != ssoSecretClientSecret {
			t.Errorf("existing provider lost its secret: %q", got.ClientSecret)
		}
		fresh := storedSSOConfig(t, ctx, h, ssoSecretProviderAlt)
		if fresh.ClientSecret != "" || fresh.BindPassword != "" || fresh.SPPrivateKey != "" {
			t.Errorf("new provider inherited secrets it was never given: %+v", fresh)
		}
	})
}
