package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/auth/totp"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// enrollFixtureUserForTOTP plants a confirmed enrollment + N
// recovery codes for the fixture's user, returning the plaintext
// secret + the first recovery code so the test can drive both
// happy paths.
func enrollFixtureUserForTOTP(t *testing.T, fx *fixture) (secret []byte, recoveryCode string) {
	t.Helper()
	// atrest must be initialised before we encrypt — the fixture
	// runs without it by default.
	if !atrest.Initialised() {
		key := make([]byte, atrest.MasterKeyLen)
		for i := range key {
			key[i] = byte((i * 41) ^ 0x77)
		}
		if err := atrest.InitWithKey(key); err != nil {
			t.Fatalf("atrest init: %v", err)
		}
	}
	var err error
	secret, err = totp.GenerateSecret()
	if err != nil {
		t.Fatalf("gen secret: %v", err)
	}
	enc, err := atrest.Encrypt(secret)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	ctx := context.Background()
	// Pre-clean prior TOTP state so the test is idempotent.
	_, _ = fx.pool.Exec(ctx, `DELETE FROM user_totp_recovery_code WHERE user_ref = $1`, fx.userRef)
	_, _ = fx.pool.Exec(ctx, `DELETE FROM user_totp WHERE user_ref = $1`, fx.userRef)
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO user_totp (user_ref, secret_enc, confirmed_at)
		VALUES ($1, $2, NOW())
	`, fx.userRef, enc); err != nil {
		t.Fatalf("seed totp row: %v", err)
	}
	codes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("gen recovery: %v", err)
	}
	for _, c := range codes {
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO user_totp_recovery_code (user_ref, code_hash) VALUES ($1, $2)`,
			fx.userRef, totp.HashRecoveryCode(c),
		); err != nil {
			t.Fatalf("seed recovery: %v", err)
		}
	}
	recoveryCode = codes[0]
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM user_totp_recovery_code WHERE user_ref = $1`, fx.userRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM user_totp WHERE user_ref = $1`, fx.userRef)
	})
	return
}

func TestLogin_2FA_RequiredWhenCodeMissing(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		_, _ = enrollFixtureUserForTOTP(t, fx)

		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		var body401 struct{ Error string }
		mustDecode(t, resp, &body401)
		if body401.Error != "2fa_required" {
			t.Errorf("error = %q, want 2fa_required", body401.Error)
		}
		// No cookie should have been set — the user is not logged in.
		for _, c := range resp.Cookies() {
			if c.Name == SessionCookieName && c.Value != "" {
				t.Errorf("response set a session cookie despite 2fa_required: %s=%q", c.Name, c.Value)
			}
		}
	})
}

func TestLogin_2FA_AcceptsValidTOTPCode(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		secret, _ := enrollFixtureUserForTOTP(t, fx)
		code := totp.Code(secret, time.Now().Unix())

		body := openapi.LoginJSONRequestBody{
			Username: fx.username, Password: fx.password, TotpCode: &code,
		}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, readBody(resp))
		}
		// Cookie set → logged in.
		gotCookie := false
		for _, c := range resp.Cookies() {
			if c.Name == SessionCookieName && c.Value != "" {
				gotCookie = true
			}
		}
		if !gotCookie {
			t.Errorf("expected session cookie on valid 2FA login")
		}
	})
}

func TestLogin_2FA_RejectsWrongCode(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		_, _ = enrollFixtureUserForTOTP(t, fx)
		bad := "000000"
		body := openapi.LoginJSONRequestBody{
			Username: fx.username, Password: fx.password, TotpCode: &bad,
		}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		var body401 struct{ Error string }
		mustDecode(t, resp, &body401)
		if body401.Error != "invalid_2fa_code" {
			t.Errorf("error = %q, want invalid_2fa_code", body401.Error)
		}
	})
}

func TestLogin_2FA_AcceptsRecoveryCodeAndMarksUsed(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		_, recovery := enrollFixtureUserForTOTP(t, fx)

		body := openapi.LoginJSONRequestBody{
			Username: fx.username, Password: fx.password, TotpCode: &recovery,
		}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, readBody(resp))
		}
		// Same recovery code should now be REJECTED on a second
		// login (single-use).
		resp2 := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Errorf("second use of recovery code should 401; got %d", resp2.StatusCode)
		}
		// Count of unused recovery codes dropped by 1.
		var unused int64
		if err := fx.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM user_totp_recovery_code WHERE user_ref = $1 AND used_at IS NULL`,
			fx.userRef,
		).Scan(&unused); err != nil {
			t.Fatalf("count unused: %v", err)
		}
		if unused != 9 {
			t.Errorf("unused recovery count = %d, want 9 (one consumed)", unused)
		}
	})
}

func TestLogin_2FA_NotEnrolledPassthrough(t *testing.T) {
	// Sanity check that the gate is a no-op for users with no
	// enrollment — guards against an accidental regression
	// gating every login.
	withFixture(t, func(ctx context.Context, fx *fixture) {
		_, _ = fx.pool.Exec(ctx, `DELETE FROM user_totp WHERE user_ref = $1`, fx.userRef)
		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("non-2FA user login regressed; status=%d body=%s",
				resp.StatusCode, readBody(resp))
		}
	})
}
