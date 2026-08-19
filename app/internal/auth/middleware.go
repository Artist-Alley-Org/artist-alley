// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Identity is what the resolver injects into the request context on a
// successful authentication. nil in the context means the caller is
// anonymous.
//
// Capabilities holds GLOBAL capability codes only (team_id IS NULL in
// the user_capability_* tables). It exists as a flat []string for the
// admin /auth/me/capabilities response and for legacy callers that
// don't need team scope.
//
// scopedCaps holds team-scoped grants, pre-expanded via team_closure so
// each descendant team is a literal entry. Per ADR 0010 Layer 5:
//   - A role assignment scoped to team X grants all of that role's caps
//     to X and (transitively) every descendant of X.
//   - A grant scoped to team X likewise applies to X and descendants.
//
// Pure-memory lookup for Can(code, InTeam(t)) — no closure walk in Go.
type Identity struct {
	UserRef    int64
	Username   string
	Fullname   *string
	Email      *string
	Usergroup  *int64
	AuthMethod string     // "session" or "token"
	TokenID    *uuid.UUID // populated when AuthMethod=="token"
	SessionID  *uuid.UUID // populated when AuthMethod=="session" — lets
	// the /account/sessions endpoint mark the row that's
	// authenticating this request as "current" so the UI can
	// hide its own revoke button (revoking your own current
	// session is a footgun — the next request 401s).
	Capabilities []string // GLOBAL capability codes (closure-expanded, NULL team_id)

	// ImpersonatedBy is the admin user ref that issued this
	// session via /admin/users/{ref}/impersonate (Phase
	// 1.19.A-2). nil = normal session. When non-nil:
	//   - UserRef + Capabilities still reflect the TARGET (so
	//     the admin sees the target's UI exactly).
	//   - Audit recorder picks up the admin separately for
	//     attribution honesty.
	//   - Server refuses dangerous mutations (password change,
	//     starting another impersonation) — defense in depth.
	ImpersonatedBy *int64

	scopedCaps map[string]map[uuid.UUID]struct{} // code -> set of effective team IDs
}

// IsImpersonating reports whether this Identity is operating
// under an admin impersonation session.
func (id *Identity) IsImpersonating() bool {
	return id != nil && id.ImpersonatedBy != nil
}

// SuperAdminCapability bypasses every Can() check. Matches the
// "system.admin" code seeded by migration 00001.
const SuperAdminCapability = "system.admin"

// canQuery accumulates Can() options. Currently just the team scope.
type canQuery struct {
	teamID *uuid.UUID
}

// CanOption configures a Can() check. Use [InTeam] to scope the check
// to a specific team.
type CanOption func(*canQuery)

// InTeam scopes a Can() check to a specific team. The check passes if
// the user holds the capability globally OR holds it for the given
// team (or any ancestor of it; the closure expansion has already been
// done at resolver time).
func InTeam(id uuid.UUID) CanOption {
	return func(q *canQuery) { q.teamID = &id }
}

// Can returns true when this identity is allowed to exercise the given
// capability code in the requested scope.
//
//   - Holding [SuperAdminCapability] globally is a wildcard regardless
//     of the scope asked for.
//   - Without [InTeam]: only global grants pass.
//   - With [InTeam]: a global grant OR a scoped grant that includes
//     the target team passes.
//
// A nil identity is never authorised. An empty capability code is
// never authorised (avoids surprises if a caller forgets to wire one).
func (id *Identity) Can(code string, opts ...CanOption) bool {
	if id == nil || code == "" {
		return false
	}
	// License gate runs BEFORE the SuperAdmin wildcard. SuperAdmin is
	// about USER authorisation; license features are about INSTALL
	// authorisation. A SuperAdmin on a Community install cannot reach
	// enterprise-only caps (sso_ldap, multi_tenant, etc.) — those are
	// install-level features the customer hasn't purchased, and no
	// per-user role can grant them.
	if !capLicenseAllows(code) {
		return false
	}
	// system.admin global wildcard — short-circuit before any scope work.
	for _, c := range id.Capabilities {
		if c == SuperAdminCapability {
			return true
		}
	}
	// Global grant for the requested code — works in any scope.
	for _, c := range id.Capabilities {
		if c == code {
			return true
		}
	}
	// Scoped check: did the caller ask for a team?
	q := canQuery{}
	for _, opt := range opts {
		opt(&q)
	}
	if q.teamID != nil {
		if codeMap, ok := id.scopedCaps[code]; ok {
			if _, ok := codeMap[*q.teamID]; ok {
				return true
			}
		}
	}
	return false
}

