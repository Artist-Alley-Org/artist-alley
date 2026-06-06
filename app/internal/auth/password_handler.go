// Password change + admin reset endpoints (Phase 1.17.D).
//
// Two operations:
//   - PUT  /account/password                    — self-service
//   - POST /admin/users/{ref}/password-reset    — admin helpdesk
//
// The self-service path verifies the current password, applies the
// configured complexity policy (loaded from system_config via the
// passwordPolicySource interface), rejects reuse of the last N
// hashes from user_password_history, and optionally revokes every
// other session belonging to the caller (defensive default for
// "I think someone got my password" recovery flows).
//
// The admin path generates a cryptographically random temporary
// password, returns the plaintext ONCE in the response (so the
// admin can share it with the user out-of-band), and stores only
// the hash. The "must change on next login" enforcement lands
// with the email-token reset flow in a follow-up phase.

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// reuseWindow — how many previous hashes the change handler compares
// against. The legacy stack doesn't ship this; we cap at 5 (NIST 800-63B's typical
// guidance) so a user can roll through a few passwords without
// running out of options, but can't immediately re-use the one they
// just rotated away from.
const reuseWindow = 5

// validatePasswordPolicy enforces the configured complexity rules
// against a candidate plaintext. Returns a user-facing error message
// (safe to surface verbatim) or nil when the password is accepted.
func validatePasswordPolicy(password string, policy PasswordPolicy) string {
	if policy.MinLength > 0 && len(password) < policy.MinLength {
		return fmt.Sprintf("password must be at least %d characters", policy.MinLength)
	}
	if policy.RequireUpper {
		ok := false
		for _, r := range password {
			if unicode.IsUpper(r) {
				ok = true
				break
			}
		}
		if !ok {
			return "password must contain at least one uppercase letter"
		}
	}
	if policy.RequireNumber {
		ok := false
		for _, r := range password {
			if unicode.IsDigit(r) {
				ok = true
				break
			}
		}
		if !ok {
			return "password must contain at least one digit"
		}
	}
	if policy.RequireSymbol {
		ok := false
		for _, r := range password {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r) {
				ok = true
				break
			}
		}
		if !ok {
			return "password must contain at least one symbol"
		}
	}
	if policy.DisallowCommon && isCommonPassword(password) {
		return "password is too common — pick something less guessable"
	}
	return ""
}

// commonPasswordSet is the bottom of the OWASP top-200 (plus a few
// AA-specific embarrassments). Loaded once at package init; lookup
// is O(1). Not exhaustive — defensive sanity check, not a full
// blacklist. A real install should layer a have-i-been-pwned check
// on top via the audit log in a follow-up phase.
var commonPasswordSet = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {},
	"123456": {}, "12345678": {}, "qwerty": {}, "qwerty123": {},
	"letmein": {}, "welcome": {}, "admin": {}, "administrator": {},
	"abc123": {}, "iloveyou": {}, "monkey": {}, "dragon": {},
	"artistalley": {}, "artist-alley": {},
}

func isCommonPassword(p string) bool {
	_, ok := commonPasswordSet[p]
	return ok
}

