package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity is what the resolver injects into the request context on a
// successful authentication. nil in the context means the caller is
// anonymous.
type Identity struct {
	UserRef      int64
	Username     string
	Fullname     *string
	Email        *string
	Usergroup    *int64
	AuthMethod   string     // "session" or "token"
	TokenID      *uuid.UUID // populated when AuthMethod=="token"
	Capabilities []string   // effective capability codes resolved at request start
}

// SuperAdminCapability bypasses every Can() check. Matches the
// "system.admin" code seeded by migration 00002.
const SuperAdminCapability = "system.admin"

// Can returns true when this identity is allowed to exercise the given
// capability code. Holding [SuperAdminCapability] is a wildcard.
//
// A nil identity is never authorised.
func (id *Identity) Can(code string) bool {
	if id == nil || code == "" {
		return false
	}
	for _, c := range id.Capabilities {
		if c == code || c == SuperAdminCapability {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	identityKey ctxKey = iota
	requestKey
)

// WithRequest stashes the incoming *http.Request in ctx so handlers
// invoked through the strict-server abstraction (which strips access
// to the raw request) can still see remote IP, User-Agent, etc. The
// ResolveIdentity middleware sets this on every request.
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestKey, r)
}

// RequestFromContext returns the request stashed by WithRequest, or
// nil if none. Handlers should tolerate nil — tests don't always
// install the resolver middleware.
func RequestFromContext(ctx context.Context) *http.Request {
	if v, ok := ctx.Value(requestKey).(*http.Request); ok {
		return v
	}
	return nil
}

// IdentityFromContext returns the resolved Identity for the request,
// or nil if the caller is anonymous. Handlers call this after the
// ResolveIdentity middleware has run.
func IdentityFromContext(ctx context.Context) *Identity {
	if v, ok := ctx.Value(identityKey).(*Identity); ok {
		return v
	}
	return nil
}

// WithIdentity is exposed mostly so tests can inject a fake Identity
// without going through the resolver.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// Resolver wires the auth-resolving middleware to its dependencies.
// Sessions is required for cookie auth; pass nil to fall back to a
// default-configured SessionManager (e.g. in tests).
type Resolver struct {
	Pool     *pgxpool.Pool
	Logger   *slog.Logger
	Sessions *SessionManager
}

func (r *Resolver) sessions() *SessionManager {
	if r.Sessions != nil {
		return r.Sessions
	}
	r.Sessions = NewSessionManager(r.Pool)
	return r.Sessions
}

// ResolveIdentity is the middleware: it tries the bearer token first,
// then the rs_session cookie, and stores any resolved Identity in the
// request context. Anonymous requests pass through with no Identity
// (the downstream handler or a RequireAuth middleware decides whether
// that's OK).
//
// We intentionally never return an error from the middleware itself —
// a malformed cookie is just "anonymous". Authentication failures are
// the handler's call (most of our endpoints will use RequireAuth, but
// some — login, public reads — must allow anonymous).
func (r *Resolver) ResolveIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Stash the raw request so downstream handlers (which the
		// strict-server abstraction wraps) can still see IP, UA,
		// headers without us having to thread an explicit param.
		ctx := WithRequest(req.Context(), req)
		req = req.WithContext(ctx)
		queries := New(r.Pool)

		if tok, ok := ExtractBearerToken(req.Header); ok && LooksLikeAPIToken(tok) {
			if id, err := r.resolveByToken(ctx, queries, tok); err == nil {
				r.loadCapabilities(ctx, queries, id)
				next.ServeHTTP(w, req.WithContext(WithIdentity(ctx, id)))
				return
			} else if !errors.Is(err, pgx.ErrNoRows) {
				r.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.token.error", slog.String("err", err.Error()))
			}
		}

		if cookie := SessionCookieValue(req); cookie != "" {
			if id, err := r.resolveBySession(ctx, queries, cookie); err == nil {
				r.loadCapabilities(ctx, queries, id)
				next.ServeHTTP(w, req.WithContext(WithIdentity(ctx, id)))
				return
			} else if !errors.Is(err, pgx.ErrNoRows) {
				r.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.session.error", slog.String("err", err.Error()))
			}
		}

		next.ServeHTTP(w, req)
	})
}

// loadCapabilities populates id.Capabilities with the user's effective
// capability set. A failure here is non-fatal — we log it and proceed
// with an empty capability set, which means the user can do nothing
// privileged. That's safer than failing the whole request because of a
// transient DB error in the cap lookup.
func (r *Resolver) loadCapabilities(ctx context.Context, q *Queries, id *Identity) {
	if id == nil {
		return
	}
	caps, err := q.EffectiveCapabilitiesForUser(ctx, id.UserRef)
	if err != nil {
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.caps.load.error",
			slog.Int64("user_ref", id.UserRef),
			slog.String("err", err.Error()),
		)
		return
	}
	id.Capabilities = caps
}

func (r *Resolver) resolveByToken(ctx context.Context, q *Queries, plaintext string) (*Identity, error) {
	row, err := q.FindActiveApiToken(ctx, HashAPIToken(plaintext))
	if err != nil {
		return nil, err
	}
	// Best-effort touch; never blocks the request.
	go func(id pgtype.UUID) {
		tq := New(r.Pool)
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tq.TouchApiToken(ctx2, id)
	}(row.ID)

	user, err := r.loadUser(ctx, q, row.RsUserID)
	if err != nil {
		return nil, err
	}

	tokenID := uuid.UUID(row.ID.Bytes)
	user.AuthMethod = "token"
	user.TokenID = &tokenID
	return user, nil
}

func (r *Resolver) resolveBySession(ctx context.Context, q *Queries, sessionToken string) (*Identity, error) {
	info, err := r.sessions().Lookup(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	id, err := r.loadUser(ctx, q, info.UserRef)
	if err != nil {
		return nil, err
	}
	id.AuthMethod = "session"
	// Best-effort: bump last_used_at so the session keeps living and
	// the next idle-timeout check uses now as the baseline. Done in a
	// goroutine so it never blocks the request.
	go func(sessionID uuid.UUID) {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.sessions().Touch(ctx2, sessionID)
	}(info.ID)
	return id, nil
}

// loadUser fetches the minimum the request-context Identity needs,
// given a user ref. Used by the token path; the session path already
// joined against user.
func (r *Resolver) loadUser(ctx context.Context, q *Queries, userRef int64) (*Identity, error) {
	// We don't have a "find by ref" query because every other path
	// already returns the user. The token resolver loads from the
	// FindActiveApiToken row's join... actually that query doesn't
	// join. Quickest fix: a tiny inline query here. We'll graduate
	// this to its own sqlc query if we end up needing it elsewhere.
	const sql = `SELECT username, fullname, email, usergroup FROM "user" WHERE ref = $1`
	var (
		username, fullname, email *string
		usergroup                 *int64
	)
	if err := r.Pool.QueryRow(ctx, sql, userRef).Scan(&username, &fullname, &email, &usergroup); err != nil {
		return nil, err
	}
	id := &Identity{
		UserRef:   userRef,
		Fullname:  fullname,
		Email:     email,
		Usergroup: usergroup,
	}
	if username != nil {
		id.Username = *username
	}
	return id, nil
}

// RequireAuth is a middleware that short-circuits with 401 if no
// Identity is in the request context. Mount it around any route that
// must not serve anonymous callers.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IdentityFromContext(r.Context()) == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
