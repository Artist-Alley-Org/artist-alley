// Package auth implements the artist-alley authentication layer:
// login, logout, /me, and personal access tokens.
//
// The HTTP contract is the relevant slice of `app/api/openapi.yaml`;
// generated types and the StrictServerInterface live in
// `app/internal/openapi`.
//
// Layout:
//
//	queries.sql            -- sqlc input
//	queries.sql.go,        -- sqlc generated; regenerate via
//	  db.go, models.go         scripts/generate.sh
//	password.go            -- RS-compatible HMAC-then-bcrypt hashing
//	session.go             -- session-token + API-token generation
//	                          and the rs_session cookie helpers
//	middleware.go          -- ResolveIdentity + RequireAuth middlewares,
//	                          Identity stored in request ctx
//	handler.go             -- strict-server method implementations
//	*_test.go              -- integration tests against live Postgres
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler implements the auth-related slice of openapi.StrictServerInterface.
type Handler struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	ScrambleKey string
	SessionDays int // how long the session cookie lives; matches RS default

	Sessions *SessionManager
	Limiter  *LoginLimiter
	Audit    auditRecorder

	tokenPrefix string // overridable in tests
}

// auditRecorder is the subset of audit.Recorder that the auth handler
// uses. Declared as an interface here so the auth package doesn't
// import the audit package directly — keeps the dependency arrow
// shallow and makes tests trivial to stub.
type auditRecorder interface {
	LoginSucceeded(ctx context.Context, req *http.Request, userRef int64, sessionID string)
	LoginFailed(ctx context.Context, req *http.Request, attemptedUsername string, userRef *int64, reason string)
	LoginRateLimited(ctx context.Context, req *http.Request, attemptedUsername, key string)
	Logout(ctx context.Context, req *http.Request, userRef int64, sessionID string)
}

// nopAudit is the default Audit when none is wired up — useful in
// older tests that haven't been updated to pass a recorder.
type nopAudit struct{}

func (nopAudit) LoginSucceeded(context.Context, *http.Request, int64, string)            {}
func (nopAudit) LoginFailed(context.Context, *http.Request, string, *int64, string)      {}
func (nopAudit) LoginRateLimited(context.Context, *http.Request, string, string)         {}
func (nopAudit) Logout(context.Context, *http.Request, int64, string)                    {}

// NewHandler constructs the auth handler. If sessionDays is <= 0 the
// default of 7 days (matching RS's rs_setcookie default) is used. The
// session manager, login limiter, and audit recorder are required for
// production wiring; pass nil for any of them in tests to get a no-op
// fallback.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, scrambleKey string, sessionDays int, sessions *SessionManager, limiter *LoginLimiter, audit auditRecorder) *Handler {
	if sessionDays <= 0 {
		sessionDays = 7
	}
	if sessions == nil {
		sessions = NewSessionManager(pool)
	}
	if limiter == nil {
		limiter = NewLoginLimiter()
	}
	if audit == nil {
		audit = nopAudit{}
	}
	return &Handler{
		Pool:        pool,
		Logger:      logger,
		ScrambleKey: scrambleKey,
		SessionDays: sessionDays,
		Sessions:    sessions,
		Limiter:     limiter,
		Audit:       audit,
	}
}

// ---------------------------------------------------------------------------
// /auth/login
// ---------------------------------------------------------------------------

