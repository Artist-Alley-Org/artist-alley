// Phase 1.17.D — integration tests for the sysconfig audit
// retrofit. Each Update* handler now snapshots the current
// value, writes the new value, and emits the typed event via
// Recorder.RecordChange with a metadata.changeset diff.
//
// These tests stand up a real *audit.Recorder against the dev DB
// + drive the handler + read the audit_events row back out to
// verify the changeset shape + sensitive-field stripping.

package sysconfig_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func TestUpdateSiteConfig_EmitsChangeset(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		// Seed an initial site so the changeset has a before value.
		if err := h.Store.SetSite(ctx, sysconfig.Site{Name: "Old", BaseURL: "https://a/"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.SiteConfig{Name: "New", BaseUrl: strPtr("https://b/")}
			resp, err := h.UpdateSiteConfig(ctx, openapi.UpdateSiteConfigRequestObject{Body: &body})
			if err != nil {
				t.Fatalf("UpdateSiteConfig: %v", err)
			}
			if _, ok := resp.(openapi.UpdateSiteConfig200JSONResponse); !ok {
				t.Fatalf("expected 200, got %T", resp)
			}
		})

		cs := readChangeset(t, pool, audit.EventAdminSiteConfigUpdated)
		if cs == nil {
			t.Fatal("no audit event with changeset emitted")
		}
		// Both fields changed; both should appear.
		if _, ok := cs["Name"]; !ok {
			t.Errorf("changeset missing Name entry: %v", cs)
		}
		if _, ok := cs["BaseURL"]; !ok {
			t.Errorf("changeset missing BaseURL entry: %v", cs)
		}
	})
}

func TestUpdateSiteConfig_NoChange_NoChangesetKey(t *testing.T) {
	// Idempotent save: same values in + same values out → no
	// changeset key in metadata. Event still emits (admin
	// action happened) so audit grep still finds it.
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.Site{Name: "Same", BaseURL: "https://same/"}
		if err := h.Store.SetSite(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.SiteConfig{Name: "Same", BaseUrl: strPtr("https://same/")}
			if _, err := h.UpdateSiteConfig(ctx, openapi.UpdateSiteConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateSiteConfig: %v", err)
			}
		})

		md := readMetadata(t, pool, audit.EventAdminSiteConfigUpdated)
		if md == nil {
			t.Fatal("no audit row for no-op save")
		}
		if _, has := md["changeset"]; has {
			t.Errorf("no-op save should not include changeset key; got %v", md)
		}
	})
}

func TestUpdateSMTPConfig_StripsPasswordField(t *testing.T) {
	// SMTP.Password matches the "password" sensitive pattern and
	// MUST NOT appear in the changeset — even though it differs
	// between before + after. Defense-in-depth: even if a future
	// refactor drops the audit:"-" tag from a sensitive field, the
	// substring pattern catches it.
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.SMTP{
			Host: "smtp.old.example", Port: 587,
			Encryption: sysconfig.SMTPEncryptionStartTLS,
			Username:   "user", Password: "OLD-SECRET", FromAddr: "x@a",
		}
		if err := h.Store.SetSMTP(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.SMTPConfig{
				Host: "smtp.new.example", Port: 465,
				Encryption: openapi.SMTPConfigEncryptionTls,
				Username:   strPtr("user"),
				Password:   strPtr("NEW-EVEN-WORSE-SECRET"),
				FromAddress: "x@a",
			}
			if _, err := h.UpdateSMTPConfig(ctx, openapi.UpdateSMTPConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateSMTPConfig: %v", err)
			}
		})

		cs := readChangeset(t, pool, audit.EventAdminSMTPConfigUpdated)
		if cs == nil {
			t.Fatal("no audit event emitted")
		}
		// Host should be in the diff.
		if _, ok := cs["Host"]; !ok {
			t.Errorf("Host change missing from changeset: %v", cs)
		}
		// Password MUST be stripped.
		if _, leaked := cs["Password"]; leaked {
			t.Errorf("Password leaked into changeset: %v", cs)
		}
		// And neither plaintext value appears anywhere in the
		// serialized metadata — defense-in-depth check against
		// the WHOLE blob (not just the changeset).
		raw := readRawMetadata(t, pool, audit.EventAdminSMTPConfigUpdated)
		if strings.Contains(raw, "OLD-SECRET") || strings.Contains(raw, "NEW-EVEN-WORSE-SECRET") {
			t.Errorf("plaintext password leaked into raw metadata: %s", raw)
		}
	})
}