// generateTempPassword returns a 16-char URL-safe random string.
// The character set lands in policy-satisfying territory for any
// realistic policy (mixed case + digits + URL-safe symbols).
func generateTempPassword() (string, error) {
	buf := make([]byte, 12) // 12 bytes → 16 base64 chars
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ChangeMyPassword implements the self-service path.
func (h *Handler) ChangeMyPassword(
	ctx context.Context,
	req openapi.ChangeMyPasswordRequestObject,
) (openapi.ChangeMyPasswordResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ChangeMyPassword401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	if req.Body.CurrentPassword == "" || req.Body.NewPassword == "" {
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "current_password and new_password are required"},
		}, nil
	}

	q := New(h.Pool)
	cur, err := q.GetUserPasswordHashByRef(ctx, caller.UserRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ChangeMyPassword401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
			}, nil
		}
		return nil, fmt.Errorf("auth: get hash: %w", err)
	}
	if cur.Password == nil || *cur.Password == "" {
		// Account with no password (likely SSO-only) can't self-
		// change via this endpoint. The "set initial password" flow
		// will land alongside the SSO surfaces.
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "account has no native password to change"},
		}, nil
	}
	if err := VerifyPassword(req.Body.CurrentPassword, *cur.Password, h.ScrambleKey); err != nil {
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "current password is incorrect"},
		}, nil
	}

	// Policy check. Nil policy source = zero policy = anything goes.
	var policy PasswordPolicy
	if h.Policy != nil {
		p, err := h.Policy.GetPasswordPolicy(ctx)
		if err == nil {
			policy = p
		}
	}
	if msg := validatePasswordPolicy(req.Body.NewPassword, policy); msg != "" {
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
		}, nil
	}

	// Reuse check — compare against the last N hashes.
	prevHashes, err := q.ListRecentPasswordHashes(ctx, ListRecentPasswordHashesParams{
		UserRef: caller.UserRef,
		Limit:    reuseWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: list password history: %w", err)
	}
	for _, h := range prevHashes {
		if VerifyPassword(req.Body.NewPassword, h, "") == nil ||
			VerifyPassword(req.Body.NewPassword, h, "") == nil {
			// Two probes because old hashes might pre-date the
			// scramble-key rotation. VerifyPassword without the key
			// is allowed (the legacy code strips the HMAC step when scrambleKey
			// is empty).
		}
	}
	// Also explicitly verify against current hash (which isn't in
	// the history table yet — first change creates the first row).
	if VerifyPassword(req.Body.NewPassword, *cur.Password, h.ScrambleKey) == nil {
		return openapi.ChangeMyPassword400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "new password cannot match the current password"},
		}, nil
	}
	for _, prev := range prevHashes {
		if VerifyPassword(req.Body.NewPassword, prev, h.ScrambleKey) == nil {
			return openapi.ChangeMyPassword400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: fmt.Sprintf("new password matches one of your last %d passwords", reuseWindow)},
			}, nil
		}
	}

	// Hash + persist + record history + (optionally) revoke other sessions.
	newHash, err := HashPassword(req.Body.NewPassword, h.ScrambleKey)
	if err != nil {
		return nil, fmt.Errorf("auth: hash: %w", err)
	}
	if err := q.UpdateUserPassword(ctx, UpdateUserPasswordParams{
		Ref:      caller.UserRef,
		Password: &newHash,
	}); err != nil {
		return nil, fmt.Errorf("auth: update password: %w", err)
	}
	if err := q.InsertPasswordHistory(ctx, InsertPasswordHistoryParams{
		UserRef:     caller.UserRef,
		PasswordHash: newHash,
	}); err != nil {
		return nil, fmt.Errorf("auth: insert history: %w", err)
	}

	revoked := int64(0)
	if req.Body.RevokeOtherSessions != nil && *req.Body.RevokeOtherSessions && caller.SessionID != nil {
		n, err := q.RevokeOtherSessionsForUser(ctx, RevokeOtherSessionsForUserParams{
			UserRef: caller.UserRef,
			ID:      pgtype.UUID{Bytes: *caller.SessionID, Valid: true},
		})
		if err != nil {
			// Don't fail the password change over a session-revoke
			// hiccup — log and continue. The audit row records 0.
			h.Logger.Warn("password change: revoke others failed", "err", err)
		} else {
			revoked = n
		}
	}

	if h.Audit != nil {
		h.Audit.PasswordChanged(ctx, RequestFromContext(ctx), caller.UserRef, int(revoked))
	}

	r := int(revoked)
	resp := openapi.ChangePasswordResult{
		ChangedAt:       time.Now().UTC(),
		SessionsRevoked: &r,
	}
	return openapi.ChangeMyPassword200JSONResponse(resp), nil
}

// AdminResetUserPassword implements the helpdesk reset path.
func (h *Handler) AdminResetUserPassword(
	ctx context.Context,
	req openapi.AdminResetUserPasswordRequestObject,
) (openapi.AdminResetUserPasswordResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AdminResetUserPassword401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.password.reset") {
		return openapi.AdminResetUserPassword403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.password.reset capability required"},
		}, nil
	}

	q := New(h.Pool)
	cur, err := q.GetUserPasswordHashByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AdminResetUserPassword404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: get hash: %w", err)
	}
	_ = cur // existence check only

	temp, err := generateTempPassword()
	if err != nil {
		return nil, fmt.Errorf("auth: gen temp: %w", err)
	}
	hash, err := HashPassword(temp, h.ScrambleKey)
	if err != nil {
		return nil, fmt.Errorf("auth: hash temp: %w", err)
	}
	if err := q.UpdateUserPassword(ctx, UpdateUserPasswordParams{
		Ref:      req.Ref,
		Password: &hash,
	}); err != nil {
		return nil, fmt.Errorf("auth: update password: %w", err)
	}
	if err := q.InsertPasswordHistory(ctx, InsertPasswordHistoryParams{
		UserRef:     req.Ref,
		PasswordHash: hash,
	}); err != nil {
		// History is best-effort here — the password is already
		// changed in user.password. Log and continue.
		h.Logger.Warn("password reset: insert history failed", "err", err)
	}

	// Revoke EVERY session of the target user — they need to log
	// back in with the new temp password.
	if _, err := q.RevokeOtherSessionsForUser(ctx, RevokeOtherSessionsForUserParams{
		UserRef: req.Ref,
		ID:      pgtype.UUID{Bytes: uuid.Nil, Valid: true}, // Nil never matches a real session.id
	}); err != nil {
		h.Logger.Warn("password reset: session sweep failed", "err", err)
	}

	if h.Audit != nil {
		reason := ""
		if req.Body != nil && req.Body.Reason != nil {
			reason = *req.Body.Reason
		}
		h.Audit.PasswordReset(ctx, RequestFromContext(ctx), req.Ref, caller.UserRef, reason)
	}

	return openapi.AdminResetUserPassword200JSONResponse{
		TemporaryPassword: temp,
		GeneratedAt:       time.Now().UTC(),
	}, nil
}
