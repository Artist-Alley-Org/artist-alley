// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// fakeSendVerification records every dispatched verification
// email so the test can assert what the user received without
// touching an SMTP relay.
type fakeSendVerification struct {
	mu      sync.Mutex
	calls   []sentVerification
	failErr error
}

type sentVerification struct {
	To, Name, URL, Expires string
}

func (f *fakeSendVerification) Fn() auth.SendVerificationFn {
	return func(_ context.Context, to, name, url, expires string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, sentVerification{To: to, Name: name, URL: url, Expires: expires})
		return f.failErr
	}
}
func (f *fakeSendVerification) Last() (sentVerification, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return sentVerification{}, false
	}
	return f.calls[len(f.calls)-1], true
}
func (f *fakeSendVerification) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func registrationSurface(enabled, verify bool, send *fakeSendVerification) auth.RegisterSurface {
	return auth.RegisterSurface{
		SendVerification: send.Fn(),
		SiteForVerify: func(_ context.Context) (auth.SiteForVerify, error) {
			return auth.SiteForVerify{Name: "Studio Alpha", URL: "https://art.example.com"}, nil
		},
		RegistrationPolicy: func(_ context.Context) (auth.RegistrationConfig, error) {
			return auth.RegistrationConfig{
				Enabled: enabled, RequireEmailVerification: verify, DefaultRole: "Base",
			}, nil
		},
	}
}