// ScopedTeams returns the closure-expanded set of teams on which this
// identity holds `code` as a SCOPED capability, sorted so callers can
// fold it into a stable cache key.
//
// It exists because `scopedCaps` is unexported and [Identity.Can] can
// only answer one team at a time — a caller that needs to test MANY
// rows against the caller's scope (the read gates in
// visibility.FieldsReadable, #939) would otherwise call Can() per row,
// or worse, re-derive the scope in SQL from `user_capability_grants`
// alone and silently miss every capability conferred through a
// team-scoped ROLE assignment.
//
// The returned slice is a fresh copy — the underlying map is shared
// with the per-user capability CACHE, and handing out a reference to it
// would let one request's mutation reach every subsequent request.
//
// Deliberately NOT included:
//
//   - GLOBAL holdings of `code`. This answers "which teams", and a
//     global grant is not a team. Callers test that separately, the way
//     Can() does.
//   - The `system.admin` wildcard, for the same reason.
//
// The license gate runs first, exactly as it does in Can(): a
// capability the install has not licensed confers no scope either.
func (id *Identity) ScopedTeams(code string) []uuid.UUID {
	if id == nil || code == "" || !capLicenseAllows(code) {
		return nil
	}
	set, ok := id.scopedCaps[code]
	if !ok || len(set) == 0 {
		return nil
	}
	out := make([]uuid.UUID, 0, len(set))
	for team := range set {
		out = append(out, team)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
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

// CachedCapSet is the closure-expanded capability state for one
// principal — the result of EffectiveScopedCapabilitiesForUser
// pre-split into the (globalCaps, scopedCaps) shape Identity uses.
// Caching the post-split form means cache hits skip both the DB
// query AND the per-row split loop.
type CachedCapSet struct {
	Global []string
	Scoped map[string]map[uuid.UUID]struct{}
}

// cacheDomainUserCaps is the NOTIFY domain for per-user-ref capability
// sets. Cross-package writers (admin endpoints in other packages that
// mutate role assignments, grants, revokes) call
// [InvalidateUserCaps] to broadcast on this channel.
const cacheDomainUserCaps = "auth.caps.user"

// Resolver wires the auth-resolving middleware to its dependencies.
// Sessions is required for cookie auth; pass nil to fall back to a
// default-configured SessionManager (e.g. in tests).
//
// caps is the in-process LRU + NOTIFY-fed capability cache. nil means
// "no cache" — every request hits the DB query in loadCapabilities.
// Tests that don't care about caching can leave it nil; production
// boots via [NewResolver] which wires it.
type Resolver struct {
	Pool     *pgxpool.Pool
	Logger   *slog.Logger
	Sessions *SessionManager

	// PublicMode reports whether the install serves its public read
	// surface to anonymous callers (#445). Consulted only when a
	// request resolved to no identity AND the path is in
	// PublicSurfaceRoutes.
	//
	// nil means OFF, which denies. That direction is chosen so a
	// forgotten boot wire shows up as "the toggle will not turn on"
	// rather than "the install is public and the toggle says it
	// isn't" — a stuck switch is a support ticket, a silently public
	// install is an incident. See NewResolver, which wires it.
	PublicMode func(ctx context.Context) bool

	caps *cache.Cache[CachedCapSet]
}

// SetPublicMode wires the public-mode reader post-construction, so the
// boot sequence can build the sysconfig Store and the Resolver in
// either order (the Store needs the pool; the Resolver is constructed
// before the sysconfig handler exists).
func (r *Resolver) SetPublicMode(f func(ctx context.Context) bool) {
	r.PublicMode = f
}

// publicModeEnabled is the nil-safe read.
func (r *Resolver) publicModeEnabled(ctx context.Context) bool {
	if r.PublicMode == nil {
		return false
	}
	return r.PublicMode(ctx)
}

// NewResolver constructs a Resolver and (when registry != nil) wires
// up the capability cache. Cache size 10_000 fits ~1MB of resident
// memory for a typical user's resolved cap set and bounds growth on
// federation peers. Larger deployments tune via a config knob later.
func NewResolver(pool *pgxpool.Pool, logger *slog.Logger, sessions *SessionManager, registry *cache.Registry) *Resolver {
	r := &Resolver{
		Pool:     pool,
		Logger:   logger,
		Sessions: sessions,
	}
	if registry != nil {
		r.caps = cache.Register[CachedCapSet](registry, cacheDomainUserCaps, 10_000)
	}
	return r
}

// InvalidateUserCaps broadcasts a cache invalidation for the given
// user's resolved capability set. Call after any DB write that could
// change the user's effective caps — role assignment changes,
// individual cap grants/revokes, role-capability mutations on roles
// the user holds.
//
// This is the cross-package entry point: writers outside the auth
// package (a future grants/revokes admin endpoint, federation
// import) call it with the shared Registry. Within the auth package,
// the Resolver's [InvalidateCaps] method is the lower-latency path —
// it does local-evict immediately AND broadcasts to peers, so the
// next request on the same process sees fresh caps without waiting
// for the NOTIFY round-trip to the LISTEN goroutine.
//
// Safe to call with a nil registry (no-op) so callers don't have to
// nil-guard.
//
// Best-effort: a failed NOTIFY logs (via the underlying Registry.Emit)
// but doesn't propagate. We don't want a user-management write to
// fail because the cache layer is transiently down.
func InvalidateUserCaps(ctx context.Context, registry *cache.Registry, userRef int64) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, cacheDomainUserCaps, strconv.FormatInt(userRef, 10))
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

		// Public-mode gate (#445). Reached only when neither the token
		// nor the cookie path resolved an identity, so an
		// authenticated caller can never be affected by this in
		// either toggle state — the two returns above are the only
		// exits for them.
		//
		// Scoped to PublicSurfaceRoutes, not to every anonymous
		// request. /auth/login, /setup/*, /appearance and the rest of
		// the surface an operator needs to reach BEFORE they have an
		// identity are outside that table and pass through here
		// untouched. That is the constraint that ranks above the
		// feature: a public-mode gate that can lock somebody out of
		// their own install is worse than no public mode.
		if !r.publicModeEnabled(ctx) && IsPublicSurface(req.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			return
		}

		next.ServeHTTP(w, req)
	})
}