func (h *Handler) Login(
	ctx context.Context,
	req openapi.LoginRequestObject,
) (openapi.LoginResponseObject, error) {
	if req.Body == nil {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "missing credentials"},
		}, nil
	}
	username := strings.TrimSpace(req.Body.Username)
	password := req.Body.Password
	if username == "" || password == "" {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "missing credentials"},
		}, nil
	}

	// Rate limit: two buckets, by IP and by attempted username. Either
	// trip rejects the attempt. The IP key isolates a noisy network
	// from drowning out other users; the username key isolates a
	// single account from being brute-forced from many IPs.
	httpReq := RequestFromContext(ctx)
	ipKey := "ip:" + clientIPKey(httpReq)
	userKey := "user:" + strings.ToLower(username)
	if !h.Limiter.Allow(ipKey) {
		h.Audit.LoginRateLimited(ctx, httpReq, username, ipKey)
		return loginRateLimitedResponse{}, nil
	}
	if !h.Limiter.Allow(userKey) {
		h.Audit.LoginRateLimited(ctx, httpReq, username, userKey)
		return loginRateLimitedResponse{}, nil
	}

	q := New(h.Pool)
	user, err := q.FindUserByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.Audit.LoginFailed(ctx, httpReq, username, nil, "unknown_user")
			// Same response shape as bad-password so we don't leak
			// which usernames exist.
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}
	if user.Password == nil {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "no_password_set")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	}

	if err := VerifyPassword(password, *user.Password, h.ScrambleKey); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "bad_password")
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}

	if user.Approved != 1 {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "not_approved")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account is not approved"},
		}, nil
	}
	if user.AccountExpires.Valid && user.AccountExpires.Time.Before(time.Now()) {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "account_expired")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account has expired"},
		}, nil
	}

	token, sessionInfo, err := h.Sessions.Issue(ctx, user.Ref, httpReq)
	if err != nil {
		return nil, err
	}

	// A successful login should drain both rate-limit buckets so an
	// earlier failed-then-recovered attempt doesn't penalise the
	// legitimate user.
	h.Limiter.Forget(ipKey)
	h.Limiter.Forget(userKey)
	h.Audit.LoginSucceeded(ctx, httpReq, user.Ref, sessionInfo.ID.String())

	current := identityToCurrentUser(&Identity{
		UserRef:    user.Ref,
		Username:   strFromPtr(user.Username),
		Fullname:   user.Fullname,
		Email:      user.Email,
		Usergroup:  user.Usergroup,
		AuthMethod: "session",
	})
	return loginSetCookieResponse{
		token:       token,
		sessionDays: h.SessionDays,
		body:        current,
	}, nil
}

// clientIPKey returns the request's best-guess client IP as a string,
// falling back to "unknown" if there's no usable address. Only used as
// a rate-limiter key, so a coarse fallback is fine.
func clientIPKey(r *http.Request) string {
	addr := addrFromRequest(r)
	if addr == nil {
		return "unknown"
	}
	return addr.String()
}

// loginRateLimitedResponse implements openapi.LoginResponseObject and
// writes a 429 with a JSON body. The strict-server codegen only knows
// about the 200/401 responses we declared in the spec, so we emit the
// 429 by hand.
type loginRateLimitedResponse struct{}

