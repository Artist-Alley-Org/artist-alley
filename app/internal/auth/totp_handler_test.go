package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/auth/totp"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// seedNativePasswordUser creates a user with a bcrypt-hashed
// password we know — so the disable / regenerate paths that
// require current_password can be exercised.
func seedNativePasswordUser(t *testing.T, pool *pgxpool.Pool, label, password string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)
	username := "totp-" + label + "-" + randHex(4)
	hash, err := auth.HashPassword(password, "test-scramble-key")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	user, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username: &username, Password: &hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})
	return user.Ref, username
}

// ensureAtrestInit sets up the atrest package with a test key if
// not already initialised. Tests that touch the TOTP handler
// need this since enroll encrypts the secret on the way in.
func ensureAtrestInit(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	key := make([]byte, atrest.MasterKeyLen)
	for i := range key {
		key[i] = byte((i * 17) ^ 0x33)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

func ctxAs(ref int64, username string) context.Context {
	id := &auth.Identity{
		UserRef: ref, Username: username, AuthMethod: "session",
	}
	return auth.WithIdentity(context.Background(), id)
}

func TestGetMyTOTP_NotEnrolled(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "status", "P@ssw0rd123")

	h := handlerFor(t, pool)
	resp, err := h.GetMyTOTP(ctxAs(user, name), openapi.GetMyTOTPRequestObject{})
	if err != nil {
		t.Fatalf("GetMyTOTP: %v", err)
	}
	ok, _ := resp.(openapi.GetMyTOTP200JSONResponse)
	if ok.Enrolled || ok.Confirmed {
		t.Errorf("fresh user should be Enrolled=false Confirmed=false; got %+v", ok)
	}
}

func TestEnrollMyTOTP_ReturnsSecretAndURI(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "enroll", "P@ssw0rd123")

	h := handlerFor(t, pool)
	resp, err := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	if err != nil {
		t.Fatalf("EnrollMyTOTP: %v", err)
	}
	ok, isOK := resp.(openapi.EnrollMyTOTP200JSONResponse)
	if !isOK {
		t.Fatalf("expected 200, got %T", resp)
	}
	if len(ok.SecretBase32) < 16 {
		t.Errorf("secret_base32 too short: %q", ok.SecretBase32)
	}
	if ok.OtpauthUrl == "" || ok.OtpauthUrl[:15] != "otpauth://totp/" {
		t.Errorf("otpauth_url malformed: %q", ok.OtpauthUrl)
	}

	// Re-enroll while unconfirmed: should succeed (overwrite).
	resp2, err := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	if err != nil {
		t.Fatalf("re-enroll unconfirmed: %v", err)
	}
	if _, ok := resp2.(openapi.EnrollMyTOTP200JSONResponse); !ok {
		t.Errorf("re-enroll-unconfirmed should 200, got %T", resp2)
	}
}

func TestConfirmMyTOTP_HappyPathReturnsRecoveryCodes(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "confirm", "P@ssw0rd123")

	h := handlerFor(t, pool)
	enrollResp, err := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	er := enrollResp.(openapi.EnrollMyTOTP200JSONResponse)
	secret, err := totp.DecodeSecret(er.SecretBase32)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	code := totp.Code(secret, time.Now().Unix())

	resp, err := h.ConfirmMyTOTP(ctxAs(user, name),
		openapi.ConfirmMyTOTPRequestObject{Body: &openapi.TOTPConfirmRequest{Code: code}})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	ok, isOK := resp.(openapi.ConfirmMyTOTP200JSONResponse)
	if !isOK {
		t.Fatalf("expected 200, got %T (%+v)", resp, resp)
	}
	if len(ok.RecoveryCodes) != 10 {
		t.Errorf("recovery_codes len = %d, want 10", len(ok.RecoveryCodes))
	}

	// Now /status should show Confirmed=true + RecoveryCodesRemaining=10.
	statusResp, _ := h.GetMyTOTP(ctxAs(user, name), openapi.GetMyTOTPRequestObject{})
	s := statusResp.(openapi.GetMyTOTP200JSONResponse)
	if !s.Confirmed {
		t.Errorf("post-confirm status should be Confirmed=true; got %+v", s)
	}
	if s.RecoveryCodesRemaining != 10 {
		t.Errorf("post-confirm RecoveryCodesRemaining = %d, want 10", s.RecoveryCodesRemaining)
	}
}

func TestConfirmMyTOTP_WrongCodeRejected(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "wrong-code", "P@ssw0rd123")

	h := handlerFor(t, pool)
	_, _ = h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	resp, err := h.ConfirmMyTOTP(ctxAs(user, name),
		openapi.ConfirmMyTOTPRequestObject{Body: &openapi.TOTPConfirmRequest{Code: "000000"}})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, ok := resp.(openapi.ConfirmMyTOTP400JSONResponse); !ok {
		t.Errorf("wrong code should 400, got %T", resp)
	}
}