// LoadIdentity builds the Identity the request middleware would hand a
// handler for this user — global capabilities AND team-scoped grants,
// closure-expanded — without going through a session or a token.
//
// It exists because `scopedCaps` is unexported and there is no other
// way for a gate outside this package to be exercised against a REAL
// scoped grant. Hand-constructing an Identity literal, which is what
// every other package's tests do, can only ever populate the global
// list — so a test for "a grant scoped to team X reaches X's
// descendants" written that way would assert against a map the test
// itself built rather than against the closure expansion the resolver
// performs. That is the difference between testing the gate and
// testing the fixture (#930).
//
// Same failure mode as loadCapabilities: a lookup error leaves the cap
// sets empty rather than failing, so the caller can do nothing
// privileged.
func (r *Resolver) LoadIdentity(ctx context.Context, userRef int64) *Identity {
	id := &Identity{UserRef: userRef, AuthMethod: "session"}
	r.loadCapabilities(ctx, New(r.Pool), id)
	return id
}

// loadCapabilities populates id.Capabilities (global) and id.scopedCaps
// (team-scoped, closure-expanded). Reads through the caps cache when
// present; on miss runs EffectiveScopedCapabilitiesForUser and
// populates the cache. Invalidation is via [InvalidateUserCaps],
// which any package mutating the user's role/grant/revoke state
// calls after commit.
//
// A failure here is non-fatal — we log it and proceed with empty cap
// sets, which means the user can do nothing privileged. That's safer
// than failing the whole request because of a transient DB error in
// the cap lookup.
func (r *Resolver) loadCapabilities(ctx context.Context, q *Queries, id *Identity) {
	if id == nil {
		return
	}
	key := strconv.FormatInt(id.UserRef, 10)
	if r.caps != nil {
		if v, ok := r.caps.Get(key); ok {
			id.Capabilities = v.Global
			id.scopedCaps = v.Scoped
			return
		}
	}
	rows, err := q.EffectiveScopedCapabilitiesForUser(ctx, id.UserRef)
	if err != nil {
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.caps.load.error",
			slog.Int64("user_ref", id.UserRef),
			slog.String("err", err.Error()),
		)
		return
	}
	// Split into global (team_id NULL) and scoped (team_id non-NULL,
	// pre-expanded via team_closure on the SQL side).
	globalSet := make(map[string]struct{}, len(rows))
	scoped := make(map[string]map[uuid.UUID]struct{})
	for _, row := range rows {
		if !row.TeamID.Valid {
			globalSet[row.Code] = struct{}{}
			continue
		}
		team := uuid.UUID(row.TeamID.Bytes)
		set, ok := scoped[row.Code]
		if !ok {
			set = make(map[uuid.UUID]struct{})
			scoped[row.Code] = set
		}
		set[team] = struct{}{}
	}
	caps := make([]string, 0, len(globalSet))
	for code := range globalSet {
		caps = append(caps, code)
	}
	sort.Strings(caps)
	id.Capabilities = caps
	id.scopedCaps = scoped
	if r.caps != nil {
		r.caps.Add(key, CachedCapSet{Global: caps, Scoped: scoped})
	}
}

// IsAnonymous reports whether the identity represents an unauthenticated
// request (sentinel UserRef=0, AuthMethod="anonymous"). Handlers that
// want to require authenticated users should check this AND nil.
func (id *Identity) IsAnonymous() bool {
	return id != nil && id.AuthMethod == "anonymous"
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

	user, err := r.loadUser(ctx, q, row.UserRef)
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
	sessID := info.ID
	id.SessionID = &sessID
	if info.ImpersonatedBy != nil {
		v := *info.ImpersonatedBy
		id.ImpersonatedBy = &v
	}
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