func TestUpdateAIConfig_StripsProvidersWithAPIKeys(t *testing.T) {
	// AIConfig.Providers carries embedded APIKey strings that the
	// diff helper can't strip per-element. The `audit:"-"` tag on
	// the slice strips the whole list — operators see "AI config
	// changed: DefaultProviderID" and read the new provider list
	// via the API.
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.AIConfig{
			DefaultProviderID: "old-id",
			Providers: []sysconfig.AIProvider{
				{ID: "old-id", Kind: "openai", Enabled: true, DisplayName: "p", APIKey: "sk-LEAKY"},
			},
		}
		if err := h.Store.SetAI(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			// openapi.AIConfig.Providers is an inline anonymous
			// struct; build a body via reflection-free literal.
			body := openapi.AIConfig{
				DefaultProviderId: strPtr("new-id"),
			}
			body.Providers = append(body.Providers, struct {
				ApiKey      *string                 `json:"api_key,omitempty"`
				BaseUrl     *string                 `json:"base_url,omitempty"`
				Config      *map[string]interface{} `json:"config,omitempty"`
				DisplayName string                  `json:"display_name"`
				Enabled     bool                    `json:"enabled"`
				Id          *string                 `json:"id,omitempty"`
				Kind        openapi.AIConfigProvidersKind `json:"kind"`
				Model       *string                 `json:"model,omitempty"`
			}{
				Id:          strPtr("new-id"),
				Kind:        openapi.AIConfigProvidersKind("openai"),
				Enabled:     true,
				DisplayName: "p2",
				ApiKey:      strPtr("sk-ALSO-LEAKY"),
			})
			if _, err := h.UpdateAIConfig(ctx, openapi.UpdateAIConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateAIConfig: %v", err)
			}
		})

		cs := readChangeset(t, pool, audit.EventAdminAIConfigUpdated)
		if cs == nil {
			t.Fatal("no audit event emitted")
		}
		if _, ok := cs["DefaultProviderID"]; !ok {
			t.Errorf("DefaultProviderID change missing: %v", cs)
		}
		if _, leaked := cs["Providers"]; leaked {
			t.Errorf("Providers slice leaked despite audit:\"-\" tag: %v", cs)
		}
		raw := readRawMetadata(t, pool, audit.EventAdminAIConfigUpdated)
		for _, secret := range []string{"sk-LEAKY", "sk-ALSO-LEAKY"} {
			if strings.Contains(raw, secret) {
				t.Errorf("API key %q leaked into raw metadata: %s", secret, raw)
			}
		}
	})
}

func TestUpdateAppearanceConfig_AllFontsDiffed(t *testing.T) {
	// All four font slots are benign strings; every change should
	// appear in the diff with no sensitive-pattern false positives.
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.AppearanceConfig{BrandFont: "Inter", DisplayFont: "Inter", BodyFont: "Inter", MonoFont: "JetBrains Mono"}
		if err := h.Store.SetAppearance(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.AppearanceConfig{
				BrandFont: strPtr("Manrope"), DisplayFont: strPtr("Manrope"),
				BodyFont: strPtr("Inter"), MonoFont: strPtr("Fira Code"),
			}
			if _, err := h.UpdateAppearanceConfig(ctx, openapi.UpdateAppearanceConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateAppearanceConfig: %v", err)
			}
		})

		cs := readChangeset(t, pool, audit.EventAdminAppearanceConfigUpdated)
		// 3 of 4 changed; BodyFont stayed the same.
		for _, want := range []string{"BrandFont", "DisplayFont", "MonoFont"} {
			if _, ok := cs[want]; !ok {
				t.Errorf("missing %s in changeset: %v", want, cs)
			}
		}
		if _, leaked := cs["BodyFont"]; leaked {
			t.Errorf("unchanged BodyFont should not appear: %v", cs)
		}
	})
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

func withAuditStore(t *testing.T, fn func(context.Context, *sysconfig.Handler, *pgxpool.Pool)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := sysconfig.NewStore(pool)
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetAuditRecorder(&audit.Recorder{Pool: pool, Logger: logger})

	// Pre- and post-clean: wipe the keys + the audit rows this
	// suite's events would have produced.
	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c,
			`DELETE FROM system_config WHERE key IN ('site','smtp','auth','ai','appearance')`)
		_, _ = pool.Exec(c, `DELETE FROM audit_events
		    WHERE event_type LIKE 'admin.system.%'`)
	}
	clean()
	t.Cleanup(clean)

	fn(ctx, h, pool)
}

// callAsAdmin runs fn with an Identity holding all the system-
// config caps installed in the ctx. Sidesteps the actual auth
// middleware so the handler reads a valid caller from
// auth.IdentityFromContext.
func callAsAdmin(t *testing.T, ctx context.Context, _ *pgxpool.Pool, fn func(context.Context)) {
	t.Helper()
	id := &auth.Identity{
		UserRef:      1,
		Username:     "admin",
		Capabilities: []string{
			sysconfig.CapConfigWrite,
			sysconfig.CapAuthWrite,
			sysconfig.CapAIWrite,
			sysconfig.CapAppearanceWrite,
			sysconfig.CapSystemAdmin,
		},
	}
	fn(auth.WithIdentity(ctx, id))
}

func readChangeset(t *testing.T, pool *pgxpool.Pool, eventType string) map[string]any {
	t.Helper()
	md := readMetadata(t, pool, eventType)
	if md == nil {
		return nil
	}
	cs, ok := md["changeset"].(map[string]any)
	if !ok {
		return nil
	}
	return cs
}

func readMetadata(t *testing.T, pool *pgxpool.Pool, eventType string) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT metadata FROM audit_events WHERE event_type = $1 ORDER BY occurred_at DESC LIMIT 1`,
		eventType,
	).Scan(&raw)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	return out
}

func readRawMetadata(t *testing.T, pool *pgxpool.Pool, eventType string) string {
	t.Helper()
	var raw string
	err := pool.QueryRow(context.Background(),
		`SELECT metadata::text FROM audit_events WHERE event_type = $1 ORDER BY occurred_at DESC LIMIT 1`,
		eventType,
	).Scan(&raw)
	if err != nil {
		return ""
	}
	return raw
}

func strPtr(s string) *string { return &s }
