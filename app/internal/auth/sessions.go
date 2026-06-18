package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionManager is the orchestrator behind the sessions table: issue,
// lookup, touch, revoke. Handlers go through this rather than poking
// the queries directly so audit emission + idle/expiry policy stay
// consistent.
//
// Phase 1.17.B dropped the PHP-coexistence dual-write to
// user.session / user.logged_in (migration 00004) — those columns are
// gone, and the only state the session lives in is the sessions table
// (id uuid PRIMARY KEY, looked up by sha256(cookie)).
type SessionManager struct {
	Pool *pgxpool.Pool

	// IdleTimeout is the inactivity window. A session whose
	// last_used_at is older than NOW()-IdleTimeout is treated as
	// expired even if its expires_at hasn't fired yet. Zero means
	// no idle timeout (rely solely on expires_at).
	IdleTimeout time.Duration

	// DefaultLifetime is the hard cap baked into expires_at on new
	// sessions. Zero means "no hard cap" — only IdleTimeout enforces
	// session age. The legacy default is 30 minutes of idle, no hard cap;
	// we match that out of the box.
	DefaultLifetime time.Duration
}

// NewSessionManager returns a manager with sensible defaults
// (30 minutes idle, no hard cap).
func NewSessionManager(pool *pgxpool.Pool) *SessionManager {
	return &SessionManager{
		Pool:            pool,
		IdleTimeout:     30 * time.Minute,
		DefaultLifetime: 0,
	}
}

// SessionInfo is the resolved session, returned by Lookup so callers
// don't have to thread the sqlc row type around.
type SessionInfo struct {
	ID         uuid.UUID
	UserRef    int64
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  *time.Time
	IP         string
	UserAgent  string
}

// Issue creates a new session for userRef and returns the plaintext
// cookie value to set on the response. The plaintext never reaches
// the DB — only its sha256 lives there.
//
// Also touches "user".last_active so downstream "active users"
// surfaces (admin list, leaderboards) see fresh logins. The touch
// failure is logged-and-swallowed: a session is real even if the
// observability column lags.
func (m *SessionManager) Issue(ctx context.Context, userRef int64, r *http.Request) (token string, info SessionInfo, err error) {
	token, err = NewSessionToken()
	if err != nil {
		return "", SessionInfo{}, fmt.Errorf("auth: mint session token: %w", err)
	}
	hash := hashCookieValue(token)

	var expires pgtype.Timestamptz
	if m.DefaultLifetime > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().Add(m.DefaultLifetime), Valid: true}
	}

	q := New(m.Pool)
	ipPtr := addrFromRequest(r)
	row, err := q.InsertSession(ctx, InsertSessionParams{
		UserRef:   userRef,
		TokenHash: hash,
		ExpiresAt: expires,
		Ip:        ipPtr,
		UserAgent: userAgentPtr(r),
	})
	if err != nil {
		return "", SessionInfo{}, fmt.Errorf("auth: insert session: %w", err)
	}

	// last_active is observability; failure here doesn't void the
	// session we just minted. Best-effort.
	_ = q.TouchUserLastActive(ctx, userRef)

	return token, sessionInfoFromInsert(row), nil
}

// Lookup resolves an incoming cookie to a session and the user it
// belongs to. Returns (nil, pgx.ErrNoRows) when the cookie matches no
// active session.
//
// Phase 1.17.B dropped the PHP-issued-session fallback (which mirrored
// "user".session rows into the sessions table on lookup). The legacy
// PHP pages are gone; the sessions table is now the only source of
// truth for an active session.
func (m *SessionManager) Lookup(ctx context.Context, plaintext string) (*SessionInfo, error) {
	if plaintext == "" {
		return nil, pgx.ErrNoRows
	}
	hash := hashCookieValue(plaintext)
	q := New(m.Pool)

	row, err := q.FindActiveSession(ctx, hash)
	if err != nil {
		return nil, err
	}
	// Idle-timeout check lives here so it's configurable per
	// request without changing the query.
	if m.IdleTimeout > 0 && time.Since(row.LastUsedAt.Time) > m.IdleTimeout {
		// Best-effort revoke — don't block the caller.
		_ = q.RevokeSession(ctx, row.ID)
		return nil, pgx.ErrNoRows
	}
	info := &SessionInfo{
		ID:         uuid.UUID(row.ID.Bytes),
		UserRef:    row.UserRef,
		CreatedAt:  row.CreatedAt.Time,
		LastUsedAt: row.LastUsedAt.Time,
	}
	if row.ExpiresAt.Valid {
		t := row.ExpiresAt.Time
		info.ExpiresAt = &t
	}
	if row.Ip != nil {
		info.IP = row.Ip.String()
	}
	if row.UserAgent != nil {
		info.UserAgent = *row.UserAgent
	}
	return info, nil
}

// Touch bumps last_used_at on every authenticated request. Cheap, and
// safe to call best-effort (errors are swallowed by the caller).
func (m *SessionManager) Touch(ctx context.Context, id uuid.UUID) error {
	q := New(m.Pool)
	return q.TouchSession(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// RevokeAllForUser cascade-revokes every active session a user has.
// Phase 1.17.A — fired by users.SetAdminUserStatus when a transition
// moves the user OUT OF UserStateActive. Returns rows-affected so
// the audit log can record the cascade size. Idempotent — re-running
// on a user with no active sessions returns 0 + nil.
func (m *SessionManager) RevokeAllForUser(ctx context.Context, userRef int64) (int64, error) {
	q := New(m.Pool)
	return q.RevokeAllSessionsForUser(ctx, userRef)
}

// RevokeByToken expires the session represented by the given plaintext
// cookie. Idempotent. Used by /auth/logout.
//
// Phase 1.17.B dropped the dual-clear of "user".session — that column
// is gone; the sessions row is the only state.
func (m *SessionManager) RevokeByToken(ctx context.Context, plaintext string) error {
	if plaintext == "" {
		return nil
	}
	q := New(m.Pool)
	hash := hashCookieValue(plaintext)
	return q.RevokeSessionByToken(ctx, hash)
}

// hashCookieValue is the one-way hash used as the sessions.token_hash
// primary key. sha256 is plenty — the input is a 144-bit random token
// already, so we're really just using the hash for length normalization
// and to avoid storing the live cookie value in plaintext.
func hashCookieValue(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// addrFromRequest extracts the best-guess client IP from r, honouring
// X-Forwarded-For (first hop) when nginx forwards it. Returns nil when
// we can't make sense of any header.
func addrFromRequest(r *http.Request) *netip.Addr {
	if r == nil {
		return nil
	}
	raw := r.Header.Get("X-Forwarded-For")
	if raw != "" {
		raw = strings.TrimSpace(strings.SplitN(raw, ",", 2)[0])
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return nil
	}
	return &addr
}

// userAgentPtr returns r's User-Agent header as a *string (sqlc emits
// *string for nullable TEXT columns). Empty values become nil so we
// don't write empty strings to the DB.
func userAgentPtr(r *http.Request) *string {
	if r == nil {
		return nil
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		return nil
	}
	return &ua
}

func sessionInfoFromInsert(row InsertSessionRow) SessionInfo {
	info := SessionInfo{
		ID:         uuid.UUID(row.ID.Bytes),
		UserRef:    row.UserRef,
		CreatedAt:  row.CreatedAt.Time,
		LastUsedAt: row.LastUsedAt.Time,
	}
	if row.ExpiresAt.Valid {
		t := row.ExpiresAt.Time
		info.ExpiresAt = &t
	}
	return info
}