func (loginRateLimitedResponse) VisitLoginResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"too many attempts; try again shortly"}`))
	return nil
}

// loginSetCookieResponse implements openapi.LoginResponseObject and
// sets the rs_session cookie on the way out. Custom response type
// because the generated 200 response doesn't know about cookies.
type loginSetCookieResponse struct {
	token       string
	sessionDays int
	body        openapi.CurrentUser
}

func (r loginSetCookieResponse) VisitLoginResponse(w http.ResponseWriter) error {
	// Build a synthetic *http.Request so the secure-cookie heuristics
	// work uniformly. The actual request scheme/header is unavailable
	// at this layer because of the strict-server abstraction.
	WriteSessionCookie(w, &http.Request{}, r.token, r.sessionDays)
	return openapi.Login200JSONResponse(r.body).VisitLoginResponse(w)
}

// ---------------------------------------------------------------------------
// /auth/logout
// ---------------------------------------------------------------------------

func (h *Handler) Logout(
	ctx context.Context,
	_ openapi.LogoutRequestObject,
) (openapi.LogoutResponseObject, error) {
	// Resolve from the request context (set by the resolver middleware).
	// If no identity, still clear the cookie and return 204 — logout is
	// idempotent. We revoke by cookie value (not user ref) so this only
	// kills *this* session, leaving other devices logged in.
	httpReq := RequestFromContext(ctx)
	cookie := ""
	if httpReq != nil {
		cookie = SessionCookieValue(httpReq)
	}
	if cookie != "" {
		if err := h.Sessions.RevokeByToken(ctx, cookie); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.logout.revoke.error",
				slog.String("err", err.Error()))
			// fall through; still expire cookie
		}
	}
	if id := IdentityFromContext(ctx); id != nil && id.AuthMethod == "session" {
		h.Audit.Logout(ctx, httpReq, id.UserRef, "")
	}
	return logoutClearCookieResponse{}, nil
}

type logoutClearCookieResponse struct{}

func (logoutClearCookieResponse) VisitLogoutResponse(w http.ResponseWriter) error {
	ClearSessionCookie(w, &http.Request{})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// /auth/me
// ---------------------------------------------------------------------------

func (h *Handler) GetCurrentUser(
	ctx context.Context,
	_ openapi.GetCurrentUserRequestObject,
) (openapi.GetCurrentUserResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetCurrentUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	return openapi.GetCurrentUser200JSONResponse(identityToCurrentUser(id)), nil
}

// ---------------------------------------------------------------------------
// /auth/tokens (list, create)
// ---------------------------------------------------------------------------

func (h *Handler) ListApiTokens(
	ctx context.Context,
	_ openapi.ListApiTokensRequestObject,
) (openapi.ListApiTokensResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListApiTokens401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListApiTokensForUser(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListApiTokens200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.ApiTokenSummary{
			Id:         openapi_types.UUID(r.ID.Bytes),
			Name:       r.Name,
			Scopes:     append([]string{}, r.Scopes...),
			CreatedAt:  r.CreatedAt.Time,
			ExpiresAt:  ptrTime(r.ExpiresAt),
			LastUsedAt: ptrTime(r.LastUsedAt),
		})
	}
	return out, nil
}

func (h *Handler) CreateApiToken(
	ctx context.Context,
	req openapi.CreateApiTokenRequestObject,
) (openapi.CreateApiTokenResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	// Spec disallows creating tokens via a token-authenticated session
	// (cookie-only). The OpenAPI spec records this but we must enforce
	// at runtime too — codegen doesn't.
	if id.AuthMethod != "session" {
		return openapi.CreateApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "token creation requires session auth"},
		}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Name) == "" {
		return openapi.CreateApiToken400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "name is required"},
		}, nil
	}

	plaintext, err := NewAPIToken()
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	if req.Body.Scopes != nil {
		scopes = *req.Body.Scopes
	}
	params := CreateApiTokenParams{
		RsUserID:  id.UserRef,
		Name:      strings.TrimSpace(req.Body.Name),
		TokenHash: HashAPIToken(plaintext),
		Scopes:    scopes,
	}
	if req.Body.ExpiresAt != nil {
		params.ExpiresAt = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}

	q := New(h.Pool)
	row, err := q.CreateApiToken(ctx, params)
	if err != nil {
		return nil, err
	}

	return openapi.CreateApiToken201JSONResponse(openapi.ApiTokenCreated{
		Id:         openapi_types.UUID(row.ID.Bytes),
		Name:       row.Name,
		Scopes:     append([]string{}, row.Scopes...),
		CreatedAt:  row.CreatedAt.Time,
		ExpiresAt:  ptrTime(row.ExpiresAt),
		LastUsedAt: ptrTime(row.LastUsedAt),
		Token:      plaintext,
	}), nil
}

// ---------------------------------------------------------------------------
// /auth/tokens/{id} (revoke)
// ---------------------------------------------------------------------------

func (h *Handler) RevokeApiToken(
	ctx context.Context,
	req openapi.RevokeApiTokenRequestObject,
) (openapi.RevokeApiTokenResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.RevokeApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	n, err := q.RevokeApiToken(ctx, RevokeApiTokenParams{
		ID:       pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		RsUserID: id.UserRef,
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return openapi.RevokeApiToken404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "token not found"},
		}, nil
	}
	return openapi.RevokeApiToken204Response{}, nil
}

// ---------------------------------------------------------------------------
// /auth/capabilities, /auth/roles, /auth/me/capabilities, /auth/users/{ref}/role
// ---------------------------------------------------------------------------

func (h *Handler) ListCapabilities(
	ctx context.Context,
	_ openapi.ListCapabilitiesRequestObject,
) (openapi.ListCapabilitiesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListCapabilities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("caps.read") {
		return openapi.ListCapabilities403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "caps.read capability required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListCapabilities(ctx)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListCapabilities200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.Capability{Code: r.Code, Description: r.Description})
	}
	return out, nil
}

func (h *Handler) ListRoles(
	ctx context.Context,
	_ openapi.ListRolesRequestObject,
) (openapi.ListRolesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListRoles401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("roles.read") {
		return openapi.ListRoles403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "roles.read capability required"},
		}, nil
	}
	q := New(h.Pool)
	roles, err := q.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListRoles200JSONResponse, 0, len(roles))
	for _, r := range roles {
		caps, err := q.ListRoleCapabilities(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		role := openapi.Role{
			Id:           openapi_types.UUID(r.ID.Bytes),
			Name:         r.Name,
			Description:  r.Description,
			Capabilities: caps,
		}
		if r.ParentID.Valid {
			p := openapi_types.UUID(r.ParentID.Bytes)
			role.ParentId = &p
		}
		out = append(out, role)
	}
	return out, nil
}

func (h *Handler) GetMyCapabilities(
	ctx context.Context,
	_ openapi.GetMyCapabilitiesRequestObject,
) (openapi.GetMyCapabilitiesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetMyCapabilities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	q := New(h.Pool)
	// The API surface currently exposes a single "role" field; with the
	// multi-role model (00016) we surface the user's first GLOBAL role
	// assignment for that field — team-scoped assignments aren't
	// representable here. The 1.7.B-7 OpenAPI widening will switch this
	// to a roles[] list with optional team scope per entry.
	roleRows, err := q.AssignedRolesForUser(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	var roleOpenapi *openapi.Role
	for _, r := range roleRows {
		if r.TeamID.Valid {
			continue // skip team-scoped assignments for the single-role field
		}
		caps, err := q.ListRoleCapabilities(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		ro := openapi.Role{
			Id:           openapi_types.UUID(r.ID.Bytes),
			Name:         r.Name,
			Description:  r.Description,
			Capabilities: caps,
		}
		if r.ParentID.Valid {
			p := openapi_types.UUID(r.ParentID.Bytes)
			ro.ParentId = &p
		}
		roleOpenapi = &ro
		break
	}

	grants, err := h.fetchSimpleCapList(ctx, `SELECT capability_code FROM user_capability_grants WHERE rs_user_id = $1 ORDER BY capability_code`, id.UserRef)
	if err != nil {
		return nil, err
	}
	revokes, err := h.fetchSimpleCapList(ctx, `SELECT capability_code FROM user_capability_revokes WHERE rs_user_id = $1 ORDER BY capability_code`, id.UserRef)
	if err != nil {
		return nil, err
	}

	caps := append([]string{}, id.Capabilities...) // copy
	resp := openapi.EffectiveCapabilities{
		UserRef:      id.UserRef,
		Capabilities: caps,
		Role:         roleOpenapi,
		Grants:       &grants,
		Revokes:      &revokes,
	}
	return openapi.GetMyCapabilities200JSONResponse(resp), nil
}

func (h *Handler) SetUserRole(
	ctx context.Context,
	req openapi.SetUserRoleRequestObject,
) (openapi.SetUserRoleResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetUserRole401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("users.write") {
		return openapi.SetUserRole403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}
	if req.Body == nil {
		return nil, errors.New("missing body") // strict-server returns 500; the spec says this is required so codegen normally rejects nil
	}

	q := New(h.Pool)
	roleUUID := pgtype.UUID{Bytes: uuid.UUID(req.Body.RoleId), Valid: true}
	// 404 if the role doesn't exist.
	if _, err := q.GetRole(ctx, roleUUID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetUserRole404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "role not found"},
			}, nil
		}
		return nil, err
	}

	// Sets the user's GLOBAL role (replaces any existing global
	// assignment; leaves team-scoped assignments intact). The admin
	// endpoint shape hasn't changed; only the storage semantics did.
	if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
		RsUserID:           req.Ref,
		RoleID:             roleUUID,
		AssignedByRsUserID: &id.UserRef,
	}); err != nil {
		return nil, err
	}
	return openapi.SetUserRole204Response{}, nil
}

// fetchSimpleCapList runs a single-column-of-text query and collects
// the results. Small helper used by GetMyCapabilities for the grant
// and revoke lists; not worth its own sqlc entry.
func (h *Handler) fetchSimpleCapList(ctx context.Context, sql string, userRef int64) ([]string, error) {
	rows, err := h.Pool.Query(ctx, sql, userRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func identityToCurrentUser(id *Identity) openapi.CurrentUser {
	cu := openapi.CurrentUser{
		Ref:        id.UserRef,
		Username:   id.Username,
		Fullname:   id.Fullname,
		Email:      id.Email,
		Usergroup:  id.Usergroup,
		AuthMethod: openapi.CurrentUserAuthMethod(id.AuthMethod),
	}
	return cu
}

func strFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