func TestEnrollMyTOTP_BlockedAfterConfirm(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "blocked", "P@ssw0rd123")

	h := handlerFor(t, pool)
	enrollResp, _ := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	er := enrollResp.(openapi.EnrollMyTOTP200JSONResponse)
	secret, _ := totp.DecodeSecret(er.SecretBase32)
	_, _ = h.ConfirmMyTOTP(ctxAs(user, name),
		openapi.ConfirmMyTOTPRequestObject{Body: &openapi.TOTPConfirmRequest{Code: totp.Code(secret, time.Now().Unix())}})

	// Re-enroll after confirm → 409.
	resp, err := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if _, ok := resp.(openapi.EnrollMyTOTP409JSONResponse); !ok {
		t.Errorf("re-enroll after confirm should 409, got %T", resp)
	}
}

func TestDisableMyTOTP_RequiresCorrectPassword(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "disable", "P@ssw0rd123")

	h := handlerFor(t, pool)
	enrollResp, _ := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	er := enrollResp.(openapi.EnrollMyTOTP200JSONResponse)
	secret, _ := totp.DecodeSecret(er.SecretBase32)
	_, _ = h.ConfirmMyTOTP(ctxAs(user, name),
		openapi.ConfirmMyTOTPRequestObject{Body: &openapi.TOTPConfirmRequest{Code: totp.Code(secret, time.Now().Unix())}})

	// Wrong password → 400.
	resp, err := h.DisableMyTOTP(ctxAs(user, name),
		openapi.DisableMyTOTPRequestObject{Body: &openapi.TOTPDisableRequest{CurrentPassword: "wrong"}})
	if err != nil {
		t.Fatalf("disable wrong: %v", err)
	}
	if _, ok := resp.(openapi.DisableMyTOTP400JSONResponse); !ok {
		t.Errorf("wrong password should 400, got %T", resp)
	}

	// Correct password → 204 + status flips back to not-enrolled.
	resp, err = h.DisableMyTOTP(ctxAs(user, name),
		openapi.DisableMyTOTPRequestObject{Body: &openapi.TOTPDisableRequest{CurrentPassword: "P@ssw0rd123"}})
	if err != nil {
		t.Fatalf("disable correct: %v", err)
	}
	if _, ok := resp.(openapi.DisableMyTOTP204Response); !ok {
		t.Errorf("correct password should 204, got %T", resp)
	}
	statusResp, _ := h.GetMyTOTP(ctxAs(user, name), openapi.GetMyTOTPRequestObject{})
	s := statusResp.(openapi.GetMyTOTP200JSONResponse)
	if s.Enrolled || s.Confirmed {
		t.Errorf("post-disable status should be unenrolled; got %+v", s)
	}
}

func TestRegenerateMyRecoveryCodes_ReplacesPriorBatch(t *testing.T) {
	pool := openTestPool(t)
	ensureAtrestInit(t)
	user, name := seedNativePasswordUser(t, pool, "regen", "P@ssw0rd123")

	h := handlerFor(t, pool)
	enrollResp, _ := h.EnrollMyTOTP(ctxAs(user, name), openapi.EnrollMyTOTPRequestObject{})
	er := enrollResp.(openapi.EnrollMyTOTP200JSONResponse)
	secret, _ := totp.DecodeSecret(er.SecretBase32)
	confResp, _ := h.ConfirmMyTOTP(ctxAs(user, name),
		openapi.ConfirmMyTOTPRequestObject{Body: &openapi.TOTPConfirmRequest{Code: totp.Code(secret, time.Now().Unix())}})
	first := confResp.(openapi.ConfirmMyTOTP200JSONResponse).RecoveryCodes

	resp, err := h.RegenerateMyRecoveryCodes(ctxAs(user, name),
		openapi.RegenerateMyRecoveryCodesRequestObject{Body: &openapi.TOTPDisableRequest{CurrentPassword: "P@ssw0rd123"}})
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	ok, _ := resp.(openapi.RegenerateMyRecoveryCodes200JSONResponse)
	if len(ok.RecoveryCodes) != 10 {
		t.Errorf("regen len = %d, want 10", len(ok.RecoveryCodes))
	}
	// First batch is no longer one of the new codes (set
	// intersection should be empty).
	old := map[string]bool{}
	for _, c := range first {
		old[c] = true
	}
	for _, c := range ok.RecoveryCodes {
		if old[c] {
			t.Errorf("regen batch contained an old code (%q) — recovery rotation didn't actually rotate", c)
		}
	}
}
