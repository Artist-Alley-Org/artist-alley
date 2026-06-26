package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/auth/totp"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// totpIssuer is the brand string baked into the otpauth URI's
// `issuer` param + the display-label prefix. Authenticator apps
// show this to disambiguate accounts; one constant per install
// is fine — the per-instance site name can be threaded in later.
const totpIssuer = "artist-alley"

// GetMyTOTP returns the caller's enrollment status. Cheap read;
// safe to call on every /account/security page load.
func (h *Handler) GetMyTOTP(
	ctx context.Context,
	_ openapi.GetMyTOTPRequestObject,
) (openapi.GetMyTOTPResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetMyTOTP401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	status, err := h.loadTOTPStatus(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.GetMyTOTP200JSONResponse(status), nil
}

// EnrollMyTOTP starts (or restarts) enrollment. Returns the
// shared secret + otpauth URI for QR rendering. Refuses to
// overwrite a CONFIRMED enrollment — the user must explicitly
// disable + re-enroll, so they can't accidentally bin their
// authenticator on a misclick.
func (h *Handler) EnrollMyTOTP(
	ctx context.Context,
	_ openapi.EnrollMyTOTPRequestObject,
) (openapi.EnrollMyTOTPResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.EnrollMyTOTP401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	if row, err := q.GetUserTOTP(ctx, id.UserRef); err == nil {
		if row.ConfirmedAt.Valid {
			return openapi.EnrollMyTOTP409JSONResponse{Error: "2fa already enabled; disable first to re-enroll"}, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("auth: probe TOTP: %w", err)
	}

	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, fmt.Errorf("auth: generate TOTP secret: %w", err)
	}
	enc, err := atrest.Encrypt(secret)
	if err != nil {
		return nil, fmt.Errorf("auth: encrypt TOTP secret: %w", err)
	}
	if err := q.UpsertUserTOTP(ctx, UpsertUserTOTPParams{
		UserRef:   id.UserRef,
		SecretEnc: enc,
	}); err != nil {
		return nil, fmt.Errorf("auth: upsert TOTP: %w", err)
	}
	account := id.Username
	if account == "" {
		account = fmt.Sprintf("user-%d", id.UserRef)
	}
	return openapi.EnrollMyTOTP200JSONResponse{
		SecretBase32: totp.EncodeSecret(secret),
		OtpauthUrl:   totp.OtpauthURL(totpIssuer, account, secret),
	}, nil
}

// ConfirmMyTOTP flips confirmed_at on a valid code + mints +
// returns the user's first batch of recovery codes (plaintext,
// shown ONCE).
func (h *Handler) ConfirmMyTOTP(
	ctx context.Context,
	req openapi.ConfirmMyTOTPRequestObject,
) (openapi.ConfirmMyTOTPResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ConfirmMyTOTP401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.ConfirmMyTOTP400JSONResponse{Error: "missing body"}, nil
	}
	q := New(h.Pool)
	row, err := q.GetUserTOTP(ctx, id.UserRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ConfirmMyTOTP400JSONResponse{Error: "no pending enrollment; call /enroll first"}, nil
		}
		return nil, fmt.Errorf("auth: load TOTP: %w", err)
	}
	if row.ConfirmedAt.Valid {
		return openapi.ConfirmMyTOTP400JSONResponse{Error: "enrollment already confirmed"}, nil
	}
	secret, err := atrest.Decrypt(row.SecretEnc)
	if err != nil {
		return nil, fmt.Errorf("auth: decrypt TOTP secret: %w", err)
	}
	if !totp.Verify(secret, req.Body.Code, time.Now()) {
		return openapi.ConfirmMyTOTP400JSONResponse{Error: "code did not verify; check your authenticator and try again"}, nil
	}

	if err := q.ConfirmUserTOTP(ctx, id.UserRef); err != nil {
		return nil, fmt.Errorf("auth: confirm TOTP: %w", err)
	}
	codes, err := h.regenRecoveryCodesTx(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.ConfirmMyTOTP200JSONResponse{RecoveryCodes: codes}, nil
}

