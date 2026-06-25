package sysconfig_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func withSMTPTestHandler(t *testing.T, fn func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := sysconfig.NewStore(pool)
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetAuditRecorder(&audit.Recorder{Pool: pool, Logger: logger})

	cap := &email.Capture{}
	h.SetEmail(&sysconfig.EmailDeps{
		Sender: cap,
		Mode:   email.ModeCapture,
		Site: func(_ context.Context) (email.SiteContext, error) {
			return email.SiteContext{Name: "Studio Alpha", URL: "https://art.example.com"}, nil
		},
	})

	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM system_config WHERE key = 'smtp'`)
	}
	clean()
	t.Cleanup(clean)

	fn(ctx, h, pool, cap)
}

func shortHash(s string) string {
	h := uint64(1469598103934665603)
	for _, r := range s {
		h ^= uint64(r)
		h *= 1099511628211
	}
	const d = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	for h > 0 {
		i--
		buf[i] = d[h%16]
		h /= 16
	}
	return string(buf[i:])
}

func adminCtxWithEmail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, addr string) context.Context {
	t.Helper()
	// Stand up a user row so the caller-email lookup finds something.
	// Username column is varchar(50); hash the test name + addr
	// for a short, unique suffix. Also delete any stale row from
	// a prior run before insert — the full-suite path serialises
	// other tests that may have inserted users with the same
	// hashed prefix.
	username := "smtptest_admin_" + shortHash(t.Name()+"_"+addr)
	_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username)
	var ref int64
	emailArg := any(addr)
	if addr == "" {
		emailArg = nil
	}
	err := pool.QueryRow(ctx, `
		INSERT INTO "user" (username, email, approved) VALUES ($1, $2, 1) RETURNING ref
	`, username, emailArg).Scan(&ref)
	if err != nil {
		t.Fatalf("seed admin user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	emailPtr := &addr
	id := &auth.Identity{
		UserRef: ref, Username: username, Email: emailPtr,
		Capabilities: []string{sysconfig.CapSystemAdmin, sysconfig.CapConfigWrite},
	}
	return auth.WithIdentity(ctx, id)
}

func TestSendSMTPTestEmail_DefaultsToCallerEmail(t *testing.T) {
	withSMTPTestHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture) {
		adminCtx := adminCtxWithEmail(t, ctx, pool, "ops@example.com")
		resp, err := h.SendSMTPTestEmail(adminCtx,
			openapi.SendSMTPTestEmailRequestObject{})
		if err != nil {
			t.Fatalf("SendSMTPTestEmail: %v", err)
		}
		ok, _ := resp.(openapi.SendSMTPTestEmail200JSONResponse)
		if !ok.Sent {
			t.Fatalf("expected Sent=true, got %+v", resp)
		}
		if ok.Recipient != "ops@example.com" {
			t.Errorf("Recipient = %q, want ops@example.com", ok.Recipient)
		}
		if cap.Len() != 1 {
			t.Fatalf("Capture.Len = %d, want 1", cap.Len())
		}
		msg, _ := cap.Last()
		if !strings.Contains(msg.Subject, "Studio Alpha") {
			t.Errorf("captured subject missing site name: %q", msg.Subject)
		}
		if !strings.Contains(msg.TextBody, "Studio Alpha") {
			t.Errorf("captured body missing site name: %q", msg.TextBody)
		}
	})
}

func TestSendSMTPTestEmail_OverrideRecipient(t *testing.T) {
	withSMTPTestHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture) {
		adminCtx := adminCtxWithEmail(t, ctx, pool, "ops@example.com")
		override := openapi_types.Email("debug@external.example.com")
		resp, err := h.SendSMTPTestEmail(adminCtx, openapi.SendSMTPTestEmailRequestObject{
			Body: &openapi.SMTPTestRequest{To: &override},
		})
		if err != nil {
			t.Fatalf("SendSMTPTestEmail: %v", err)
		}
		ok, _ := resp.(openapi.SendSMTPTestEmail200JSONResponse)
		if ok.Recipient != "debug@external.example.com" {
			t.Errorf("override Recipient = %q, want debug@external.example.com", ok.Recipient)
		}
	})
}

func TestSendSMTPTestEmail_NoCallerEmail_NoOverride_400(t *testing.T) {
	withSMTPTestHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture) {
		// Caller has no email on file + no override → 400.
		adminCtx := adminCtxWithEmail(t, ctx, pool, "")
		// Patch the Identity to clear the in-memory Email field too
		// (the lookup uses the DB, the Identity's Email is for
		// header rendering; both must be empty for this test path).
		id := auth.IdentityFromContext(adminCtx)
		id.Email = nil
		resp, err := h.SendSMTPTestEmail(adminCtx,
			openapi.SendSMTPTestEmailRequestObject{})
		if err != nil {
			t.Fatalf("SendSMTPTestEmail: %v", err)
		}
		if _, ok := resp.(openapi.SendSMTPTestEmail400JSONResponse); !ok {
			t.Errorf("expected 400 response, got %T", resp)
		}
		if cap.Len() != 0 {
			t.Errorf("Capture.Len = %d, want 0 (no recipient → no send)", cap.Len())
		}
	})
}

func TestSendSMTPTestEmail_Unauthenticated_401(t *testing.T) {
	withSMTPTestHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture) {
		// No identity on context.
		resp, err := h.SendSMTPTestEmail(ctx, openapi.SendSMTPTestEmailRequestObject{})
		if err != nil {
			t.Fatalf("SendSMTPTestEmail: %v", err)
		}
		if _, ok := resp.(openapi.SendSMTPTestEmail401JSONResponse); !ok {
			t.Errorf("expected 401, got %T", resp)
		}
	})
}

func TestSendSMTPTestEmail_MissingCapability_403(t *testing.T) {
	withSMTPTestHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool, cap *email.Capture) {
		id := &auth.Identity{
			UserRef: 1, Username: "u",
			Capabilities: []string{}, // no admin caps
		}
		nonAdmin := auth.WithIdentity(ctx, id)
		resp, err := h.SendSMTPTestEmail(nonAdmin, openapi.SendSMTPTestEmailRequestObject{})
		if err != nil {
			t.Fatalf("SendSMTPTestEmail: %v", err)
		}
		if _, ok := resp.(openapi.SendSMTPTestEmail403JSONResponse); !ok {
			t.Errorf("expected 403, got %T", resp)
		}
	})
}
