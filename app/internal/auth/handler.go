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
	Pool         *pgxpool.Pool
	Logger       *slog.Logger
	ScrambleKey  string
	SessionDays  int    // how long the rs_session cookie lives; matches RS default
	tokenPrefix  string // overridable in tests
}

// NewHandler constructs the auth handler. If sessionDays is <= 0 the
// default of 7 days (matching RS's rs_setcookie default) is used.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, scrambleKey string, sessionDays int) *Handler {
	if sessionDays <= 0 {
		sessionDays = 7
	}
	return &Handler{
		Pool:        pool,
		Logger:      logger,
		ScrambleKey: scrambleKey,
		SessionDays: sessionDays,
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

	q := New(h.Pool)
	user, err := q.FindUserByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Same response shape as bad-password so we don't leak
			// which usernames exist.
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}
	if user.Password == nil {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	}

	if err := VerifyPassword(password, *user.Password, h.ScrambleKey); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}

	if user.Approved != 1 {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account is not approved"},
		}, nil
	}
	if user.AccountExpires.Valid && user.AccountExpires.Time.Before(time.Now()) {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account has expired"},
		}, nil
	}

	token, err := NewSessionToken()
	if err != nil {
		return nil, err
	}
	if err := q.SetUserSession(ctx, SetUserSessionParams{
		Session: &token,
		Ref:     user.Ref,
	}); err != nil {
		return nil, err
	}

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
	// idempotent.
	id := IdentityFromContext(ctx)
	if id != nil && id.AuthMethod == "session" {
		q := New(h.Pool)
		if err := q.ClearUserSession(ctx, id.UserRef); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.logout.clear_session.error",
				slog.String("err", err.Error()))
			// fall through; still expire cookie
		}
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