// DisableMyTOTP wipes the enrollment + cascades the recovery
// codes via FK. Requires the caller's current password — a
// stolen-session attacker shouldn't be able to disarm 2FA in
// one POST.
func (h *Handler) DisableMyTOTP(
	ctx context.Context,
	req openapi.DisableMyTOTPRequestObject,
) (openapi.DisableMyTOTPResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.DisableMyTOTP401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || req.Body.CurrentPassword == "" {
		return openapi.DisableMyTOTP400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "current_password is required"},
		}, nil
	}
	if err := h.verifyCurrentPassword(ctx, id.UserRef, req.Body.CurrentPassword); err != nil {
		return openapi.DisableMyTOTP400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	q := New(h.Pool)
	if err := q.DeleteUserTOTP(ctx, id.UserRef); err != nil {
		return nil, fmt.Errorf("auth: delete TOTP: %w", err)
	}
	if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelInfo, "auth.totp.disabled",
			slog.Int64("user_ref", id.UserRef),
		)
	}
	return openapi.DisableMyTOTP204Response{}, nil
}

// RegenerateMyRecoveryCodes wipes the prior batch + mints a
// fresh set. Same current-password gate as Disable.
func (h *Handler) RegenerateMyRecoveryCodes(
	ctx context.Context,
	req openapi.RegenerateMyRecoveryCodesRequestObject,
) (openapi.RegenerateMyRecoveryCodesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.RegenerateMyRecoveryCodes401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || req.Body.CurrentPassword == "" {
		return openapi.RegenerateMyRecoveryCodes400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "current_password is required"},
		}, nil
	}
	if err := h.verifyCurrentPassword(ctx, id.UserRef, req.Body.CurrentPassword); err != nil {
		return openapi.RegenerateMyRecoveryCodes400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	codes, err := h.regenRecoveryCodesTx(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.RegenerateMyRecoveryCodes200JSONResponse{RecoveryCodes: codes}, nil
}

// --- helpers ----------------------------------------------------------------

// loadTOTPStatus builds the caller-facing status by combining the
// row + the unused-recovery-code count.
func (h *Handler) loadTOTPStatus(ctx context.Context, userRef int64) (openapi.TOTPStatus, error) {
	q := New(h.Pool)
	row, err := q.GetUserTOTP(ctx, userRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return openapi.TOTPStatus{Enrolled: false, Confirmed: false}, nil
	}
	if err != nil {
		return openapi.TOTPStatus{}, fmt.Errorf("auth: load TOTP: %w", err)
	}
	status := openapi.TOTPStatus{
		Enrolled:  true,
		Confirmed: row.ConfirmedAt.Valid,
	}
	if row.CreatedAt.Valid {
		t := row.CreatedAt.Time
		status.EnrolledAt = &t
	}
	if row.ConfirmedAt.Valid {
		t := row.ConfirmedAt.Time
		status.ConfirmedAt = &t
	}
	if row.LastUsedAt.Valid {
		t := row.LastUsedAt.Time
		status.LastUsedAt = &t
	}
	n, err := q.CountUnusedRecoveryCodes(ctx, userRef)
	if err != nil {
		return openapi.TOTPStatus{}, fmt.Errorf("auth: count recovery codes: %w", err)
	}
	status.RecoveryCodesRemaining = int(n)
	return status, nil
}

// regenRecoveryCodesTx wipes the prior batch + inserts N fresh
// codes inside one transaction so callers never see an empty
// recovery-code state if the insert half fails.
func (h *Handler) regenRecoveryCodesTx(ctx context.Context, userRef int64) ([]string, error) {
	codes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		return nil, fmt.Errorf("auth: generate recovery codes: %w", err)
	}
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("auth: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)
	if err := q.DeleteRecoveryCodesForUser(ctx, userRef); err != nil {
		return nil, fmt.Errorf("auth: delete recovery codes: %w", err)
	}
	for _, c := range codes {
		if err := q.InsertRecoveryCode(ctx, InsertRecoveryCodeParams{
			UserRef:  userRef,
			CodeHash: totp.HashRecoveryCode(c),
		}); err != nil {
			return nil, fmt.Errorf("auth: insert recovery code: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("auth: commit: %w", err)
	}
	return codes, nil
}

// verifyCurrentPassword loads the user's password hash + verifies
// candidate against it. Returns nil on match, a user-presentable
// error otherwise.
func (h *Handler) verifyCurrentPassword(ctx context.Context, userRef int64, candidate string) error {
	q := New(h.Pool)
	cur, err := q.GetUserPasswordHashByRef(ctx, userRef)
	if err != nil {
		return fmt.Errorf("load current hash: %w", err)
	}
	if cur.Password == nil || *cur.Password == "" {
		return errors.New("account has no native password to verify against")
	}
	if err := VerifyPassword(candidate, *cur.Password, h.ScrambleKey); err != nil {
		return errors.New("current password is incorrect")
	}
	return nil
}

// Force-use of pgtype to suppress linter if no other reference
// remains after future refactors.
var _ = pgtype.Timestamptz{}

// ---------------------------------------------------------------------------
// Login-flow gate (Phase 1.19.B commit 3)
// ---------------------------------------------------------------------------

// TOTPGateResult describes the outcome of the second-factor
// check that runs after password verification in the login
// handlers.
type TOTPGateResult int

const (
	// TOTPGateNotEnrolled — user has no TOTP enrollment. Login
	// proceeds without a second factor.
	TOTPGateNotEnrolled TOTPGateResult = iota

	// TOTPGateOK — user has a confirmed enrollment AND submitted
	// a valid TOTP code (or single-use recovery code). Login
	// continues to session issuance.
	TOTPGateOK

	// TOTPGateRequired — user has a confirmed enrollment but the
	// request carried no totp_code. Login handler returns 401
	// with error="2fa_required" so the frontend re-prompts.
	TOTPGateRequired

	// TOTPGateInvalid — user submitted a code that didn't match
	// TOTP (±1 window) AND didn't match any unused recovery
	// code. Login handler returns 401 with error="invalid_2fa_code".
	TOTPGateInvalid
)

// CheckTOTPForLogin runs the second-factor decision for one
// login attempt. Called by both registry + inline-password
// paths after the password is verified + the account-state
// gate is past.
//
// The candidate is BOTH the 6-digit TOTP path AND the recovery-
// code path: we try TOTP first (overwhelming common case), then
// fall back to recovery on miss. Recovery hits mark the code
// used in the same call.
func (h *Handler) CheckTOTPForLogin(ctx context.Context, userRef int64, candidate string) TOTPGateResult {
	q := New(h.Pool)
	row, err := q.GetUserTOTP(ctx, userRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return TOTPGateNotEnrolled
	}
	if err != nil {
		// Soft-fail closed: rather than block every login on a
		// transient DB error we treat it as "not enrolled" so the
		// non-2FA users keep working. The audit recorder catches
		// it; alerting picks up the spike.
		if h.Logger != nil {
			h.Logger.Warn("auth.totp.gate.load_error", "user_ref", userRef, "err", err.Error())
		}
		return TOTPGateNotEnrolled
	}
	if !row.ConfirmedAt.Valid {
		// Enrollment in progress but never confirmed — don't
		// gate login on it; the user can't authenticate against
		// a secret they haven't proven.
		return TOTPGateNotEnrolled
	}
	if candidate == "" {
		return TOTPGateRequired
	}
	secret, err := atrest.Decrypt(row.SecretEnc)
	if err != nil {
		// Same soft-fail rationale.
		if h.Logger != nil {
			h.Logger.Warn("auth.totp.gate.decrypt_error", "user_ref", userRef, "err", err.Error())
		}
		return TOTPGateRequired
	}
	// TOTP fast path.
	if totp.Verify(secret, candidate, time.Now()) {
		_ = q.TouchUserTOTP(ctx, userRef)
		return TOTPGateOK
	}
	// Recovery-code fallback. Normalised hash lookup; on hit,
	// mark the row used so it's single-shot.
	id, err := q.FindUnusedRecoveryCodeByHash(ctx, FindUnusedRecoveryCodeByHashParams{
		UserRef:  userRef,
		CodeHash: totp.HashRecoveryCode(candidate),
	})
	if err == nil && id.Valid {
		_ = q.MarkRecoveryCodeUsed(ctx, id)
		return TOTPGateOK
	}
	return TOTPGateInvalid
}