func cleanupRegisteredUser(t *testing.T, username string) {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		DELETE FROM email_verification_token WHERE user_ref IN (
			SELECT ref FROM "user" WHERE username = $1
		)`, username)
	_, _ = pool.Exec(ctx, `DELETE FROM user_roles WHERE user_ref IN (SELECT ref FROM "user" WHERE username = $1)`, username)
	_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username)
}

func registerHandlerWith(t *testing.T, surface auth.RegisterSurface) *auth.Handler {
	h := handlerFor(t, openTestPool(t))
	h.SetRegistrationSurface(surface)
	return h
}

func TestRegister_DisabledReturns403(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(false, true, send))
	username := "reg-disabled-" + randHex(4)
	t.Cleanup(func() { cleanupRegisteredUser(t, username) })

	resp, err := h.Register(context.Background(), openapi.RegisterRequestObject{
		Body: &openapi.RegisterRequest{
			Username: username, Email: openapi_types.Email(username + "@example.com"),
			Password: "P@ssw0rd123",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := resp.(openapi.Register403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
	if send.Len() != 0 {
		t.Errorf("no email should have been sent; got %d", send.Len())
	}
}

func TestRegister_HappyPath_SendsVerifyEmail(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	username := "reg-happy-" + randHex(4)
	emailAddr := username + "@example.com"
	t.Cleanup(func() { cleanupRegisteredUser(t, username) })

	resp, err := h.Register(context.Background(), openapi.RegisterRequestObject{
		Body: &openapi.RegisterRequest{
			Username: username, Email: openapi_types.Email(emailAddr),
			Password: "P@ssw0rd123",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ok, isOK := resp.(openapi.Register202JSONResponse)
	if !isOK {
		t.Fatalf("expected 202, got %T (%+v)", resp, resp)
	}
	if ok.UserRef == 0 {
		t.Errorf("UserRef should be set")
	}
	if string(ok.Email) != emailAddr {
		t.Errorf("Email = %q, want %q", ok.Email, emailAddr)
	}
	if send.Len() != 1 {
		t.Fatalf("expected one verification email; got %d", send.Len())
	}
	got, _ := send.Last()
	if got.To != emailAddr {
		t.Errorf("send.To = %q, want %q", got.To, emailAddr)
	}
	if !strings.Contains(got.URL, "/auth/verify?token=") {
		t.Errorf("verify URL malformed: %q", got.URL)
	}
}

func TestRegister_DuplicateUsername_409(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	username := "reg-dup-" + randHex(4)
	t.Cleanup(func() { cleanupRegisteredUser(t, username) })

	body := &openapi.RegisterRequest{
		Username: username, Email: openapi_types.Email(username + "@example.com"),
		Password: "P@ssw0rd123",
	}
	if _, err := h.Register(context.Background(), openapi.RegisterRequestObject{Body: body}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	resp, err := h.Register(context.Background(), openapi.RegisterRequestObject{Body: body})
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if _, ok := resp.(openapi.Register409JSONResponse); !ok {
		t.Errorf("expected 409, got %T", resp)
	}
}

func TestRegister_InvalidUsername_400(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	resp, err := h.Register(context.Background(), openapi.RegisterRequestObject{
		Body: &openapi.RegisterRequest{
			Username: "bad name with spaces",
			Email:    openapi_types.Email("x@example.com"),
			Password: "P@ssw0rd123",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := resp.(openapi.Register400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
}

func TestVerifyEmail_ConsumesToken_AndFlipsUser(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	username := "reg-verify-" + randHex(4)
	emailAddr := username + "@example.com"
	t.Cleanup(func() { cleanupRegisteredUser(t, username) })

	if _, err := h.Register(context.Background(), openapi.RegisterRequestObject{
		Body: &openapi.RegisterRequest{
			Username: username, Email: openapi_types.Email(emailAddr),
			Password: "P@ssw0rd123",
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, _ := send.Last()
	// Extract token from the URL — same path the frontend uses.
	idx := strings.Index(got.URL, "token=")
	if idx < 0 {
		t.Fatalf("URL missing token param: %q", got.URL)
	}
	token := got.URL[idx+len("token="):]

	verResp, err := h.VerifyEmail(context.Background(), openapi.VerifyEmailRequestObject{
		Body: &openapi.VerifyEmailRequest{Token: token},
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if _, ok := verResp.(openapi.VerifyEmail200JSONResponse); !ok {
		t.Fatalf("expected 200, got %T", verResp)
	}

	// Second use of the same token must 400 (single-shot).
	verResp2, err := h.VerifyEmail(context.Background(), openapi.VerifyEmailRequestObject{
		Body: &openapi.VerifyEmailRequest{Token: token},
	})
	if err != nil {
		t.Fatalf("second VerifyEmail: %v", err)
	}
	if _, ok := verResp2.(openapi.VerifyEmail400JSONResponse); !ok {
		t.Errorf("second use of token should 400, got %T", verResp2)
	}

	// Confirm user.email_verified_at is now set.
	pool := openTestPool(t)
	var verified bool
	if err := pool.QueryRow(context.Background(),
		`SELECT email_verified_at IS NOT NULL FROM "user" WHERE username = $1`, username,
	).Scan(&verified); err != nil {
		t.Fatalf("check verified: %v", err)
	}
	if !verified {
		t.Errorf("user.email_verified_at should be set after VerifyEmail")
	}
}

func TestVerifyEmail_BadToken_400(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	resp, err := h.VerifyEmail(context.Background(), openapi.VerifyEmailRequestObject{
		Body: &openapi.VerifyEmailRequest{Token: "not-a-real-token"},
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if _, ok := resp.(openapi.VerifyEmail400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
}

func TestResendVerification_UnknownEmailStillReturns202(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	resp, err := h.ResendVerificationEmail(context.Background(),
		openapi.ResendVerificationEmailRequestObject{
			Body: &openapi.ResendVerificationRequest{Email: openapi_types.Email("nobody-here@example.com")},
		})
	if err != nil {
		t.Fatalf("ResendVerificationEmail: %v", err)
	}
	if _, ok := resp.(openapi.ResendVerificationEmail202Response); !ok {
		t.Errorf("expected 202 (anti-enumeration), got %T", resp)
	}
	if send.Len() != 0 {
		t.Errorf("no email should have been sent for unknown address; got %d", send.Len())
	}
}

func TestResendVerification_UnverifiedUser_SendsAnother(t *testing.T) {
	send := &fakeSendVerification{}
	h := registerHandlerWith(t, registrationSurface(true, true, send))
	username := "reg-resend-" + randHex(4)
	emailAddr := username + "@example.com"
	t.Cleanup(func() { cleanupRegisteredUser(t, username) })

	if _, err := h.Register(context.Background(), openapi.RegisterRequestObject{
		Body: &openapi.RegisterRequest{
			Username: username, Email: openapi_types.Email(emailAddr),
			Password: "P@ssw0rd123",
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	before := send.Len()
	resp, err := h.ResendVerificationEmail(context.Background(),
		openapi.ResendVerificationEmailRequestObject{
			Body: &openapi.ResendVerificationRequest{Email: openapi_types.Email(emailAddr)},
		})
	if err != nil {
		t.Fatalf("ResendVerificationEmail: %v", err)
	}
	if _, ok := resp.(openapi.ResendVerificationEmail202Response); !ok {
		t.Errorf("expected 202, got %T", resp)
	}
	if send.Len() != before+1 {
		t.Errorf("send count = %d, want %d (one more)", send.Len(), before+1)
	}
}
